# Building Nix frames

The scripts in `scripts/nix/` build frames with a working single-user Nix
installation. They do not create, replace, or delete refs; each successful run
prints one new frame UUID on stdout, so builds are nondestructive and can run in
parallel.

```bash
DEBIAN_FRAME=$(./scripts/nix/build-debian.sh)
ALPINE_FRAME=$(./scripts/nix/build-alpine.sh)

ts go "$DEBIAN_FRAME"
ts go "$ALPINE_FRAME"
```

Both builders start from a clean `<docker-snap>:nil:nil` frame, leaving `/home`
and `/work` empty. Nix is installed as `user` with a temporary home under
`/var/lib`, then exposed to login shells through `/etc/profile.d/nix.sh`. The
system profile and nixpkgs channel are independent of `/home`, while an end
user's Nix profile, channels, and configuration in `/home` take precedence.

The implementation is split into:

- `build-debian.sh` / `build-alpine.sh`: create a frame and install the base
  packages needed by the upstream Nix installer.
- `build-alpine-minimal.sh`: build Alpine, copy the resulting frame, and
  minimize the copy in one step.
- `build.sh`: install, configure, and verify Nix in that frame.
- `profile.sh`: login-shell activation installed into the frame.
- `minimize-alpine.sh`: remove installer-only Alpine packages from a completed
  frame.

Build the experimental minimal Alpine variant in one step:

```bash
MINIMAL_FRAME=$(./scripts/nix/build-alpine-minimal.sh)
ts go "$MINIMAL_FRAME"
```

Or copy and minimize an existing Alpine Nix frame without modifying it:

```bash
MINIMAL_FRAME=$(./scripts/nix/minimize-alpine.sh "$ALPINE_FRAME")
```

The minimizer currently removes `curl`, `xz`, and GNU `coreutils`. It keeps
`sudo` for normal frame operation and `ca-certificates` for TLS. The
result remains Alpine-based and retains BusyBox, `ash`, and `apk`; it is not a
from-scratch Nix root.

---

## What you're working with

- You're already inside a thundersnap instance. The frame's `/id` directory
  is owned by the default `user` account (uid 7575), so ordinary `ts ...`
  commands can reach the control socket without `sudo`.
- Frames disable sudo's system-audit logging because frame processes do not
  have a host-audit connection (and intentionally do not retain
  `CAP_AUDIT_WRITE`). Sudo's normal syslog and I/O logging remain enabled.
- `ts go <frame-or-ref> -c 'cmd'` runs a command inside a frame and exits. **Its
  stdout is clean** (no PTY escape noise) — verified with `cat -A` — so you
  can capture output from it. This is how you run things inside a frame
  without dropping into an interactive session. Always pass `-c` (or wrap
  in a timeout); a bare `ts go` opens an interactive session and will
  hang an automated agent.
- `ts go ::` is the fast "fork current frame" path: it captures the current
  frame as a background snap and clones a new frame from the live
  filesystem without waiting for content-addressable indexing. Use it to
  get a scratch frame to mutate — but note it inherits your live `/home`
  and `/work`, which is why the quick start above uses `<snap>:nil:nil`
  instead for a clean, reproducible construction frame.

## The model you need to internalize (read this part)

A thundersnap **frame** is an execution environment made of three btrfs
subvolumes: root (`/`), home (`/home`), work (`/work`). A **snap** is the
content-addressed triplet of all three: `rootID:homeID:workID`. A **ref**
is a named pointer to a frame UUID.

The builders create a reusable environment by making a clean frame from a
Docker snap and mutating it in place. The resulting frame UUID can be passed
directly to `ts go`, or snapshotted and named later if desired:

1. Create a frame with empty home/work: `ts frame <snap>:nil:nil`.
2. Install prerequisites as root via `ts go root@<frame> -c`.
3. Run the Nix installer as `user` via `ts go user@<frame> -c`.
4. Return the frame UUID.

`<snap>:nil:nil` means: use `<snap>` for root, and **empty** (the literal
token `nil`) for home and work — thundersnap creates fresh empty
subvolumes for those. Empty components in a triplet can also be left blank
(`ts frame <snap>::`), which *inherits* them from the current frame; `nil`
is the explicit-empty form that doesn't depend on what frame you're in.
Prefer `nil:nil` for anything you want reproducible.

## Detailed walkthrough (Debian) — the whys and dead ends

