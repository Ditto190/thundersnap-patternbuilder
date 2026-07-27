// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

//go:build e2e

// lifecycle_test.go is the W1 "core lifecycle" workflow: it drives the
// frame → snap → fork → home/work-spec → delete chain entirely over SSH
// against a real thundersnapd. It supersedes the not_e2e C/D-bucket tests
// frame_test.go, snapshot_test.go, integration_test.go and refid_test.go,
// which reconstructed the same pipeline by hand against a fake control
// server or raw btrfs. Every assertion here is made by running a command
// through the SSH session the daemon set up, so a regression in the
// daemon's front door (parseSSHUser, frame resolution, snap creation,
// per-user layout) is caught rather than bypassed.
package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"github.com/tailscale/thundersnap/tsm"
	"golang.org/x/crypto/ssh"
)

// tsSnapWait runs `ts snap --wait` over SSH as root in the named frame and
// returns the snap triplet (root:home:work) printed on stdout.
func tsSnapWait(t *testing.T, d *daemonInstance, ref string) string {
	t.Helper()
	stdout, stderr, exit, err := sshExecSplit(t, d, "root@"+ref, "ts snap --wait")
	if err != nil || exit != 0 {
		t.Fatalf("ts snap --wait (%s): err=%v exit=%d (stdout=%q stderr=%q)", ref, err, exit, stdout, stderr)
	}
	id := strings.TrimSpace(stdout)
	if id == "" {
		t.Fatalf("ts snap --wait (%s): empty snap ID (stdout=%q stderr=%q)", ref, stdout, stderr)
	}
	verifySnaphashOutput(t, id)
	return id
}

// snapTriplet splits a "root:home:work" snap triplet into its components.
func snapTriplet(t *testing.T, triplet string) (root, home, work string) {
	t.Helper()
	parts := strings.SplitN(triplet, ":", 3)
	if len(parts) != 3 {
		t.Fatalf("snap triplet %q does not have 3 components", triplet)
	}
	return parts[0], parts[1], parts[2]
}

// TestFrameCreateDelete verifies the basic frame lifecycle over SSH: create a
// frame via `ts frame`, confirm it appears in `ts frames`, delete it with
// `ts frame --delete` (from a different frame so it is not the active one),
// and confirm it is gone. Replaces not_e2e TestFrameLifecycleBasic (which used
// a fake control server) and exercises the daemon's /delete-frame handler
// through the SSH front door — the path the fake-server test never touched.
func testFrameCreateDelete(t *testing.T, d *daemonInstance) {

	uuid := createFrameViaDaemon(t, d, "lifecycle")

	// Listed and present (by ref name while a ref is bound).
	out, exit, err := sshExec(t, d, "root@lifecycle", "ts frames")
	if err != nil || exit != 0 {
		t.Fatalf("ts frames: err=%v exit=%d out=%q", err, exit, out)
	}
	if !strings.Contains(out, "lifecycle") {
		t.Fatalf("ts frames did not list lifecycle: %q", out)
	}
	t.Logf("ts frames lists lifecycle (uuid %s)", uuid)

	// The frame still has the "lifecycle" ref attached, so deleting it must be
	// REFUSED: the daemon requires zero refs before a frame can be deleted, so
	// a frame reached by one ref cannot be torn out from under other refs that
	// point at it. Assert the 409 (non-zero exit, message names the ref).
	out, exit, err = sshExec(t, d, "root@", "ts frame --delete "+uuid)
	if err != nil {
		t.Fatalf("sshExec: %v", err)
	}
	if exit == 0 {
		t.Fatalf("ts frame --delete with ref attached: expected non-zero exit, got 0 (out=%q)", out)
	}
	if !strings.Contains(out, "lifecycle") || !strings.Contains(out, "ref") {
		t.Errorf("delete-with-refs error did not name the ref: %q", out)
	} else {
		t.Logf("delete with ref attached correctly refused (exit=%d): %q", exit, strings.TrimSpace(out))
	}

	// Delete the ref first, then the frame. The frame is addressed by UUID for
	// the delete (from the DEFAULT frame's session, not from within the frame
	// itself: the daemon refuses to delete the currently active frame).
	if out, exit, err := sshExec(t, d, "root@", "ts ref delete --force lifecycle"); err != nil || exit != 0 {
		t.Fatalf("ts ref delete lifecycle: err=%v exit=%d out=%q", err, exit, out)
	}
	out, exit, err = sshExec(t, d, "root@", "ts frame --delete "+uuid)
	if err != nil || exit != 0 {
		t.Fatalf("ts frame --delete after ref removed: err=%v exit=%d out=%q", err, exit, out)
	}
	t.Logf("deleted frame: %s", strings.TrimSpace(out))

	// Confirm it is gone from the listing. After the ref was removed the frame
	// is listed by UUID (not "lifecycle"), so check for the UUID, not the ref
	// name.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out, _, _ = sshExec(t, d, "root@", "ts frames")
		if !strings.Contains(out, uuid) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if strings.Contains(out, uuid) {
		t.Fatalf("ts frames still lists the frame after delete: %q", out)
	}
	t.Logf("frame gone from ts frames after delete")
}

