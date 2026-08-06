// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

// Package refid manages the per-ref identity subvolumes that live under a
// frame's /id directory.
//
// Inside a frame, /id is its own btrfs subvolume that is never captured in a
// snapshot (btrfs excludes nested subvolumes, and the snapshot indexer skips
// across filesystem boundaries). Each subdirectory of /id is itself a btrfs
// subvolume corresponding to one ref that points at this frame, holding that
// ref's private state (keys, tsnet identity, etc.).
//
// When a ref moves from one frame to another, its identity subvolume is moved
// with it: a plain rename across the two frames' /id subvolumes preserves the
// nested subvolume and its contents (both frames live on the same btrfs
// filesystem under the data dir).
package refid

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tailscale/thundersnap/btrfsutil"
	"github.com/tailscale/thundersnap/tsm"
)

// idDirName is the per-frame directory holding per-ref identity subvolumes.
const idDirName = "id"

// ErrInvalidRefName is returned when a ref name cannot be used as a single
// path component under a frame's /id directory.
var ErrInvalidRefName = errors.New("invalid ref name")

// validateRefName rejects ref names that would escape or not resolve to a
// single child of the /id directory. Callers (the daemon) already validate ref
// names via refs.ValidateName, but refid is an importable package operating on
// real filesystem paths, so it guards itself: "", ".", "..", and any name
// containing a path separator (e.g. "../escape" or "a/b") are rejected.
func validateRefName(refName string) error {
	if refName == "" || refName == "." || refName == ".." {
		return fmt.Errorf("%w: %q", ErrInvalidRefName, refName)
	}
	if strings.ContainsRune(refName, filepath.Separator) || strings.ContainsRune(refName, '/') {
		return fmt.Errorf("%w: %q contains a path separator", ErrInvalidRefName, refName)
	}
	return nil
}

// createSubvol creates a btrfs subvolume at path.
func createSubvol(path string) error {
	return btrfsutil.CreateSubvol(path)
}

// configureIDDir keeps the top-level /id directory root-owned and
// non-writable by frame users. Users may traverse it to reach the control
// socket and their per-ref identity directories, but cannot put ephemeral
// files directly in /id.
func configureIDDir(path string) error {
	if err := os.Chown(path, 0, 0); err != nil {
		return fmt.Errorf("chown id directory %s: %w", path, err)
	}
	if err := os.Chmod(path, 0755); err != nil {
		return fmt.Errorf("chmod id directory %s: %w", path, err)
	}
	return nil
}

// configureRefDir makes a per-ref identity directory private and writable by
// the frame's default user. Apply it to existing subvolumes too so directories
// created by older daemon versions are repaired in place.
func configureRefDir(path string) error {
	if err := os.Chown(path, tsm.ThundersnapUID, tsm.ThundersnapGID); err != nil {
		return fmt.Errorf("chown ref identity directory %s: %w", path, err)
	}
	if err := os.Chmod(path, 0700); err != nil {
		return fmt.Errorf("chmod ref identity directory %s: %w", path, err)
	}
	return nil
}

// removeIfPlainDir removes path if it exists as a plain (non-subvolume)
// directory. Such leftovers can appear because a read-only btrfs snapshot does
// not recreate nested subvolumes, leaving an empty or plain directory in their
// place. A path that is already a subvolume, or does not exist, is left alone.
func removeIfPlainDir(path string) error {
	if btrfsutil.IsSubvolume(path) {
		return nil
	}
	if fi, err := os.Stat(path); err == nil && fi.IsDir() {
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove non-subvolume dir %s: %w", path, err)
		}
	}
	return nil
}

// IDDir returns the path to a frame's /id directory.
func IDDir(framePath string) string {
	return filepath.Join(framePath, idDirName)
}

// Path returns the path to a ref's identity subvolume within a frame, i.e.
// <framePath>/id/<refName>.
func Path(framePath, refName string) string {
	return filepath.Join(IDDir(framePath), refName)
}

// ensureIDSubvol makes sure <framePath>/id exists as a root-owned, 0755 btrfs
// subvolume. A plain directory left over from a snapshot is replaced with a
// fresh subvolume.
func ensureIDSubvol(framePath string) error {
	idPath := IDDir(framePath)
	if !btrfsutil.IsSubvolume(idPath) {
		// Not (yet) a subvolume: drop any leftover plain directory, then create it.
		if err := removeIfPlainDir(idPath); err != nil {
			return err
		}
		if err := createSubvol(idPath); err != nil {
			return err
		}
	}
	return configureIDDir(idPath)
}