This is the original verified run (on the `deb` ref, a Debian forky/sid
system). The quick start above is the condensed, cleaned-up version; this
section records the reasoning and the footguns that caused round trips.

**Note:** this walkthrough predates the pristine-`/home` approach (throwaway
`HOME`, profile relocation to `/nix/var/nix/profiles`, `/etc/profile.d/nix.sh`
activation). It describes installing nix straight into `/home` and editing
`~/.profile` / `~/.bash_profile` by hand — which works but bakes nix
dotfiles into `/home`, so the resulting snap can't be cleanly overlaid with
an end user's home. The current quick start and both
`scripts/nix/build-*.sh` scripts use
the pristine-`/home` approach instead (see "Keeping `/home` pristine").
The dead-ends and footguns recorded here still apply to a hand-driven
install.

### 1. Get a frame to mutate

The first time, we forked the current frame:

```bash
FORK=$(ts frame ::)   # prints a new frame UUID
echo "$FORK"
```

Don't install nix directly into `deb` or whatever your current frame is —
fork (or, better, build a clean `<snap>:nil:nil` frame) first. The
quick-start's `:nil:nil` approach is preferred: it doesn't drag your 30G
`/work` into the construction frame, so the final `nix` snap is small and
identical for anyone who follows the recipe.

### 2. Install single-user Nix inside the frame

```bash
ts go "$FORK" -c '
  set -e
  curl -sSL https://nixos.org/nix/install -o /tmp/nix-install.sh
  sh /tmp/nix-install.sh --no-daemon --yes
'
```

Notes on this step:

- **Use `https://nixos.org/nix/install`, not `https://nix.org/install`.**
  In this environment `nix.org` did not resolve in DNS at all (`nixos.org`
  did). The `nixos.org` URL 302-redirects to
  `releases.nixos.org/nix/nix-<ver>/install`, which is what you want.
- **`--no-daemon` is essential.** Frames have no systemd / no init, so the
  multi-user (daemon) install won't work. Single-user nix needs no daemon:
  it just chowns `/nix` to the installing user and runs the nix binaries
  directly. This fits containers perfectly.
- **`--yes` skips the interactive confirmation.**
- The installer uses `sudo` internally to `mkdir -m 0755 /nix && chown
  user /nix`. Passwordless sudo works inside the frame (the `ts go`
  session enters as `user`, who is in the `sudo` group), so this just
  works — *provided sudo is installed and the NOPASSWD drop-in exists*
  (see the bootstrap step; on a stock docker image neither is present).