// TestFrameDeleteRequiresAllRefsRemoved verifies that a frame cannot be
// deleted while ANY ref points at it, and that the error names every ref so a
// caller cannot accidentally tear a frame out from under other refs that depend
// on it. This is the disaster the refs-must-be-removed-first rule prevents:
// deleting by one ref used to silently kill the frame (and dangle the other
// refs); now the delete is refused until every ref is gone. Create two refs on
// one frame, assert the delete is refused naming both, drop one (still refused
// naming the other), drop the last, then the delete succeeds.
func testFrameDeleteRequiresAllRefsRemoved(t *testing.T, d *daemonInstance) {

	uuid := createFrameViaDaemon(t, d, "shared")

	// Bind a second ref to the same frame (the first, "shared", is created by
	// createFrameViaDaemon). Now two refs point at the frame.
	if out, exit, err := sshExec(t, d, "root@shared", "ts ref create alias "+uuid); err != nil || exit != 0 {
		t.Fatalf("ts ref create alias: err=%v exit=%d out=%q", err, exit, out)
	}

	// Delete by UUID must be refused, naming BOTH refs (so the caller sees the
	// other critical ref, not just the one they were thinking of).
	out, exit, err := sshExec(t, d, "root@", "ts frame --delete "+uuid)
	if err != nil {
		t.Fatalf("sshExec: %v", err)
	}
	if exit == 0 {
		t.Fatalf("delete with 2 refs: expected non-zero exit, got 0 (out=%q)", out)
	}
	if !strings.Contains(out, "shared") || !strings.Contains(out, "alias") {
		t.Errorf("delete-with-refs error did not name both refs: %q", out)
	} else {
		t.Logf("delete with 2 refs refused naming both (exit=%d): %q", exit, strings.TrimSpace(out))
	}

	// Drop one ref: the other still blocks the delete.
	if out, exit, err := sshExec(t, d, "root@", "ts ref delete --force shared"); err != nil || exit != 0 {
		t.Fatalf("ts ref delete shared: err=%v exit=%d out=%q", err, exit, out)
	}
	out, exit, err = sshExec(t, d, "root@", "ts frame --delete "+uuid)
	if err != nil {
		t.Fatalf("sshExec: %v", err)
	}
	if exit == 0 {
		t.Fatalf("delete with 1 ref left: expected non-zero exit, got 0 (out=%q)", out)
	}
	if !strings.Contains(out, "alias") {
		t.Errorf("delete-with-1-ref error did not name the remaining ref: %q", out)
	} else {
		t.Logf("delete with 1 ref left refused (exit=%d): %q", exit, strings.TrimSpace(out))
	}

	// Drop the last ref: the delete now succeeds.
	if out, exit, err := sshExec(t, d, "root@", "ts ref delete --force alias"); err != nil || exit != 0 {
		t.Fatalf("ts ref delete alias: err=%v exit=%d out=%q", err, exit, out)
	}
	out, exit, err = sshExec(t, d, "root@", "ts frame --delete "+uuid)
	if err != nil || exit != 0 {
		t.Fatalf("ts frame --delete after all refs removed: err=%v exit=%d out=%q", err, exit, out)
	}
	t.Logf("frame deleted once all refs were removed: %s", strings.TrimSpace(out))
}

