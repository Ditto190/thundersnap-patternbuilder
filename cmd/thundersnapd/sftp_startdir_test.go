// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tailscale/thundersnap/tsm"
)

// writeFile is a tiny helper for creating a file with content in one call.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// mkroot builds a minimal frame rootfs in a temp dir: an empty /home subvolume
// stand-in (just a dir) and, if withPasswd is true, an /etc/passwd. passwdRoot
// adds a root entry with the given home; passwdUser adds the thundersnap "user"
// entry with home=/home (mirroring EnsureUserInPasswd).
func mkroot(t *testing.T, withPasswd, passwdRoot, passwdUser bool) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "home"), 0755); err != nil {
		t.Fatal(err)
	}
	if withPasswd {
		if err := os.MkdirAll(filepath.Join(root, "etc"), 0755); err != nil {
			t.Fatal(err)
		}
		var lines string
		if passwdRoot {
			lines += "root:x:0:0:root:/root:/bin/sh\n"
		}
		if passwdUser {
			lines += "user:x:7575:7575:user:/home:/bin/sh\n"
		}
		writeFile(t, filepath.Join(root, "etc", "passwd"), lines)
	}
	return root
}

// TestResolveSFTPStartDirDefaultUser covers the default (no root@ prefix) login:
// selectTargetUser ensures the "user" passwd entry (home=/home) and /home
// exists, so scp starts in /home.
func TestResolveSFTPStartDirDefaultUser(t *testing.T) {
	root := mkroot(t, false, false, false) // nil:nil:nil-style: no passwd, /home exists
	got := resolveSFTPStartDir(root, "", tsm.LookupUser(root, selectTargetUser(root, "")))
	if got != "/home" {
		t.Fatalf("default user start dir = %q, want /home", got)
	}
}

// TestResolveSFTPStartDirRootNoPasswd reproduces the reported bug: connecting
// as root@ to a passwd-less nil:nil:nil frame. The old code returned the
// nonexistent /home/user; now it falls back to /home.
func TestResolveSFTPStartDirRootNoPasswd(t *testing.T) {
	root := mkroot(t, false, false, false)
	got := resolveSFTPStartDir(root, "root", tsm.LookupUser(root, "root"))
	if got != "/home" {
		t.Fatalf("root@ (no passwd) start dir = %q, want /home", got)
	}
}

// TestResolveSFTPStartDirRootRealImage covers root@ to a real image whose
// /etc/passwd has root -> /root. /root is deliberately skipped so scp lands in
// the shared /home (where `ssh host` puts the default user), not /root.
func TestResolveSFTPStartDirRootRealImage(t *testing.T) {
	root := mkroot(t, true, true, false)
	if err := os.MkdirAll(filepath.Join(root, "root"), 0755); err != nil {
		t.Fatal(err)
	}
	got := resolveSFTPStartDir(root, "root", tsm.LookupUser(root, "root"))
	if got != "/home" {
		t.Fatalf("root@ (real image) start dir = %q, want /home (root skipped)", got)
	}
}

// TestResolveSFTPStartDirUbuntu confirms a real per-user home (ubuntu's
// /home/ubuntu) is honored when it exists in the rootfs.
func TestResolveSFTPStartDirUbuntu(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "home", "ubuntu"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "etc", "passwd"),
		"ubuntu:x:1000:1000:ubuntu:/home/ubuntu:/bin/bash\n")
	got := resolveSFTPStartDir(root, "ubuntu", tsm.LookupUser(root, "ubuntu"))
	if got != "/home/ubuntu" {
		t.Fatalf("ubuntu start dir = %q, want /home/ubuntu", got)
	}
}

// TestResolveSFTPStartDirUserHomeMissing verifies that if the looked-up home
// does not exist in the rootfs (e.g. user entry says /home/user but only /home
// exists), we fall back to /home rather than a nonexistent directory.
func TestResolveSFTPStartDirUserHomeMissing(t *testing.T) {
	root := mkroot(t, true, false, true) // "user" -> /home, which exists
	got := resolveSFTPStartDir(root, "", tsm.LookupUser(root, selectTargetUser(root, "")))
	if got != "/home" {
		t.Fatalf("user (home=/home) start dir = %q, want /home", got)
	}
}
