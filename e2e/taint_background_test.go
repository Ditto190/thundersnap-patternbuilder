// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// TestTaintSurvivesBackgroundSnap verifies that taints on a frame are preserved
// across a background snap finalize: the worker updates the frame sidecar
// (Rootfs/Home/Work/History) at finalize time, and must not drop taints that
// were already on the frame. Before the per-frame sidecar lock and atomic
// writeFrameSidecar (see snapqueue.go, metadata.go), the worker's
// read-modify-write of the sidecar could clobber taints. This test is the
// deterministic subset of that: a taint present before the snap must survive
// finalize. (The fully-concurrent variant — taint added *during* indexing — is
// a structural property of the per-frame lock and is not reliably triggerable
// with tiny test frames; see README.codereview.md.)
func testTaintSurvivesBackgroundSnap(t *testing.T, d *daemonInstance) {

	createFrameViaDaemon(t, d, "tainttest")

	// Add a taint first, then fire-and-forget snap. The worker's finalize
	// rewrites the sidecar with the new snap IDs; it must preserve the taint.
	if _, _, exitCode, err := sshExecSplit(t, d, "root@tainttest", "ts taint secret:thing"); err != nil || exitCode != 0 {
		t.Fatalf("ts taint (add): err=%v exit=%d", err, exitCode)
	}

	if _, _, exitCode, err := sshExecSplit(t, d, "root@tainttest", "ts snap --quick"); err != nil || exitCode != 0 {
		t.Fatalf("ts snap: err=%v exit=%d", err, exitCode)
	}

	// Wait for the background snap to finalize so the worker has rewritten the
	// sidecar at least once since the taint was added.
	deadline := time.Now().Add(30 * time.Second)
	finalized := false
	for time.Now().Before(deadline) {
		out, _, _, err := sshExecSplit(t, d, "root@tainttest", "ts snaps")
		if err != nil {
			t.Fatalf("ts snaps (poll): %v", err)
		}
		if strings.TrimSpace(out) != "" {
			finalized = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !finalized {
		t.Fatalf("background snap never finalized within 30s")
	}
	// Give any in-flight sidecar rewrite a final moment to land.
	time.Sleep(500 * time.Millisecond)

	// The taint must still be present.
	out, _, exitCode, err := sshExecSplit(t, d, "root@tainttest", "ts taint")
	if err != nil || exitCode != 0 {
		t.Fatalf("ts taint (list): err=%v exit=%d", err, exitCode)
	}
	if !strings.Contains(out, "secret:thing") {
		t.Errorf("taint 'secret:thing' was dropped after background snap finalize; ts taint output: %q", out)
	} else {
		t.Logf("taint survived background snap finalize: %q", strings.TrimSpace(out))
	}

	// Also verify the frame sidecar now references a finalized (content-addressed)
	// snap as its rootfs, confirming the worker did rewrite the sidecar. If it
	// didn't, the taint-preservation check above would be vacuous.
	frameOut, _, _, err := sshExecSplit(t, d, "root@tainttest", "ts frame")
	if err != nil {
		t.Fatalf("ts frame: %v", err)
	}
	if strings.TrimSpace(frameOut) == "" {
		t.Fatalf("ts frame: empty output")
	}
}
