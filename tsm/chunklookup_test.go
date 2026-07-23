// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package tsm

import (
	"crypto/sha256"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBloomFilter covers the filter directly: it never produces false negatives
// and its false-positive rate stays near the configured target.
func TestBloomFilter(t *testing.T) {
	// An empty filter (n==0) reports "not present" for everything.
	empty := newBloomFilter(0, 0.01)
	var anyKey [32]byte
	if empty.test(anyKey) {
		t.Error("empty bloom reported a hit")
	}

	// Populate with 1000 keys derived from sha256(i).
	const n = 1000
	bf := newBloomFilter(n, 0.01)
	keys := make([][32]byte, n)
	for i := uint64(0); i < n; i++ {
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], i)
		keys[i] = sha256.Sum256(buf[:])
		bf.add(keys[i])
	}

	// No false negatives: every added key must test true.
	for i, k := range keys {
		if !bf.test(k) {
			t.Errorf("false negative for added key %d", i)
		}
	}

	// False positives among absent keys must stay near the 1% target; allow
	// up to 3% (many standard deviations above the expectation) to avoid
	// flakiness while still validating the rate is bounded.
	const trials = 5000
	var fp int
	for i := uint64(0); i < trials; i++ {
		var buf [8]byte
		// Offset the input space so these keys were never added.
		binary.BigEndian.PutUint64(buf[:], i+1_000_000)
		if bf.test(sha256.Sum256(buf[:])) {
			fp++
		}
	}
	if rate := float64(fp) / trials; rate > 0.03 {
		t.Errorf("false-positive rate %g (%d/%d) exceeds 3%%", rate, fp, trials)
	}
}

