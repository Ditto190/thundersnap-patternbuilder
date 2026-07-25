# TODO — tracked fixes and improvements

Items here were surfaced by review (notably the gpt-5.5 + deepseek-v4-pro
cross-family review of the aggregated changes since `main@origin`) and
deliberately deferred. Cross them off as they land.

## Crash recovery / ops

- [ ] **Startup cleanup of orphaned `.tmp` snap artifacts.** If `thundersnapd`
  crashes during background indexing, the captured btrfs subvolume
  (`snaps/<jobid>-<kind>.tmp`) and its `.tmp.stamp` / `.tmp.tsm` / `.tmp.tsc`
  sidecars are left on disk. `main.go` skips `.tmp` entries when listing snaps,
  so they don't appear in `ts snaps` — but they occupy disk and are never
  reclaimed except by hand. `snapqueue.go` has `cleanupTmp` for error paths and
  `processJob` cleans un-indexed components on error, but there is no startup
  recovery pass. Fix: in `initSnapQueue` (or `main` after `--data-dir` is
  resolved) scan `snaps/` for `*.tmp` artifacts and remove them — `.tmp`
  subvols are by definition incomplete/un-indexed, so they are always safe to
  delete. (deepseek-v4-pro review, MEDIUM.)

## Test coverage gaps (real-e2e)

- [ ] **Mesh `download-snap` between two daemons has no real-e2e test.** The
      deleted `TestE2EDownloadSnap` was the only two-daemon download test; the
      current suite covers it only at the unit level (`tsm/download_test.go`
      with httptest peers) and via `not_e2e/mesh_test.go` fakes. `tsm/download.go`
      + the new ChunkIndex dedup changed substantially. Add the W6 workflow from
      `not_e2e/not-e2e-enough.md`: start daemons A+B, snap on A via SSH,
      download-snap on B through the production handler/CLI, fork a frame from
      the downloaded snap, verify content over SSH. (Both reviews, HIGH/MEDIUM.)
- [ ] **`/home` and `/work` ownership not asserted by any real-e2e test.** The
      deleted `TestE2EFrameHomeWorkOwnership` checked uid/gid 1000 and
      subvolume-ness; the uid since moved to `tsm.ThundersnapUID` (7575), also
      unverified. Add an ownership check over SSH: `stat -c %u:%g /home` and
      `/work` after `ts frame`, assert `ThundersnapUID:ThundersnapGID`.
      (Both reviews, MEDIUM.)
- [ ] **Session PTY visibility in container not asserted.** The deleted
      `TestE2EPtyVisibleInContainer` verified the session's PTY slave appears in
      the container's `/dev/pts`; current tests only check `/dev/pts` exists.
      Add an `sshInteractive` test that runs `tty` and asserts the path exists
      (`test -c $(tty)`). (deepseek-v4-pro review, MEDIUM.)
- [ ] **`TestForkUndoRollsBackToForkPoint` reproduces `ts undo`'s effect by
      parsing `ts log` instead of driving `ts undo -c`.** `TestTsUndo` shows the
      `-c` infrastructure works; this test could run
      `ts undo -c 'read line < /marker && echo $line'` on the forked frame and
      assert the marker is `v2` — a stronger test of the actual fork-point fix.
      (deepseek-v4-pro review, LOW.)

## SFTP / scp

- [ ] **`sftpfs` Setstat ignores mtime/atime.** `Filecmd`'s `Setstat` handles
      only `Size` and `Permissions`, so SFTP `Chtimes` (and `scp -p`, which
      preserves mtimes) is a silent no-op. Honor `AttrFlags().Acmodtime` with
      `os.Chtimes`. Small, correct, standard-SFTP behavior. (Surfaced while
      writing `TestCrossInstanceSnapDeterminism`, which pins mtimes via busybox
      `touch` as a workaround.)
- [ ] **`parseSnapProgress` hardcodes 3 components** (root/home/work) and may
      mis-handle `nil:nil:nil` frames whose `ts snap` progress only has a root
      component. Make it handle 1–3 components. (deepseek-v4-pro review, NIT.)
