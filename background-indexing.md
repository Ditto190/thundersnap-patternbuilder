# Background snap indexing

`ts snap --quick` and `ts go ::` do not block on content-addressable indexing.

## Why

Indexing (walking the tree and hashing every file to derive the SHA-256 snap
ID) is the slow part of a snap. The btrfs snapshot itself — the actual
"capture" of the filesystem state — is instant (COW). Callers don't need the
content-addressable ID to *do their job* (enter a frame, branch, etc.); they
only need it eventually, for sharing/dedup/history. So we capture
synchronously and index in the background.

## Behavior

### `ts snap`

- Default: waits for indexing, streams progress to stderr, and prints the
  `root:home:work` triplet (or single ID for `ts snap <path>`) to stdout.
- `ts snap --quick` (`-q`): captures the frame immediately and returns at
  once. Successful capture is silent on both stdout and stderr; the
  content-addressable ID is not known yet and indexing continues in the
  background.
- `ts snap --wait` (`-w`) is retained for backward compatibility and has the
  same behavior as the default. If both `--quick` and `--wait` are supplied,
  `--wait` wins.

Callers that need the ID (e.g. `ts undo`, which records the current snap for
history pruning) may continue to use `--wait` explicitly.

### `ts go ::` / `ts frame ::`

`::` means "save the current state and branch from it." It now hits a single
`/fork` endpoint that:

1. Captures the current frame (instant btrfs snapshot) and enqueues it for
   background indexing — so the current frame's snap is recorded eventually.
2. Clones the current frame's live rootfs/home/work subvolumes into a new
   frame (instant btrfs snapshots, no indexing).
3. Copies the current frame's sidecar/stamp/history into the new frame.
4. Wires the new frame's *first* snap to chain against the captured job, so
   that snap indexes incrementally against the (by-then finalized) fork-point
   snap rather than re-hashing everything.

The user lands in the new frame immediately. (`ts go <uuid>`, `ts go <ref>`,
and `ts go <root:home:work>` were already fast — no indexing — and are
unchanged.)

## Serialization + incremental chaining

A single daemon-wide worker indexes snaps one at a time, in capture order
(FIFO). This gives two properties the old code lacked:

1. **No unbounded parallelism.** Concurrent `ts snap` calls capture quickly
   (serializing only the brief btrfs snapshot under a mutex) and then queue
   for indexing; the worker drains them serially.
2. **Rapid double-`ts snap` chains incrementally.** Each snap records its
   "base" — the previous snap of that frame. When the worker reaches snap N,
   snap N-1 (its base) is already finalized, so snap N loads N-1's TSM/TSC
   manifest and reuses chunk hashes for unchanged files instead of
   re-reading/re-hashing them. With the old synchronous code, a second snap
   fired before the first finished indexing would re-hash everything against
   a stale parent (or race on the stamp).

Mechanism: a per-frame `framePendingSnap` map points to the most recent
not-yet-finalized snap job. At capture, if a pending job exists for the frame,
the new job's base is that job (resolved to its content IDs at index time);
otherwise the base is the frame's stamp/sidecar (the last finalized snap).
At finalize, the map entry is cleared if it still points at the finished job.
Forks set the *new* frame's pending entry to the captured job so the new
frame's first snap chains to it.

## On-disk during the pending window

A captured-but-unindexed snap lives at `snaps/<jobid>.tmp/` (a read-only btrfs
snapshot) plus `.tmp.tsm`/`.tmp.tsc` once indexed. At finalize the worker
renames these to the content-addressable `snaps/<sha256>/` names and writes
`<sha256>.jsonc`. The frame's `.stamp`/sidecar and `ts log` history are
updated at finalize (not capture), so during the pending window the frame's
metadata reflects the last *finalized* snap (slightly stale but always a
valid content-addressable ID). This keeps `ts undo`/`ts frame`/`ts log` safe
to run mid-indexing.

## Concurrency: sidecar updates and fork

The frame sidecar (`<frame>.jsonc`) is read-modify-written by three parties:
the indexing worker (at finalize), `/taint`, and fork. These are serialized
by a per-frame mutex (`snapQueue.frameMetaLock` via `updateFrameMetaLocked`)
so a taint added while a snap is finalizing is not silently dropped and the
new snap IDs are not clobbered. `writeFrameSidecar` itself is now atomic
(temp + rename), matching `frames.Store.write`, so a concurrent reader never
sees a truncated/partial sidecar.

`forkFrame` holds the global `snapQueue.mu` only across the btrfs snapshots
of the captured tmp subvols into the new frame (the part that must observe
the tmp paths before the worker renames them); the rest of frame setup
(empty-subvol creation, symlink, `/id`, sidecar/stamp, `finalizeFrameRootfs`)
runs unlocked so one fork does not stall every other frame's snaps. The
new frame's history is copied by `/fork` itself, so `ts go ::` no longer
calls `/clone-history` afterward (that used to re-read the source's history
— which may or may not yet include the fork-point snap, depending on
indexing timing — and overwrite the new frame's history nondeterministically).

`finishJob` clears *every* `framePendingSnap` entry pointing at the job (a
fork registers the same job for both the source and the new frame), so a
forked frame that never takes a full snap does not leak a map entry.

## Limitations / follow-ups

- `ts log` only shows an entry once indexing completes (no entry at capture
  time). Giving snaps a stable internal name recorded in history at capture
  time, with the content-addressable alias filled in later, is the planned
  follow-up (see the "internal names + alias layer" discussion); it also
  makes fork-during-indexing references robust across daemon restarts.
- If the daemon is killed mid-indexing, captured `.tmp` snaps are left
  orphaned and the in-memory `framePendingSnap` chain is lost. The resulting
  state is *wasteful* (orphaned tmp subvols that need `btrfs subvolume delete`),
  not *corrupt*: the frame stamp/sidecar only ever reference finalized snaps,
  so the next `ts snap` chains against a valid parent (just
  non-incrementally), and a crashed fork leaves a fully-valid new frame.
  Restart recovery (resume/reap orphaned tmp snaps) is part of the
  internal-names follow-up.
- A pre-existing (not introduced here) crash edge in `indexAndFinalizeSubvol`:
  the rename sequence (subvol first, then `.tsm`/`.tsc`) means a crash
  mid-rename can leave a `snaps/<sha>` subvol whose manifest is missing, and
  the dedup `os.Stat` short-circuit would then return that ID with no `.tsm`.
  This existed verbatim in the old synchronous code and is merely relocated.
