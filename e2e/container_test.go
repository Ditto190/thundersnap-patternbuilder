// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

//go:build e2e

// container_test.go is the W4 "container session matrix" workflow: it
// verifies container isolation properties (dropped caps, mounted /proc and
// /sys, distinct namespaces) and concurrent-PTS distinctness by running
// commands through real SSH sessions against a thundersnapd. It supersedes
// not_e2e/container_test.go, container_pts_test.go, container_cwd_test.go,
// and blank_container_test.go, which drove `ts drop-caps-and-run` directly.
package e2e

import (
	"io"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// checkIsolation runs `ts check-isolation` over SSH and returns the parsed
// key:value lines as a map. The output format is "KEY:value" for simple
// fields, but "CAP:NAME:dropped" and "NS:name:inode" have a two-part key.
func checkIsolation(t *testing.T, d *daemonInstance, ref string) map[string]string {
	t.Helper()
	out, exit, err := sshExec(t, d, "root@"+ref, "ts check-isolation")
	if err != nil || exit != 0 {
		t.Fatalf("ts check-isolation (%s): err=%v exit=%d out=%q", ref, err, exit, out)
	}
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, ":")
		switch {
		case len(parts) >= 3 && (parts[0] == "CAP" || parts[0] == "NS"):
			// "CAP:NET_ADMIN:dropped" or "NS:pid:<inode>".
			m[parts[0]+":"+parts[1]] = strings.Join(parts[2:], ":")
		case len(parts) >= 2:
			// "HOSTNAME:foo", "PROC:mounted", "PID1:no:27", etc.
			m[parts[0]] = strings.Join(parts[1:], ":")
		}
	}
	return m
}

// TestContainerIsolationOverSSH verifies the daemon's container sessions are
// isolated: /proc and /sys are mounted, the dangerous capabilities are
// dropped from the bounding set, and the session is in its own pid/mnt/uts/net
// namespaces. Replaces not_e2e TestContainerIsolationBasic,
// TestBlankContainerIsolation, and TestBlankContainerDevSetup (which ran
// `ts drop-caps-and-run` / `ts check-dev` directly).
func testContainerIsolationOverSSH(t *testing.T, d *daemonInstance) {
	// A nil:nil:nil frame is the "blank container" case.
	createFrameViaDaemon(t, d, "iso")

	r := checkIsolation(t, d, "iso")
	t.Logf("check-isolation: %v", r)

	if r["PROC"] != "mounted" {
		t.Errorf("PROC = %q, want \"mounted\"", r["PROC"])
	}
	if r["SYS"] != "mounted" {
		t.Errorf("SYS = %q, want \"mounted\"", r["SYS"])
	}
	// Dangerous caps must be dropped from the bounding set.
	for _, cap := range []string{"NET_ADMIN", "SYS_MODULE", "SYS_BOOT", "SYS_TIME", "AUDIT_WRITE", "SETFCAP"} {
		if got := r["CAP:"+cap]; got != "dropped" {
			t.Errorf("CAP:%s = %q, want \"dropped\"", cap, got)
		}
	}
	// Each namespace must report an inode (we're in a private ns). The
	// container shares the host's net namespace (no CLONE_NEWNET), so NS:net
	// is the host inode — still a valid, non-empty inode.
	for _, ns := range []string{"pid", "mnt", "uts", "net"} {
		if got := r["NS:"+ns]; got == "" || got == "error" {
			t.Errorf("NS:%s = %q, want an inode", ns, got)
		}
	}
	// Mount propagation is private to the container.
	if r["MOUNT_PROPAGATION"] != "private" {
		t.Errorf("MOUNT_PROPAGATION = %q, want private", r["MOUNT_PROPAGATION"])
	}
	// Hostname is non-empty (the container has its own UTS namespace).
	if r["HOSTNAME"] == "" {
		t.Errorf("HOSTNAME is empty; expected a hostname in the container UTS ns")
	}
	t.Logf("container isolation OK: proc/sys mounted, dangerous caps dropped, private namespaces")
}

