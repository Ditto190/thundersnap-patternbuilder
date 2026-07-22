package tsm

import (
	"crypto/sha256"
	"encoding/binary"
	"os"
	"strings"
	"testing"
)

// Corrupt a .tsc: inflate the header chunk count and recompute the footer so
// it passes the (old) footer-only validation. With the fix, this file must be
// rejected by count/length validation and never reach lookup.
func TestCorruptTSCInflatedCountRejected(t *testing.T) {
	dir := t.TempDir()
	base := makeTestSnap(t, dir, "snap1", map[string]string{
		"f.txt": "content for corruption test\n",
	})
	data, err := os.ReadFile(base + ".tsc")
	if err != nil {
		t.Fatal(err)
	}
	// Inflate the count field (bytes 8:16) to something larger than the data.
	binary.BigEndian.PutUint64(data[8:16], 1<<20)
	// Recompute footer over content (header+entries) so footer-only validation
	// would pass.
	content := data[:len(data)-32]
	h := sha256.New()
	h.Write(content)
	copy(data[len(data)-32:], h.Sum(nil))
	if err := os.WriteFile(base+".tsc", data, 0644); err != nil {
		t.Fatal(err)
	}

	// readTSCContentValidated must reject the inconsistent count.
	if _, err := readTSCContentValidated(base + ".tsc"); err == nil {
		t.Error("readTSCContentValidated accepted a .tsc with inflated count (expected rejection)")
	}

	// OpenChunkIndex must skip the bad snap (not panic, not OOM).
	ci, err := OpenChunkIndex(dir)
	if err != nil {
		t.Fatalf("OpenChunkIndex: %v", err)
	}
	if ci.SnapCount() != 0 {
		t.Errorf("SnapCount=%d, want 0 (bad .tsc should be skipped)", ci.SnapCount())
	}
	// FindChunk on an absent key must not panic even though the bad file exists.
	// Using a defer/recover to turn a panic into a failure is intentionally NOT
	// done here: a panic would fail the test run directly.
	var absent [32]byte
	absent[0] = 0x42
	if _, found := ci.FindChunk(absent); found {
		t.Error("FindChunk reported a hit on an index with no snaps")
	}
}

// A .tsc truncated (count too small for the data) must also be rejected.
func TestCorruptTSCTruncatedCountRejected(t *testing.T) {
	dir := t.TempDir()
	base := makeTestSnap(t, dir, "snap1", map[string]string{
		"f.txt": strings.Repeat("content for truncation test needs many chunks\n", 4096),
	})
	// Truncate the file mid-entries so count no longer fits, then fix the
	// footer to match the truncated content.
	data, _ := os.ReadFile(base + ".tsc")
	// Need at least a few entries so a half-entry cut is possible.
	if len(data) < TSCHeaderSize+3*TSCEntrySize+TSCFooterSize {
		t.Fatalf("snap .tsc too small (%d bytes) to truncate; make content bigger", len(data))
	}
	cut := TSCHeaderSize + 2*TSCEntrySize + TSCEntrySize/2 // 2.5 entries
	content := data[:cut]
	// Declare 2 entries; the half-entry leftover makes expected != entriesLen.
	binary.BigEndian.PutUint64(content[8:16], 2)
	h := sha256.New()
	h.Write(content)
	out := append(content, h.Sum(nil)...)
	if err := os.WriteFile(base+".tsc", out, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readTSCContentValidated(base + ".tsc"); err == nil {
		t.Error("readTSCContentValidated accepted a truncated .tsc (expected rejection)")
	}
	ci, err := OpenChunkIndex(dir)
	if err != nil {
		t.Fatalf("OpenChunkIndex: %v", err)
	}
	if ci.SnapCount() != 0 {
		t.Errorf("SnapCount=%d, want 0", ci.SnapCount())
	}
}
