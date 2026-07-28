// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

//go:build e2e

// vm_test.go is the W5 "VM (vmx) session matrix" workflow: it connects to a
// real thundersnapd with the vm/ isolation prefix so the daemon boots a
// cloud-hypervisor VM and runs the container inside it, then asserts the
// same observable behaviors as the container matrix (echo, root vs non-root
// write-to-/, working directory) plus a PTY size check. It supersedes
// not_e2e/ssh_vm_test.go (TestSSHVmBasic/UserRoot/UserNonRoot/WorkingDir)
// and the VM half of not_e2e/ssh_cwd_test.go, which hand-spawned the VM.
//
// These tests require VM dependencies (cloud-hypervisor, vmlinux, virtiofsd,
// passt, /dev/kvm). They cannot run inside containers without KVM
// passthrough; in such environments they fail (e2e tests never skip).
package e2e

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// startVMDaemon starts a thundersnapd with a vmx-isolation policy and returns
// it, failing the test if VM dependencies are unavailable.
func startVMDaemon(t *testing.T, env *testEnv) *daemonInstance {
	t.Helper()
	_ = requireVMDeps(t)
	policyPath := filepath.Join(env.root, "policy.json")
	policyContent := `{
		"grants": [
			{
				"principals": ["*"],
				"cap": {
					"role": "developer",
					"isolation": "vmx",
					"maxFrames": 10
				}
			}
		]
	}`
	if err := os.WriteFile(policyPath, []byte(policyContent), 0644); err != nil {
		t.Fatalf("write policy file: %v", err)
	}
	return startDaemonWithPolicy(t, env, policyPath)
}

// TestVMSSHSessionMatrix verifies the daemon's VM (vmx) session path produces
// the same observable behavior as the container path: a real cloud-hypervisor
// VM is booted by the daemon (not hand-spawned), and over SSH we see echo
// work, root can write to /, a non-root user cannot, and the login shell
// starts in /home. Replaces not_e2e ssh_vm_test.go and the VM half of
// ssh_cwd_test.go.
func testVMSSHSessionMatrix(t *testing.T, d *daemonInstance) {
	createFrameViaDaemon(t, d, "vmmatrix")

	// "vm/<frame>" routes through runVMXSession: the daemon boots a VM and
	// runs the container inside it. Reuse one frame so the booted VM is shared
	// across the assertions. The non-root user@ steps exercise the native ts su
	// path: a nil:nil:nil frame has no real su, so the daemon symlinks
	// /bin/su -> ts and vshd's `su - user` is served by ts's built-in su mode.

	// 1. echo works through the VM session.
	out, exit, err := sshExec(t, d, "vm/vmmatrix", "echo hello-vm")
	if err != nil || exit != 0 {
		t.Fatalf("vm echo: err=%v exit=%d out=%q", err, exit, out)
	}
	if !strings.Contains(out, "hello-vm") {
		t.Errorf("vm echo: expected hello-vm, got %q", out)
	} else {
		t.Logf("vm echo OK: %q", strings.TrimSpace(out))
	}

	// 2. root can write to / inside the VM.
	out, exit, err = sshExec(t, d, "vm/root@vmmatrix", "echo hi > /rootprobe && echo OK")
	if err != nil || exit != 0 {
		t.Errorf("vm root write to /: err=%v exit=%d out=%q", err, exit, out)
	} else if !strings.Contains(out, "OK") {
		t.Errorf("vm root write to /: expected OK, got %q", out)
	} else {
		t.Logf("vm root write to / OK")
	}

	// 3. non-root user cannot write to / inside the VM.
	out, exit, err = sshExec(t, d, "vm/user@vmmatrix", "echo hi > /userprobe && echo OK")
	if err != nil {
		t.Fatalf("vm user write: err=%v", err)
	}
	if exit == 0 || strings.Contains(out, "OK") {
		t.Errorf("vm non-root write to /: expected failure, got exit=%d out=%q", exit, out)
	} else {
		t.Logf("vm non-root write to / correctly failed (exit=%d)", exit)
	}

	// 4. login shell starts in /home (the shared home subvolume).
	out, exit, err = sshExec(t, d, "vm/user@vmmatrix", "pwd")
	if err != nil {
		t.Fatalf("vm pwd: err=%v", err)
	}
	if exit != 0 {
		t.Fatalf("vm pwd: expected exit 0, got %d (out=%q)", exit, out)
	}
	pwd := strings.TrimSpace(strings.ReplaceAll(out, "\r", ""))
	if pwd != "/home" {
		t.Errorf("vm pwd: expected /home, got %q (raw out=%q)", pwd, out)
	} else {
		t.Logf("vm pwd OK: %q", pwd)
	}
}

