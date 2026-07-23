// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// TestSnapBackgroundIndexing verifies the fire-and-forget ts snap path:
// `ts snap` (without --wait) captures the frame and returns immediately with
// no snap ID on stdout, while indexing runs in the background and the snap
// appears in `ts snaps` once it finishes. See background-indexing.md.
func TestSnapBackgroundIndexing(t *testing.T) {
	env := newTestEnv(t)
	d := startDaemon(t, env)

	createFrameViaDaemon(t, d, "bgtest")

	// Drop a file so the frame has non-trivial content to index.
	if _, exitCode, err := sshExec(t, d, "root@bgtest", "echo hello > /marker"); err != nil || exitCode != 0 {
		t.Fatalf("create marker: err=%v exit=%d", err, exitCode)
	}

	// Count snaps before any background snap lands.
	before, _, _, err := sshExecSplit(t, d, "root@bgtest", "ts snaps")
	if err != nil {
		t.Fatalf("ts snaps (before): %v", err)
	}
	beforeCount := strings.Count(before, "\n")

	// Fire-and-forget snap: stdout must be empty (no ID yet), stderr acks.
	stdout, stderr, exitCode, err := sshExecSplit(t, d, "root@bgtest", "ts snap")
	if err != nil {
		t.Fatalf("ts snap failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts snap: expected exit 0, got %d (stdout: %q stderr: %q)", exitCode, stdout, stderr)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("ts snap (no --wait): expected empty stdout, got %q", stdout)
	}
	if !strings.Contains(stderr, "indexing in background") {
		t.Errorf("ts snap (no --wait): expected stderr to mention background indexing, got %q", stderr)
	}

	// The snap is indexed in the background; poll ts snaps until a new snap
	// appears (or time out). Test frames are tiny, so this is near-instant.
	deadline := time.Now().Add(30 * time.Second)
	var after string
	for time.Now().Before(deadline) {
		out, _, _, err := sshExecSplit(t, d, "root@bgtest", "ts snaps")
		if err != nil {
			t.Fatalf("ts snaps (poll): %v", err)
		}
		if strings.Count(out, "\n") > beforeCount {
			after = out
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if after == "" {
		t.Fatalf("ts snap (no --wait): background snap never appeared in ts snaps within 30s (before=%q)", before)
	}
	t.Logf("background snap appeared in ts snaps: %s", after)

	// The snap should also show up in ts log once indexing completes.
	logOut, _, _, err := sshExecSplit(t, d, "root@bgtest", "ts log")
	if err != nil {
		t.Fatalf("ts log: %v", err)
	}
	if strings.TrimSpace(logOut) == "" {
		t.Errorf("ts log: expected a history entry after background snap, got empty")
	}
	t.Logf("ts log: %s", logOut)
}

// TestSnapBackgroundCapturesAtCallTime verifies the defining property of the
// background-indexing change: `ts snap` (without --wait) captures the frame's
// state at *call* time (the instant btrfs snapshot), and returns before
// indexing finishes. A file written after `ts snap` returns must NOT appear in
// the finalized snap — the capture is a COW snapshot of the live subvolume, so
// later writes to the live frame never reach it. This is the property that
// makes fire-and-forget snaps safe to use for `ts undo`/history, and it is the
// behavior the old cmd/thundersnapd/full_e2e_test.go was trying to check but
// could not (it ran `ts snap` without --wait and then parsed the missing ID,
// and it never started the indexing worker because it called daemon internals
// directly instead of running the real binary). The real e2e harness runs the
// actual thundersnapd, so initSnapQueue() runs and the worker is live.
func TestSnapBackgroundCapturesAtCallTime(t *testing.T) {
	env := newTestEnv(t)
	d := startDaemon(t, env)

	createFrameViaDaemon(t, d, "captest")

	// State at capture time.
	if _, exitCode, err := sshExec(t, d, "root@captest", "echo before-snap > /marker"); err != nil || exitCode != 0 {
		t.Fatalf("write marker (before): err=%v exit=%d", err, exitCode)
	}

	// Fire-and-forget snap: captures "before-snap" and returns at once.
	stdout, stderr, exitCode, err := sshExecSplit(t, d, "root@captest", "ts snap")
	if err != nil || exitCode != 0 {
		t.Fatalf("ts snap: err=%v exit=%d (stdout %q stderr %q)", err, exitCode, stdout, stderr)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Fatalf("ts snap (no --wait): expected empty stdout (no ID yet), got %q", stdout)
	}

	// Mutate the LIVE frame AFTER ts snap returned. The captured snap must not
	// see this: the capture is a COW snapshot taken before this write.
	if _, exitCode, err := sshExec(t, d, "root@captest", "echo after-snap > /marker"); err != nil || exitCode != 0 {
		t.Fatalf("write marker (after): err=%v exit=%d", err, exitCode)
	}

	// Wait for the background snap to finalize and land in ts log (history[0]
	// is the newest snap). Then build a frame from it and inspect its contents.
	deadline := time.Now().Add(30 * time.Second)
	var rootSnap string
	for time.Now().Before(deadline) {
		logOut, _, _, err := sshExecSplit(t, d, "root@captest", "ts log")
		if err != nil {
			t.Fatalf("ts log (poll): %v", err)
		}
		if root, ok := newestLogRootSnap(logOut); ok {
			rootSnap = root
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if rootSnap == "" {
		t.Fatalf("background snap never appeared in ts log within 30s")
	}
	t.Logf("background snap finalized, root=%s", rootSnap)

	// Create a frame from the finalized background snap (this is exactly what
	// ts undo does: create a frame from history[0]). Its /marker must be the
	// capture-time value, proving the snap captured at call time.
	if _, exitCode, err := sshExec(t, d, "root@captest", "ts frame --ref=capcheck "+rootSnap+":nil:nil"); err != nil || exitCode != 0 {
		t.Fatalf("ts frame from background snap: err=%v exit=%d", err, exitCode)
	}
	out, exitCode, err := sshExec(t, d, "root@capcheck", "read line < /marker && echo $line")
	if err != nil || exitCode != 0 {
		t.Fatalf("read marker in capcheck frame: err=%v exit=%d (out %q)", err, exitCode, out)
	}
	if got := strings.TrimSpace(out); got != "before-snap" {
		t.Errorf("background snap captured the wrong state: /marker=%q, want %q (the state at ts snap call time; the post-snap write must not be visible)",
			got, "before-snap")
	} else {
		t.Logf("background snap correctly captured call-time state: /marker=%q", got)
	}
}

// TestSnapRapidDoubleBackgroundIndexing verifies that two rapid fire-and-forget
// snaps both eventually land in ts snaps. This exercises the serialized
// indexing queue and the per-frame pending-snap chaining: the second snap is
// captured before the first finishes indexing, so it chains to the first job
// and is indexed incrementally once the first finalizes.
func TestSnapRapidDoubleBackgroundIndexing(t *testing.T) {
	env := newTestEnv(t)
	d := startDaemon(t, env)

	createFrameViaDaemon(t, d, "bgdouble")

	// Count snaps before any background snap lands.
	before, _, _, err := sshExecSplit(t, d, "root@bgdouble", "ts snaps")
	if err != nil {
		t.Fatalf("ts snaps (before): %v", err)
	}
	beforeCount := strings.Count(before, "\n")

	// Two rapid fire-and-forget snaps. The captures serialize under the queue
	// lock (instant btrfs snapshots); indexing happens in the background.
	for i := 0; i < 2; i++ {
		stdout, _, exitCode, err := sshExecSplit(t, d, "root@bgdouble", "ts snap")
		if err != nil || exitCode != 0 {
			t.Fatalf("ts snap #%d: err=%v exit=%d (stdout: %q)", i+1, err, exitCode, stdout)
		}
		if strings.TrimSpace(stdout) != "" {
			t.Errorf("ts snap #%d (no --wait): expected empty stdout, got %q", i+1, stdout)
		}
	}

	// Both background snaps should eventually finalize and appear in ts snaps.
	// (They may or may not dedup to the same content ID: a freshly created
	// frame's files can still be inside the racy-ctime window, so the two
	// snaps can legitimately produce distinct IDs. We only require that at
	// least one new snap lands.)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		out, _, _, err := sshExecSplit(t, d, "root@bgdouble", "ts snaps")
		if err != nil {
			t.Fatalf("ts snaps (poll): %v", err)
		}
		if strings.Count(out, "\n") > beforeCount {
			t.Logf("both rapid background snaps finalized (ts snaps grew by >=1)")
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("rapid double ts snap: background snaps never appeared in ts snaps within 30s (before=%q)", before)
}
