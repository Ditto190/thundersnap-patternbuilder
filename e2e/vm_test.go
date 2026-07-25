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
	"os"
	"path/filepath"
	"strings"
	"testing"
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
func TestVMSSHSessionMatrix(t *testing.T) {
	env := newTestEnv(t)
	d := startVMDaemon(t, env)
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
func TestVMXPtyWinsize(t *testing.T) {
	env := newTestEnv(t)
	d := startVMDaemon(t, env)
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
}