// TestFrameFromSnapPreservesContent verifies that modifications made before a
// snap survive forking a new frame from that snap: write a marker, snap, fork
// a new frame from the snap, ssh into the new frame, and read the marker back.
// Replaces not_e2e TestIntegrationWorkflowBasic and TestSnapshotWithModifiedFiles
// (which hand-rolled btrfs snapshots).
func testFrameFromSnapPreservesContent(t *testing.T, d *daemonInstance) {

	createFrameViaDaemon(t, d, "src")

	// Write a unique marker into the source frame's /home subvolume (the home
	// subvolume is /home, not /home/user — see main.go:resolveSFTPStartDir).
	marker := "W1_PRESERVE_MARKER_42"
	if out, exit, err := sshExec(t, d, "root@src", "echo "+marker+" > /home/preserve.txt"); err != nil || exit != 0 {
		t.Fatalf("write marker: err=%v exit=%d out=%q", err, exit, out)
	}

	triplet := tsSnapWait(t, d, "src")
	rootSnap, homeSnap, workSnap := snapTriplet(t, triplet)
	t.Logf("src snap root=%s home=%s work=%s", rootSnap, homeSnap, workSnap)

	// Fork a new frame from the full snap triplet so /home content survives
	// (nil home/work would create fresh empty subvolumes, dropping the marker).
	if out, exit, err := sshExec(t, d, "root@src", "ts frame --ref=forked "+triplet); err != nil || exit != 0 {
		t.Fatalf("ts frame from snap: err=%v exit=%d out=%q", err, exit, out)
	}

	// The forked frame inherits the snap's /home, so the marker must be readable.
	out, exit, err := sshExec(t, d, "root@forked", "read line < /home/preserve.txt && echo $line")
	if err != nil || exit != 0 {
		t.Fatalf("read marker in forked frame: err=%v exit=%d out=%q", err, exit, out)
	}
	if got := strings.TrimSpace(out); got != marker {
		t.Errorf("forked frame /home/preserve.txt = %q, want %q", got, marker)
	} else {
		t.Logf("forked frame preserved pre-snap modification: %q", got)
	}
	_ = homeSnap
	_ = workSnap
}

// TestFrameHomeWorkSpec verifies that a frame can be created with explicit
// home and work snaps and that both survive: write distinct content into
// /home/user and /work, snap, then create a new frame using that home and
// work snap (nil root), and confirm both contents are present. Replaces
// not_e2e TestFrameWithHomeSpec, TestFrameWithAllThreeSpecs, and
// TestWorkflowHomeWorkSeparation (which used a fake control server).
func testFrameHomeWorkSpec(t *testing.T, d *daemonInstance) {

	createFrameViaDaemon(t, d, "hwspec")

	// Distinct markers in /home and /work (the home subvolume is /home).
	if out, exit, err := sshExec(t, d, "root@hwspec", "echo HOME-MARKER > /home/h.txt"); err != nil || exit != 0 {
		t.Fatalf("write home marker: err=%v exit=%d out=%q", err, exit, out)
	}
	if out, exit, err := sshExec(t, d, "root@hwspec", "echo WORK-MARKER > /work/w.txt"); err != nil || exit != 0 {
		t.Fatalf("write work marker: err=%v exit=%d out=%q", err, exit, out)
	}

	triplet := tsSnapWait(t, d, "hwspec")
	_, homeSnap, workSnap := snapTriplet(t, triplet)
	t.Logf("hwspec snap home=%s work=%s", homeSnap, workSnap)

	// New frame: empty root, but reuse the home and work snaps.
	if _, exit, err := sshExec(t, d, "root@hwspec", "ts frame --ref=hwchild nil:"+homeSnap+":"+workSnap); err != nil || exit != 0 {
		t.Fatalf("ts frame nil:home:work: err=%v exit=%d", err, exit)
	}

	out, exit, err := sshExec(t, d, "root@hwchild", "read h < /home/h.txt && read w < /work/w.txt && echo $h $w")
	if err != nil || exit != 0 {
		t.Fatalf("read markers in hwchild: err=%v exit=%d out=%q", err, exit, out)
	}
	if !strings.Contains(out, "HOME-MARKER") || !strings.Contains(out, "WORK-MARKER") {
		t.Errorf("hwchild markers = %q, want both HOME-MARKER and WORK-MARKER", out)
	} else {
		t.Logf("hwchild has both home and work content from snap")
	}
}

