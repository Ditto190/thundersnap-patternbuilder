// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// TestCrossInstanceSnapDeterminism verifies the foundation of content-addressed
// mesh replication: two INDEPENDENT thundersnapd daemons, given identical frame
// content (with identical mtimes), produce identical snap IDs. If two daemons
// could diverge for the same content, mesh dedup (snap ID equality implies
// identical content) and cross-daemon `ts download-snap` dedup would be
// unsound.
//
// This guards the guarantee lost when cmd/thundersnapd/full_e2e_test.go's
// TestE2ETwoInstancesSameSnap / TestE2ESnapDeterministic were deleted. Unlike
// the e2e suite's existing "snap idempotence" check (TestSSHContainerBasic),
// which snaps the SAME frame on the SAME daemon twice, this starts two
// separate daemons with separate data/state dirs and compares across them.
//
// mtime is part of the content-addressed snap ID (see tsm.encodeEntryForHash:
// ctime/atime are zeroed for the hash, mtime is kept, and the indexer zeroes
// only the root entry's timestamps). So the test writes identical content with
// identical fixed mtimes via SFTP Chtimes on both daemons, then snaps a subdir
// (/det) so the daemon-created minimal rootfs files (whose mtimes are
// wall-clock and would differ) are excluded from the hashed tree. The subdir
// snap's root (/det) has its timestamps zeroed in the hash, so only the files
// inside /det — fully under the test's control — contribute.
func TestCrossInstanceSnapDeterminism(t *testing.T) {
	env1 := newTestEnv(t)
	d1 := startDaemon(t, env1)
	env2 := newTestEnv(t)
	d2 := startDaemon(t, env2)

	// Create an independent frame on each daemon.
	createFrameViaDaemon(t, d1, "det")
	createFrameViaDaemon(t, d2, "det")

	// Write byte-for-byte identical content (files, modes, owners, mtimes)
	// into /det on each frame via SFTP — the same transport scp uses.
	writeDeterministicDetContent(t, d1, "det")
	writeDeterministicDetContent(t, d2, "det")

	// Snap the /det subdir on each daemon, waiting for indexing so the
	// content-addressed ID is known.
	id1 := snapSubdirWait(t, d1, "det", "/det")
	id2 := snapSubdirWait(t, d2, "det", "/det")
	t.Logf("cross-instance snap IDs: d1=%s d2=%s", id1, id2)

	if id1 == "" || id2 == "" {
		t.Fatalf("expected non-empty snap IDs from both daemons (d1=%q d2=%q)", id1, id2)
	}
	if id1 != id2 {
		t.Fatalf("cross-instance snap determinism broken: identical /det content on two daemons produced different snap IDs (d1=%s d2=%s)", id1, id2)
	}
	t.Logf("PASS: two independent daemons produced identical snap ID %s for identical /det content", id1)
}

