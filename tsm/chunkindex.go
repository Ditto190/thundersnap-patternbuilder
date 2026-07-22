// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package tsm

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// ChunkIndex is an efficient, bup-inspired index over the chunks of every
// local snapshot, used by Download to copy chunks from existing local
// snapshots instead of fetching them over the network. Using a pre-existing
// local chunk is essentially always cheaper than downloading it.
//
// It replaces the older all-in-memory ChunkMap (built by the former
// LoadLocalChunkMap), which eagerly parsed every snapshot's .tsm and .tsc,
// statted every file, and held one ChunkLocation per chunk per snapshot in a
// single giant sorted slice. That worked but scaled poorly: rebuilding it on
// every download was expensive, and it pinned the whole repository's chunk
// set in RAM at once.
//
// ChunkIndex borrows three ideas from bup (see bup-midx(1) and bup's
// PackIdxList in lib/bup/git.py):
//
//  1. Per-snapshot .tsc files are the on-disk chunk indexes. Each .tsc is a
//     SHA-256-sorted array of (sha,size) records and is binary-searched
//     directly from its raw bytes; entries are never parsed into structs.
//     This is the analogue of bup's per-pack .idx files.
//
//  2. Snapshots are searched in most-recently-used order, and a successful
//     lookup moves that snapshot to the front. Because the chunks of the file
//     currently being downloaded are usually co-located in one source
//     snapshot, subsequent lookups hit on the first snap checked. This is
//     bup's "consecutive objects are often stored in the same pack, so we can
//     search that one first using an MRU algorithm" (bup-midx(1)).
//
//  3. A Bloom filter summarizes every chunk across all snapshots so that a
//     chunk present in no local snapshot is rejected in O(1) without scanning
//     any .tsc. This is bup's bup.bloom, which exists because otherwise
//     "searching for a nonexistent object ... necessarily requires searching
//     through all the index files".
//
// To bound memory, the number of .tsc byte buffers and the number of
// per-snapshot location maps held in memory at once are each LRU-capped: the
// least-recently-used one is dropped when the cap is exceeded, and a dropped
// snapshot is transparently reloaded on its next hit. This mirrors bup's
// --max-files limit on simultaneously open .idx files.
type ChunkIndex struct {
	dir string

	mu    sync.Mutex
	snaps []*snapIdx // MRU order: snaps[0] is most-recently-used
	bloom *bloomFilter

	maxOpenTSC int // max .tsc byte buffers held in memory
	maxLocMaps int // max location maps held in memory
}

const (
	defaultMaxOpenTSC = 16
	defaultMaxLocMaps = 8
)

// snapIdx is the per-snapshot lazy index. Its .tsc bytes and location map are
// loaded on demand and may be evicted under the ChunkIndex's LRU caps.
type snapIdx struct {
	snapID  string
	snapDir string
	tscPath string
	tsmPath string

	// Lazy state, guarded by the parent ChunkIndex.mu:
	tsc       *tscView
	tscLoaded bool // true once load attempted (even on failure; don't retry)
	locMap    map[[32]byte]chunkLoc
	locBuilt  bool     // true once build attempted (even on failure; don't retry)
	filePaths []string // fileIdx -> absolute path, set when locMap is built
}

// tscView is a raw, sorted view of a .tsc file's chunk entries. It supports
// binary search by SHA (for existence) and direct indexing by TSC index (for
// building location maps). TSM ChunkRefs reference the sorted on-disk order
// (the writer remaps original->sorted via indexMap; see tsm.go), so
// entryByIndex(tscIdx) corresponds to the chunk a ChunkRef points at.
type tscView struct {
	data       []byte
	count      int
	entriesOff int // == TSCHeaderSize
	entrySize  int // == TSCEntrySize
}

// chunkLoc is a chunk's location within a single snapshot's extracted tree.
type chunkLoc struct {
	fileIdx uint32
	offset  int64
}

// OpenChunkIndex scans a snapshots directory and builds an efficient chunk
// lookup index over all local snapshots. It reads each .tsc once (to populate
// the Bloom filter) but does not parse any .tsm or stat any file; per-snapshot
// location maps are built lazily on the first chunk found in that snapshot.
//
// A snapshot is discovered from its .tsc file and included only if its .tsm
// and extracted directory both exist. Files ending in .tsc.tmp (in-progress
// downloads) are ignored, so a download never deduplicates against itself.
//
// This is the TSM/TSC-based successor to the former LoadLocalChunkMap, used by
// Download for cross-snapshot chunk deduplication.
func OpenChunkIndex(snapsDir string) (*ChunkIndex, error) {
	return OpenChunkIndexWith(snapsDir, 0, 0)
}