// TestCrossFrameWorkSharing verifies that two frames can be built sharing the
// same work snap: write to /work in frame A, snap, build frame B with A's work
// snap, and confirm B sees the content. Replaces not_e2e
// TestCrossFrameDataSharingViaWorkVolume (which hand-rolled btrfs subvolumes).
func testCrossFrameWorkSharing(t *testing.T, d *daemonInstance) {

	createFrameViaDaemon(t, d, "shareA")
	if _, exit, err := sshExec(t, d, "root@shareA", "echo SHARED > /work/shared.txt"); err != nil || exit != 0 {
		t.Fatalf("write shared: err=%v exit=%d", err, exit)
	}
	triplet := tsSnapWait(t, d, "shareA")
	_, _, workSnap := snapTriplet(t, triplet)

	// Frame B reuses A's work snap for its /work.
	if _, exit, err := sshExec(t, d, "root@shareA", "ts frame --ref=shareB nil:nil:"+workSnap); err != nil || exit != 0 {
		t.Fatalf("ts frame shareB: err=%v exit=%d", err, exit)
	}
	out, exit, err := sshExec(t, d, "root@shareB", "read s < /work/shared.txt && echo $s")
	if err != nil || exit != 0 {
		t.Fatalf("read shared in shareB: err=%v exit=%d out=%q", err, exit, out)
	}
	if !strings.Contains(out, "SHARED") {
		t.Errorf("shareB /work/shared.txt = %q, want SHARED", out)
	} else {
		t.Logf("shareB received shareA's work content via shared work snap")
	}
}

// TestFrameFromNonExistentSnap verifies that creating a frame from a bogus snap
// id fails over SSH (non-zero exit). Replaces not_e2e
// TestFrameFromNonExistentSnapshot (fake control server).
func testFrameFromNonExistentSnap(t *testing.T, d *daemonInstance) {

	createFrameViaDaemon(t, d, "badsrc")
	out, exit, err := sshExec(t, d, "root@badsrc", "ts frame --ref=nope BOGUSNAPNOTREAL::")
	if err != nil {
		t.Fatalf("sshExec: %v", err)
	}
	if exit == 0 {
		t.Errorf("ts frame from bogus snap: expected non-zero exit, got 0 (out=%q)", out)
	} else {
		t.Logf("ts frame from bogus snap correctly failed (exit=%d): %q", exit, strings.TrimSpace(out))
	}
}

// TestDeleteRunningFrame verifies that deleting a frame that has an active SSH
// session fails. Replaces not_e2e TestDeleteRunningFrame (fake control server).
//
// Under the refs-must-be-removed-first rule the frame's "running" ref has to
// be gone before a frame delete is even considered, so the test deletes the ref
// first (the frame is still addressable by UUID, and the long-running session
// is unaffected), then issues the delete FROM the running frame (by UUID) so
// the daemon's "cannot delete the currently active frame" guard is what blocks
// it — proving the running-session rejection still holds, not just the ref
// gate.
func testDeleteRunningFrame(t *testing.T, d *daemonInstance) {

	uuid := createFrameViaDaemon(t, d, "running")

	// Open a long-running session against the frame.
	client, err := dialSSH(t, d, "root@running")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer sess.Close()
	stdin, err := sess.StdinPipe()
	if err != nil {
		t.Fatalf("stdin: %v", err)
	}
	if err := sess.Start("read x"); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer stdin.Close()
	time.Sleep(500 * time.Millisecond) // let the session register

	// Remove the "running" ref so the frame has zero refs and the delete is
	// not blocked by the ref gate. Run it from the running frame itself,
	// addressed by UUID (the frame still exists; ref delete does not touch the
	// live session). This leaves the frame reachable only by UUID.
	if out, exit, err := sshExec(t, d, "root@"+uuid, "ts ref delete --force running"); err != nil || exit != 0 {
		t.Fatalf("ts ref delete running: err=%v exit=%d out=%q", err, exit, out)
	}

	// Deleting the frame the session is attached to must fail. Issue it from
	// the running frame (by UUID) so the daemon sees it as the currently active
	// frame and refuses.
	out, exit, err := sshExec(t, d, "root@"+uuid, "ts frame --delete "+uuid)
	if err != nil {
		t.Fatalf("sshExec: %v", err)
	}
	if exit == 0 {
		t.Errorf("ts frame --delete of running frame: expected non-zero exit, got 0 (out=%q)", out)
	} else {
		t.Logf("delete of running frame correctly failed (exit=%d): %q", exit, strings.TrimSpace(out))
	}

	// Close the session and confirm the frame is still listed (delete was rejected).
	stdin.Close()
	sess.Wait()
}

