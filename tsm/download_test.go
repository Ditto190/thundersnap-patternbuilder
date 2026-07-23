// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package tsm

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFetchFullFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("test content"))
	}))
	defer server.Close()

	data, err := fetchFullFile(http.DefaultClient, server.URL)
	if err != nil {
		t.Fatalf("fetchFullFile: %v", err)
	}
	if string(data) != "test content" {
		t.Errorf("got %q, want %q", data, "test content")
	}
}

func TestFetchRanges(t *testing.T) {
	content := "Hello, World! This is test content."
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rangeHeader := r.Header.Get("Range")
		if rangeHeader == "" {
			w.Write([]byte(content))
			return
		}

		// Parse range header (simple version)
		var start, end int64
		fmt.Sscanf(rangeHeader, "bytes=%d-%d", &start, &end)
		if start < 0 || end >= int64(len(content)) || start > end {
			http.Error(w, "invalid range", http.StatusRequestedRangeNotSatisfiable)
			return
		}

		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(content)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write([]byte(content[start : end+1]))
	}))
	defer server.Close()

	ranges := []rangeSpec{
		{offset: 0, size: 5}, // "Hello"
		{offset: 7, size: 6}, // "World!"
	}

	results, err := fetchRanges(http.DefaultClient, server.URL, ranges)
	if err != nil {
		t.Fatalf("fetchRanges: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}

	if string(results[0]) != "Hello" {
		t.Errorf("range 0: got %q, want %q", results[0], "Hello")
	}
	if string(results[1]) != "World!" {
		t.Errorf("range 1: got %q, want %q", results[1], "World!")
	}
}

func TestDownloadIntegration(t *testing.T) {
	// Create a temp directory structure simulating a peer's snapshots
	peerDir := t.TempDir()
	snapName := "testsnap123"
	snapDir := filepath.Join(peerDir, snapName)

	// Create snapshot content
	if err := os.MkdirAll(filepath.Join(snapDir, "subdir"), 0755); err != nil {
		t.Fatal(err)
	}

	testContent := []byte("This is test file content for the download test.\n")
	if err := os.WriteFile(filepath.Join(snapDir, "testfile.txt"), testContent, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapDir, "subdir", "nested.txt"), []byte("nested content\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create symlink
	if err := os.Symlink("testfile.txt", filepath.Join(snapDir, "link.txt")); err != nil {
		t.Fatal(err)
	}

	// Generate TSM/TSC
	outBase := filepath.Join(peerDir, snapName)
	if err := Create(snapDir, outBase, IndexerOptions{}); err != nil {
		t.Fatal(err)
	}

	// Create stamp file
	if err := os.WriteFile(outBase+".stamp", []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	// Create HTTP server serving the peer's files
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/bupdate/")
		fullPath := filepath.Join(peerDir, path)

		// Handle range requests
		rangeHeader := r.Header.Get("Range")
		if rangeHeader != "" {
			var start, end int64
			fmt.Sscanf(rangeHeader, "bytes=%d-%d", &start, &end)

			f, err := os.Open(fullPath)
			if err != nil {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			defer f.Close()

			stat, _ := f.Stat()
			if end >= stat.Size() {
				end = stat.Size() - 1
			}

			f.Seek(start, 0)
			data := make([]byte, end-start+1)
			f.Read(data)

			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, stat.Size()))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(data)
			return
		}

		// Serve full file
		http.ServeFile(w, r, fullPath)
	}))
	defer server.Close()

	// Download to a new directory
	localDir := t.TempDir()

	result, err := Download(DownloadOptions{
		SnapshotID: snapName,
		SnapsDir:   localDir,
		BaseURL:    server.URL,
	})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}

	if result.AlreadyExists {
		t.Error("expected AlreadyExists=false")
	}

	// Verify downloaded content
	downloadedPath := filepath.Join(localDir, snapName)
	if _, err := os.Stat(downloadedPath); err != nil {
		t.Fatalf("snapshot dir not created: %v", err)
	}

	// Check testfile.txt
	data, err := os.ReadFile(filepath.Join(downloadedPath, "testfile.txt"))
	if err != nil {
		t.Fatalf("reading testfile.txt: %v", err)
	}
	if string(data) != string(testContent) {
		t.Errorf("testfile.txt content mismatch")
	}

	// Check nested file
	data, err = os.ReadFile(filepath.Join(downloadedPath, "subdir", "nested.txt"))
	if err != nil {
		t.Fatalf("reading nested.txt: %v", err)
	}
	if string(data) != "nested content\n" {
		t.Errorf("nested.txt content mismatch")
	}

	// Check symlink
	target, err := os.Readlink(filepath.Join(downloadedPath, "link.txt"))
	if err != nil {
		t.Fatalf("reading link.txt: %v", err)
	}
	if target != "testfile.txt" {
		t.Errorf("symlink target=%q, want testfile.txt", target)
	}

	// Check metadata files
	if _, err := os.Stat(filepath.Join(localDir, snapName+".tsm")); err != nil {
		t.Errorf("TSM file not saved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(localDir, snapName+".tsc")); err != nil {
		t.Errorf("TSC file not saved: %v", err)
	}

	t.Log("Download integration test passed")
}

