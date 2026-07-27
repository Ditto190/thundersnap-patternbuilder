#!/bin/bash
# test-cleanup.sh <dir> — reclaim leftover btrfs subvolumes and temp files
# from a thundersnap e2e/not_e2e test run under <dir>.
#
# <dir> is the TMPDIR the test suite uses (e.g. .tmp-e2e). Everything UNDER
# <dir> is removed; <dir> itself is kept and re-modeled root-owned, mode 0700,
# so non-root tools (find/grep/ls) don't recurse into it.
#
# Safe to run before a test run (clean slate) and after a successful run
# (reclaim orphans from killed tests). The Makefile deliberately does NOT run
# it after a FAILED run so orphans are left for debugging.
#
# Must run as root (btrfs subvolume delete requires it). If invoked as non-root,
# re-execs under sudo (passwordless sudo assumed, as everywhere else in this
# repo's test setup).
set -u

dir="${1:-}"
if [ -z "$dir" ]; then
	echo "usage: $0 <dir>" >&2
	exit 2
fi
# Canonicalize (does not require the path to exist under GNU readlink -f).
dir="$(readlink -f "$dir")" || { echo "$0: bad path $1" >&2; exit 2; }

if [ "$(id -u)" -ne 0 ]; then
	exec sudo -E "$0" "$dir"
fi

mkdir -p "$dir"

# Delete every btrfs subvolume under $dir, deepest-first. A subvolume with
# nested subvolumes can't be deleted until its children are, so sort by path
# length descending (longer path = deeper). `btrfs subvolume show` filters to
# actual subvolumes (plain dirs are skipped, no error).
find "$dir" -mindepth 1 -type d 2>/dev/null \
	| awk '{ print length($0), $0 }' \
	| sort -rn \
	| cut -d' ' -f2- \
	| while read -r v; do
		if btrfs subvolume show "$v" >/dev/null 2>&1; then
			btrfs subvolume delete "$v" >/dev/null 2>&1 || true
		fi
	done

# Remove any remaining non-subvol files/dirs (t.TempDir leftovers, sockets,
# stray files). Subvols that somehow survived the delete above are skipped by
# rm (rm cannot remove a subvolume) — the `|| true` keeps the sweep going.
find "$dir" -mindepth 1 -maxdepth 1 -exec rm -rf {} + 2>/dev/null || true

# Reap orphaned VM helper processes the test suite leaves behind. The VM
# teardown path kills cloud-hypervisor and virtiofsd but `passt` daemonizes
# and escapes the daemon's process-group kill, so it gets reparented to init
# (PPID=1) when the spawning thundersnapd exits. Only kill ORPHANS (PPID=1)
# so a still-running test's live VM helpers are never touched. The comm is
# `passt.avx2` on x86-64 (an avx2 variant), so match the `passt` prefix.
for comm in passt cloud-hypervisor virtiofsd thundersnapd; do
	pgrep -x "$comm" 2>/dev/null | while read -r pid; do
		[ "$(awk '/^PPid:/{print $2}' /proc/$pid/status 2>/dev/null)" = "1" ] && kill -9 "$pid" 2>/dev/null || true
	done
	done
# Same for the passt.avx2 variant (ps comm truncates; match via /proc).
for pid in $(pgrep -f '/passt' 2>/dev/null); do
	[ "$(awk '/^PPid:/{print $2}' /proc/$pid/status 2>/dev/null)" = "1" ] && kill -9 "$pid" 2>/dev/null || true
done

# Remove orphaned VM helper sockets under /tmp (passt-/virtiofs-/vsock-
# test-*). These accumulate across runs (hundreds observed) since the test
# only removes its own on the happy path.
rm -f /tmp/passt-*.sock /tmp/virtiofs-*.sock /tmp/vsock-*.sock 2>/dev/null || true

# Keep $dir itself: root-owned, mode 0700, so non-root tools skip it. This is
# mostly cosmetic (stops `find .`/`grep -r` from descending into test scratch
# space) but also matches what sudo-created contents already leave behind.
chmod 0700 "$dir" 2>/dev/null || true
chown root:root "$dir" 2>/dev/null || true