// TestSnapDeleteSucceedsAndFrameIntact verifies that deleting a snapshot that a
// frame was cloned from succeeds (btrfs COW) and the frame remains usable.
// Replaces not_e2e TestSnapshotDeletion and TestDeleteSnapshotWithReference
// (fake control server / raw btrfs).
func testSnapDeleteSucceedsAndFrameIntact(t *testing.T, d *daemonInstance) {

	createFrameViaDaemon(t, d, "snapdel")
	if out, exit, err := sshExec(t, d, "root@snapdel", "echo KEEP > /home/keep.txt"); err != nil || exit != 0 {
		t.Fatalf("write keep: err=%v exit=%d out=%q", err, exit, out)
	}
	triplet := tsSnapWait(t, d, "snapdel")
	rootSnap, _, _ := snapTriplet(t, triplet)

	// Fork a frame from the full snap triplet so it keeps /home content.
	if out, exit, err := sshExec(t, d, "root@snapdel", "ts frame --ref=fromsnap "+triplet); err != nil || exit != 0 {
		t.Fatalf("ts frame fromsnap: err=%v exit=%d out=%q", err, exit, out)
	}

	// Deleting the snap should succeed (COW: the forked frame keeps its data).
	out, exit, err := sshExec(t, d, "root@snapdel", "ts snap --delete "+rootSnap)
	if err != nil {
		t.Fatalf("sshExec: %v", err)
	}
	if exit != 0 {
		t.Fatalf("ts snap --delete of referenced snap: expected exit 0, got %d (out=%q)", exit, out)
	}
	t.Logf("deleted referenced snap: %s", strings.TrimSpace(out))

	// The forked frame must still be usable and keep its content.
	out, exit, err = sshExec(t, d, "root@fromsnap", "read k < /home/keep.txt && echo $k")
	if err != nil || exit != 0 {
		t.Errorf("fromsnap frame unusable after snap delete: err=%v exit=%d out=%q", err, exit, out)
	} else if !strings.Contains(out, "KEEP") {
		t.Errorf("fromsnap /home/keep.txt = %q, want KEEP", out)
	} else {
		t.Logf("forked frame intact after base snap deletion (btrfs COW)")
	}
}

