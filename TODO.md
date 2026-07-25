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
      with httptest peers). The `not_e2e/mesh_test.go` and `streaming_test.go`
      fakes were deleted (they reimplemented the protocol and tested the test's
      own copy, not the real handlers — false green). `tsm/download.go` + the
      ChunkIndex dedup changed substantially. Add the W6 workflow from
      `not_e2e/not-e2e-enough.md`: start daemons A+B, snap on A via SSH,
      download-snap on B through the production handler/CLI, fork a frame from
      the downloaded snap, verify content over SSH. This needs a mesh
      peer-config seam in `--test-listen` mode (the test-mode daemon has no
      tsnet peers) — a small product change. (Both reviews, HIGH/MEDIUM.)
- [ ] **`/home` and `/work` ownership not asserted by any real-e2e test.**
      `e2e/fidelity_test.go` now asserts a chown'd file's uid/gid survive a
      snap+fork, but does not assert the daemon-created `/home` and `/work`
      subvolumes are owned by `tsm.ThundersnapUID` (7575). Add a `stat -c
      %u:%g /home` and `/work` check after `ts frame`. (Both reviews, MEDIUM.)
- [x] **Session PTY visibility in container not asserted.** Covered by
      `e2e/container_test.go` `TestContainerConcurrentDistinctPTS`, which opens
      two concurrent PTY SSH sessions, runs `tty` in each, and asserts they get
      distinct `/dev/pts<N>` devices. (deepseek-v4-pro review, MEDIUM.)
- [ ] **`TestForkUndoRollsBackToForkPoint` reproduces `ts undo`'s effect by
      parsing `ts log` instead of driving `ts undo -c`.** `TestTsUndo` shows the
      `-c` infrastructure works; this test could run
      `ts undo -c 'read line < /marker && echo $line'` on the forked frame and
      assert the marker is `v2` — a stronger test of the actual fork-point fix.
      (deepseek-v4-pro review, LOW.)

## not_e2e teardown (remaining)

The not_e2e suite has been largely emptied into the real e2e harness (see
`not_e2e/not-e2e-enough.md` for the plan). The remaining not_e2e files are all
VM tests that hand-spawn cloud-hypervisor and need a working KVM environment
to port onto the daemon-driven `vm/` SSH harness:

- [ ] **Port the deep VM tests to e2e.** `vm_test.go` (launch, networking,
      virtiofs, vshd comm, concurrent sessions, graceful shutdown, panic
      recovery, insufficient memory, user switching, process isolation),
      `vmx_test.go` (basic/concurrent/container-isolation/outer-shell/shared-
      VM), `minimal_shell_test.go` (shell features), `vshd_devpts_test.go`
      (devpts), and the VM helpers in `e2e_test.go`/`vshd_proto_test.go`. The
      daemon-driven VM SSH path is already covered by `e2e/vm_test.go`
      (TestVMSSHSessionMatrix, TestVMXPtyWinsize) and `TestVMNamespaceSetup`;
      the deep tests should go through `vm/` sessions against a real
      `thundersnapd` instead of hand-spawning cloud-hypervisor. Per
      not-e2e-enough.md W5, keep panic-recovery / insufficient-memory as
      targeted negatives that still go through the daemon. Requires a host
      with /dev/kvm passthrough to write and verify.

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