// OpenChunkIndexWith is like OpenChunkIndex but lets the caller tune the LRU
// caps on simultaneously loaded .tsc buffers (maxOpenTSC) and built location
// maps (maxLocMaps). Zero means use the default. It is primarily exposed for
// tests; production callers should use OpenChunkIndex.
func OpenChunkIndexWith(snapsDir string, maxOpenTSC, maxLocMaps int) (*ChunkIndex, error) {
	if maxOpenTSC <= 0 {
		maxOpenTSC = defaultMaxOpenTSC
	}
	if maxLocMaps <= 0 {
		maxLocMaps = defaultMaxLocMaps
	}

	ci := &ChunkIndex{
		dir:        snapsDir,
		maxOpenTSC: maxOpenTSC,
		maxLocMaps: maxLocMaps,
	}

	entries, err := os.ReadDir(snapsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return ci, nil // empty index over a missing directory
		}
		return nil, err
	}

	// First pass: discover snaps and sum chunk counts from .tsc headers, so we
	// can size the Bloom filter before populating it. Reading only the 64-byte
	// header is cheap; the full .tsc is streamed in the second pass below.
	var snaps []*snapIdx
	var totalChunks uint64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".tsc") || strings.HasSuffix(name, ".tsc.tmp") {
			continue
		}
		base := strings.TrimSuffix(name, ".tsc")
		tscPath := filepath.Join(snapsDir, name)
		tsmPath := filepath.Join(snapsDir, base+".tsm")
		snapDir := filepath.Join(snapsDir, base)
		if _, err := os.Stat(tsmPath); err != nil {
			continue
		}
		if _, err := os.Stat(snapDir); err != nil {
			continue
		}
		count, err := readTSCChunkCount(tscPath)
		if err != nil {
			continue // skip unreadable/invalid .tsc
		}
		snaps = append(snaps, &snapIdx{
			snapID:  base,
			snapDir: snapDir,
			tscPath: tscPath,
			tsmPath: tsmPath,
		})
		totalChunks += count
	}

	// Deterministic initial order (before MRU reordering takes over).
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].snapID < snaps[j].snapID })

	// Second pass: validate each .tsc (footer checksum) and stream its SHAs
	// into the Bloom filter. The bytes are not retained here; each .tsc is
	// re-read lazily, under the LRU cap, when first searched. A snap whose
	// .tsc fails validation is dropped from the index entirely.
	bloom := newBloomFilter(totalChunks, 0.01)
	good := snaps[:0]
	for _, s := range snaps {
		content, err := readTSCContentValidated(s.tscPath)
		if err != nil {
			continue
		}
		streamTSCSHAs(content, bloom.add)
		good = append(good, s)
	}
	ci.snaps = good
	ci.bloom = bloom

	return ci, nil
}

// FindChunk looks up a chunk by SHA-256 across all local snapshots and returns
// a readable location if found. Lookups are accelerated by the Bloom filter
// (fast negative) and ordered by most-recently-used snapshot (so the source
// snapshot of the current file is checked first). The returned ChunkLocation
// is only valid for as long as the source file on disk is not modified; the
// caller should read it promptly (Download does so immediately).
func (ci *ChunkIndex) FindChunk(sha [32]byte) (*ChunkLocation, bool) {
	ci.mu.Lock()
	defer ci.mu.Unlock()

	// Bloom: a "no" is definitive and avoids scanning any .tsc. A "yes" is a
	// maybe and falls through to the per-snap search.
	if ci.bloom != nil && !ci.bloom.test(sha) {
		return nil, false
	}

	for i := 0; i < len(ci.snaps); i++ {
		s := ci.snaps[i]

		if !s.tscLoaded {
			ci.loadTSCLocked(s)
		}
		if s.tsc == nil {
			continue
		}
		size, ok := s.tsc.lookup(sha)
		if !ok {
			continue
		}

		// The chunk is indexed in this snapshot; resolve it to a readable
		// file location via the (lazily built) location map.
		if !s.locBuilt {
			ci.buildLocMapLocked(s)
		}
		loc, ok := s.locMap[sha]
		if !ok {
			// Indexed but not materialized on disk here (the containing file
			// is missing). Try the next snapshot; another one may have it.
			continue
		}

		ci.moveToFrontLocked(i)
		return &ChunkLocation{
			SHA256:   sha,
			Filename: s.filePaths[loc.fileIdx],
			Offset:   loc.offset,
			Size:     size,
		}, true
	}
	return nil, false
}

