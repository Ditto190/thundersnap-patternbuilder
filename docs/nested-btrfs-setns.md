# Nested Thundersnap: btrfs + setns Stale Root Dentry

This document explains a subtle kernel interaction that breaks thundersnap
when running nested (a frame inside a frame, inside a frame, ...) on btrfs.
If you're debugging "executable not found" or "not a directory" errors that
only appear when thundersnap runs inside a thundersnap container, read this
first — it will save you hours.

## The symptom

When thundersnap runs inside a thundersnap container (e.g., developing
thundersnap inside a thundersnap frame), every container session fails with:

```
error: nsenter: stage2: executable not found: /work/.../bin/ts
```

or:

```
error: failed to chroot to /work/.../frame-uuid: not a directory
```

The path is correct — `/work/.../bin/ts` exists on disk. But the kernel
reports it as "not a directory" or "not found" after `setns(CLONE_NEWNS)`.

## The root cause

When `ts nsenter` stage2 joins the container-init's mount namespace via
`setns(CLONE_NEWNS)`, the process's **namespace ID** is correct (verified by
comparing `/proc/self/ns/mnt` before and after). However, the process's
**root dentry** — the in-memory VFS dentry for `"/"` — is NOT updated.
It still points to the root of the OLD mount namespace.

On a normal (non-btrfs) filesystem, this is harmless because the root dentry
is the same inode. But on btrfs, the parent container's `/work` is a **btrfs
subvolume** (a separate inode tree with its own root inode 256). After
`setns`, path resolution starts from the stale root dentry, which doesn't
know about the btrfs subvolume boundary at `/work`. The kernel sees `/work`
as a regular file (inode 16, not a directory) because the stale dentry cache
from the old namespace doesn't have the btrfs subvolume dentry.

This is **not** a bug in thundersnap or in the go-sdk — it's a kernel VFS
behavior: `setns(CLONE_NEWNS)` updates the mount namespace but does not
invalidate the process's root dentry cache.

## The fix

The fix has three parts, all in `cmd/ts/`:

### 1. `container-init`: bind-mount btrfs subvolume ancestors

`bindMountSubvolumeAncestors` in `container_setup.go` walks the path from `/`
to the chroot target and bind-mounts each ancestor that is a btrfs subvolume
root (detected by inode number 256). This creates explicit mount table
entries for subvolume boundaries, which helps the kernel resolve them through
the mount table rather than the stale dentry cache.

### 2. `ts nsenter` stage2: open executable before setns (fexecve pattern)

`openExecutableForExec` in `nsenter.go` opens the executable file BEFORE
`setns(CLONE_NEWNS)`, while the path still resolves correctly. After setns,
it execs via `/proc/self/fd/<fd>` — the kernel's exec path follows the fd to
the inode, not the (stale) path. This is the CGO-free equivalent of
`fexecve(3)`.

### 3. `ts nsenter` stage2 + `drop-caps-and-run`: chroot via /proc/\<pid\>/root

This is the subtle one. `openChrootFd` opens the chroot directory via
`/proc/<container-init-pid>/root` (NOT the direct `--chroot=` path), and
passes the fd to `drop-caps-and-run` as `--chroot-fd=N`. `drop-caps-and-run`
then uses `fchdir(fd)` + `chroot(".")` instead of `chroot(path)`.

**Why `/proc/<pid>/root` and not the direct path?** Because `container-init`
bind-mounts `chrootPath` to itself before chrooting and mounting `/proc`,
`/sys`, `/dev`. Those mounts are on the **bind mount's dentry tree**, not the
original dentry tree. Opening the `--chroot=` path directly gives a fd to the
original dentry tree (where proc/sys/dev don't exist). Opening
`/proc/<initPid>/root` gives a fd to the container-init's root dentry, which
IS the bind mount — so proc/sys/dev are visible after chroot.

The fd is opened BEFORE `setns` (while `/proc` is still accessible from the
outer mount namespace) and passed WITHOUT `O_CLOEXEC` so it survives exec
into `drop-caps-and-run`.

## Debugging tips

If you see "executable not found" or "not a directory" errors that only
appear in nested thundersnap:

1. Check if you're inside a thundersnap container: `cat /proc/self/cgroup`
   should show `thundersnapd.service` or similar, and `/bin/ts` should exist
   (from the parent container).

2. Check if `/work` is a btrfs subvolume: `btrfs subvolume show /work` or
   `stat -c '%i' /work` (inode 256 = subvolume root).

3. The cgroup warnings (`failed to create parent cgroup ...: no such file or
   directory`) are **red herrings** — cgroup setup is best-effort and fails
   harmlessly inside containers with read-only `/sys/fs/cgroup`. Don't waste
   time on them.

4. The `busybox` package on Debian is dynamically linked. The `nil:nil:nil`
   frame rootfs has no `/lib64`, so dynamic binaries fail with
   "no such file or directory" (the ELF interpreter is missing, not the
   binary). Install `busybox-static` instead.

## Why not pivot_root?

`pivot_root` is not needed. The code is the same inside and outside VMs —
`chroot` works fine in both. The only new requirement is the fd-based
exec/chroot for the nested-btrfs case, which falls back to path-based
exec/chroot when the fds can't be opened (the normal non-nested case).