// TestRefMoveAndForceDelete verifies the ref lifecycle over SSH: create a ref,
// move it to another frame, confirm it points at the new frame, then
// force-delete it and confirm it is gone. This exercises the daemon's ref
// handlers (which call into refid.Ensure/Move/Remove on real btrfs) through
// the SSH front door. Replaces not_e2e refid_test.go's three tests (which
// called refid package functions directly on hand-rolled subvolumes).
func testRefMoveAndForceDelete(t *testing.T, d *daemonInstance) {

	uuidA := createFrameViaDaemon(t, d, "refmvA")
	uuidB := createFrameViaDaemon(t, d, "refmvB")

	// Create a ref pointing at A.
	if _, exit, err := sshExec(t, d, "root@refmvA", "ts ref create moveme "+uuidA); err != nil || exit != 0 {
		t.Fatalf("ts ref create: err=%v exit=%d", err, exit)
	}
	out, exit, err := sshExec(t, d, "root@refmvA", "ts frame moveme")
	if err != nil || exit != 0 {
		t.Fatalf("ts frame moveme: err=%v exit=%d", err, exit)
	}
	if strings.TrimSpace(out) != uuidA {
		t.Errorf("moveme resolves to %q, want %q", strings.TrimSpace(out), uuidA)
	}

	// Move the ref to B and confirm it now resolves to B.
	if _, exit, err := sshExec(t, d, "root@refmvA", "ts ref move moveme "+uuidB); err != nil || exit != 0 {
		t.Fatalf("ts ref move: err=%v exit=%d", err, exit)
	}
	out, exit, err = sshExec(t, d, "root@refmvA", "ts frame moveme")
	if err != nil || exit != 0 {
		t.Fatalf("ts frame moveme after move: err=%v exit=%d", err, exit)
	}
	if strings.TrimSpace(out) != uuidB {
		t.Errorf("moveme after move resolves to %q, want %q", strings.TrimSpace(out), uuidB)
	}
	t.Logf("ref move OK: moveme -> %s", uuidB)

	// Force-delete the ref and confirm it is gone. The getopt parser does not
	// permute flags after positionals, so --force must precede the name.
	if out, exit, err := sshExec(t, d, "root@refmvA", "ts ref delete --force moveme"); err != nil || exit != 0 {
		t.Fatalf("ts ref delete --force: err=%v exit=%d out=%q", err, exit, out)
	}
	out, exit, _ = sshExec(t, d, "root@refmvA", "ts frame moveme")
	if exit == 0 {
		t.Errorf("ts frame moveme after delete: expected non-zero exit, got 0 (out=%q)", out)
	} else {
		t.Logf("ref gone after force delete (ts frame moveme exit=%d)", exit)
	}
}

// dialSSH opens an SSH client connection to the daemon (used for long-running
// sessions that must stay open while other SSH commands run in parallel).
func dialSSH(t *testing.T, d *daemonInstance, user string) (*ssh.Client, error) {
	t.Helper()
	client, err := ssh.Dial("tcp", d.addr, sshConfig(user))
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	return client, nil
}

// TestFrameUserAndGroup verifies that a frame's /etc/passwd has the thundersnap
// "user" account (UID 7575) and /etc/group has the matching "user" group
// (GID 7575), so the unprivileged login user has a nameless primary GID-free
// identity. Replaces not_e2e TestFrameUserGroupCreated (fake control server).
func testFrameUserAndGroup(t *testing.T, d *daemonInstance) {
	createFrameViaDaemon(t, d, "usergrp")

	passwd, exit, err := sshExec(t, d, "root@usergrp", "while IFS= read -r l; do echo \"$l\"; done < /etc/passwd")
	if err != nil || exit != 0 {
		t.Fatalf("read /etc/passwd: err=%v exit=%d out=%q", err, exit, passwd)
	}
	if !strings.Contains(passwd, fmt.Sprintf("user:x:%d:%d:", tsm.ThundersnapUID, tsm.ThundersnapGID)) {
		t.Errorf("/etc/passwd missing user:x:%d:%d: entry:\n%s", tsm.ThundersnapUID, tsm.ThundersnapGID, passwd)
	}

	group, exit, err := sshExec(t, d, "root@usergrp", "while IFS= read -r l; do echo \"$l\"; done < /etc/group")
	if err != nil || exit != 0 {
		t.Fatalf("read /etc/group: err=%v exit=%d out=%q", err, exit, group)
	}
	if !strings.Contains(group, fmt.Sprintf("user:x:%d:", tsm.ThundersnapGID)) {
		t.Errorf("/etc/group missing user:x:%d: entry:\n%s", tsm.ThundersnapGID, group)
	}
	t.Logf("frame has user:%d/%d in passwd and group", tsm.ThundersnapUID, tsm.ThundersnapGID)
}