- The installer is *supposed* to append a `. /home/.nix-profile/etc/profile.d/nix.sh`
  line to your shell profile, but whether it actually does is image-dependent:
  on the forky/sid `deb` ref it wrote `~/.bash_profile`; on `debian:latest`
  (trixie) it did NOT auto-write anything and just printed a manual
  instruction. So write the line yourself. Which file? `ts go -c` runs
  `su - user -c`, i.e. `user`'s *login* shell. bash (the shell on trixie —
  `getent passwd user` → `/bin/bash`) sources `~/.bash_profile` on login
  and skips `~/.profile` when `~/.bash_profile` exists; `/bin/sh` (the
  shell on the older `deb` ref and on alpine) sources `~/.profile`. The
  quick start writes BOTH `~/.bash_profile` and `~/.profile` so it works
  regardless of shell. Check `getent passwd user` if unsure (see alpine
  dead-end #3 and footgun #7).
- **Fresh `debian:latest` (trixie) wedges `ts go -c` unless you clear
  `user`'s password expiry first.** The image ships `user` with "password
  must be changed" enforced; `su - user` (what `ts go -c` runs) then
  prints "You are required to change your password immediately" and
  prompts on a non-existent TTY, hanging the non-interactive session
  forever. Symptom: `ts go nixbase -c '...'` never returns, and `ts
  frames` shows a stale session count for the frame with no live
  `session-serve` to reap. Fix it in the bootstrap step (as root via
  `ts go root@<frame> -c`): `chage -d "$(date +%F)" -m 0 -M 99999 -I -1
  -E -1 user && passwd -d user` (the quick start's step 2 does this). The install itself is
  fast (~10s); if `ts go -c '<install>'` appears to hang, it's this, not
  a slow download. See footgun #18.

### 3. Snapshot the frame

```bash
ts go "$FORK" -c 'ts snap' | tee /tmp/nix.triplet
```

This prints the triplet, e.g.:
```
TH-doXTy...:Uu982kXp...:OVt2g6gr...
```

**Footguns on this step:**

- **The default waits and prints the triplet.** `-w` remains accepted for
  backward compatibility. Use `-q` only when you explicitly want a quick,
  silent capture whose content-addressable ID will be available later.
  (Progress lines may appear on stderr; stdout contains only the triplet.)
- **Don't be alarmed by `du -sh /work` showing ~30G.** The snap is
  *incremental*: it indexes each subvolume against its parent stamp, so a
  fork whose `/work` is byte-identical to its parent's dedupes almost
  entirely. The whole three-component snap here took ~17 seconds despite
  `/work` being 32G. The only subvolume that grew meaningfully was root
  (nix added ~557M under `/nix`). With the `:nil:nil` construction there's
  no inherited `/work` to worry about at all.

### 4. Create the nix frame + ref from the triplet

```bash
TRIPLET=$(cat /tmp/nix.triplet)
NIXUUID=$(ts frame --ref nix "$TRIPLET")
echo "nix -> $NIXUUID"
ts refs   # should now list nix
```

**Footgun on this step:**

- **`--ref` must come BEFORE the triplet.** The CLI uses getopt, which
  stops parsing flags at the first positional argument. So
  `ts frame <triplet> --ref nix` silently fails (prints usage, exits 1,
  creates nothing). It must be `ts frame --ref nix <triplet>`. This one
  cost a round trip.

### 5. Verify it works end to end

```bash
ts go nix -c '
  echo "user=$(whoami) uid=$(id -u) HOME=$HOME"
  command -v nix && nix --version
  nix-build -E "(import <nixpkgs> {}).hello" && ./result/bin/hello
  nix-shell -p cowsay --run "cowsay works"
'
```

All of these should succeed and pull from `cache.nixos.org`. If
`command -v nix` is empty, the profile wasn't sourced — check that
`/home/.profile` contains the `. /home/.nix-profile/etc/profile.d/nix.sh`
line (the quick start adds it explicitly).

## Alpine: the extra dead ends

Alpine is musl-based and ships busybox coreutils by default, so the same
installer hits four problems the Debian flow doesn't. The single-user nix
installer itself works fine on alpine once these are cleared — nix
binaries are glibc-linked but carry their own glibc in their closure
(absolute ELF interpreter paths into `/nix/store/...-glibc.../lib/ld-linux...`),
so they run on musl systems without any system glibc. (Background:
NixOS/nix#6751 — "the normal nix installer (single user) works on Alpine
Linux also.")

1. **No `sudo`, so `user` can't elevate.** Stock alpine has no `sudo`.
   Without a `<user>@` prefix, `ts go -c` runs as `user` (the `/enter`
   protocol sends an empty username, so the daemon auto-detects `user`).
   Thundersnap's `EnsureSudoers` would write the NOPASSWD drop-in for
   `user`, but it skips itself when `/etc/sudoers.d` doesn't exist —
   which it doesn't, because sudo isn't installed. **Fix:** get a root
   session via `ts go root@<frame> -c` (or `ssh root@<ref>@thundersnap`
   on an older daemon — SSH sends an explicit username the daemon
   honours), `apk add sudo curl xz bash ca-certificates coreutils`, then
   write `/etc/sudoers.d/thundersnap-user` with
   `Defaults:user !log_allowed, !log_denied` and `user ALL=(ALL)
   NOPASSWD: ALL` (mode 0440) by hand. The first line avoids warnings from
   sudo's unavailable host-audit connection; it does not disable sudo's
   normal syslog or I/O logging. After this, `user` has passwordless sudo
   and the nix installer's `mkdir /nix && chown user /nix` works.

2. **busybox `cp` aborts the installer.** The nix installer runs
   `cp --preserve=ownership,timestamps` when copying Nix into
   `/nix/store`. Busybox `cp` doesn't understand `--preserve=` and dies
   with `cp: unrecognized option: preserve=ownership,timestamps` partway
   through "copying Nix to /nix/store". **Fix:** `apk add coreutils` (GNU
   cp 9.x) *before* running the installer, then `rm -rf /nix` to clear the
   partial copy and re-run the installer. GNU cp is at `/bin/cp` and
   takes precedence in the default PATH.

3. **The installer doesn't always auto-write the shell profile.** On the
   forky/sid `deb` ref the installer appended its source line to
   `~/.bash_profile`; on alpine (no pre-existing profile, `user` shell
   `/bin/sh`) and on `debian:latest` (trixie) the auto-edit didn't fire at
   all, so nix wasn't on PATH in fresh `ts go -c` sessions. Which file you
   must write depends on `user`'s login shell (`getent passwd user`):
   `/bin/sh` sources `~/.profile`; `/bin/bash` (trixie) sources
   `~/.bash_profile` and skips `~/.profile` when `~/.bash_profile` exists.
   **Fix:** write `. /home/.nix-profile/etc/profile.d/nix.sh` into BOTH
   `~/.profile` and `~/.bash_profile` (the quick start does this), so it
   works regardless of shell. Verified: a fresh `ts go alpine -c` / `ts go
   nix -c` then has `nix` on PATH with no manual sourcing.

4. **Base image.** Use `ts download-docker alpine:latest` to get the snap,
   then `ts frame --ref nixbase <snap>:nil:nil` for a clean construction
   frame (empty `/home`, `/work`). Don't `ts frame ::`-fork from a Debian
   frame — you'd get a Debian `/home` and `/work` in your "alpine" frame.

## Keeping `/home` pristine (and why we don't bootstrap as root)

A Nix frame can be snapshotted and composed with an end user's own `/home`
subvolume: `ts frame <nix-root-snap>:<their-home-snap>:nil`. For that to
work, the Nix root must not have baked any nix-specific state
into `/home` (or `/root`) — otherwise the user's home overlay would either
clobber nix's activation or inherit a pile of bootstrap dotfiles they didn't
ask for. Three things make this work:

1. **The nix installer runs as `user` with `HOME=/var/lib/nix/home`, not
   `/home`.** The single-user installer writes its profile, channels,
   `~/.nix-defexpr`, and `~/.cache` under `$HOME`; pointing `HOME` at a
   throwaway directory in the root subvol keeps all of that out of `/home`
   (and `/root`). The throwaway home is pre-created and `chown`'d to `user`
   in the root bootstrap step, then deleted once the profile is relocated.

2. **The profile and channels are relocated to the conventional
   multi-user locations under `/nix/var/nix/profiles/`:**
   - `default` → the nix binaries profile (`/nix/var/nix/profiles/default`,
     the same path multi-user nix uses for the system profile),
   - `per-user/root/channels` → the `nixpkgs` channel
     (`/nix/var/nix/profiles/per-user/root/channels`, where multi-user nix
     puts root's channels).
   These are symlinks into `/nix/store`, so they're independent of any
   user's home. The relocation is just `cp -d` of the generation links the
   installer created under the throwaway home, plus `ln -sfn` to name them.

3. **Nix is activated system-wide via `/etc/profile.d/nix.sh`,** sourced
   by `/etc/profile` for every login shell (both debian/trixie and alpine
   source `/etc/profile.d/*.sh`). The script puts
   `/nix/var/nix/profiles/default/bin` on `PATH`, sets `NIX_PROFILES`, and
   points `NIX_PATH` at `$HOME/.nix-defexpr/channels` (the user's own
   channels, once they create them) **and**
   `/nix/var/nix/profiles/per-user/root/channels` (the system `nixpkgs`
   channel) as a fallback — nix silently skips missing `NIX_PATH` entries,
   so `<nixpkgs>` resolves via the system channel before the user sets up
   their own, and via the user's own once they do. No per-user dotfiles are
   required for nix to work.

**Why not just bootstrap nix as root?** It doesn't work: the upstream
single-user installer refuses root outright (`warning: installing Nix as
root is not supported by this script!`), then fails because single-user nix
tries to use the `nixbld` build-users group (`error: the group 'nixbld'
specified in 'build-users-group' does not exist`), which the single-user
installer never creates. (Multi-user nix creates `nixbld` users, but that
needs a running nix-daemon, and frames have no init/systemd — see footgun
#9.) Running the installer as `user` with a throwaway `HOME` sidesteps
both problems and keeps `/home` clean for free.

**The end-user overlay, verified.** Build a frame from the nix root snap
plus a custom home snap (e.g. one with the user's `.bashrc`, `.profile`,
`.nix-channels`):

```bash
# nix-root is the root component of the nix ref's snap triplet:
#   ts refs        # nix -> <uuid>
#   ts snaps | grep <uuid>   # or read the triplet off the ref's frame
NIXROOT=$(ts snaps | awk '/<nix-snap-id>/{print $1}')   # root snap id
MYHOME=$(ts snaps | awk '/<my-home-snap-id>/{print $1}') # home snap id
ts frame "${NIXROOT}:${MYHOME}:nil"
```

Verified: a frame built from the nix root snap + a custom home (containing
a `.bashrc` with `export GREETING=...`, a `.profile` with
`export MY_SETTING=42`, and a custom `.nix-channels`) keeps all of those
home files, sources `.profile` on login (so `MY_SETTING=42` is exported in
the `ts go -c` login shell), **and** nix still works (`nix --version` →
2.35.1, `nix-build -E '(import <nixpkgs> {}).hello'` → "Hello, world!",
`<nixpkgs>` resolves via the system channel). The nix activation does not
depend on anything in `/home`.

A note on the benign warning: a fresh Nix command may print
`warning: Nix search path entry '/home/.nix-defexpr/channels' does not
exist, ignoring`. That's nix telling you where per-user channels *would*
go; `<nixpkgs>` still resolves via the system channel fallback in
`NIX_PATH`. On interactive login, `/etc/profile.d/nix.sh` explains that an
empty `~/.nix-defexpr/channels` directory silences the warning. Running
`nix-channel --update` also creates the path and installs a real per-user
channel.

## Cleaning up

- **Don't obsess about deleting frames; they're cheap.** Leave the
  intermediate `nixbase` (and any fork) frames around. Their subvolumes
  stay on disk but that's fine.
- If you made a temporary ref to point at a construction frame, delete
  just the ref:
  ```bash
  ts ref delete nixbase
  ```
- **Ref names must start with an alphanumeric.** A leading underscore or
  dash is rejected (`invalid ref name: must start with alphanumeric,
  contain only alphanumeric/dash/underscore/dot`). `_nixbase` is
  invalid; `nixbase` is fine.

## Footguns reference (the stuff that wasted time)

1. **`ts` subcommands don't understand `--help`.** `ts download-snap
   --help` prints `unknown option: -` and the usage. To learn a
   subcommand's options, run it with no args / bad args and read the
   usage it prints, or read `cmd/ts/main.go`.
2. **`ts` commands do not need `sudo`.** `/id` is private to the default
   frame user, but that user owns it and can reach `/id/thunder.sock`.
3. **`ts go` sessions are unprivileged.** They log in as `user` (uid
   7575), `HOME=/home`, `shell` = `/bin/sh` (older `deb` ref, alpine) or
   `/bin/bash` (`debian:latest`/trixie) — not root. This is why
   single-user nix (owned by `user`) is the right install mode. Use an
   explicit `root@` prefix only for commands that actually need root.
4. **`ts go` takes an optional `<user>@` prefix to run as a specific
   user.** `ts go root@<ref> -c '...'` runs the session as root; without
   a prefix it auto-detects `user` (uid 7575). This is now the
   recommended way to bootstrap a fresh frame as root (install sudo,
   write the NOPASSWD drop-in, clear `user`'s password expiry). The older
   `ssh root@<ref>@thundersnap` path (SSH sends an explicit username the
   daemon honours) still works but is no longer needed. `ts autorun` enters
   like a `user@` session (including `HOME=/home`) and restarts on exit with
   backoff, so it's worse for one-shot bootstrap commands.
5. **`ts go -c` stdout is clean** (no PTY escape codes). You can capture
   snap IDs and command output from it directly.
6. **A bare `ts go` opens an interactive session and hangs.** Always
   pass `-c 'cmd'` (and a timeout) in automated contexts.
7. **Which profile `ts go -c` sources depends on `user`'s login shell.**
   `ts go -c` runs `su - user -c`, i.e. `user`'s login shell. `/bin/sh`
   (older `deb` ref, alpine) sources `~/.profile`; `/bin/bash`
   (`debian:latest`/trixie) sources `~/.bash_profile` and skips
   `~/.profile` when `~/.bash_profile` exists. **This is no longer
   relevant for the `nix` ref** — the current build scripts activate
   nix via `/etc/profile.d/nix.sh` (sourced by `/etc/profile` for every
   login shell, regardless of shell), so no per-user profile editing is
   needed. The footgun is recorded here for anyone bootstrapping a frame
   by hand without that script: check `getent passwd user` and put the
   nix `source` line in the right file — or both.
8. **`nix.org` doesn't resolve here; `nixos.org` does.** Use
   `https://nixos.org/nix/install`.
9. **`--no-daemon`** — frames have no init/systemd, so multi-user nix is
   out. Single-user nix needs no daemon and chowns `/nix` to `user`.
10. **getopt stops at the first positional.** Put `--ref` (and any other
    flags) before the triplet in `ts frame`.
11. **`ts snap` waits and prints its ID by default.** `-w` is a compatible
    explicit spelling. `-q` returns after capture, prints nothing on success,
    and leaves indexing to finish in the background.
12. **Big `/work` is fine.** Snaps are incremental against the parent
    stamp; an unchanged 32G `/work` dedupes to nearly nothing. With the
    `:nil:nil` construction there's no inherited `/work` to worry about.
13. **You can't `curl --unix-socket /id/thunder.sock` directly.** The
    socket speaks a custom handshake, not plain HTTP, so raw curl gets
    `ERROR invalid handshake`. Always go through the `ts` client.
14. **`ts frame --delete <uuid>` (`-d`) works.** The server's
    `DeleteFrameRequest` (`cmd/thundersnapd/main.go`) accepts either
    `uuid` or `frame_name`, and the client sends `{"uuid":...}`, so
    `ts frame -d <uuid>` deletes the frame and prints
    `Deleted frame <uuid>`. (Earlier runs hit `error: frame_name is
    required` because the *running* daemon was a stale PID 1 started
    before that fix landed — there's no in-place daemon restart, so the
    fix only took effect when the outer container was recreated. If you
    ever see `frame_name is required` again from a *current* binary, the
    mismatch bug is back.) To drop just a named pointer regardless of
    daemon age, use `ts ref delete <name>` (that has always worked).
15. **Alpine: busybox `cp` breaks the nix installer.** `apk add
    coreutils` (GNU cp) before running the installer; `rm -rf /nix` and
    re-run if it already aborted mid-copy.
16. **Alpine: no `sudo` and `user` can't elevate.** Bootstrap as root
    via `ts go root@<frame> -c` (or `ssh root@<ref>@thundersnap` on an
    older daemon without the `<user>@` prefix), install `sudo`, and write
    the NOPASSWD drop-in by hand (`EnsureSudoers` skips itself when
    `/etc/sudoers.d` is absent).
17. **The installer doesn't always auto-write the shell profile.**
    **Moot for the `nix` ref:** the build scripts don't rely on the
    installer's profile auto-edit at all — it installs a system-wide
    `/etc/profile.d/nix.sh` instead (see "Keeping `/home` pristine"). The
    historical advice, for anyone bootstrapping by hand without that
    script: write `. /home/.nix-profile/etc/profile.d/nix.sh` into both
    `~/.profile` and `~/.bash_profile` yourself (which one `ts go -c`
    sources depends on `user`'s login shell — see #7), or nix won't be on
    PATH in `ts go -c` sessions.
18. **Fresh `debian:latest` (trixie) wedges `ts go -c` via `user` password
    expiry.** The image ships `user` with "password must be changed"
    enforced; `su - user` (what `ts go -c` runs) prompts "You are required
    to change your password immediately" and hangs the non-interactive
    session forever. Clear it in the bootstrap step (as root via `ts go root@<frame> -c`):
    `chage -d "$(date +%F)" -m 0 -M 99999 -I -1 -E -1 user && passwd -d user`.
    The install itself is fast (~10s); a hung `ts go -c '<install>'` is
    this, not a slow download. The pre-existing `deb` ref is fine (its
    `user` was set up in an earlier run); a fresh `alpine:latest` frame
    was not observed to have this issue (busybox loginutils doesn't enforce
    shadow ageing like trixie's PAM), but if `ts go -c` hangs on any fresh
    frame, check `chage -l user` / `cat /etc/shadow` for `user` first.
19. **Don't timeout-kill a `ts go -c` mid-session — it wedges the frame.**
    If an agent wrapper (or `timeout`) kills a `ts go <ref> -c '...'` that
    is still running, the daemon can be left with a stale session count for
    that frame (`ts frames` shows a number instead of `stopped`) and no
    live `session-serve` process to reap it. The frame then refuses new
    `ts go -c` sessions (they hang on attach), and there's no CLI to clear
    the stale session (`ts frame --delete` reclaims it — see #14). Recovery:
    abandon the frame and rebuild from the snap (`ts frame --ref <new>
    <snap>:nil:nil`). Easy to hit in combination with #18, because the
    natural response to a password-expiry hang is to kill the `ts go -c` —
    which then compounds the problem.
20. **`ts download-docker` keeps stdout machine-readable.** It always prints
    only the snap ID on stdout. Fresh-download progress and the final size/rate
    go to stderr; a cached result prints a cache notice to stderr instead.
