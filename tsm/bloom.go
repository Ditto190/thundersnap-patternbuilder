// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package tsm

import (
	"encoding/binary"
	"math"
)

// bloomFilter is a classic Bloom filter over SHA-256 chunk keys, used by
// ChunkIndex to reject chunks that are absent from every local snapshot in
// O(1), without scanning per-snapshot .tsc indexes.
//
// This mirrors bup's bup.bloom, which exists for exactly the same reason:
// without it, "searching for a nonexistent object in the repository
// necessarily requires searching through all the index files" (bup-midx(1)).
// During a snapshot download most chunks are *not* present locally (that is
// why they are being fetched), so fast negative rejection is the dominant
// cost.
//
// Keys are already SHA-256 digests (well-distributed), so we derive the k
// probe positions with Kirsch-Mitzenmacher double hashing from the first two
// 64-bit words of the key: pos_i = (h1 + i*h2) mod m. This avoids allocating
// k independent hash functions while keeping the false-positive rate near the
// theoretical optimum.
type bloomFilter struct {
	bits    []uint64 // packed bit array, len == bitsLen/64
	bitsLen uint64   // number of bits
	k       int      // number of probe functions
}

// newBloomFilter creates a Bloom filter sized for n elements at the given
// false-positive rate. n==0 yields a filter that reports "not present" for
// every key (see test).
func newBloomFilter(n uint64, fp float64) *bloomFilter {
	if n == 0 {
		return &bloomFilter{}
	}
	if fp <= 0 || fp >= 1 {
		fp = 0.01
	}
	ln2 := math.Ln2
	// Optimal bit count m = -n*ln(fp)/(ln2)^2.
	m := math.Ceil(float64(n) * -math.Log(fp) / (ln2 * ln2))
	if m < 64 {
		m = 64
	}
	// Optimal probe count k = (m/n)*ln2.
	k := int(math.Ceil(ln2 * m / float64(n)))
	if k < 1 {
		k = 1
	}
	bitsLen := uint64(m)
	if rem := bitsLen % 64; rem != 0 {
		bitsLen += 64 - rem // round up to a whole number of words
	}
	return &bloomFilter{
		bits:    make([]uint64, bitsLen/64),
		bitsLen: bitsLen,
		k:       k,
	}
}

// add inserts a chunk key into the filter.
func (b *bloomFilter) add(sha [32]byte) {
	if b.bitsLen == 0 {
		return
	}
	h1, h2 := bloomHashes(sha)
	for i := 0; i < b.k; i++ {
		pos := (h1 + uint64(i)*h2) % b.bitsLen
		b.bits[pos/64] |= 1 << (pos % 64)
	}
}

// test reports whether the key may be present. A false result is definitive
// (the key is definitely absent); a true result is a "maybe".
func (b *bloomFilter) test(sha [32]byte) bool {
	if b.bitsLen == 0 {
		return false
	}
	h1, h2 := bloomHashes(sha)
	for i := 0; i < b.k; i++ {
		pos := (h1 + uint64(i)*h2) % b.bitsLen
		if b.bits[pos/64]&(1<<(pos%64)) == 0 {
			return false
		}
	}
	return true
}

// bloomHashes returns the two 64-bit hashes used for double hashing. h2 is
// forced non-zero so the probe sequence does not collapse to a single bit.
func bloomHashes(sha [32]byte) (h1, h2 uint64) {
	h1 = binary.BigEndian.Uint64(sha[0:8])
	h2 = binary.BigEndian.Uint64(sha[8:16])
	if h2 == 0 {
		h2 = 1
	}
	return h1, h2
}
