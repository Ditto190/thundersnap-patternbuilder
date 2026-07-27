// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

//go:build e2e

// fidelity_test.go is the W2 "snapshot fidelity" sweep: it builds a frame
// with files carrying special properties (non-root ownership, setuid/setgid
// bits, hardlinks, a symlink), snaps it, forks a fresh frame from the snap,
// and verifies every property survived — all over SSH against a real
// thundersnapd. It supersedes not_e2e/uid_test.go and hardlink_test.go,
// which checked the same properties by stat'ing the on-disk btrfs subvolume
// directly. Dedup (identical content → identical snap ID) is already covered
// by TestSSHContainerBasic and TestCrossInstanceSnapDeterminism.
package e2e

import (
	"strconv"
	"strings"
	"testing"
)

// installBusyboxApplets installs the named busybox applets into the frame over
// SFTP so the minimal rootfs (which only has the ts shell) can run stat/chmod/
// chown/ln for the fidelity probes.
func installBusyboxApplets(t *testing.T, d *daemonInstance, ref string, applets ...string) {
	t.Helper()
	for _, a := range applets {
		installBusyboxAppletInFrame(t, d, ref, a)
	}
}

// statField runs `busybox stat -c <format> <path>` over SSH and returns the
// trimmed stdout.
func statField(t *testing.T, d *daemonInstance, ref, format, path string) string {
	t.Helper()
	out, exit, err := sshExec(t, d, "root@"+ref, "stat -c '"+format+"' "+path)
	if err != nil || exit != 0 {
		t.Fatalf("stat -c %s %s (%s): err=%v exit=%d out=%q", format, path, ref, err, exit, out)
	}
	return strings.TrimSpace(out)
}

