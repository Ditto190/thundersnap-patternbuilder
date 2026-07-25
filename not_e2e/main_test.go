// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

//go:build e2e

package e2e

import (
	"flag"
	"fmt"
	"os"
	"testing"
)

// TestMain runs the e2e suite in tiers, from simplest/cheapest to
// hardest/most-expensive. If any earlier tier fails, the remaining tiers are
// skipped entirely (the binary exits non-zero immediately).
//
// The rationale (see todo): there is little point running the full, slow VM
// test suite if a foundational test like "can we create a base snapshot" has
// already failed. Almost all later failures in that situation are just
// downstream symptoms of the same root cause, so we abort early and surface the
// first failing tier.
//
// Ordering is by test-name regexp. The tiers are evaluated in order; each tier
// is run via -test.run so individual test names never have to be enumerated.
// Every top-level test is matched by at least one tier (kept in sync by hand;
// an unmatched test simply never runs, which a maintainer would notice as a
// missing PASS line). If a caller passes an explicit -test.run, tiering is
// disabled and that filter is honored as-is.
func TestMain(m *testing.M) {
	flag.Parse()

	// Honor an explicit -test.run from the caller: if the user asked to run a
	// specific subset, don't impose tiering on top of it.
	if f := flag.Lookup("test.run"); f != nil && f.Value.String() != "" {
		os.Exit(m.Run())
	}

	// Tiers run in order, cheapest/most-foundational first. Each pattern is an
	// anchored alternation of test-name prefixes. Every top-level test must be
	// matched by exactly one tier (verified by TestTierCoverage).
	tiers := []struct {
		name string
		run  string
	}{
		// Tier 0: pure in-process package checks (no btrfs, no daemon).
		// The bulk of the unit-level refs/frames/snaphash/frameid coverage now
		// lives in those packages' own _test.go files (run by `make test`); the
		// only remaining in-process checks here exercise the not_e2e test
		// fixture generator itself.
		{"unit", `^Test(FixtureCreatesAllFileTypes|DefaultTestContainerSpecCompleteness)$`},

		// Tier 1: snapshot file-type handling (uid/hardlink/setuid/setgid fidelity
		// now covered by the real e2e fidelity_test.go; the in-process tsm/
		// snap-incremental/subdir/progress checks are covered by the tsm
		// package's own _test.go and the e2e snap tests).
		{"snapshot", `^Test(E2EBasicSnapshot|E2EOwnership|E2EDevSetup|ConcurrentModificationDuringSnapshot|Symlink).*$`},

		// Tier 2: VM/VMX tests (slowest: boot cloud-hypervisor). The container
		// SSH tests now live in the real e2e/ssh_test.go and the VM SSH session
		// matrix in e2e/vm_test.go; the remaining tests here hand-spawn VMs.
		// (mesh/streaming fake-control-server tests were deleted as false-green
		// C-bucket fakes; the real /who-has and /download-snap handlers still
		// need a mesh peer-config seam for real e2e coverage.)
		{"vm", `^Test(VM|E2EVMPanicRecovery|Vshd|MinimalShell).*$`},
	}

	for _, tier := range tiers {
		if err := flag.Set("test.run", tier.run); err != nil {
			fmt.Fprintf(os.Stderr, "e2e: failed to set test.run for tier %q: %v\n", tier.name, err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "\n=== running e2e tier %q ===\n", tier.name)
		if code := m.Run(); code != 0 {
			fmt.Fprintf(os.Stderr, "\n=== e2e tier %q failed; skipping all later tiers ===\n", tier.name)
			os.Exit(code)
		}
	}

	os.Exit(0)
}