// makeTestSnap creates a snapshot directory under dir with the given files
// (map of relpath->content) and indexes it, producing <dir>/<name>/.tsm/.tsc.
// It returns the snap's base path (without extension).
func makeTestSnap(t *testing.T, dir, name string, files map[string]string) string {
	t.Helper()
	snapDir := filepath.Join(dir, name)
	if err := os.MkdirAll(snapDir, 0755); err != nil {
		t.Fatal(err)
	}
	for rel, content := range files {
		full := filepath.Join(snapDir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	base := filepath.Join(dir, name)
	if err := Create(snapDir, base, IndexerOptions{}); err != nil {
		t.Fatal(err)
	}
	return base
}

// firstChunkSHA returns the SHA of the first chunk in a snapshot's .tsc.
func firstChunkSHA(t *testing.T, tscPath string) [32]byte {
	t.Helper()
	r, err := ReadTSC(tscPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Entries) == 0 {
		t.Fatalf("%s has no chunks", tscPath)
	}
	return r.Entries[0].SHA256
}

// TestOpenChunkIndex builds an index over a real snapshot and verifies a chunk
// can be found and read back with a matching hash. This replaces the former
// TestLoadLocalChunkMap.
func TestOpenChunkIndex(t *testing.T) {
	tmpDir := t.TempDir()
	base := makeTestSnap(t, tmpDir, "snap1", map[string]string{
		"testfile.txt": "Hello, World! This is test content for chunking.\n",
	})

	ci, err := OpenChunkIndex(tmpDir)
	if err != nil {
		t.Fatalf("OpenChunkIndex: %v", err)
	}
	if ci.SnapCount() != 1 {
		t.Errorf("SnapCount = %d, want 1", ci.SnapCount())
	}

	sha := firstChunkSHA(t, base+".tsc")
	loc, found := ci.FindChunk(sha)
	if !found {
		t.Fatal("FindChunk did not find a known chunk")
	}
	data, err := loc.VerifyAndRead()
	if err != nil {
		t.Fatalf("VerifyAndRead: %v", err)
	}
	if got := BlobSHA256(data); got != sha {
		t.Errorf("VerifyAndRead returned data with sha %x, want %x", got, sha)
	}
}

func TestOpenChunkIndexEmptyDir(t *testing.T) {
	tmpDir := t.TempDir()
	ci, err := OpenChunkIndex(tmpDir)
	if err != nil {
		t.Fatalf("OpenChunkIndex on empty dir: %v", err)
	}
	if ci.SnapCount() != 0 {
		t.Errorf("SnapCount = %d, want 0", ci.SnapCount())
	}
	if _, found := ci.FindChunk([32]byte{0x01}); found {
		t.Error("FindChunk on empty index returned a hit")
	}
}

func TestOpenChunkIndexNonExistentDir(t *testing.T) {
	ci, err := OpenChunkIndex("/nonexistent/path")
	if err != nil {
		t.Fatalf("OpenChunkIndex on nonexistent dir: %v", err)
	}
	if ci.SnapCount() != 0 {
		t.Errorf("SnapCount = %d, want 0", ci.SnapCount())
	}
	if _, found := ci.FindChunk([32]byte{0x01}); found {
		t.Error("FindChunk on missing-dir index returned a hit")
	}
}

// TestChunkIndexMRUOrdering verifies that a successful lookup moves the serving
// snapshot to the front, so a chunk present in multiple snapshots is served
// from the most-recently-used one.
func TestChunkIndexMRUOrdering(t *testing.T) {
	tmpDir := t.TempDir()
	// shared content X appears in both snaps; onlyB content is exclusive to
	// snapB. Identical bytes produce identical chunk hashes.
	const shared = "shared content that will be chunked identically everywhere it appears\n"
	const onlyB = "this content exists only in snapB and gives snapB-exclusive chunks\n"

	snapABase := makeTestSnap(t, tmpDir, "snapA", map[string]string{"shared.txt": shared})
	snapBBase := makeTestSnap(t, tmpDir, "snapB", map[string]string{
		"shared.txt": shared,
		"onlyB.txt":  onlyB,
	})
	snapBDir := filepath.Join(tmpDir, "snapB")

	ci, err := OpenChunkIndex(tmpDir)
	if err != nil {
		t.Fatalf("OpenChunkIndex: %v", err)
	}

	// Find a chunk that is exclusive to snapB, which moves snapB to the front.
	bOnlySHA := firstChunkSHA(t, snapBBase+".tsc")
	// Make sure the first chunk of snapB is actually in snapA's set too, by
	// accident; if so, pick a chunk we can prove is B-only by cross-checking.
	aTSC, _ := ReadTSC(snapABase + ".tsc")
	inA := make(map[[32]byte]bool, len(aTSC.Entries))
	for _, e := range aTSC.Entries {
		inA[e.SHA256] = true
	}
	if inA[bOnlySHA] {
		// Fall back to scanning snapB's .tsc for a B-exclusive chunk.
		bTSC, _ := ReadTSC(snapBBase + ".tsc")
		found := false
		for _, e := range bTSC.Entries {
			if !inA[e.SHA256] {
				bOnlySHA = e.SHA256
				found = true
				break
			}
		}
		if !found {
			t.Fatal("could not find a snapB-exclusive chunk; test setup is wrong")
		}
	}

	if loc, found := ci.FindChunk(bOnlySHA); !found {
		t.Fatal("FindChunk did not find snapB-exclusive chunk")
	} else if !strings.HasPrefix(loc.Filename, snapBDir) {
		t.Errorf("B-only chunk served from %q, want under %q", loc.Filename, snapBDir)
	}

	// Now look up a chunk present in both snaps. With snapB at the front, it
	// must be served from snapB (the MRU snap), not snapA.
	sharedSHA := firstChunkSHA(t, snapABase+".tsc")
	loc, found := ci.FindChunk(sharedSHA)
	if !found {
		t.Fatal("FindChunk did not find the shared chunk")
	}
	if !strings.HasPrefix(loc.Filename, snapBDir) {
		t.Errorf("shared chunk served from %q, want MRU snap under %q", loc.Filename, snapBDir)
	}
}

// TestChunkIndexLRUEviction verifies that correctness is maintained when the
// LRU caps force .tsc buffers and location maps to be evicted and reloaded.
func TestChunkIndexLRUEviction(t *testing.T) {
	tmpDir := t.TempDir()
	snapABase := makeTestSnap(t, tmpDir, "snapA", map[string]string{
		"a.txt": "content for snap A, distinct from snap B\n",
	})
	snapBBase := makeTestSnap(t, tmpDir, "snapB", map[string]string{
		"b.txt": "completely different content for snap B\n",
	})

	// Caps of 1 force eviction on every cross-snap lookup.
	ci, err := OpenChunkIndexWith(tmpDir, 1, 1)
	if err != nil {
		t.Fatalf("OpenChunkIndexWith: %v", err)
	}

	chunkA := firstChunkSHA(t, snapABase+".tsc")
	chunkB := firstChunkSHA(t, snapBBase+".tsc")

	// Alternate between the two snapshots so each lookup evicts the other's
	// loaded state and must reload it. Each result must still verify.
	for i := 0; i < 3; i++ {
		for _, tc := range []struct {
			name string
			sha  [32]byte
		}{
			{"A", chunkA},
			{"B", chunkB},
			{"A", chunkA},
			{"B", chunkB},
		} {
			loc, found := ci.FindChunk(tc.sha)
			if !found {
				t.Fatalf("pass %d %s: FindChunk miss", i, tc.name)
			}
			if _, err := loc.VerifyAndRead(); err != nil {
				t.Fatalf("pass %d %s: VerifyAndRead: %v", i, tc.name, err)
			}
		}
	}
}

// TestChunkIndexAbsentChunk verifies a chunk present in no snapshot is not
// found (the Bloom filter short-circuits the common case, but the result must
// be false regardless).
func TestChunkIndexAbsentChunk(t *testing.T) {
	tmpDir := t.TempDir()
	makeTestSnap(t, tmpDir, "snap1", map[string]string{
		"testfile.txt": "some content\n",
	})
	ci, err := OpenChunkIndex(tmpDir)
	if err != nil {
		t.Fatalf("OpenChunkIndex: %v", err)
	}
	var absent [32]byte
	absent[0] = 0xff // arbitrary, almost certainly not in the snap
	if _, found := ci.FindChunk(absent); found {
		t.Error("FindChunk reported a hit for an absent chunk")
	}
}

// TestOpenChunkIndexSkipsInProgressTmpSnap verifies that OpenChunkIndex does
// NOT index a background indexer's in-progress snap, which is laid down as
// <jobid>.tmp/ + <jobid>.tmp.tsm + <jobid>.tmp.tsc and renamed to final
// content-addressed names once indexing completes (see snapqueue.go). Indexing
// it would create a stale entry whose paths the worker renames away, which
// (with downloadFile's local-read path) can abort a concurrent download.
func TestOpenChunkIndexSkipsInProgressTmpSnap(t *testing.T) {
	tmpDir := t.TempDir()
	makeTestSnap(t, tmpDir, "snapA", map[string]string{"a.txt": "content for snap A\n"})

	// Build a second, real snap, then rename its on-disk index artifacts to
	// the in-progress <name>.tmp naming the background indexer uses.
	makeTestSnap(t, tmpDir, "snapB", map[string]string{"b.txt": "content for snap B\n"})
	for _, pair := range [][2]string{
		{filepath.Join(tmpDir, "snapB"), filepath.Join(tmpDir, "snapB.tmp")},
		{filepath.Join(tmpDir, "snapB.tsm"), filepath.Join(tmpDir, "snapB.tmp.tsm")},
		{filepath.Join(tmpDir, "snapB.tsc"), filepath.Join(tmpDir, "snapB.tmp.tsc")},
	} {
		if err := os.Rename(pair[0], pair[1]); err != nil {
			t.Fatalf("rename %s -> %s: %v", pair[0], pair[1], err)
		}
	}

	ci, err := OpenChunkIndex(tmpDir)
	if err != nil {
		t.Fatalf("OpenChunkIndex: %v", err)
	}
	if ci.SnapCount() != 1 {
		t.Errorf("SnapCount = %d, want 1 (in-progress .tmp snap must be excluded)", ci.SnapCount())
	}
	// A chunk that only exists in the .tmp snap must not be found.
	bTSC, err := ReadTSC(filepath.Join(tmpDir, "snapB.tmp.tsc"))
	if err != nil || len(bTSC.Entries) == 0 {
		t.Fatalf("snapB.tmp.tsc unreadable/empty: %v", err)
	}
	if _, found := ci.FindChunk(bTSC.Entries[0].SHA256); found {
		t.Error("FindChunk found a chunk belonging only to the in-progress .tmp snap")
	}
}

// TestChunkIndexReloadSkipsFooterRehash verifies that an LRU-driven reload of
// a .tsc does NOT recompute the footer SHA-256: a .tsc whose footer is
// corrupted after OpenChunkIndex validated it is still usable on reload,
// because the footer was checked once at open and .tsc files are immutable.
// (Chunk data itself is always hash-verified at read time, so index-level
// trust is safe.) This locks in the efficiency fix that prevents a disjoint
// download from re-hashing the whole store on every Bloom false positive.
func TestChunkIndexReloadSkipsFooterRehash(t *testing.T) {
	tmpDir := t.TempDir()
	snapABase := makeTestSnap(t, tmpDir, "snapA", map[string]string{
		"a.txt": "content for snap A, distinct from snap B\n",
	})
	snapBBase := makeTestSnap(t, tmpDir, "snapB", map[string]string{
		"b.txt": "completely different content for snap B\n",
	})

	// Caps of 1 force eviction: looking up a chunk in one snap evicts the
	// other's loaded .tsc, so the next lookup of the evicted snap reloads it.
	ci, err := OpenChunkIndexWith(tmpDir, 1, 1)
	if err != nil {
		t.Fatalf("OpenChunkIndexWith: %v", err)
	}

	chunkA := firstChunkSHA(t, snapABase+".tsc")
	chunkB := firstChunkSHA(t, snapBBase+".tsc")

	// Load snapA's .tsc, then snapB's (evicting snapA's), so snapA is stale.
	if _, found := ci.FindChunk(chunkA); !found {
		t.Fatal("initial FindChunk(chunkA) miss")
	}
	if _, found := ci.FindChunk(chunkB); !found {
		t.Fatal("initial FindChunk(chunkB) miss")
	}

	// Corrupt snapA's .tsc footer (the last 32 bytes). The header + entries
	// (which the footer covers) are left intact, so a reload that skips the
	// footer hash still sees a valid index; a reload that re-hashes would
	// reject the file.
	data, err := os.ReadFile(snapABase + ".tsc")
	if err != nil {
		t.Fatal(err)
	}
	for i := len(data) - TSCFooterSize; i < len(data); i++ {
		data[i] ^= 0xff
	}
	if err := os.WriteFile(snapABase+".tsc", data, 0644); err != nil {
		t.Fatal(err)
	}

	// FindChunk(chunkA) must reload snapA's .tsc (skipping the footer hash)
	// and still resolve the chunk, whose file data is intact.
	loc, found := ci.FindChunk(chunkA)
	if !found {
		t.Fatal("FindChunk(chunkA) miss after footer corruption + reload; " +
			"reload should skip the footer SHA-256")
	}
	if _, err := loc.VerifyAndRead(); err != nil {
		t.Fatalf("VerifyAndRead after reload: %v", err)
	}
}