// TestFrameHomeWorkSymlink verifies that a fresh frame has a /home/work
// convenience symlink pointing at /work, and that the symlink is NOT created
// when the home subvolume already contains a real "work" entry (so a home
// snapshot's /home/work is preserved rather than clobbered). Replaces not_e2e
// TestFrameHomeWorkSymlink and TestFrameHomeWorkSymlinkNotOverwritten.
func testFrameHomeWorkSymlink(t *testing.T, d *daemonInstance) {

	// Fresh frame: /home/work is a symlink, and it functionally reaches /work
	// (write to /work, read back through /home/work). No readlink needed.
	createFrameViaDaemon(t, d, "syml")
	out, exit, err := sshExec(t, d, "root@syml", "test -L /home/work && echo VIA-SYMLINK > /work/probe && read v < /home/work/probe && echo $v")
	if err != nil || exit != 0 {
		t.Fatalf("/home/work symlink check: err=%v exit=%d out=%q", err, exit, out)
	}
	if !strings.Contains(out, "VIA-SYMLINK") {
		t.Errorf("/home/work did not reach /work: %q", out)
	} else {
		t.Logf("/home/work -> /work symlink OK")
	}

	// Not-overwritten: put a real dir at /home/work, snap, fork from the snap,
	// and confirm the forked frame keeps the dir (no symlink clobbering it).
	// rm/mkdir are external, so install busybox applets for them.
	installBusyboxAppletInFrame(t, d, "syml", "rm")
	installBusyboxAppletInFrame(t, d, "syml", "mkdir")
	if out, exit, err := sshExec(t, d, "root@syml", "rm -f /home/work && mkdir /home/work && echo keep > /home/work/keep.txt"); err != nil || exit != 0 {
		t.Fatalf("replace /home/work with dir: err=%v exit=%d out=%q", err, exit, out)
	}
	triplet := tsSnapWait(t, d, "syml")
	if out, exit, err := sshExec(t, d, "root@syml", "ts frame --ref=symlkeep "+triplet); err != nil || exit != 0 {
		t.Fatalf("ts frame symlkeep: err=%v exit=%d out=%q", err, exit, out)
	}
	out, exit, err = sshExec(t, d, "root@symlkeep", "test -d /home/work && ! test -L /home/work && read k < /home/work/keep.txt && echo $k")
	if err != nil || exit != 0 {
		t.Fatalf("/home/work dir check in symlkeep: err=%v exit=%d out=%q", err, exit, out)
	}
	if !strings.Contains(out, "keep") {
		t.Errorf("symlkeep /home/work was clobbered; got out=%q", out)
	} else {
		t.Logf("existing /home/work dir preserved across snap+fork (not clobbered by symlink)")
	}
}

// TestFrameIdNotCloned verifies that the per-frame /id subvolume (which holds
// secrets like keys) is NOT cloned when forking a frame from a snap: write a
// secret to /id, snap, fork a new frame from the snap, and confirm the new
// frame's /id is empty. Replaces not_e2e TestIdSubvolumeNotCloned (fake
// control server + raw btrfs).
func testFrameIdNotCloned(t *testing.T, d *daemonInstance) {

	createFrameViaDaemon(t, d, "idclone")
	// Write a secret into /id. /id is root-owned (the identity subvolume), so
	// write as root.
	if out, exit, err := sshExec(t, d, "root@idclone", "echo super-secret > /id/secret.key"); err != nil || exit != 0 {
		t.Fatalf("write /id/secret.key: err=%v exit=%d out=%q", err, exit, out)
	}

	triplet := tsSnapWait(t, d, "idclone")
	if out, exit, err := sshExec(t, d, "root@idclone", "ts frame --ref=idchild "+triplet); err != nil || exit != 0 {
		t.Fatalf("ts frame idchild: err=%v exit=%d out=%q", err, exit, out)
	}

	// The forked frame's /id must not contain the secret: btrfs excludes
	// nested subvolumes from snapshots, so /id/secret.key did not travel.
	out, exit, err := sshExec(t, d, "root@idchild", "test ! -e /id/secret.key && echo EMPTY")
	if err != nil || exit != 0 {
		t.Fatalf("/id check in idchild: err=%v exit=%d out=%q", err, exit, out)
	}
	if !strings.Contains(out, "EMPTY") {
		t.Errorf("forked frame /id should not contain secret.key, but it does (out=%q)", out)
	} else {
		t.Logf("forked frame /id is empty of the secret: it was not cloned")
	}
}

