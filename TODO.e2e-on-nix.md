# Building Thundersnap in the minimal Alpine + Nix frame

## Result

I built the frame with `scripts/nix/build-alpine-minimal.sh`, snapshotted it,
used the root snap in a frame that retained this container's `/home` and
`/work`, and built the repository there.

The successful root snap was:

```
ODHl2LVIvINGv8pNFCsuBdOJR40T7fc5ipiSaQ4WzDs
```

A representative development frame was:

```
019fab05-b35c-709c-b13e-8579f349b956
```

`make build` and `make test` pass. The Nix environment also ran much of the
e2e suite, including MCP, nested Thundersnap, and the container matrix. A full
`make e2e` / `make not_e2e` pass still needs one clean run after the final
environment and BusyBox fixes described below.

## Reproduction

Build and compose the frame:

```sh
nix_frame=$(scripts/nix/build-alpine-minimal.sh)
triad=$(ts go user@"$nix_frame" -c 'ts snap')
nix_root=${triad%%:*}
dev_frame=$(ts frame "$nix_root::")
ts go user@"$dev_frame"
```

The Alpine root needs its native `btrfs` command because the nested e2e test
copies the binary and its ELF dependencies into an empty frame. Copying the
Nix/glibc build into an Alpine/musl root mixes interpreters and libraries:

```sh
sudo apk add btrfs-progs
sudo ln -sfn /sbin/btrfs /usr/bin/btrfs
```

Enter the development shell and run targets:

```sh
cd /work/ts2
nix-shell -p \
  bash go gnumake git gcc busybox util-linux virtiofsd passt procps \
  pkgsStatic.busybox \
  --run 'scripts/nix/dev-shell.sh make build test e2e not_e2e'
```

`scripts/nix/dev-shell.sh` supplies the unavoidable FHS bridges and short temp
path required by the current tests.

## Findings

1. **Compilation is straightforward.** Go 1.26.5 from current nixpkgs builds
   all deb/rpm/tgz targets and all architectures.
2. **Unix socket path length matters.** Nix sets a long
   `/tmp/nix-shell-...` temporary path. A unit-test control socket then exceeds
   Linux's 108-byte `sockaddr_un` limit and fails with `EINVAL`. `TMPDIR=/t`
   and `GOTMPDIR=/t` fix it.
3. **Some tests require fixed FHS paths.** `test-cleanup.sh` uses
   `#!/bin/bash`; VM code opens `/usr/libexec/virtiofsd`; and sudo resets PATH.
   The helper creates links for these and for host tools used after sudo.
4. **The e2e suite really requires static BusyBox.** Nix's normal `busybox`
   is dynamically linked to Nix-store libraries and cannot execute after it is
   copied into `nil:nil:nil`. `pkgsStatic.busybox` is required. Both
   `busybox-static` (the generic test helper's preferred name) and `busybox`
   (the nested test's lookup name) must resolve to it.
5. **Nested e2e needs an Alpine-native btrfs binary.** Its dependency copier
   invokes `ldd`. A Nix/glibc binary seen through Alpine's musl `ldd` produces
   unusable copied dependencies. Installing `btrfs-progs` with `apk` avoids
   that ABI mismatch.
6. **One test assumption was Debian-specific.** The user-namespace assertion
   installed only BusyBox's `unshare` applet and assumed an external `echo`
   existed. Empty frames do not have one, and the Nix static BusyBox exposes
   the issue. The test now installs the `echo` applet explicitly.
7. **Nix channel warning is benign.** Every invocation reports that
   `/home/.nix-defexpr/channels` does not exist, but `<nixpkgs>` resolves from
   the system profile configured by the frame builder.

## Validation observed

- `scripts/nix/build-alpine-minimal.sh`: PASS
- Nix hello/cowsay verification: PASS
- `make build`: PASS
- `make test`: PASS
- e2e MCP tests: PASS after static BusyBox/FHS setup
- e2e nested Thundersnap: progressed through starting the inner daemon and
  creating its first frame after using Alpine-native btrfs
- e2e container matrix: progressed through namespace checks; exposed and fixed
  the missing BusyBox `echo` applet
- Full Nix `make e2e`: not yet recorded as PASS after the final fix
- Full Nix `make not_e2e`: not yet run
- Required host sequence `make test`, `make e2e`, `make not_e2e`: PASS
