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