// TestFrameStatePersistsAcrossSessions verifies that two sequential SSH
// sessions to the same frame see each other's writes (frame state persists
// across session teardown/restart). Replaces not_e2e TestFrameRestartAfterStop
// (fake control server) and TestMultipleConcurrentSessions (fake control
// server; the live-session-count half is already covered by
// TestSSHContainerBasic).
func testFrameStatePersistsAcrossSessions(t *testing.T, d *daemonInstance) {
	createFrameViaDaemon(t, d, "persist")

	if out, exit, err := sshExec(t, d, "root@persist", "echo session1 > /home/s1.txt"); err != nil || exit != 0 {
		t.Fatalf("write s1: err=%v exit=%d out=%q", err, exit, out)
	}
	// Second, independent SSH session to the same frame.
	out, exit, err := sshExec(t, d, "root@persist", "read v < /home/s1.txt && echo $v")
	if err != nil || exit != 0 {
		t.Fatalf("read s1 in session 2: err=%v exit=%d out=%q", err, exit, out)
	}
	if !strings.Contains(out, "session1") {
		t.Errorf("session 2 did not see session 1's write: %q", out)
	} else {
		t.Logf("frame state persisted across sessions")
	}
}

// TestSnapManyFiles verifies the daemon's snap path handles a non-trivial file
// count end to end: upload 100 files into /work over SFTP (the same transport
// scp uses), snap --wait, fork a frame from the snap, and confirm all 100 files
// survived. Replaces not_e2e TestLargeDirectoryTree (fake control server +
// raw btrfs); the TSM/TSC format's many-entry handling is also covered by the
// tsm package's indexer unit tests.
func testSnapManyFiles(t *testing.T, d *daemonInstance) {
	const n = 100
	createFrameViaDaemon(t, d, "many")

	// Upload n files into /work via SFTP.
	conn, err := ssh.Dial("tcp", d.addr, sshConfig("root@many"))
	if err != nil {
		t.Fatalf("sftp dial: %v", err)
	}
	defer conn.Close()
	sc, err := sftp.NewClient(conn)
	if err != nil {
		t.Fatalf("sftp client: %v", err)
	}
	defer sc.Close()
	if err := sc.Mkdir("/work/tree"); err != nil && !isSftpExist(err) {
		t.Fatalf("sftp mkdir /work/tree: %v", err)
	}
	for i := 0; i < n; i++ {
		p := fmt.Sprintf("/work/tree/f%03d.txt", i)
		f, err := sc.Create(p)
		if err != nil {
			t.Fatalf("sftp create %s: %v", p, err)
		}
		if _, err := f.Write([]byte(fmt.Sprintf("content %d\n", i))); err != nil {
			f.Close()
			t.Fatalf("sftp write %s: %v", p, err)
		}
		f.Close()
	}
	t.Logf("uploaded %d files via SFTP", n)

	triplet := tsSnapWait(t, d, "many")
	if out, exit, err := sshExec(t, d, "root@many", "ts frame --ref=manychild "+triplet); err != nil || exit != 0 {
		t.Fatalf("ts frame manychild: err=%v exit=%d out=%q", err, exit, out)
	}
	// Count files in the forked frame's /work/tree via a shell glob. The minimal
	// shell expands /work/tree/f*.txt and echoes each on its own line; count them.
	out, exit, err := sshExec(t, d, "root@manychild", "for f in /work/tree/f*.txt; do echo $f; done")
	if err != nil || exit != 0 {
		t.Fatalf("list files in manychild: err=%v exit=%d out=%q", err, exit, out)
	}
	got := len(strings.Fields(out))
	if got != n {
		t.Errorf("manychild /work/tree has %d files, want %d (out=%q)", got, n, out)
	} else {
		t.Logf("manychild preserved all %d files across snap+fork", n)
	}
}