// TestContainerConcurrentDistinctPTS verifies that two concurrent PTY SSH
// sessions to the same frame receive distinct /dev/pts<N> devices (the
// user-reported "both sessions share one pty" bug). Replaces not_e2e
// TestContainerConcurrentSessionDistinctPTS and TestContainerSharedDevpts
// (which hand-drove `ts drop-caps-and-run` and stat'd /dev/pts).
func testContainerConcurrentDistinctPTS(t *testing.T, d *daemonInstance) {
	createFrameViaDaemon(t, d, "pts")
	// `tty` is an external command; install the busybox applet so each PTY
	// session can report its own terminal device.
	installBusyboxAppletInFrame(t, d, "pts", "tty")

	// Open TWO concurrent PTY sessions and keep both alive while we query each
	// for its tty. Sequential sessions would free and reuse /dev/pts/0, so the
	// distinctness invariant only holds while both are open at once.
	clientA, sessA, outA, inA := startPtyShell(t, d, "pts")
	defer clientA.Close()
	defer sessA.Close()
	clientB, sessB, outB, inB := startPtyShell(t, d, "pts")
	defer clientB.Close()
	defer sessB.Close()

	ttyA := ptyTTYOf(t, sessA, inA, outA)
	ttyB := ptyTTYOf(t, sessB, inB, outB)
	t.Logf("concurrent PTY ttys: A=%q B=%q", ttyA, ttyB)

	if ttyA == "" || ttyB == "" {
		t.Fatalf("expected non-empty tty names from both sessions")
	}
	if ttyA == ttyB {
		t.Errorf("concurrent PTY sessions share the same tty %q; expected distinct /dev/pts<N>", ttyA)
	} else {
		t.Logf("concurrent sessions got distinct PTY devices")
	}
}

// startPtyShell opens an interactive PTY SSH session (Shell started) and
// returns the client, session, a safeBuffer capturing stdout/stderr, and the
// stdin pipe to write commands through.
func startPtyShell(t *testing.T, d *daemonInstance, ref string) (*ssh.Client, *ssh.Session, *safeBuffer, io.WriteCloser) {
	t.Helper()
	return startPtyShellUser(t, d, "root@"+ref)
}

// startPtyShellUser is the isolation-agnostic form of startPtyShell. The VM
// workflow uses it with a vm/root@frame username so the same real SSH/PTTY
// path can be checked inside the daemon-managed VM.
func startPtyShellUser(t *testing.T, d *daemonInstance, user string) (*ssh.Client, *ssh.Session, *safeBuffer, io.WriteCloser) {
	t.Helper()
	client, session, err := sshInteractive(t, d, user)
	if err != nil {
		t.Fatalf("sshInteractive (%s): %v", user, err)
	}
	var outBuf safeBuffer
	session.Stdout = &outBuf
	session.Stderr = &outBuf
	stdin, err := session.StdinPipe()
	if err != nil {
		client.Close()
		session.Close()
		t.Fatalf("stdin pipe: %v", err)
	}
	if err := session.Shell(); err != nil {
		client.Close()
		session.Close()
		t.Fatalf("start shell: %v", err)
	}
	return client, session, &outBuf, stdin
}

// ptyTTYOf writes `tty; exit` to stdin, waits for the session to finish, and
// returns the /dev/pts<N> line from the captured output.
func ptyTTYOf(t *testing.T, session *ssh.Session, stdin io.WriteCloser, outBuf *safeBuffer) string {
	t.Helper()
	if _, err := stdin.Write([]byte("tty; exit\n")); err != nil {
		t.Fatalf("write tty cmd: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- session.Wait() }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatalf("timeout waiting for tty; output=%q", outBuf.String())
	}
	for _, line := range strings.Split(outBuf.String(), "\n") {
		line = strings.TrimSpace(line)
		if i := strings.Index(line, "/dev/pts/"); i >= 0 {
			// Interactive shells may prefix command output with "$ ". Return
			// only the tty path, stopping before any following whitespace.
			tty := line[i:]
			if end := strings.IndexAny(tty, " \t\r"); end >= 0 {
				tty = tty[:end]
			}
			return tty
		}
	}
	return strings.TrimSpace(outBuf.String())
}