// TestDownloadRestoresMtime verifies that extracting a downloaded snapshot
// restores each file's original mtime, as recorded in the manifest at index
// time - not the time at which the file happened to be extracted.
func TestDownloadRestoresMtime(t *testing.T) {
	peerDir := t.TempDir()
	snapName := "mtimesnap"
	snapDir := filepath.Join(peerDir, snapName)

	if err := os.MkdirAll(snapDir, 0755); err != nil {
		t.Fatal(err)
	}

	testFile := filepath.Join(snapDir, "testfile.txt")
	if err := os.WriteFile(testFile, []byte("content\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Set a distinctive mtime (including a non-zero nanosecond component,
	// to check restoration is nanosecond-accurate) before indexing, so the
	// manifest records exactly this value rather than "now".
	wantMtime := time.Date(2005, time.March, 4, 12, 34, 56, 123456789, time.UTC)
	if err := os.Chtimes(testFile, wantMtime, wantMtime); err != nil {
		t.Fatal(err)
	}

	// Generate TSM/TSC from the file with its pre-set mtime.
	outBase := filepath.Join(peerDir, snapName)
	if err := Create(snapDir, outBase, IndexerOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outBase+".stamp", []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	// Serve the peer's files over HTTP, same as TestDownloadIntegration.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/bupdate/")
		fullPath := filepath.Join(peerDir, path)

		rangeHeader := r.Header.Get("Range")
		if rangeHeader != "" {
			var start, end int64
			fmt.Sscanf(rangeHeader, "bytes=%d-%d", &start, &end)

			f, err := os.Open(fullPath)
			if err != nil {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			defer f.Close()

			stat, _ := f.Stat()
			if end >= stat.Size() {
				end = stat.Size() - 1
			}

			f.Seek(start, 0)
			data := make([]byte, end-start+1)
			f.Read(data)

			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, stat.Size()))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(data)
			return
		}

		http.ServeFile(w, r, fullPath)
	}))
	defer server.Close()

	// Extract the snapshot into a different directory, simulating extraction
	// on a completely different host/filesystem.
	localDir := t.TempDir()
	if _, err := Download(DownloadOptions{
		SnapshotID: snapName,
		SnapsDir:   localDir,
		BaseURL:    server.URL,
	}); err != nil {
		t.Fatalf("Download: %v", err)
	}

	extractedFile := filepath.Join(localDir, snapName, "testfile.txt")
	info, err := os.Stat(extractedFile)
	if err != nil {
		t.Fatalf("stat extracted file: %v", err)
	}

	if !info.ModTime().Equal(wantMtime) {
		t.Errorf("extracted file mtime = %s, want %s", info.ModTime(), wantMtime)
	}
}

func TestDownloadAlreadyExists(t *testing.T) {
	tmpDir := t.TempDir()
	snapName := "existing"

	// Create an existing snapshot
	if err := os.MkdirAll(filepath.Join(tmpDir, snapName), 0755); err != nil {
		t.Fatal(err)
	}

	result, err := Download(DownloadOptions{
		SnapshotID: snapName,
		SnapsDir:   tmpDir,
		BaseURL:    "http://localhost:9999", // Won't be used
	})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}

	if !result.AlreadyExists {
		t.Error("expected AlreadyExists=true")
	}
}

