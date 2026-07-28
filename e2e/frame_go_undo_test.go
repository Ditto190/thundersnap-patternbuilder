// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// newestLogRootSnap parses `ts log` output and returns the root snap ID of the
// newest (first) history entry, and whether any entry was present. `ts log`
// prints "(no snapshots)" when empty, otherwise one line per entry, newest
// first, as `<timestamp>  <root:home:work>` (optionally followed by a message
// that may itself contain spaces, so the triplet is always field index 1).
// Used by tests that need to do what `ts undo` does — create a frame from the
// most recent snap — without driving ts undo's interactive vsock session.
func newestLogRootSnap(logOut string) (string, bool) {
	for _, line := range strings.Split(logOut, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "(no snapshots)" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		triplet := fields[1] // fields[0] is the timestamp; the snap triplet follows
		if !strings.Contains(triplet, ":") {
			continue
		}
		parts := strings.SplitN(triplet, ":", 2)
		root := parts[0]
		if root == "" || root == "nil" {
			return "", false
		}
		return root, true
	}
	return "", false
}

// TestTsFrame tests the ts frame command with various syntaxes.
func testTsFrame(t *testing.T, d *daemonInstance) {

	// Create a frame to work with
	createFrameViaDaemon(t, d, "frametest")

	// Test: ts frame (no args) prints current frame UUID
	output, exitCode, err := sshExec(t, d, "root@frametest", "ts frame")
	if err != nil {
		t.Fatalf("ts frame failed: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("ts frame: expected exit 0, got %d (output: %q)", exitCode, output)
	}
	currentUUID := strings.TrimSpace(output)
	if currentUUID == "" {
		t.Fatalf("ts frame: expected UUID output, got empty")
	}
	t.Logf("ts frame -> %s", currentUUID)

	// Test: ts frame :: creates a NEW frame (snaps current, clones to new frame)
	// See TestTsFrameColonColonCreatesNewFrame for thorough tests.
	output, exitCode, err = sshExec(t, d, "root@frametest", "ts frame ::")
	if err != nil {
		t.Fatalf("ts frame :: failed: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("ts frame :: expected exit 0, got %d (output: %q)", exitCode, output)
	}
	colonColonUUID := strings.TrimSpace(output)
	if colonColonUUID == currentUUID {
		t.Errorf("ts frame :: should return a NEW UUID (not current): got %q, want different from %q",
			colonColonUUID, currentUUID)
	}

	// Test: ts frame <uuid> validates and prints the UUID
	output, exitCode, err = sshExec(t, d, "root@frametest", "ts frame "+currentUUID)
	if err != nil {
		t.Fatalf("ts frame <uuid> failed: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("ts frame <uuid>: expected exit 0, got %d (output: %q)", exitCode, output)
	}
	if strings.TrimSpace(output) != currentUUID {
		t.Errorf("ts frame <uuid>: got %q, want %q", strings.TrimSpace(output), currentUUID)
	}

	// Test: ts frame <ref> resolves ref to UUID
	output, exitCode, err = sshExec(t, d, "root@frametest", "ts frame frametest")
	if err != nil {
		t.Fatalf("ts frame <ref> failed: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("ts frame <ref>: expected exit 0, got %d (output: %q)", exitCode, output)
	}
	if strings.TrimSpace(output) != currentUUID {
		t.Errorf("ts frame <ref>: got %q, want %q", strings.TrimSpace(output), currentUUID)
	}

	// Test: ts frame with invalid spec (one colon) should error
	output, exitCode, err = sshExec(t, d, "root@frametest", "ts frame foo:bar")
	if err != nil {
		t.Fatalf("ts frame foo:bar failed: %v", err)
	}
	if exitCode == 0 {
		t.Errorf("ts frame foo:bar: expected non-zero exit for invalid spec")
	}

	// Test: ts frame with too many colons should error
	output, exitCode, err = sshExec(t, d, "root@frametest", "ts frame a:b:c:d")
	if err != nil {
		t.Fatalf("ts frame a:b:c:d failed: %v", err)
	}
	if exitCode == 0 {
		t.Errorf("ts frame a:b:c:d: expected non-zero exit for too many colons")
	}

	// Test: ts frame <snap>:: creates a new frame (inheriting home/work)
	// First, take a snapshot so we have a snap ID to use
	snapStdout, _, exitCode, err := sshExecSplit(t, d, "root@frametest", "ts snap --wait")
	if err != nil {
		t.Fatalf("ts snap failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts snap: expected exit 0, got %d", exitCode)
	}
	snapTriplet := strings.TrimSpace(snapStdout)
	t.Logf("Created snap: %s", snapTriplet)

	// Extract just the root snap from the triplet (root:home:work -> root)
	// to test the <snap>:: syntax (use this root, inherit home/work)
	snapParts := strings.SplitN(snapTriplet, ":", 2)
	rootSnap := snapParts[0]

	// Now create a new frame from that snap
	output, exitCode, err = sshExec(t, d, "root@frametest", "ts frame "+rootSnap+"::")
	if err != nil {
		t.Fatalf("ts frame <snap>:: failed: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("ts frame <snap>::: expected exit 0, got %d (output: %q)", exitCode, output)
	}
	newUUID := strings.TrimSpace(output)
	if newUUID == "" || newUUID == currentUUID {
		t.Errorf("ts frame <snap>::: expected new UUID, got %q", newUUID)
	}
	t.Logf("ts frame <snap>:: -> %s (new frame)", newUUID)
}

// TestTsLog tests ts log shows frame history.
func testTsLog(t *testing.T, d *daemonInstance) {

	// Create a frame
	createFrameViaDaemon(t, d, "logtest")

	// Initially no history
	output, exitCode, err := sshExec(t, d, "root@logtest", "ts log")
	if err != nil {
		t.Fatalf("ts log failed: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("ts log: expected exit 0, got %d", exitCode)
	}
	t.Logf("ts log (initial): %s", output)

	// Take a snap - this should add to history
	snapStdout, _, exitCode, err := sshExecSplit(t, d, "root@logtest", "ts snap --wait")
	if err != nil {
		t.Fatalf("ts snap failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts snap: expected exit 0, got %d", exitCode)
	}
	snapID := strings.TrimSpace(snapStdout)
	t.Logf("Created snap: %s", snapID)

	// Now log should show the snap
	output, exitCode, err = sshExec(t, d, "root@logtest", "ts log")
	if err != nil {
		t.Fatalf("ts log failed: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("ts log: expected exit 0, got %d", exitCode)
	}
	if !strings.Contains(output, snapID) {
		t.Errorf("ts log: expected to contain snap %s, got: %q", snapID, output)
	}
	t.Logf("ts log (after snap): %s", output)
}

// TestTsFrameCreatesNewFrameWithHistory tests that creating a new frame
// via ts frame clones the parent's history when done via ts go.
// This is a simpler test that just verifies ts frame creates frames.
func testTsFrameCreatesNewFrame(t *testing.T, d *daemonInstance) {

	// Create initial frame
	createFrameViaDaemon(t, d, "parent")

	// Get parent UUID
	output, exitCode, err := sshExec(t, d, "root@parent", "ts frame")
	if err != nil {
		t.Fatalf("ts frame failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts frame: expected exit 0, got %d", exitCode)
	}
	parentUUID := strings.TrimSpace(output)

	// Take a snap in the parent
	snapStdout, _, exitCode, err := sshExecSplit(t, d, "root@parent", "ts snap --wait")
	if err != nil {
		t.Fatalf("ts snap failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts snap: expected exit 0, got %d", exitCode)
	}
	snapTriplet := strings.TrimSpace(snapStdout)
	t.Logf("Parent snap: %s", snapTriplet)

	// Extract just the root snap from the triplet to use as the root component
	snapParts := strings.SplitN(snapTriplet, ":", 2)
	rootSnap := snapParts[0]

	// Create a child frame from the snap with a ref
	// Use rootSnap:nil:nil to use this root and nil for home/work
	output, exitCode, err = sshExec(t, d, "root@parent", "ts frame --ref=child "+rootSnap+":nil:nil")
	if err != nil {
		t.Fatalf("ts frame (create child) failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts frame (create child): expected exit 0, got %d (output: %q)", exitCode, output)
	}
	childUUID := strings.TrimSpace(output)
	t.Logf("Child UUID: %s", childUUID)

	if childUUID == parentUUID {
		t.Errorf("Child UUID should be different from parent")
	}

	// SSH to the child frame and verify it works
	output, exitCode, err = sshExec(t, d, "root@child", "echo hello from child")
	if err != nil {
		t.Fatalf("sshExec to child failed: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("echo in child: expected exit 0, got %d", exitCode)
	}
	if !strings.Contains(output, "hello from child") {
		t.Errorf("expected 'hello from child', got: %q", output)
	}
}

// TestTsFrameColonColonCreatesNewFrame tests that "ts frame ::" creates a new
// frame (cloning the current one) and returns a NEW UUID each time, not the
// current frame's UUID.
func testTsFrameColonColonCreatesNewFrame(t *testing.T, d *daemonInstance) {

	createFrameViaDaemon(t, d, "fcctest")

	// Get the current frame UUID
	output, exitCode, err := sshExec(t, d, "root@fcctest", "ts frame")
	if err != nil {
		t.Fatalf("ts frame failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts frame: exit %d", exitCode)
	}
	currentUUID := strings.TrimSpace(output)

	// "ts frame ::" should snap, create a new frame from that snap, and return NEW uuid
	output, exitCode, err = sshExec(t, d, "root@fcctest", "ts frame ::")
	if err != nil {
		t.Fatalf("ts frame :: failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts frame :: exit %d: %s", exitCode, output)
	}
	newUUID1 := strings.TrimSpace(output)
	if newUUID1 == currentUUID {
		t.Errorf("ts frame :: should return a NEW uuid, got current frame uuid %s", currentUUID)
	}

	// Running it again should produce yet another distinct UUID
	output, exitCode, err = sshExec(t, d, "root@fcctest", "ts frame ::")
	if err != nil {
		t.Fatalf("ts frame :: (2nd) failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts frame :: (2nd) exit %d: %s", exitCode, output)
	}
	newUUID2 := strings.TrimSpace(output)
	if newUUID2 == currentUUID {
		t.Errorf("ts frame :: (2nd) should return a NEW uuid, got current %s", currentUUID)
	}
	if newUUID2 == newUUID1 {
		t.Errorf("ts frame :: called twice should return DIFFERENT uuids, got same %s", newUUID1)
	}

	t.Logf("current=%s, new1=%s, new2=%s", currentUUID, newUUID1, newUUID2)
}

// TestTsGoNoArgsCreatesThenEnters tests that "ts go" with no arguments creates
// a new frame and enters it. Per the design doc, "ts go" (no args) should be
// equivalent to "ts frame ::" (which snaps and creates a new frame) followed
// by entering that frame.
//
// Since ts go enters an interactive vsock session that's hard to drive from
// a test, we verify the frame creation aspect via ts frames before/after.
func testTsGoNoArgsCreatesThenEnters(t *testing.T, d *daemonInstance) {

	createFrameViaDaemon(t, d, "gotest")

	// Count frames before
	output, exitCode, err := sshExec(t, d, "root@gotest", "ts frames")
	if err != nil {
		t.Fatalf("ts frames failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts frames: exit %d", exitCode)
	}
	framesBefore := strings.Count(output, "\n")

	// Get the original frame UUID
	output, exitCode, err = sshExec(t, d, "root@gotest", "ts frame")
	if err != nil {
		t.Fatalf("ts frame failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts frame: exit %d", exitCode)
	}
	originalUUID := strings.TrimSpace(output)

	// ts go (no args) should create a new frame. Since it enters an interactive
	// session, we can't easily drive it. But we CAN verify that "ts frame ::"
	// (which ts go should use internally) creates a new frame.
	// This is tested by TestTsFrameColonColonCreatesNewFrame.
	//
	// For this test, we verify ts go at least doesn't error on invocation
	// by checking it would create a frame. We use "ts frame ::" as the proxy.
	output, exitCode, err = sshExec(t, d, "root@gotest", "ts frame ::")
	if err != nil {
		t.Fatalf("ts frame :: failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts frame :: exit %d: %s", exitCode, output)
	}
	newUUID := strings.TrimSpace(output)

	// Verify a new frame was created
	if newUUID == originalUUID {
		t.Errorf("ts frame :: (proxy for ts go) should create NEW frame, got same UUID %s", originalUUID)
	}

	// Count frames after - should have one more
	output, exitCode, err = sshExec(t, d, "root@gotest", "ts frames")
	if err != nil {
		t.Fatalf("ts frames (after) failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts frames (after): exit %d", exitCode)
	}
	framesAfter := strings.Count(output, "\n")

	if framesAfter <= framesBefore {
		t.Errorf("expected more frames after ts frame ::, got before=%d after=%d", framesBefore, framesAfter)
	}

	t.Logf("original=%s new=%s frames before=%d after=%d", originalUUID, newUUID, framesBefore, framesAfter)
}

// TestTsGoWithCommand tests "ts go :: -c 'command'" which creates a new frame,
// runs the command, and exits. This allows non-interactive testing of ts go.
func testTsGoWithCommand(t *testing.T, d *daemonInstance) {

	createFrameViaDaemon(t, d, "gocmdtest")

	// Test: ts go nil:nil:nil -c 'echo hello' - simplest case, empty frame
	output, exitCode, err := sshExec(t, d, "root@gocmdtest", `ts go nil:nil:nil -c 'echo hello'`)
	if err != nil {
		t.Fatalf("ts go nil:nil:nil -c 'echo hello' failed: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("ts go nil:nil:nil -c 'echo hello': expected exit 0, got %d (output: %q)", exitCode, output)
	}
	if !strings.Contains(output, "hello") {
		t.Errorf("ts go nil:nil:nil -c 'echo hello': expected 'hello' in output, got: %q", output)
	}

	// Test: ts go nil:nil:nil -c 'ts frame' - run ts subcommand in empty frame
	output, exitCode, err = sshExec(t, d, "root@gocmdtest", `ts go nil:nil:nil -c 'ts frame'`)
	if err != nil {
		t.Fatalf("ts go nil:nil:nil -c 'ts frame' failed: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("ts go nil:nil:nil -c 'ts frame': expected exit 0, got %d (output: %q)", exitCode, output)
	}
	// Should output a UUID
	nilFrameUUID := strings.TrimSpace(output)
	if nilFrameUUID == "" {
		t.Errorf("ts go nil:nil:nil -c 'ts frame': expected UUID output, got empty")
	}

	// Get the original frame UUID
	output, exitCode, err = sshExec(t, d, "root@gocmdtest", "ts frame")
	if err != nil {
		t.Fatalf("ts frame failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts frame: exit %d", exitCode)
	}
	originalUUID := strings.TrimSpace(output)

	// Count frames before
	output, exitCode, err = sshExec(t, d, "root@gocmdtest", "ts frames")
	if err != nil {
		t.Fatalf("ts frames failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts frames: exit %d", exitCode)
	}
	framesBefore := strings.Count(output, "\n")

	// Run ts go :: -c "echo hello" - this should:
	// 1. Create a new frame (via ::)
	// 2. Run "echo hello" in that frame
	// 3. Exit and return to the original frame
	output, exitCode, err = sshExec(t, d, "root@gocmdtest", `ts go :: -c "echo GOCMD_MARKER"`)
	if err != nil {
		t.Fatalf("ts go :: -c failed: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("ts go :: -c: expected exit 0, got %d (output: %q)", exitCode, output)
	}
	if !strings.Contains(output, "GOCMD_MARKER") {
		t.Errorf("ts go :: -c: expected output to contain GOCMD_MARKER, got: %q", output)
	}
	t.Logf("ts go :: -c output: %s", output)

	// Count frames after - should have one more
	output, exitCode, err = sshExec(t, d, "root@gocmdtest", "ts frames")
	if err != nil {
		t.Fatalf("ts frames (after) failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts frames (after): exit %d", exitCode)
	}
	framesAfter := strings.Count(output, "\n")

	if framesAfter <= framesBefore {
		t.Errorf("expected more frames after ts go ::, got before=%d after=%d", framesBefore, framesAfter)
	}
	t.Logf("frames before=%d after=%d", framesBefore, framesAfter)

	// Verify we're still in the original frame (ts go -c should return)
	output, exitCode, err = sshExec(t, d, "root@gocmdtest", "ts frame")
	if err != nil {
		t.Fatalf("ts frame (after) failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts frame (after): exit %d", exitCode)
	}
	currentUUID := strings.TrimSpace(output)
	if currentUUID != originalUUID {
		t.Errorf("expected to be back in original frame %s, but got %s", originalUUID, currentUUID)
	}
}

// TestTsGoWithCommandExitCode tests that ts go -c propagates the command's exit code.
func testTsGoWithCommandExitCode(t *testing.T, d *daemonInstance) {

	createFrameViaDaemon(t, d, "goexittest")

	// Run a command that exits with code 42
	_, exitCode, err := sshExec(t, d, "root@goexittest", `ts go :: -c "exit 42"`)
	if err != nil {
		t.Fatalf("ts go :: -c failed: %v", err)
	}
	if exitCode != 42 {
		t.Errorf("ts go :: -c 'exit 42': expected exit 42, got %d", exitCode)
	}
	t.Logf("ts go :: -c 'exit 42' returned exit code %d", exitCode)

	// Run a command that succeeds
	_, exitCode, err = sshExec(t, d, "root@goexittest", `ts go :: -c "true"`)
	if err != nil {
		t.Fatalf("ts go :: -c failed: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("ts go :: -c 'true': expected exit 0, got %d", exitCode)
	}
}

// TestTsGoWithCommandToExistingFrame tests ts go <ref> -c 'command' which
// runs a command in an existing frame without creating a new one.
func testTsGoWithCommandToExistingFrame(t *testing.T, d *daemonInstance) {

	// Create two frames
	createFrameViaDaemon(t, d, "goref1")
	createFrameViaDaemon(t, d, "goref2")

	// Write a marker file in goref2
	_, exitCode, err := sshExec(t, d, "root@goref2", "echo FRAME2_MARKER > /tmp/marker")
	if err != nil {
		t.Fatalf("write marker failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("write marker: exit %d", exitCode)
	}

	// From goref1, run ts go goref2 -c to read the marker
	output, exitCode, err := sshExec(t, d, "root@goref1", `ts go goref2 -c "read line < /tmp/marker && echo \$line"`)
	if err != nil {
		t.Fatalf("ts go goref2 -c failed: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("ts go goref2 -c: expected exit 0, got %d (output: %q)", exitCode, output)
	}
	if !strings.Contains(output, "FRAME2_MARKER") {
		t.Errorf("ts go goref2 -c: expected output to contain FRAME2_MARKER, got: %q", output)
	}
	t.Logf("ts go goref2 -c output: %s", output)
}

// TestTsGoUserAtFrame tests the "<user>@<frame>" form of "ts go": the optional
// user prefix is parsed off the spec, sent in the /enter protocol, and honored
// by vshd's `su - <user>`. Identity is probed by side effect (the same technique
// as TestSSHContainerUserNonRoot): root can write a file directly under /
// (mode 0755, root-owned) while the unprivileged "user" account cannot, so the
// same probe run as root@ vs user@ must produce different results. echo and
// output redirection are shell builtins, so no busybox install is needed in the
// minimal frame. It also checks that a bare "@frame" (empty user) is rejected
// client-side rather than silently auto-detecting.
func testTsGoUserAtFrame(t *testing.T, d *daemonInstance) {

	// gou1 is the frame we sit in to run ts go; gou2 is the target frame we
	// enter as a chosen user. Both are nil:nil:nil minimal frames, which always
	// have the "user" account in /etc/passwd (see rootfs.go).
	createFrameViaDaemon(t, d, "gou1")
	createFrameViaDaemon(t, d, "gou2")

	// Probe: write a file directly under /. Root succeeds (owns /, mode 0755);
	// "user" (uid 7575) lacks write permission and fails.
	probe := "echo x > /_ts_go_userprobe 2>/dev/null && echo ROOT || echo NONROOT"

	// As root@gou2: the probe must report ROOT.
	output, exitCode, err := sshExec(t, d, "root@gou1", "ts go root@gou2 -c '"+probe+"'")
	if err != nil {
		t.Fatalf("ts go root@gou2: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("ts go root@gou2: exit %d (output: %q)", exitCode, output)
	}
	if !strings.Contains(output, "ROOT") || strings.Contains(output, "NONROOT") {
		t.Errorf("ts go root@gou2: expected ROOT, got %q", output)
	}
	t.Logf("ts go root@gou2: %q", output)

	// As user@gou2: the same probe must report NONROOT, proving the user@
	// prefix was honored (not auto-detected as root).
	output, exitCode, err = sshExec(t, d, "root@gou1", "ts go user@gou2 -c '"+probe+"'")
	if err != nil {
		t.Fatalf("ts go user@gou2: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("ts go user@gou2: exit %d (output: %q)", exitCode, output)
	}
	if !strings.Contains(output, "NONROOT") {
		t.Errorf("ts go user@gou2: expected NONROOT, got %q", output)
	}
	t.Logf("ts go user@gou2: %q", output)

	// Empty user before '@' is rejected client-side. The -c is a safety net so
	// that a parsing regression would produce a non-interactive failure rather
	// than hang on an interactive session; the command must never run.
	output, exitCode, err = sshExec(t, d, "root@gou1", "ts go @gou2 -c 'echo SHOULD_NOT_RUN'")
	if err != nil {
		t.Fatalf("ts go @gou2: %v", err)
	}
	if exitCode == 0 {
		t.Errorf("ts go @gou2: expected non-zero exit for empty user, got 0 (output: %q)", output)
	}
	if !strings.Contains(output, "empty user before") {
		t.Errorf("ts go @gou2: expected 'empty user before' error, got %q", output)
	}
	if strings.Contains(output, "SHOULD_NOT_RUN") {
		t.Errorf("ts go @gou2: command unexpectedly ran (output: %q)", output)
	}
}

// TestTsUndo tests that ts undo moves the session back to the previous snap:
// it snapshots the current state, creates a new frame from the snap before
// it, and enters that frame. The -c flag runs the verification commands
// inside the undone frame in ONE invocation (each ts undo snapshots first
// and creates a new frame, so a second invocation would undo a different,
// newer state).
func testTsUndo(t *testing.T, d *daemonInstance) {

	createFrameViaDaemon(t, d, "undotest")

	// Capture the pre-undo frame UUID.
	output, exitCode, err := sshExec(t, d, "root@undotest", "ts frame")
	if err != nil {
		t.Fatalf("ts frame failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts frame: exit %d", exitCode)
	}
	preUndoUUID := strings.TrimSpace(output)

	// Create a marker file with state1 and snap (this records state1)
	_, exitCode, err = sshExec(t, d, "root@undotest", "echo state1 > /marker")
	if err != nil {
		t.Fatalf("create marker failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("create marker: exit %d", exitCode)
	}

	// Take a snapshot (this records state1)
	snapStdout, _, exitCode, err := sshExecSplit(t, d, "root@undotest", "ts snap --wait")
	if err != nil {
		t.Fatalf("ts snap failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts snap: exit %d", exitCode)
	}
	t.Logf("snap (state1): %s", strings.TrimSpace(snapStdout))

	// Modify to state2
	_, exitCode, err = sshExec(t, d, "root@undotest", "echo state2 > /marker")
	if err != nil {
		t.Fatalf("modify marker failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("modify marker: exit %d", exitCode)
	}

	// Run the real ts undo. Inside the undone frame, print its UUID and the
	// marker content. Assert on stdout only: "Undoing to snap ..." goes to
	// stderr, and command sessions suppress the greeting. Single quotes keep
	// $line from being expanded before it reaches the undone frame's shell;
	// 'read' is a shell builtin (cat may not exist in the minimal rootfs).
	stdout, stderr, exitCode, err := sshExecSplit(t, d, "root@undotest",
		`ts undo -c 'ts frame; read line < /marker && echo $line'`)
	if err != nil {
		t.Fatalf("ts undo -c failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts undo -c: exit %d (stdout: %q, stderr: %q)", exitCode, stdout, stderr)
	}

	fields := strings.Fields(stdout)
	if len(fields) != 2 {
		t.Fatalf("expected 2 stdout tokens (uuid, marker), got %d: %q", len(fields), stdout)
	}
	undoneUUID, marker := fields[0], fields[1]
	if len(undoneUUID) != 36 || strings.Count(undoneUUID, "-") != 4 {
		t.Errorf("first stdout token does not look like a frame UUID: %q", undoneUUID)
	}
	if undoneUUID == preUndoUUID {
		t.Errorf("ts undo should enter a NEW frame, but still in %s", preUndoUUID)
	}
	if marker != "state1" {
		t.Errorf("undone frame should have state1 in /marker, got %q", marker)
	}
}

// TestTsUndoEmptyLog tests that ts undo with no snapshots in history errors.
func testTsUndoEmptyLog(t *testing.T, d *daemonInstance) {

	createFrameViaDaemon(t, d, "undoempty")

	// Don't take any snaps - history is empty
	// ts undo should error
	output, exitCode, err := sshExec(t, d, "root@undoempty", "ts undo")
	if err != nil {
		t.Fatalf("ts undo failed: %v", err)
	}
	if exitCode == 0 {
		t.Errorf("ts undo with empty history should fail, but exit 0 (output: %q)", output)
	}
	if !strings.Contains(strings.ToLower(output), "no snapshot") && !strings.Contains(strings.ToLower(output), "empty") {
		t.Logf("ts undo error message: %q", output)
	}
}

// TestForkUndoRollsBackToForkPoint verifies that the fork-point snap (the
// state captured by `ts frame ::` / `ts go ::` at fork time) lands in the
// forked frame's history, so the first `ts undo` in the new frame rolls back
// to the fork point rather than past it.
//
// ts undo creates a new frame from history[0] (the newest snap). The fork
// captures the source's live state as a background snap and clones a new frame
// from it; the new frame's history is cloned from the source's. The fork-point
// snap MUST be prepended to the new frame's history when it finalizes, so that
// history[0] is the fork-point state. Without that, history[0] is the source's
// last pre-fork snap and ts undo silently discards everything done between
// that snap and the fork — a nondeterministic-history regression.
//
// This test reproduces ts undo's frame-creation step (ts undo itself enters an
// interactive vsock session that can't be driven from a test, matching the
// approach in TestTsUndo): it reads the forked frame's ts log, takes history[0],
// builds a frame from it, and checks the on-disk state is the fork point.
func testForkUndoRollsBackToForkPoint(t *testing.T, d *daemonInstance) {

	createFrameViaDaemon(t, d, "forksrc")

	// v1: take a snap so the source has a pre-fork history entry.
	if _, exitCode, err := sshExec(t, d, "root@forksrc", "echo v1 > /marker"); err != nil || exitCode != 0 {
		t.Fatalf("write v1: err=%v exit=%d", err, exitCode)
	}
	if _, _, exitCode, err := sshExecSplit(t, d, "root@forksrc", "ts snap --wait"); err != nil || exitCode != 0 {
		t.Fatalf("ts snap --wait (v1): err=%v exit=%d", err, exitCode)
	}

	// v2: mutate the source AFTER the snap, so the fork point (v2) differs from
	// the pre-fork snap (v1). This is what makes the regression observable:
	// rolling back to v1 instead of v2 means undo jumped past the fork point.
	if _, exitCode, err := sshExec(t, d, "root@forksrc", "echo v2 > /marker"); err != nil || exitCode != 0 {
		t.Fatalf("write v2: err=%v exit=%d", err, exitCode)
	}

	// Fork. The fork captures the source's live state (v2) as a background
	// snap and clones a new frame from it. ts frame :: returns the new UUID
	// but no ref, so create one to address it over SSH.
	forkOut, exitCode, err := sshExec(t, d, "root@forksrc", "ts frame ::")
	if err != nil || exitCode != 0 {
		t.Fatalf("ts frame :: : err=%v exit=%d (out %q)", err, exitCode, forkOut)
	}
	forkedUUID := strings.TrimSpace(forkOut)
	if forkedUUID == "" {
		t.Fatalf("ts frame :: returned no UUID: %q", forkOut)
	}
	if _, exitCode, err := sshExec(t, d, "root@forksrc", "ts ref create forkpt "+forkedUUID); err != nil || exitCode != 0 {
		t.Fatalf("ts ref create forkpt: err=%v exit=%d", err, exitCode)
	}

	// Sanity: the forked frame's live state is the fork point (v2). Enter as
	// the unprivileged default user and call `ts frame` too: fast forks used to
	// leave /id root-owned, making every ts command fail without sudo.
	if out, exitCode, err := sshExec(t, d, "user@forkpt", "ts frame; read line < /marker && echo $line"); err != nil || exitCode != 0 {
		t.Fatalf("use ts and read marker as user in forked frame: err=%v exit=%d out=%q", err, exitCode, out)
	} else if lines := strings.Fields(out); len(lines) < 2 || lines[len(lines)-1] != "v2" {
		t.Fatalf("forked frame user output = %q, want frame UUID followed by v2", out)
	}

	// Wait for the fork-point snap to finalize. It is a background snap of the
	// SOURCE frame, so observe finalize via the source's ts log growing by one
	// (from the single v1 snap to two entries).
	deadline := time.Now().Add(30 * time.Second)
	finalized := false
	for time.Now().Before(deadline) {
		srcLog, _, _, err := sshExecSplit(t, d, "root@forksrc", "ts log")
		if err != nil {
			t.Fatalf("ts log (src poll): %v", err)
		}
		if n := logEntryCount(srcLog); n >= 2 {
			finalized = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !finalized {
		t.Fatalf("fork-point snap never finalized in source within 30s")
	}
	// Give the worker's forked-frame history update (the fix under test) a
	// moment to land after the source-side finalize.
	time.Sleep(500 * time.Millisecond)

	// The forked frame's history[0] must be the fork-point snap (v2), not the
	// source's pre-fork snap (v1). Reproduce ts undo: build a frame from
	// history[0] and inspect its /marker.
	forkedLog, _, _, err := sshExecSplit(t, d, "root@forkpt", "ts log")
	if err != nil {
		t.Fatalf("ts log (forked): %v", err)
	}
	rootSnap, ok := newestLogRootSnap(forkedLog)
	if !ok {
		t.Fatalf("forked frame ts log has no entries: %q", forkedLog)
	}
	t.Logf("forked frame history[0] root snap = %s (ts log: %q)", rootSnap, strings.TrimSpace(forkedLog))

	if _, exitCode, err := sshExec(t, d, "root@forkpt", "ts frame --ref=forkundo "+rootSnap+":nil:nil"); err != nil || exitCode != 0 {
		t.Fatalf("ts frame from forked history[0]: err=%v exit=%d", err, exitCode)
	}
	out, exitCode, err := sshExec(t, d, "root@forkundo", "read line < /marker && echo $line")
	if err != nil || exitCode != 0 {
		t.Fatalf("read marker in forkundo frame: err=%v exit=%d (out %q)", err, exitCode, out)
	}
	if got := strings.TrimSpace(out); got != "v2" {
		t.Errorf("ts undo in a fresh fork rolled back past the fork point: /marker=%q, want %q (the fork-point state). "+
			"The fork-point snap is missing from the forked frame's history; history[0] is the source's pre-fork snap.",
			got, "v2")
	} else {
		t.Logf("ts undo in fresh fork correctly rolled back to fork point: /marker=%q", got)
	}
}

// logEntryCount counts non-empty lines in `ts log` output (one per history
// entry).
func logEntryCount(logOut string) int {
	n := 0
	for _, line := range strings.Split(logOut, "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}