// TestVMXPtyWinsize verifies that the daemon relays the client's PTY size
// through to the VM's inner pty end to end (the daemon sends the initial
// winsize as a FrameWinsize; vshd/servePTY applies it via pty.Setsize before
// starting the child). It is the VM-path twin of TestContainerPtyWinsize:
// both share the vshdsession.servePTY / proxyVshdSessionGeneric relay, so this
// one additionally guards the vsock transport in both directions. Replaces
// not_e2e TestVMXPtyWinsize (which hand-spawned the VM and drove vshd).
//
// Uses sshPtyRun (PTY + non-interactive Run) so the stream is exactly stty's
// output -- no shell prompt, no typed-input echo, no greeting -- for an
// exact-match assertion. See TestContainerPtyEcho for the interactive echo
// invariant and TestContainerPtyWriteOrder for the stdout/stderr race guard.
func testVMDeepWorkflow(t *testing.T, d *daemonInstance) {
	for _, ref := range []string{"vmdeepa", "vmdeepb"} {
		createFrameViaDaemon(t, d, ref)
	}

	// Hostname/networking are properties of the daemon-launched outer VM. The
	// old tests hand-built a cloud-hypervisor command line; checking them here
	// also covers the daemon's passt and VMConfig wiring.
	out, exit, err := sshExec(t, d, "vm/root@vmdeepa", "read h < /proc/sys/kernel/hostname; echo HOST=$h; test -d /sys/class/net/eth0 && echo ETH0; read c < /proc/cmdline; case $c in *ip=10.0.2.15*) echo IPCONFIG;; esac")
	if err != nil || exit != 0 {
		t.Fatalf("VM network/hostname probe: err=%v exit=%d out=%q", err, exit, out)
	}
	for _, marker := range []string{"HOST=", "ETH0", "IPCONFIG"} {
		if !strings.Contains(out, marker) {
			t.Errorf("VM network/hostname probe missing %q: %q", marker, out)
		}
	}

	// Keep sessions to two different frames alive concurrently. They must both
	// execute successfully through the shared outer VM while retaining separate
	// frame root filesystems.
	installBusyboxAppletInFrame(t, d, "vmdeepa", "sleep")
	installBusyboxAppletInFrame(t, d, "vmdeepb", "sleep")
	const sessions = 4
	var wg sync.WaitGroup
	errs := make(chan error, sessions)
	for i := 0; i < sessions; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ref := "vmdeepa"
			if i%2 != 0 {
				ref = "vmdeepb"
			}
			marker := fmt.Sprintf("VMCONCURRENT%d", i)
			out, exit, err := sshExec(t, d, "vm/root@"+ref, "echo "+marker+"; sleep 1")
			if err != nil || exit != 0 || !strings.Contains(out, marker) {
				errs <- fmt.Errorf("%s: err=%v exit=%d out=%q", ref, err, exit, out)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	// The nil:nil:nil frame deliberately contains only ts as /bin/sh. Exercise
	// the built-in shell through the complete daemon -> SSH -> VM -> vshd path.
	script := `x=hello; echo "$x world"; echo $(echo nested); for i in a b c; do echo LOOP$i; done; echo REDIRECT > /tmp/minimal; read v < /tmp/minimal; echo $v; false || echo CAUGHT`
	out, exit, err = sshExec(t, d, "vm/root@vmdeepa", script)
	if err != nil || exit != 0 {
		t.Fatalf("minimal VM shell: err=%v exit=%d out=%q", err, exit, out)
	}
	for _, marker := range []string{"hello world", "nested", "LOOPa", "LOOPb", "LOOPc", "REDIRECT", "CAUGHT"} {
		if !strings.Contains(out, marker) {
			t.Errorf("minimal VM shell missing %q: %q", marker, out)
		}
	}

	// Finally verify concurrent PTYs are allocated in the container's shared
	// devpts instance, rather than in vshd's outer mount namespace.
	installBusyboxAppletInFrame(t, d, "vmdeepa", "tty")
	clientA, sessA, outA, inA := startPtyShellUser(t, d, "vm/root@vmdeepa")
	defer clientA.Close()
	defer sessA.Close()
	clientB, sessB, outB, inB := startPtyShellUser(t, d, "vm/root@vmdeepa")
	defer clientB.Close()
	defer sessB.Close()
	ttyA := ptyTTYOf(t, sessA, inA, outA)
	ttyB := ptyTTYOf(t, sessB, inB, outB)
	if !strings.HasPrefix(ttyA, "/dev/pts/") || !strings.HasPrefix(ttyB, "/dev/pts/") || ttyA == ttyB {
		t.Errorf("VM concurrent PTYs: want distinct /dev/pts/N, got %q and %q", ttyA, ttyB)
	}
}

// testVMXPtyWinsize verifies both the initial PTY size and a live SSH
// window-change request. The latter was covered only by the hand-driven vshd
// protocol test before this daemon-level workflow was added.
func testVMXPtyWinsize(t *testing.T, d *daemonInstance) {
	createFrameViaDaemon(t, d, "vmwin")
	installBusyboxAppletInFrame(t, d, "vmwin", "stty")

	out, exit, err := sshPtyRun(t, d, "vm/root@vmwin", "stty size")
	if err != nil {
		t.Fatalf("sshPtyRun: %v", err)
	}
	if exit != 0 {
		t.Fatalf("stty size exited %d (output %q)", exit, out)
	}
	t.Logf("vm stty size output: %q", out)
	const want = "40 80\r\n"
	if out != want {
		t.Errorf("vm PTY winsize: expected exactly %q, got %q", want, out)
	} else {
		t.Logf("vm PTY size = 40 x 80")
	}

	client, session, err := sshInteractive(t, d, "vm/root@vmwin")
	if err != nil {
		t.Fatalf("interactive VM PTY: %v", err)
	}
	defer client.Close()
	defer session.Close()
	var buf safeBuffer
	session.Stdout, session.Stderr = &buf, &buf
	stdin, err := session.StdinPipe()
	if err != nil {
		t.Fatalf("VM PTY stdin: %v", err)
	}
	if err := session.Shell(); err != nil {
		t.Fatalf("VM PTY shell: %v", err)
	}
	if err := session.WindowChange(50, 120); err != nil {
		t.Fatalf("VM PTY resize: %v", err)
	}
	if _, err := io.WriteString(stdin, "stty size; echo RESIZE''DONE; exit\n"); err != nil {
		t.Fatalf("VM PTY command: %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for !strings.Contains(buf.String(), "RESIZEDONE") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := buf.String(); !strings.Contains(got, "50 120") {
		t.Errorf("resized VM PTY: expected 50 120, got %q", got)
	}
}