// SnapCount returns the number of local snapshots included in the index.
func (ci *ChunkIndex) SnapCount() int {
	ci.mu.Lock()
	defer ci.mu.Unlock()
	return len(ci.snaps)
}

// loadTSCLocked loads s.tsc from disk, evicting the least-recently-used .tsc
// buffer if the cap would be exceeded. Must be called with ci.mu held.
func (ci *ChunkIndex) loadTSCLocked(s *snapIdx) {
	s.tscLoaded = true
	content, err := readTSCContentValidated(s.tscPath)
	if err != nil {
		s.tsc = nil // don't retry (tscLoaded stays true)
		return
	}
	s.tsc = newTSCView(content)
	ci.evictLRUTSCLocked(s)
}

// buildLocMapLocked builds s.locMap from s.tsc and the snapshot's .tsm,
// evicting the least-recently-used location map if the cap would be exceeded.
// Must be called with ci.mu held, and requires s.tsc to be loaded.
func (ci *ChunkIndex) buildLocMapLocked(s *snapIdx) {
	s.locBuilt = true
	if s.tsc == nil {
		return
	}
	tsm, err := ReadTSM(s.tsmPath)
	if err != nil {
		return // don't retry (locBuilt stays true)
	}

	// Map each chunk SHA to one readable (file, offset). A chunk may appear in
	// several files within a snapshot (the .tsc dedups chunks); any occurrence
	// is fine since the data is identical, so we keep the first we encounter.
	m := make(map[[32]byte]chunkLoc, len(tsm.Entries)*4)
	var paths []string
	for _, fe := range tsm.Entries {
		if fe.Type != EntryTypeFile || fe.ChunkCount == 0 {
			continue
		}
		fp := filepath.Join(s.snapDir, fe.Path)
		if _, err := os.Stat(fp); err != nil {
			continue // file is absent on disk; its chunks are not readable here
		}
		fileIdx := uint32(len(paths))
		paths = append(paths, fp)
		var off int64
		for _, tscIdx := range fe.ChunkRefs {
			sha, size, ok := s.tsc.entryByIndex(int(tscIdx))
			if !ok {
				break // corrupt chunk ref; skip the rest of this file
			}
			if _, exists := m[sha]; !exists {
				m[sha] = chunkLoc{fileIdx: fileIdx, offset: off}
			}
			off += int64(size)
		}
	}
	s.locMap = m
	s.filePaths = paths
	ci.evictLRULocMapLocked(s)
}

// evictLRUTSCLocked drops the least-recently-used loaded .tsc buffer (other
// than keep) until at most maxOpenTSC are loaded. Must be called with ci.mu
// held. The snaps slice is MRU-ordered, so the tail is least-recently-used.
func (ci *ChunkIndex) evictLRUTSCLocked(keep *snapIdx) {
	loaded := 0
	for _, s := range ci.snaps {
		if s.tscLoaded && s.tsc != nil {
			loaded++
		}
	}
	for loaded > ci.maxOpenTSC {
		var victim *snapIdx
		for j := len(ci.snaps) - 1; j >= 0; j-- {
			s := ci.snaps[j]
			if s != keep && s.tscLoaded && s.tsc != nil {
				victim = s
				break
			}
		}
		if victim == nil {
			break
		}
		victim.tsc = nil
		victim.tscLoaded = false
		loaded--
	}
}

// evictLRULocMapLocked drops the least-recently-used built location map (other
// than keep) until at most maxLocMaps are built. Must be called with ci.mu
// held. The snaps slice is MRU-ordered, so the tail is least-recently-used.
func (ci *ChunkIndex) evictLRULocMapLocked(keep *snapIdx) {
	built := 0
	for _, s := range ci.snaps {
		if s.locBuilt && s.locMap != nil {
			built++
		}
	}
	for built > ci.maxLocMaps {
		var victim *snapIdx
		for j := len(ci.snaps) - 1; j >= 0; j-- {
			s := ci.snaps[j]
			if s != keep && s.locBuilt && s.locMap != nil {
				victim = s
				break
			}
		}
		if victim == nil {
			break
		}
		victim.locMap = nil
		victim.locBuilt = false
		victim.filePaths = nil
		built--
	}
}

