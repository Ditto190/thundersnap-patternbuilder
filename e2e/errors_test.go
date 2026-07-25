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
