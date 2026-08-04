// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// TestContainerPtyEcho verifies the defining property of a PTY (vs. pipe)
// session: the kernel line discipline echoes bytes written to the pty master
// back into the output stream. This is the invariant the relay must preserve
// in PTY mode (stdin -> pty master, pty slave output -> FrameStdout), and it
// is what makes an interactive shell usable over SSH.
//
// We send a comment line ("# ECHOPROBE") -- a comment produces no output, so
// the probe text can appear in the stream ONLY via the pty echoing our
// keystrokes. A containment check (not an ordering check) is therefore both
// necessary and sufficient, and is robust to the startup timing race where
// the echo of our input lands before the shell prints its first prompt (a
// real human never types that fast, so that ordering is not an invariant we
// care about -- only "echo happened at all" is).
//
// This single test guards the shared vshdsession.servePTY / relay echo path;
// the container and VM session paths share it, so we do not duplicate the
// check on the VM path.
func testContainerNonRootPtyJobControl(t *testing.T, d *daemonInstance) {
	createFrameViaDaemon(t, d, "userpty")
	client, session, out, stdin := startPtyShellUser(t, d, "user@userpty")
	defer client.Close()
	defer session.Close()
	if _, err := stdin.Write([]byte("echo NONROOT-PTY-OK; exit\n")); err != nil {
		t.Fatalf("write command: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- session.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("non-root PTY session: %v (output %q)", err, out.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("timeout waiting for non-root PTY; output=%q", out.String())
	}
	got := out.String()
	if !strings.Contains(got, "NONROOT-PTY-OK") {
		t.Fatalf("non-root PTY command output missing: %q", got)
	}
	if strings.Contains(got, "can't access tty") || strings.Contains(got, "job control turned off") {
		t.Fatalf("non-root shell could not access its PTY: %q", got)
	}
}

func testContainerPtyEcho(t *testing.T, d *daemonInstance) {
	createFrameViaDaemon(t, d, "echopty")

	// root@ runs /bin/sh -l directly (no su); the shell is ts's built-in
	// mvdan.cc/sh. echo is a shell builtin, so no busybox install is needed.
	client, session, err := sshInteractive(t, d, "root@echopty")
	if err != nil {
		t.Fatalf("sshInteractive: %v", err)
	}
	defer client.Close()
	defer session.Close()

	var outBuf safeBuffer
	session.Stdout = &outBuf
	session.Stderr = &outBuf
	stdin, err := session.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	if err := session.Shell(); err != nil {
		t.Fatalf("start shell: %v", err)
	}

	// A comment line yields no output; the only way "# ECHOPROBE" reaches the
	// stream is the pty echoing what we typed. Then exit to end the session.
	const probe = "# ECHOPROBE"
	stdin.Write([]byte(probe + "\n"))
	stdin.Write([]byte("exit\n"))

	done := make(chan error, 1)
	go func() { done <- session.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("session exited with error: %v (output %q)", err, outBuf.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("timeout waiting for session; output=%q", outBuf.String())
	}

	out := outBuf.String()
	t.Logf("pty echo output: %q", out)
	if !strings.Contains(out, probe) {
		t.Errorf("PTY did not echo stdin: %q not found in output %q (echo is the core PTY invariant)", probe, out)
	} else {
		t.Logf("PTY echo OK: %q reflected in stream", probe)
	}
}

// TestContainerPtyWinsize is the container-path CONTROL for TestVMXPtyWinsize:
// it verifies that a 40x80 PTY requested by the client is relayed end to end
// so the inner pty reports that size. It uses sshPtyRun (PTY + non-interactive
// Run), so the captured stream is exactly stty's output with no shell prompt,
// no typed-input echo, and no greeting -- allowing an exact-match assertion
// that would fail on any relay reordering, dropped/extra bytes, or a wrong
// winsize. (The interactive-shell form can't do this: the prompt position is
// nondeterministic and `stty -echo` cannot suppress the echo of the line it is
// typed on, so no exact suffix is reliably producible.)
func testContainerPtyWinsize(t *testing.T, d *daemonInstance) {
	createFrameViaDaemon(t, d, "cptywin")
	installBusyboxAppletInFrame(t, d, "cptywin", "stty")

	// sshPtyRun requests a 40x80 PTY and runs the command non-interactively.
	// root@ runs /bin/sh -c "stty size"; stty is the installed busybox applet.
	out, exit, err := sshPtyRun(t, d, "root@cptywin", "stty size")
	if err != nil {
		t.Fatalf("sshPtyRun: %v", err)
	}
	if exit != 0 {
		t.Fatalf("stty size exited %d (output %q)", exit, out)
	}
	t.Logf("container stty size output: %q", out)
	// stty size prints "rows cols" with a cooked-mode CRLF. No prompt, no
	// echo, no greeting -- the stream is exactly this. Any deviation (a
	// reordered/corrupted/extra byte) fails the exact match.
	const want = "40 80\r\n"
	if out != want {
		t.Errorf("container PTY winsize: expected exactly %q, got %q", want, out)
	} else {
		t.Logf("container PTY size = 40 x 80")
	}
}

// TestContainerPtyWriteOrder verifies that in PTY mode the relay preserves the
// kernel's write ordering of the child's stdout and stderr: both fds point at
// the pty slave, so writes interleave in the order the child issues them, and
// the whole combined stream arrives as FrameStdout. If the relay ever split
// stdout and stderr into separate frames and reassembled them out of order
// (the "stdout/stderr race" class of bug), the exact sequence A1B2C3 would
// break.
//
// This is the precise race detector: a command writes A1 to stdout, B2 to
// stderr, C3 to stdout, in that order; the client must receive exactly
// "A1B2C3". It runs non-interactively (sshPtyRun) so the stream is only the
// command's output -- no prompt or echo to obscure a reordering.
func testContainerPtyWriteOrder(t *testing.T, d *daemonInstance) {
	createFrameViaDaemon(t, d, "ptyorder")

	// printf is a builtin in ts's mvdan.cc/sh, so no busybox install needed.
	out, exit, err := sshPtyRun(t, d, "root@ptyorder", "printf A1; printf B2 >&2; printf C3")
	if err != nil {
		t.Fatalf("sshPtyRun: %v", err)
	}
	if exit != 0 {
		t.Fatalf("printf order exited %d (output %q)", exit, out)
	}
	t.Logf("pty write-order output: %q", out)
	const want = "A1B2C3"
	if out != want {
		t.Errorf("PTY stdout/stderr write order: expected exactly %q, got %q (a reordering indicates a relay race)", want, out)
	} else {
		t.Logf("PTY write order OK: %q", want)
	}
}
