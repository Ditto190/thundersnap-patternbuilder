// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

//go:build e2e

// Package e2e contains end-to-end tests for thundersnap VM mode.
package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// vmSession encapsulates a running VM test session with all its components.
type vmSession struct {
	t             *testing.T
	virtiofsdCmd  *exec.Cmd
	passtCmd      *exec.Cmd
	chvCmd        *exec.Cmd
	chvPty        *os.File
	eventReadPipe *os.File
	vsockSock     string
	virtiofsSock  string
	passtSock     string
	vmLogs        *vmConsoleMonitor
	vmPanicked    chan struct{}
	vmExited      chan error
}

// startVM starts a VM with the given configuration and returns a vmSession.
// The caller must call session.cleanup() when done.
func startVM(t *testing.T, env *testEnv, framePath, vmDir string, memoryMB int, cmdline string) (*vmSession, error) {
	t.Helper()

	absFramePath, err := filepath.Abs(framePath)
	if err != nil {
		return nil, fmt.Errorf("abs path: %w", err)
	}

	// Create unique socket paths
	sessionID := fmt.Sprintf("%d%d", os.Getpid(), time.Now().UnixNano())
	virtiofsSock := filepath.Join("/tmp", fmt.Sprintf("virtiofs-vm-%s.sock", sessionID))
	vsockSock := filepath.Join("/tmp", fmt.Sprintf("vsock-vm-%s.sock", sessionID))
	passtSock := filepath.Join("/tmp", fmt.Sprintf("passt-vm-%s.sock", sessionID))

	session := &vmSession{
		t:            t,
		virtiofsSock: virtiofsSock,
		vsockSock:    vsockSock,
		passtSock:    passtSock,
		vmPanicked:   make(chan struct{}),
		vmExited:     make(chan error, 1),
	}

	// Start virtiofsd via the shared helper, which always passes
	// --modcaps=-setfcap (matching thundersnap.StartVM). Without that flag,
	// virtiofsd's drop_capabilities() capset()s CAP_SETFCAP, which is absent
	// from many container runtimes' bounding sets; virtiofsd then exits(1)
	// immediately and the VM never boots.
	cmd, err := startVirtiofsd(virtiofsSock, absFramePath)
	if err != nil {
		session.cleanup()
		return nil, err
	}
	session.virtiofsdCmd = cmd

	// Start passt
	session.passtCmd = exec.Command("passt",
		"--socket", passtSock,
		"--vhost-user",
		"--foreground",
		"--quiet",
		"-a", "10.0.2.15",
		"-g", "10.0.2.2",
		"-D", "none",
	)
	session.passtCmd.Stderr = os.Stderr
	if err := session.passtCmd.Start(); err != nil {
		session.cleanup()
		return nil, fmt.Errorf("start passt: %w", err)
	}

	// Wait for passt socket
	if !waitForSocket(passtSock, 5*time.Second) {
		session.cleanup()
		return nil, fmt.Errorf("passt socket not created")
	}

	// Create pipe for event monitor
	eventReadPipe, eventWritePipe, err := os.Pipe()
	if err != nil {
		session.cleanup()
		return nil, fmt.Errorf("create event pipe: %w", err)
	}
	session.eventReadPipe = eventReadPipe

	// Start cloud-hypervisor
	chvPath := filepath.Join(vmDir, "cloud-hypervisor")
	kernelPath := filepath.Join(vmDir, "vmlinux")

	session.chvCmd = exec.Command(chvPath,
		"--kernel", kernelPath,
		"--cpus", "boot=1",
		"--memory", fmt.Sprintf("size=%dM,shared=on", memoryMB),
		"--fs", fmt.Sprintf("tag=rootfs,socket=%s", virtiofsSock),
		"--net", fmt.Sprintf("vhost_user=true,socket=%s,num_queues=2", passtSock),
		"--cmdline", cmdline,
		"--serial", "tty",
		"--console", "off",
		"--vsock", fmt.Sprintf("cid=3,socket=%s", vsockSock),
		"--pvpanic",
		"--event-monitor", "fd=3",
	)
	session.chvCmd.ExtraFiles = []*os.File{eventWritePipe}

	// Start with PTY for serial console
	chvPty, err := startWithPty(session.chvCmd)
	if err != nil {
		eventReadPipe.Close()
		eventWritePipe.Close()
		session.cleanup()
		return nil, fmt.Errorf("start cloud-hypervisor: %w", err)
	}
	session.chvPty = chvPty

	// Close write end in parent
	eventWritePipe.Close()

	// Monitor VM process exit
	go func() {
		session.vmExited <- session.chvCmd.Wait()
	}()

	// Monitor for panic events
	go monitorVMEvents(t, eventReadPipe, session.vmPanicked)

	// Collect VM console output
	session.vmLogs = &vmConsoleMonitor{}
	go session.vmLogs.monitor(t, chvPty)

	return session, nil
}

