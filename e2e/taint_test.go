// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

//go:build e2e

// taint_test.go is the W3 "taint propagation" workflow: add, list, dedup,
// and propagate taints through snap+fork, all over SSH against a real
// thundersnapd. It supersedes not_e2e/taint_test.go, whose
// TestTaintPropagation was a stub (the fake control server did not track
// taints per frame or propagate them). Here propagation is real: a frame
// forked from a tainted snap inherits the snap's taints via the daemon's
// UnionTaints in ensureFrameFS.
package e2e

import (
	"strings"
	"testing"
)

// taintList runs `ts taint` over SSH and returns the listed taints (one per
// line, trimmed).
func taintList(t *testing.T, d *daemonInstance, ref string) []string {
	t.Helper()
	out, exit, err := sshExec(t, d, "root@"+ref, "ts taint")
	if err != nil || exit != 0 {
		t.Fatalf("ts taint (list on %s): err=%v exit=%d out=%q", ref, err, exit, out)
	}
	var taints []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			taints = append(taints, line)
		}
	}
	return taints
}

// hasTaint reports whether the taint list contains needle.
func hasTaint(taints []string, needle string) bool {
	for _, t := range taints {
		if t == needle {
			return true
		}
	}
	return false
}

// addTaint runs `ts taint <name>` over SSH and fails on non-zero exit.
func addTaint(t *testing.T, d *daemonInstance, ref, name string) {
	t.Helper()
	if out, exit, err := sshExec(t, d, "root@"+ref, "ts taint "+name); err != nil || exit != 0 {
		t.Fatalf("ts taint %s (%s): err=%v exit=%d out=%q", name, ref, err, exit, out)
	}
}

// TestTaintAddListDedup verifies adding taints over SSH records them, lists
// them, and deduplicates repeats. Replaces not_e2e TestTaintSystemBasic,
// TestMultipleTaintsOnFrame, TestTaintDeduplication, and TestQueryFrameTaints
// (fake control server).
func TestTaintAddListDedup(t *testing.T) {
	env := newTestEnv(t)
	d := startDaemon(t, env)
	createFrameViaDaemon(t, d, "tnt")

	addTaint(t, d, "tnt", "pii:customers")
	addTaint(t, d, "tnt", "unsafe-permissions")
	addTaint(t, d, "tnt", "untrusted-code:github.com/u/r")

	taints := taintList(t, d, "tnt")
	t.Logf("taints after adds: %v", taints)
	for _, want := range []string{"pii:customers", "unsafe-permissions", "untrusted-code:github.com/u/r"} {
		if !hasTaint(taints, want) {
			t.Errorf("ts taint missing %q in %v", want, taints)
		}
	}

	// Dedup: adding an existing taint again must not duplicate it.
	addTaint(t, d, "tnt", "pii:customers")
	taints = taintList(t, d, "tnt")
	count := 0
	for _, tt := range taints {
		if tt == "pii:customers" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("pii:customers appears %d times after re-add, want 1 (no dedup)", count)
	} else {
		t.Logf("taint dedup OK: pii:customers appears once")
	}
}

// TestTaintPropagatesThroughFork verifies that taints on a frame propagate to
// a frame forked from its snap: add taints, snap, fork a new frame from the
// snap, and confirm the forked frame's `ts taint` lists the same taints.
// This is the real propagation the not_e2e TestTaintPropagation stub never
// checked (the fake server did not propagate taints).
func TestTaintPropagatesThroughFork(t *testing.T) {
	env := newTestEnv(t)
	d := startDaemon(t, env)
	createFrameViaDaemon(t, d, "tprop")

	addTaint(t, d, "tprop", "pii:source-data")
	addTaint(t, d, "tprop", "untrusted-code:external")

	triplet := tsSnapWait(t, d, "tprop")
	// Fork a new frame from the full snap triplet; ensureFrameFS unions the
	// component snaps' taints into the new frame.
	if out, exit, err := sshExec(t, d, "root@tprop", "ts frame --ref=tpropchild "+triplet); err != nil || exit != 0 {
		t.Fatalf("ts frame tpropchild: err=%v exit=%d out=%q", err, exit, out)
	}

	childTaints := taintList(t, d, "tpropchild")
	t.Logf("forked frame taints: %v", childTaints)
	for _, want := range []string{"pii:source-data", "untrusted-code:external"} {
		if !hasTaint(childTaints, want) {
			t.Errorf("taint %q did not propagate to forked frame; got %v", want, childTaints)
		}
	}
	if len(childTaints) == 0 {
		t.Fatalf("forked frame has no taints; propagation broken")
	}
	t.Logf("taints propagated through snap+fork: %v", childTaints)
}
