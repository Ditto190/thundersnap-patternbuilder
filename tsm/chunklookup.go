// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package tsm

import (
	"fmt"
	"io"
	"os"
)

// ChunkLocation maps a chunk SHA-256 to its location in a local file. It is
// produced by ChunkIndex.FindChunk and consumed by Download to copy a chunk
// from an existing local snapshot instead of fetching it over the network.
//
// This is the TSM/TSC-based equivalent of bupdate.FidxMapping.
type ChunkLocation struct {
	SHA256   [32]byte
	Filename string // Full path to the file containing this chunk
	Offset   int64  // Byte offset within the file
	Size     uint32 // Chunk size
}

// ReadData reads the chunk data from its file location.
// Returns the data and nil on success, or nil and an error on failure.
func (loc *ChunkLocation) ReadData() ([]byte, error) {
	f, err := os.Open(loc.Filename)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", loc.Filename, err)
	}
	defer f.Close()

	if _, err := f.Seek(loc.Offset, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seeking to offset %d: %w", loc.Offset, err)
	}

	data := make([]byte, loc.Size)
	if _, err := io.ReadFull(f, data); err != nil {
		return nil, fmt.Errorf("reading %d bytes: %w", loc.Size, err)
	}

	return data, nil
}

// VerifyAndRead reads the chunk data and verifies its SHA-256.
// Returns the data if the hash matches, or an error if it doesn't.
func (loc *ChunkLocation) VerifyAndRead() ([]byte, error) {
	data, err := loc.ReadData()
	if err != nil {
		return nil, err
	}

	computed := BlobSHA256(data)
	if computed != loc.SHA256 {
		return nil, fmt.Errorf("chunk hash mismatch: expected %x, got %x", loc.SHA256, computed)
	}

	return data, nil
}