// moveToFrontLocked moves snaps[i] to the front of the MRU-ordered slice.
// Must be called with ci.mu held.
func (ci *ChunkIndex) moveToFrontLocked(i int) {
	if i == 0 {
		return
	}
	s := ci.snaps[i]
	copy(ci.snaps[1:i+1], ci.snaps[:i])
	ci.snaps[0] = s
}

// newTSCView wraps validated .tsc content (header+entries, no footer) as a
// tscView. Returns nil if the content is too short.
func newTSCView(content []byte) *tscView {
	if len(content) < TSCHeaderSize {
		return nil
	}
	count := int(binary.BigEndian.Uint64(content[8:16]))
	return &tscView{
		data:       content,
		count:      count,
		entriesOff: TSCHeaderSize,
		entrySize:  TSCEntrySize,
	}
}

// lookup binary-searches the sorted entries for sha and returns the chunk's
// size if present.
func (v *tscView) lookup(sha [32]byte) (size uint32, ok bool) {
	if v.count == 0 {
		return 0, false
	}
	idx := sort.Search(v.count, func(i int) bool {
		off := v.entriesOff + i*v.entrySize
		return bytes.Compare(v.data[off:off+32], sha[:]) >= 0
	})
	if idx < v.count {
		off := v.entriesOff + idx*v.entrySize
		if bytes.Equal(v.data[off:off+32], sha[:]) {
			return binary.BigEndian.Uint32(v.data[off+32 : off+36]), true
		}
	}
	return 0, false
}

// entryByIndex returns the SHA and size of the entry at sorted TSC index i,
// which is the index TSM ChunkRefs reference. Used when building a location
// map.
func (v *tscView) entryByIndex(i int) (sha [32]byte, size uint32, ok bool) {
	if i < 0 || i >= v.count {
		return [32]byte{}, 0, false
	}
	off := v.entriesOff + i*v.entrySize
	if off+TSCEntrySize > len(v.data) {
		return [32]byte{}, 0, false
	}
	copy(sha[:], v.data[off:off+32])
	return sha, binary.BigEndian.Uint32(v.data[off+32 : off+36]), true
}

// readTSCChunkCount reads just a .tsc header and returns the chunk count,
// without parsing entries. Used to size the Bloom filter cheaply.
func readTSCChunkCount(path string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var hdr [TSCHeaderSize]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return 0, err
	}
	if string(hdr[0:4]) != TSCMagic {
		return 0, fmt.Errorf("invalid TSC magic")
	}
	return binary.BigEndian.Uint64(hdr[8:16]), nil
}

// readTSCContentValidated reads a .tsc file, verifies its footer checksum, and
// returns the content bytes (header + entries, excluding the 32-byte footer).
// Entries are not parsed into structs.
func readTSCContentValidated(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < TSCHeaderSize+TSCFooterSize {
		return nil, fmt.Errorf("tsc file too short")
	}
	if string(data[0:4]) != TSCMagic {
		return nil, fmt.Errorf("invalid TSC magic")
	}
	footer := data[len(data)-TSCFooterSize:]
	content := data[:len(data)-TSCFooterSize]
	h := sha256.New()
	h.Write(content)
	if !bytes.Equal(h.Sum(nil), footer) {
		return nil, fmt.Errorf("tsc checksum mismatch")
	}
	return content, nil
}

// streamTSCSHAs calls fn for each chunk SHA in validated .tsc content, in
// sorted (on-disk) order.
func streamTSCSHAs(content []byte, fn func(sha [32]byte)) {
	if len(content) < TSCHeaderSize {
		return
	}
	count := int(binary.BigEndian.Uint64(content[8:16]))
	off := TSCHeaderSize
	for i := 0; i < count; i++ {
		end := off + TSCEntrySize
		if end > len(content) {
			return
		}
		var sha [32]byte
		copy(sha[:], content[off:off+32])
		fn(sha)
		off = end
	}
}