// TestDownloadDedupFromOtherSnap proves that downloading a snapshot reuses
// chunks already present in a *different* local snapshot, rather than fetching
// them from the peer. It does so decisively: the peer serves the new snap's
// metadata (.stamp/.tsm/.tsc) but refuses to serve any chunk data, so the
// download can only succeed if every chunk is copied from the pre-existing
// local snapshot.
//
// The two snapshots have different paths (hence different snapshot IDs) but
// identical file *content*, so content-defined chunking produces an identical
// chunk set, and every chunk of the new snap is locatable in the old one via
// ChunkIndex.
func TestDownloadDedupFromOtherSnap(t *testing.T) {
	peerDir := t.TempDir()
	localDir := t.TempDir()

	// Identical content in both snapshots; different filename so the snapshot
	// IDs (.tsm hashes) differ but the chunk hashes are identical.
	content := []byte("shared file content for cross-snapshot chunk dedup test\n" +
		strings.Repeat("the quick brown fox jumps over the lazy dog\n", 64))

	// Local, pre-existing snapshot "snapA".
	if err := os.MkdirAll(filepath.Join(localDir, "snapA"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "snapA", "file.txt"), content, 0644); err != nil {
		t.Fatal(err)
	}
	if err := Create(filepath.Join(localDir, "snapA"), filepath.Join(localDir, "snapA"), IndexerOptions{}); err != nil {
		t.Fatal(err)
	}

	// Peer snapshot "snapB": same bytes, different path.
	if err := os.MkdirAll(filepath.Join(peerDir, "snapB"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(peerDir, "snapB", "renamed.txt"), content, 0644); err != nil {
		t.Fatal(err)
	}
	if err := Create(filepath.Join(peerDir, "snapB"), filepath.Join(peerDir, "snapB"), IndexerOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(peerDir, "snapB.stamp"), []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	// Server: serve metadata files fully; REFUSE all chunk-data requests.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/bupdate/")
		fullPath := filepath.Join(peerDir, path)

		switch {
		case strings.HasSuffix(path, ".stamp"),
			strings.HasSuffix(path, ".tsm"),
			strings.HasSuffix(path, ".tsc"):
			http.ServeFile(w, r, fullPath)
		default:
			// Chunk data: refuse. If the download needs any byte from the
			// peer, it must fail.
			http.Error(w, "chunk data not served by this peer", http.StatusNotFound)
		}
	}))
	defer server.Close()

	result, err := Download(DownloadOptions{
		SnapshotID: "snapB",
		SnapsDir:   localDir,
		BaseURL:    server.URL,
	})
	if err != nil {
		t.Fatalf("Download: %v (expected pure local dedup; peer refuses data)", err)
	}
	if result.AlreadyExists {
		t.Error("expected AlreadyExists=false")
	}

	got, err := os.ReadFile(filepath.Join(localDir, "snapB", "renamed.txt"))
	if err != nil {
		t.Fatalf("reading downloaded renamed.txt: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("downloaded content mismatch (len got=%d want=%d)", len(got), len(content))
	}
}

// TestDownloadFallsBackToRemoteOnLocalCorruption verifies that when a locally
// deduplicated chunk's source file is unreadable (hash mismatch / bit-rot / a
// stale ChunkIndex entry whose source the background indexer renamed away),
// downloadFile fetches that one chunk from the peer instead of aborting the
// whole download. The old code's "fall back to remoteData[i]" branch was dead
// (locally-deduped chunks were excluded from remoteChunks), so any local read
// failure aborted the download.
func TestDownloadFallsBackToRemoteOnLocalCorruption(t *testing.T) {
	peerDir := t.TempDir()
	localDir := t.TempDir()

	// Identical content in both snapshots so the chunk hashes match; different
	// filenames so the snapshot IDs differ.
	content := []byte("shared file content for local-corruption fallback test\n" +
		strings.Repeat("the quick brown fox jumps over the lazy dog\n", 64))

	// Local snapA: index it, THEN corrupt its file data on disk so the .tsc
	// still lists the chunk but VerifyAndRead (which hash-checks the bytes)
	// fails.
	if err := os.MkdirAll(filepath.Join(localDir, "snapA"), 0755); err != nil {
		t.Fatal(err)
	}
	snapAFile := filepath.Join(localDir, "snapA", "file.txt")
	if err := os.WriteFile(snapAFile, content, 0644); err != nil {
		t.Fatal(err)
	}
	if err := Create(filepath.Join(localDir, "snapA"), filepath.Join(localDir, "snapA"), IndexerOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snapAFile, append([]byte("X"), content[1:]...), 0644); err != nil {
		t.Fatal(err) // flip the first byte; same length, different hash
	}

	// Peer snapB: same bytes, different path.
	if err := os.MkdirAll(filepath.Join(peerDir, "snapB"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(peerDir, "snapB", "renamed.txt"), content, 0644); err != nil {
		t.Fatal(err)
	}
	if err := Create(filepath.Join(peerDir, "snapB"), filepath.Join(peerDir, "snapB"), IndexerOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(peerDir, "snapB.stamp"), []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	// Server: serve metadata files AND chunk data (by HTTP range on the file).
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/bupdate/")
		fullPath := filepath.Join(peerDir, path)
		switch {
		case strings.HasSuffix(path, ".stamp"),
			strings.HasSuffix(path, ".tsm"),
			strings.HasSuffix(path, ".tsc"):
			http.ServeFile(w, r, fullPath)
		default:
			// Chunk data: serve the file honoring Range (http.ServeFile does).
			http.ServeFile(w, r, fullPath)
		}
	}))
	defer server.Close()

	result, err := Download(DownloadOptions{
		SnapshotID: "snapB",
		SnapsDir:   localDir,
		BaseURL:    server.URL,
	})
	if err != nil {
		t.Fatalf("Download: %v (expected local-corruption fallback to remote)", err)
	}
	if result.AlreadyExists {
		t.Error("expected AlreadyExists=false")
	}

	got, err := os.ReadFile(filepath.Join(localDir, "snapB", "renamed.txt"))
	if err != nil {
		t.Fatalf("reading downloaded renamed.txt: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("downloaded content mismatch (len got=%d want=%d)", len(got), len(content))
	}
}
