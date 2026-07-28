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
// `ts snap --quick` captures the frame and returns immediately with
// no snap ID on stdout, while indexing runs in the background and the snap
// appears in `ts snaps` once it finishes. See background-indexing.md.
func testSnapBackgroundIndexing(t *testing.T, d *daemonInstance) {

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

	// Quick snap: success must be silent on both stdout and stderr.
	stdout, stderr, exitCode, err := sshExecSplit(t, d, "root@bgtest", "ts snap --quick")
	if err != nil {
		t.Fatalf("ts snap failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts snap: expected exit 0, got %d (stdout: %q stderr: %q)", exitCode, stdout, stderr)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("ts snap --quick: expected empty stdout, got %q", stdout)
	}
	if strings.TrimSpace(stderr) != "" {
		t.Errorf("ts snap --quick: expected empty stderr, got %q", stderr)
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
		t.Fatalf("ts snap --quick: background snap never appeared in ts snaps within 30s (before=%q)", before)
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
// background-indexing change: `ts snap --quick` captures the frame's
// state at *call* time (the instant btrfs snapshot), and returns before
// indexing finishes. A file written after `ts snap` returns must NOT appear in
// the finalized snap — the capture is a COW snapshot of the live subvolume, so
// later writes to the live frame never reach it. This is the property that
// makes fire-and-forget snaps safe to use for `ts undo`/history, and it is the
// behavior the old cmd/thundersnapd/full_e2e_test.go was trying to check but
// could not (it ran `ts snap --quick` and then parsed the missing ID,
// and it never started the indexing worker because it called daemon internals
// directly instead of running the real binary). The real e2e harness runs the
// actual thundersnapd, so initSnapQueue() runs and the worker is live.
func testSnapBackgroundCapturesAtCallTime(t *testing.T, d *daemonInstance) {

	createFrameViaDaemon(t, d, "captest")

	// State at capture time.
	if _, exitCode, err := sshExec(t, d, "root@captest", "echo before-snap > /marker"); err != nil || exitCode != 0 {
		t.Fatalf("write marker (before): err=%v exit=%d", err, exitCode)
	}

	// Fire-and-forget snap: captures "before-snap" and returns at once.
	stdout, stderr, exitCode, err := sshExecSplit(t, d, "root@captest", "ts snap --quick")
	if err != nil || exitCode != 0 {
		t.Fatalf("ts snap: err=%v exit=%d (stdout %q stderr %q)", err, exitCode, stdout, stderr)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Fatalf("ts snap --quick: expected empty stdout (no ID yet), got %q", stdout)
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
// snaps BOTH eventually finalize. This exercises the serialized indexing queue
// and the per-frame pending-snap chaining: the second snap is captured before
// the first finishes indexing, so it chains to the first job and is indexed
// incrementally once the first finalizes.
//
// The two snaps capture DISTINCT content (a v1 then a v2 marker file), so they
// must produce two distinct snap IDs and two new ts log history entries. The
// previous version only asserted `ts snaps` grew by >=1, which passed even if
// the queue dropped the second job or finalized only one of the two — exactly
// the concurrency regression the test purports to guard. Asserting both land
// catches a dropped/overwritten second pending entry.
func testSnapRapidDoubleBackgroundIndexing(t *testing.T, d *daemonInstance) {

	createFrameViaDaemon(t, d, "bgdouble")

	// Count ts log history entries before any background snap lands. Each
	// finalized snap prepends one history entry, so +2 entries means both jobs
	// finalized.
	beforeLog, _, _, err := sshExecSplit(t, d, "root@bgdouble", "ts log")
	if err != nil {
		t.Fatalf("ts log (before): %v", err)
	}
	beforeCount := len(nonEmptyLogLines(beforeLog))

	// Snap v1: write a marker, fire-and-forget snap (--quick).
	if _, exit, err := sshExec(t, d, "root@bgdouble", "echo v1 > /marker"); err != nil || exit != 0 {
		t.Fatalf("write v1 marker: err=%v exit=%d", err, exit)
	}
	if stdout, _, exit, err := sshExecSplit(t, d, "root@bgdouble", "ts snap --quick"); err != nil || exit != 0 {
		t.Fatalf("ts snap #1: err=%v exit=%d (stdout: %q)", err, exit, stdout)
	} else if strings.TrimSpace(stdout) != "" {
		t.Errorf("ts snap #1 (--quick): expected empty stdout, got %q", stdout)
	}

	// Snap v2: change the marker to distinct content, fire-and-forget snap.
	// The capture happens immediately (before the first job finishes
	// indexing), so v2's snap must reflect the v2 marker.
	if _, exit, err := sshExec(t, d, "root@bgdouble", "echo v2 > /marker"); err != nil || exit != 0 {
		t.Fatalf("write v2 marker: err=%v exit=%d", err, exit)
	}
	if stdout, _, exit, err := sshExecSplit(t, d, "root@bgdouble", "ts snap --quick"); err != nil || exit != 0 {
		t.Fatalf("ts snap #2: err=%v exit=%d (stdout: %q)", err, exit, stdout)
	} else if strings.TrimSpace(stdout) != "" {
		t.Errorf("ts snap #2 (--quick): expected empty stdout, got %q", stdout)
	}

	// Poll ts log until BOTH snaps have finalized (>=2 new history entries).
	// Each finalized job prepends a history entry, so this directly proves both
	// pending jobs were indexed rather than one being dropped/overwritten.
	deadline := time.Now().Add(30 * time.Second)
	var logOut string
	for time.Now().Before(deadline) {
		out, _, _, err := sshExecSplit(t, d, "root@bgdouble", "ts log")
		if err != nil {
			t.Fatalf("ts log (poll): %v", err)
		}
		logOut = out
		if len(nonEmptyLogLines(out))-beforeCount >= 2 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	lines := nonEmptyLogLines(logOut)
	if got := len(lines) - beforeCount; got < 2 {
		t.Fatalf("rapid double ts snap: only %d of 2 background snaps finalized in ts log within 30s (before=%d after=%d log=%q)",
			got, beforeCount, len(lines), logOut)
	}

	// The two newest history entries (ts log prints newest first) must have
	// distinct root snap IDs: v1 and v2 captured different /marker content, so
	// they content-address to different IDs. Identical IDs would mean the
	// second capture was lost or merged into the first.
	rootSnaps := make([]string, 0, 2)
	for _, line := range lines[:2] {
		// ts log line: "<timestamp>  <root:home:work> [message]". fields[1] is
		// the snap triplet; the root ID is the part before the first colon.
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.Contains(fields[1], ":") {
			t.Fatalf("could not parse root snap from ts log line %q", line)
		}
		root := strings.SplitN(fields[1], ":", 2)[0]
		if root == "" || root == "nil" {
			t.Fatalf("parsed nil/empty root snap from ts log line %q", line)
		}
		rootSnaps = append(rootSnaps, root)
	}
	if rootSnaps[0] == rootSnaps[1] {
		t.Fatalf("expected two distinct root snap IDs for v1/v2, both are %q (log=%q)", rootSnaps[0], logOut)
	}
	t.Logf("both rapid background snaps finalized with distinct IDs: %s, %s", rootSnaps[0], rootSnaps[1])
}

// nonEmptyLogLines returns the non-empty, non-"(no snapshots)" lines of ts log
// output, preserving order (newest first).
func nonEmptyLogLines(logOut string) []string {
	var out []string
	for _, line := range strings.Split(logOut, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "(no snapshots)" {
			continue
		}
		out = append(out, line)
	}
	return out
}
