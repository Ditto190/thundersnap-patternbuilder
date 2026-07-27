// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

//go:build e2e

// errors_test.go is the W7 "errors & negative cases" workflow: it exercises
// the daemon's error paths over SSH (unknown frame, deleting a nonexistent
// frame, snapping a frame with a symlink loop) against a real thundersnapd.
// It supersedes not_e2e/error_test.go's fake-control-server negative cases
// (TestSymlinkLoopDetection stays a unit-level indexer check covered by the
// tsm package, but the live snap-of-a-loop path is asserted here).
package e2e

import (
	"strings"
	"testing"
	"time"
)

// TestErrorSSHUnknownFrame verifies that SSHing to a frame that does not exist
// fails (the daemon's frame resolution surfaces an error rather than silently
// creating/connecting a phantom frame). Replaces not_e2e
// TestErrorSnapNonexistentFrame (fake control server).
func TestErrorSSHUnknownFrame(t *testing.T) {
	env := newTestEnv(t)
	d := startDaemon(t, env)

	// Connect to a frame name that was never created.
	out, exit, err := sshExec(t, d, "root@does-not-exist", "echo hi")
	if err == nil && exit == 0 {
		t.Errorf("SSH to unknown frame: expected failure, got exit 0 (out=%q)", out)
	} else {
		t.Logf("SSH to unknown frame correctly failed (exit=%d): %q", exit, strings.TrimSpace(out))
	}
}

// TestErrorDeleteNonexistentFrame verifies that deleting a nonexistent frame
// UUID over SSH fails. Replaces not_e2e TestErrorDeleteNonexistentFrame (fake
// control server).
func TestErrorDeleteNonexistentFrame(t *testing.T) {
	env := newTestEnv(t)
	d := startDaemon(t, env)
	createFrameViaDaemon(t, d, "errhost") // a real frame to run the command from

	out, exit, err := sshExec(t, d, "root@errhost", "ts frame --delete 00000000-0000-0000-0000-000000000000")
	if err != nil {
		t.Fatalf("sshExec: %v", err)
	}
	if exit == 0 {
		t.Errorf("ts frame --delete of nonexistent UUID: expected non-zero exit, got 0 (out=%q)", out)
	} else {
		t.Logf("delete of nonexistent frame correctly failed (exit=%d): %q", exit, strings.TrimSpace(out))
	}
}

// TestErrorSnapSymlinkLoop verifies that snapping a frame whose /work contains
// a symlink loop completes (the indexer detects the loop rather than recursing
// forever). Replaces not_e2e TestSymlinkLoopDetection (fake control server)
// for the live snap path.
func TestErrorSnapSymlinkLoop(t *testing.T) {
	env := newTestEnv(t)
	d := startDaemon(t, env)
	createFrameViaDaemon(t, d, "loop")
	installBusyboxAppletInFrame(t, d, "loop", "ln")

	// Create a symlink loop: /work/a -> /work/b -> /work/a.
	if out, exit, err := sshExec(t, d, "root@loop", "ln -s /work/b /work/a && ln -s /work/a /work/b"); err != nil || exit != 0 {
		t.Fatalf("create symlink loop: err=%v exit=%d out=%q", err, exit, out)
	}

	// The snap must complete (not hang) despite the loop. Wrap in a deadline.
	done := make(chan struct{})
	var id string
	go func() {
		defer close(done)
		id = tsSnapWait(t, d, "loop")
	}()
	select {
	case <-done:
		t.Logf("snap of symlink-loop frame completed: %s", id)
	case <-time.After(30 * time.Second):
		t.Fatalf("snap of a symlink loop hung past 30s (indexer loop detection broken)")
	}
}