// cleanup terminates all VM processes and removes sockets.
func (s *vmSession) cleanup() {
	if s.chvCmd != nil && s.chvCmd.Process != nil {
		s.chvCmd.Process.Kill()
		s.chvCmd.Wait()
	}
	if s.chvPty != nil {
		s.chvPty.Close()
	}
	if s.eventReadPipe != nil {
		s.eventReadPipe.Close()
	}
	if s.virtiofsdCmd != nil && s.virtiofsdCmd.Process != nil {
		s.virtiofsdCmd.Process.Kill()
		s.virtiofsdCmd.Wait()
	}
	if s.passtCmd != nil && s.passtCmd.Process != nil {
		s.passtCmd.Process.Kill()
		s.passtCmd.Wait()
	}
	os.Remove(s.virtiofsSock)
	os.Remove(s.vsockSock)
	os.Remove(s.passtSock)
	// Also remove port-specific vsock sockets
	os.Remove(fmt.Sprintf("%s_%d", s.vsockSock, 5222))
	os.Remove(fmt.Sprintf("%s_%d", s.vsockSock, 5223))
}

// waitForVshd waits for vshd to become ready by trying to connect via vsock.
func (s *vmSession) waitForVshd(timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		// Check if VM panicked or exited
		select {
		case <-s.vmPanicked:
			return "", fmt.Errorf("VM kernel panic detected\n\nConsole:\n%s", s.vmLogs.output())
		case err := <-s.vmExited:
			return "", fmt.Errorf("VM exited unexpectedly: %v\n\nConsole:\n%s", err, s.vmLogs.output())
		default:
		}

		// Try to connect to vshd via vsock handshake
		if err := tryVsockConnect(s.vsockSock, 5222); err == nil {
			return s.vsockSock, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return "", fmt.Errorf("vshd did not become ready\n\nConsole:\n%s", s.vmLogs.output())
}

// waitForSocket waits for a socket file to exist.
func waitForSocket(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// prepareVMFrame creates a frame suitable for VM testing with ts and vshd binaries.
func prepareVMFrame(t *testing.T, env *testEnv, name string) string {
	t.Helper()

	baseSnap := env.createBaseSnapshot()
	framePath := filepath.Join(env.fsDir, "testuser", name)

	if err := os.MkdirAll(filepath.Dir(framePath), 0755); err != nil {
		t.Fatalf("mkdir frame parent: %v", err)
	}

	cmd := exec.Command("btrfs", "subvolume", "snapshot",
		filepath.Join(env.snapshotsDir, baseSnap), framePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot: %v\n%s", err, out)
	}

	// Copy ts binary
	tsDst := filepath.Join(framePath, "bin/ts")
	if err := copyFile(env.tsBinary, tsDst); err != nil {
		t.Fatalf("copy ts to frame: %v", err)
	}

	// Copy vshd binary
	vshdBinary := env.requireBinary("vshd")
	vshdDst := filepath.Join(framePath, "sbin/vshd")
	if err := os.MkdirAll(filepath.Dir(vshdDst), 0755); err != nil {
		t.Fatalf("mkdir sbin: %v", err)
	}
	if err := copyFile(vshdBinary, vshdDst); err != nil {
		t.Fatalf("copy vshd to frame: %v", err)
	}

	// Copy su binary - vshd uses "su" to switch users when running commands
	if su, err := exec.LookPath("su"); err == nil {
		suDst := filepath.Join(framePath, "bin/su")
		if err := copyFile(su, suDst); err != nil {
			t.Fatalf("Failed to copy su: %v", err)
		}
		t.Logf("Copied su from %s to %s", su, suDst)
	} else {
		t.Fatalf("su binary not found in PATH: %v", err)
	}

	return framePath
}

// standardVMCmdline returns the kernel command line for a standard VM test.
// Uses kernel IP autoconfiguration (ip=) instead of manual ip commands because
// the test container doesn't have the ip binary. This matches thundersnap/vm.go.
func standardVMCmdline() string {
	return vmCmdlineWithHostname("thundersnap")
}

// vmCmdlineWithHostname returns a kernel command line with the specified hostname.
// The hostname is passed via the kernel IP autoconfig ip= parameter:
// ip=<client-ip>::<gw-ip>:<netmask>:<hostname>:<device>:<autoconf>
//
// ts is run directly as the kernel's init (PID 1) so it can perform mount
// setup — matches thundersnap/vm.go. The old init=/bin/sh -c 'exec /bin/ts ...'
// form tripped the pid-1 safety gate in drop-caps-and-run.
func vmCmdlineWithHostname(hostname string) string {
	return fmt.Sprintf(`console=ttyS0 panic=1 rootfstype=virtiofs root=rootfs rw ip=10.0.2.15::10.0.2.2:255.255.255.0:%s:eth0:off init=/bin/ts -- drop-caps-and-run --vsock /bin/sh -c "echo nameserver 8.8.8.8 > /etc/resolv.conf; exec /sbin/vshd"`, hostname)
}

// TestVMLaunchSuccess tests that a VM launches successfully with sufficient memory.
// TestVMLaunchInsufficientMemory tests that a VM fails gracefully with insufficient memory.
// Note: cloud-hypervisor may accept very low memory values but the VM won't boot properly.
func TestVMLaunchInsufficientMemory(t *testing.T) {
	env := newTestEnv(t)
	vmDir := requireVMDeps(t)

	framePath := prepareVMFrame(t, env, "vm-low-memory")

	// Try to start VM with only 64MB - way too little for a Linux kernel
	// We expect the VM to either fail to start or crash during boot.
	session, err := startVM(t, env, framePath, vmDir, 64, standardVMCmdline())
	if err != nil {
		// VM failed to start - this is acceptable
		t.Logf("VM failed to start with 64MB (expected): %v", err)
		return
	}
	defer session.cleanup()

	// If VM did start, it should fail during boot or vshd won't come up
	_, err = session.waitForVshd(5 * time.Second)
	if err != nil {
		// Expected - VM couldn't boot properly
		t.Logf("VM with 64MB failed to become ready (expected): %v", err)
		return
	}

	// If vshd came up with 64MB, that's unexpected but not necessarily wrong
	t.Log("VM surprisingly became ready with 64MB memory")
}

// TestVMVirtiofsSharing tests that virtiofs filesystem sharing works correctly.
// TestVMVshdCommunication tests that vshd communication over vsock works correctly.
// TestVMNetworkingPasst tests that VM networking via passt works.
// TestVMHostname tests that the hostname is correctly set via kernel IP autoconfig.
// TestVMProcessIsolation tests that the VM is properly isolated from the host.
// TestVMGracefulShutdown tests that a VM shuts down cleanly.
// TestVMPanicRecoveryTimeout tests panic recovery with various timeout scenarios.
// This extends the basic panic test by verifying timing.
func TestVMPanicRecoveryTimeout(t *testing.T) {
	env := newTestEnv(t)
	vmDir := requireVMDeps(t)

	framePath := prepareVMFrame(t, env, "vm-panic-timeout")

	// Create a cmdline that triggers a panic via sysrq
	panicCmdline := `console=ttyS0 panic=1 rootfstype=virtiofs root=rootfs rw init=/bin/sh -- -c "mount -t proc proc /proc; echo 1 > /proc/sys/kernel/sysrq; echo c > /proc/sysrq-trigger"`

	startTime := time.Now()

	session, err := startVM(t, env, framePath, vmDir, 512, panicCmdline)
	if err != nil {
		t.Fatalf("Failed to start VM: %v", err)
	}
	defer session.cleanup()

	// Wait for panic to be detected
	select {
	case <-session.vmPanicked:
		elapsed := time.Since(startTime)
		t.Logf("Panic detected after %v", elapsed)

		// With panic=1, the kernel should reboot within ~1 second of panic
		// Allow some extra time for boot and panic trigger
		if elapsed > 10*time.Second {
			t.Errorf("Panic detection took too long: %v (expected < 10s)", elapsed)
		}
	case err := <-session.vmExited:
		elapsed := time.Since(startTime)
		t.Logf("VM exited after %v: %v", elapsed, err)
		// This is also acceptable - panic=1 causes reboot which may exit cloud-hypervisor
	case <-time.After(10 * time.Second):
		t.Fatalf("Panic not detected within 10 seconds\n\nConsole:\n%s", session.vmLogs.output())
	}

	t.Log("VM panic recovery timing verified")
}

// TestVMConcurrentSessions tests running multiple commands concurrently via vshd.
// TestVMUserSwitching tests that vshd correctly runs commands as root.
// Note: running as non-root users requires the su binary, which has dynamic
// library dependencies not available in our minimal test container.
// runVshCommandWithStdin runs a non-PTY command in the VM with input data,
// returning combined stdout+stderr via the shared vshdproto TLV helper.
func runVshCommandWithStdin(vsockSock, user, stdin string, args ...string) (string, error) {
	return runVshdCommand(vsockSock, "", user, stdin, args...)
}

// vmEventMonitor is used by tests to detect VM events.
type vmEventMonitor struct {
	mu       sync.Mutex
	events   []vmEvent
	panicked bool
}

type vmEvent struct {
	Source string `json:"source"`
	Event  string `json:"event"`
}

func (m *vmEventMonitor) monitor(r io.Reader) {
	decoder := json.NewDecoder(r)
	for {
		var event vmEvent
		if err := decoder.Decode(&event); err != nil {
			return
		}
		m.mu.Lock()
		m.events = append(m.events, event)
		if event.Source == "guest" && event.Event == "panic" {
			m.panicked = true
		}
		m.mu.Unlock()
	}
}

func (m *vmEventMonitor) hasPanicked() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.panicked
}

// Helper to get host PID count for isolation test
func getHostPIDCount() int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	count := 0
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err == nil {
			count++
		}
	}
	return count
}