// Ensure creates the identity subvolume for refName in framePath if it does
// not already exist. It is idempotent: an existing subvolume is left untouched
// (its contents are preserved). The parent /id is created as a subvolume too if
// needed.
func Ensure(framePath, refName string) error {
	if err := validateRefName(refName); err != nil {
		return err
	}
	if err := ensureIDSubvol(framePath); err != nil {
		return err
	}
	refPath := Path(framePath, refName)
	if !btrfsutil.IsSubvolume(refPath) {
		// A leftover plain directory (e.g. from an older layout) is replaced so the
		// ref state is always a real subvolume that snapshots exclude.
		if err := removeIfPlainDir(refPath); err != nil {
			return err
		}
		if err := createSubvol(refPath); err != nil {
			return err
		}
	}
	return configureRefDir(refPath)
}

// RepairOwnership reapplies the ownership contract to a frame's existing /id
// tree. The daemon calls this whenever it prepares a frame for entry so
// per-ref subvolumes created by older versions are repaired without requiring
// the ref to be recreated or moved. Non-directory entries such as
// thunder.sock are ignored.
func RepairOwnership(framePath string) error {
	if err := ensureIDSubvol(framePath); err != nil {
		return err
	}
	entries, err := os.ReadDir(IDDir(framePath))
	if err != nil {
		return fmt.Errorf("read id directory: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := Path(framePath, entry.Name())
		if !btrfsutil.IsSubvolume(path) {
			continue
		}
		if err := configureRefDir(path); err != nil {
			return err
		}
	}
	return nil
}

// Move relocates refName's identity subvolume from srcFramePath to
// dstFramePath, preserving its contents. If the source subvolume does not
// exist, Move ensures a fresh empty one at the destination instead (the ref had
// no prior identity state). If the destination already holds a subvolume for
// this ref, it is removed first so the moved one takes its place.
func Move(srcFramePath, dstFramePath, refName string) error {
	if err := validateRefName(refName); err != nil {
		return err
	}
	if err := ensureIDSubvol(dstFramePath); err != nil {
		return err
	}
	src := Path(srcFramePath, refName)
	dst := Path(dstFramePath, refName)

	if !btrfsutil.IsSubvolume(src) {
		// The ref had no prior identity state (its source subvolume was never
		// created, e.g. the ref was only ever attached to an empty frame), so
		// there is nothing to relocate. Give the destination a fresh, empty
		// identity subvolume so callers can rely on it existing afterwards.
		return Ensure(dstFramePath, refName)
	}

	// Clear any existing destination so the rename can land. Delete it as a
	// subvolume if it is one, otherwise drop a leftover plain directory.
	if btrfsutil.IsSubvolume(dst) {
		if err := btrfsutil.DeleteSubvol(dst); err != nil {
			return err
		}
	} else if err := removeIfPlainDir(dst); err != nil {
		return err
	}

	// This os.Rename works because of a precise btrfs invariant: src is itself
	// a subvolume root (created by Ensure/createSubvol). rename(2) can relink a
	// subvolume root from one subvolume's directory into another's as a pure
	// metadata move of that one object, even though the two parents (the frames'
	// /id subvolumes) have distinct inode namespaces. The same rename of a
	// *plain* directory across that boundary, or across a separately mounted
	// filesystem, returns EXDEV. So this holds only while (1) both frames live
	// under the same btrfs mount (true today: <data-dir>/fs/<uuid>) and (2) src
	// is exactly a subvolume root. If frames were ever mounted individually, or
	// src were a plain dir, this would need a btrfs snapshot+delete fallback.
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("move ref id subvolume %s -> %s: %w", src, dst, err)
	}
	return configureRefDir(dst)
}

// Remove deletes refName's identity subvolume from framePath, if present.
func Remove(framePath, refName string) error {
	if err := validateRefName(refName); err != nil {
		return err
	}
	refPath := Path(framePath, refName)
	if !btrfsutil.IsSubvolume(refPath) {
		// Fall back to removing a plain directory if one exists.
		if err := os.RemoveAll(refPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove ref id dir: %w", err)
		}
		return nil
	}
	return btrfsutil.DeleteSubvol(refPath)
}