// TestSnapFidelityPreservesSpecialFiles verifies that non-root ownership,
// setuid/setgid bits, hardlinks, and symlinks all survive a snap + fork.
// Replaces not_e2e uid_test.go (TestUIDPermissionsBasic, TestSetuidPreservation,
// TestSetgidBinaryExecution, TestSetuidBinaryExecution, TestUIDPreservation,
// TestHardlinkSetuidBinaryInSnapshot) and hardlink_test.go (all three).
func testSnapFidelityPreservesSpecialFiles(t *testing.T, d *daemonInstance) {
	createFrameViaDaemon(t, d, "fid")

	// The minimal rootfs only has the ts shell; install busybox applets for
	// chmod/chown/ln/stat so we can create and probe special files.
	installBusyboxApplets(t, d, "fid", "chmod", "chown", "ln", "stat", "rm")

	// Non-root ownership: create a file and chown it to 1000:1000.
	if out, exit, err := sshExec(t, d, "root@fid", "echo owned > /work/owned.txt && chown 1000:1000 /work/owned.txt"); err != nil || exit != 0 {
		t.Fatalf("create+chown owned.txt: err=%v exit=%d out=%q", err, exit, out)
	}
	// setuid + setgid bits on scripts.
	if out, exit, err := sshExec(t, d, "root@fid", "echo '#!/bin/sh' > /work/suid.sh && chmod 4755 /work/suid.sh"); err != nil || exit != 0 {
		t.Fatalf("create suid.sh: err=%v exit=%d out=%q", err, exit, out)
	}
	if out, exit, err := sshExec(t, d, "root@fid", "echo '#!/bin/sh' > /work/sgid.sh && chmod 2755 /work/sgid.sh"); err != nil || exit != 0 {
		t.Fatalf("create sgid.sh: err=%v exit=%d out=%q", err, exit, out)
	}
	// Hardlink: /work/hard.txt shares the inode of /work/orig.txt.
	if out, exit, err := sshExec(t, d, "root@fid", "echo orig > /work/orig.txt && ln /work/orig.txt /work/hard.txt"); err != nil || exit != 0 {
		t.Fatalf("create hardlink: err=%v exit=%d out=%q", err, exit, out)
	}
	// Symlink: /work/link.txt -> /work/orig.txt.
	if out, exit, err := sshExec(t, d, "root@fid", "ln -s /work/orig.txt /work/link.txt"); err != nil || exit != 0 {
		t.Fatalf("create symlink: err=%v exit=%d out=%q", err, exit, out)
	}
	// Hardlink of a SETUID file: /work/suidbin_link shares the inode of
	// /work/suidbin (which also carries the setuid bit). This is the combined
	// invariant from not_e2e TestHardlinkSetuidBinaryInSnapshot: after a
	// snap+fork both paths must remain hardlinked (same inode, nlink >= 2) AND
	// both must retain the setuid bit — a snapshot that strips special mode
	// bits on hardlinked inodes, or breaks the link, would fail here. The
	// single-file setuid and hardlink checks above do not exercise this combo.
	if out, exit, err := sshExec(t, d, "root@fid", "echo '#!/bin/sh' > /work/suidbin && chmod 4755 /work/suidbin && ln /work/suidbin /work/suidbin_link"); err != nil || exit != 0 {
		t.Fatalf("create setuid hardlink: err=%v exit=%d out=%q", err, exit, out)
	}

	triplet := tsSnapWait(t, d, "fid")
	if out, exit, err := sshExec(t, d, "root@fid", "ts frame --ref=fidchild "+triplet); err != nil || exit != 0 {
		t.Fatalf("ts frame fidchild: err=%v exit=%d out=%q", err, exit, out)
	}

	// Ownership preserved.
	ug := statField(t, d, "fidchild", "%u %g", "/work/owned.txt")
	if ug != "1000 1000" {
		t.Errorf("owned.txt uid:gid = %q, want \"1000 1000\"", ug)
	} else {
		t.Logf("ownership preserved: %s", ug)
	}

	// setuid bit preserved.
	if out, exit, _ := sshExec(t, d, "root@fidchild", "test -u /work/suid.sh && echo SUID"); exit != 0 || !strings.Contains(out, "SUID") {
		t.Errorf("setuid bit not preserved on suid.sh (out=%q exit=%d)", out, exit)
	} else {
		t.Logf("setuid bit preserved")
	}
	// setgid bit preserved.
	if out, exit, _ := sshExec(t, d, "root@fidchild", "test -g /work/sgid.sh && echo SGID"); exit != 0 || !strings.Contains(out, "SGID") {
		t.Errorf("setgid bit not preserved on sgid.sh (out=%q exit=%d)", out, exit)
	} else {
		t.Logf("setgid bit preserved")
	}

	// Hardlink preserved: same inode.
	inoOrig := statField(t, d, "fidchild", "%i", "/work/orig.txt")
	inoHard := statField(t, d, "fidchild", "%i", "/work/hard.txt")
	if inoOrig == "" || inoOrig != inoHard {
		t.Errorf("hardlink broken: orig inode=%q hard inode=%q (want equal)", inoOrig, inoHard)
	} else {
		t.Logf("hardlink preserved: inode %s", inoOrig)
	}

	// Hardlink of a setuid file preserved: same inode, nlink >= 2, and BOTH
	// paths retain the setuid bit. Replaces not_e2e
	// TestHardlinkSetuidBinaryInSnapshot and the nlink>=2 check from
	// TestHardlinkHandlingBasic, which the single-file checks above do not.
	inoSuid := statField(t, d, "fidchild", "%i", "/work/suidbin")
	inoSuidLink := statField(t, d, "fidchild", "%i", "/work/suidbin_link")
	if inoSuid == "" || inoSuid != inoSuidLink {
		t.Errorf("setuid hardlink broken: suidbin inode=%q link inode=%q (want equal)", inoSuid, inoSuidLink)
	}
	nlinkSuid := statField(t, d, "fidchild", "%h", "/work/suidbin")
	if n, err := strconv.Atoi(nlinkSuid); err != nil || n < 2 {
		t.Errorf("setuid hardlink nlink=%q, want >= 2 (err=%v)", nlinkSuid, err)
	} else {
		t.Logf("setuid hardlink nlink=%d", n)
	}
	if out, exit, _ := sshExec(t, d, "root@fidchild", "test -u /work/suidbin && test -u /work/suidbin_link && echo SUIDS"); exit != 0 || !strings.Contains(out, "SUIDS") {
		t.Errorf("setuid bit not preserved on both hardlinked copies (out=%q exit=%d)", out, exit)
	} else {
		t.Logf("setuid bit preserved on both hardlinked copies")
	}

	// Symlink preserved: still a symlink reaching the target.
	if out, exit, _ := sshExec(t, d, "root@fidchild", "test -L /work/link.txt && read v < /work/link.txt && echo $v"); exit != 0 || !strings.Contains(out, "orig") {
		t.Errorf("symlink not preserved/working (out=%q exit=%d)", out, exit)
	} else {
		t.Logf("symlink preserved and reaches target")
	}
}