// writeDeterministicDetContent creates an identical /det subtree (two files of
// distinct sizes + a nested directory with its own file) in the given frame,
// with every entry's mtime pinned to a fixed value.
//
// Pinning mtimes is required because mtime is part of the snap ID (see
// tsm.encodeEntryForHash: ctime/atime are zeroed for the hash, mtime is kept,
// and the indexer zeroes only the root entry's timestamps). Without it, the
// two frames' files (created at different wall-clock times on different
// daemons) would content-address to different IDs even with identical bytes.
//
// File bytes are written over SFTP (exact content + explicit 0644 mode); mtimes
// are then pinned over SSH with busybox `touch -d @<epoch>`. (We can't pin
// mtimes via SFTP Chtimes: sftpfs's Setstat handler honors only Size and
// Permissions, not times, so Chtimes is currently a silent no-op. busybox touch
// is installed into the frame first via installBusyboxAppletInFrame.)
func writeDeterministicDetContent(t *testing.T, d *daemonInstance, refName string) {
	t.Helper()

	// Install the busybox `touch` applet so we can pin mtimes from inside the
	// minimal container (no other touch is available in a nil:nil:nil frame).
	installBusyboxAppletInFrame(t, d, refName, "touch")

	// Write identical bytes via SFTP (the same transport scp uses).
	conn, err := ssh.Dial("tcp", d.addr, sshConfig("root@"+refName))
	if err != nil {
		t.Fatalf("sftp dial (%s): %v", refName, err)
	}
	defer conn.Close()
	sc, err := sftp.NewClient(conn)
	if err != nil {
		t.Fatalf("sftp client (%s): %v", refName, err)
	}
	defer sc.Close()

	if err := sc.Mkdir("/det"); err != nil && !isSftpExist(err) {
		t.Fatalf("sftp mkdir /det: %v", err)
	}
	if err := sc.Mkdir("/det/nested"); err != nil && !isSftpExist(err) {
		t.Fatalf("sftp mkdir /det/nested: %v", err)
	}
	files := []struct {
		path    string
		content []byte
	}{
		{"/det/file.txt", []byte("hello from the determinism test\n")},
		{"/det/notes.md", []byte("# notes\nidentical content across two daemons\n")},
	}
	// 64 bytes of 'A' so /det/nested/deeper.bin is at least one full chunk of
	// deterministic content (exercises the chunk-ref path, not just tiny files).
	deeper := make([]byte, 64)
	for i := range deeper {
		deeper[i] = 'A'
	}
	files = append(files, struct {
		path    string
		content []byte
	}{"/det/nested/deeper.bin", deeper})

	for _, f := range files {
		out, err := sc.Create(f.path)
		if err != nil {
			t.Fatalf("sftp create %s: %v", f.path, err)
		}
		if _, err := out.Write(f.content); err != nil {
			out.Close()
			t.Fatalf("sftp write %s: %v", f.path, err)
		}
		if err := out.Close(); err != nil {
			t.Fatalf("sftp close %s: %v", f.path, err)
		}
		if err := sc.Chmod(f.path, 0644); err != nil {
			t.Fatalf("sftp chmod %s: %v", f.path, err)
		}
	}

	// Pin mtimes identically on both daemons via busybox touch. Do this AFTER
	// all writes so no subsequent write bumps a dir mtime. /det's own mtime is
	// zeroed in the subdir-snap hash (it's the root), but pin it anyway for
	// tidiness; /det/nested's mtime IS in the hash, so it must be pinned.
	const fixedEpoch = "@1700000000" // 2023-11-14T22:13:20Z
	pinCmd := fmt.Sprintf(
		"touch -d %s /det /det/file.txt /det/notes.md /det/nested /det/nested/deeper.bin",
		fixedEpoch)
	if out, exit, err := sshExec(t, d, "root@"+refName, pinCmd); err != nil || exit != 0 {
		t.Fatalf("pin mtimes via touch (%s): err=%v exit=%d out=%q", refName, err, exit, out)
	}
}

// snapSubdirWait runs `ts snap <path> --wait` over SSH and returns the snap ID
// printed on stdout. --wait blocks until background indexing finishes and the
// content-addressed ID is known.
func snapSubdirWait(t *testing.T, d *daemonInstance, refName, subdir string) string {
	t.Helper()
	// Flag before the path: the getopt parser used by `ts snap` does not
	// permute args, so `ts snap /det --wait` would treat --wait as a second
	// positional and fail with "snap takes at most one path argument".
	stdout, stderr, exit, err := sshExecSplit(t, d, "root@"+refName, fmt.Sprintf("ts snap --wait %s", subdir))
	if err != nil {
		t.Fatalf("ts snap %s --wait (%s): %v (stdout=%q stderr=%q)", subdir, refName, err, stdout, stderr)
	}
	if exit != 0 {
		t.Fatalf("ts snap %s --wait (%s): exit %d (stdout=%q stderr=%q)", subdir, refName, exit, stdout, stderr)
	}
	id := strings.TrimSpace(stdout)
	if id == "" {
		t.Fatalf("ts snap %s --wait (%s): empty snap ID (stdout=%q stderr=%q)", subdir, refName, stdout, stderr)
	}
	return id
}

// isSftpExist reports whether an sftp error is a "file already exists" status,
// so callers can treat Mkdir on an existing dir as success.
func isSftpExist(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "Failure") || strings.Contains(err.Error(), "exists") ||
		strings.Contains(err.Error(), "file already")
}