// TestErrorSSHUnknownUser verifies that SSHing to a real frame as a user that
// does not exist in the frame's /etc/passwd fails, rather than silently
// dropping to a root shell. vshd runs `su - <sshuser>` via ts's built-in su;// before runAsSu rejected unknown users, a bogus username fell back to uid 0 —
// an isolation bypass (any nonexistent username @ a frame got root). This is
// the security regression test for that fix.
func TestErrorSSHUnknownUser(t *testing.T) {
	env := newTestEnv(t)
	d := startDaemon(t, env)
	createFrameViaDaemon(t, d, "userhost")

	// SSH as a user that does not exist in the frame's /etc/passwd. The session
	// must fail; a successful exit 0 here means the unknown user got a shell
	// (as root, per the old fallback) — the bypass.
	out, exit, err := sshExec(t, d, "nosuchuser@userhost", "id -u")
	if err == nil && exit == 0 {
		t.Errorf("SSH as unknown user: expected failure, got exit 0 (out=%q) — bogus username got a shell (root-shell privilege bypass)", out)
	} else {
		t.Logf("SSH as unknown user correctly failed (exit=%d): %q", exit, strings.TrimSpace(out))
	}
}

// TestErrorInvalidFrameSpec verifies that a frame spec whose rootfs component
// escapes the snaps directory (e.g. "../../etc/passwd") is rejected, not
// resolved via filepath.Join into an arbitrary path. Restores the path-
// traversal negative case from not_e2e TestErrorInvalidSnapshotFormat, which
// the fake control server checked but the real daemon did not (a spec like
// "../fs/<user>/<uuid>::" would have resolved to another tenant's frame
// subvolume). The daemon now validates snap-id components.
func TestErrorInvalidFrameSpec(t *testing.T) {
	env := newTestEnv(t)
	d := startDaemon(t, env)
	createFrameViaDaemon(t, d, "spechost")

	out, exit, err := sshExec(t, d, "root@spechost", "ts frame '../../etc/passwd::'")
	if err != nil {
		t.Fatalf("sshExec: %v", err)
	}
	if exit == 0 {
		t.Errorf("ts frame with path-traversal spec: expected non-zero exit, got 0 (out=%q) — traversal was not rejected", out)
	} else {
		t.Logf("path-traversal frame spec correctly rejected (exit=%d): %q", exit, strings.TrimSpace(out))
	}
}

// TestErrorInvalidFrameName verifies that a name used to address a frame that
// is neither a valid UUID nor a valid ref name is rejected — at the CLI (ts
// frame <bad>) and over SSH (root@<bad>) — rather than being turned into a
// filesystem path. Ref names must start with a letter and contain only
// letters/digits/dash/underscore (no dots, no slashes); a dot or slash in a
// frame name is a path-traversal hazard, so it is refused.
func TestErrorInvalidFrameName(t *testing.T) {
	env := newTestEnv(t)
	d := startDaemon(t, env)
	createFrameViaDaemon(t, d, "namehost")

	// `ts frame <bad>` is rejected client-side for a non-UUID, non-ref name.
	for _, bad := range []string{"foo.bar", "foo/bar", ".."} {
		out, exit, err := sshExec(t, d, "root@namehost", "ts frame "+bad)
		if err != nil {
			t.Fatalf("sshExec ts frame %s: %v", bad, err)
		}
		if exit == 0 {
			t.Errorf("ts frame %q: expected non-zero exit, got 0 (out=%q)", bad, out)
		} else {
			t.Logf("ts frame %q correctly rejected (exit=%d): %q", bad, exit, strings.TrimSpace(out))
		}
	}

	// SSH to `root@<bad>` is rejected by the daemon's frame resolution (a
	// bogus name must not resolve to a path or create a phantom frame).
	for _, bad := range []string{"foo.bar", "foo/bar"} {
		out, exit, err := sshExec(t, d, "root@"+bad, "echo hi")
		if err == nil && exit == 0 {
			t.Errorf("ssh root@%q: expected failure, got exit 0 (out=%q)", bad, out)
		} else {
			t.Logf("ssh root@%q correctly failed (exit=%d): %q", bad, exit, strings.TrimSpace(out))
		}
	}
}
