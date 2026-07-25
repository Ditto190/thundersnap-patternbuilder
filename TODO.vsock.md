# TODO: vsock / thunder.sock architecture cleanup

## Desired state

The intended architecture for how a `ts` client inside a container reaches the
thundersnapd control server:

1. **Inside a container:** `ts` always talks to a local Unix socket
   (`/id/thunder.sock`). **No vsock ever appears inside a container.** The
   container's mount namespace has no `/dev/vsock`, and `ts` never branches on
   its presence.

2. **Outside the container** (the layer that serves that `thunder.sock` back to
   the host) is one of two things:
   - On the **root system** (bare host): the real thundersnapd, which serves
     `thunder.sock` directly. (This is the only place the full control server
     lives.)
   - **Inside a VM:** a relay process that listens on a local `thunder.sock`
     inside the VM and proxies each connection over `/dev/vsock` back to the
     thundersnapd that launched the VM.

So from `ts`'s point of view there is exactly one transport: the local Unix
socket. The VM-vs-host distinction is handled entirely by whatever is serving
that socket on the other side. `ts` should never `vsock.Dial(...)` directly.

The `/dev/vsock` device exists only in the VM's outer namespace (so the relay
can reach the host), never inside a container.

## Current state (analysis)

The code does not match the desired state. There are **two** `inVM()` branches
(one I found, one DeepSeek's review found), and the VM control path is partially
broken and partially live-but-incomplete. References are to the tree as of the
"split --skip-mount-setup" commit.

### Two `inVM()` branches, different statuses

**A. `dialEnter()` vsock branch — `cmd/ts/main.go:2105-2118`**
- `inVM()` (`cmd/ts/main.go:2100`) returns true iff `/dev/vsock` exists.
- `dialEnter()` (`cmd/ts/main.go:2109`) branches: if `inVM()`, dials
  `vsock.Dial(hostCID, 5224)` (`thunderproto.EnterPort`); else dials the unix
  socket `*sockPath` with a CONNECT-5224 handshake.
- Callers: `runVsockSession` (`cmd/ts/main.go:2138`), which is called only by
  `cmdGo` (`cmd/ts/main.go:2088`) and `cmdUndo` (`cmd/ts/main.go:2447`) — i.e.
  `ts go` and `ts undo`, the session-enter commands.
- **Status: DEAD + BROKEN.** `thundersnap/vm.go` creates a vsock listener only
  for port 5223 (`VsockPort`, `vm.go:46/338-347`). **No code ever creates a
  listener for port 5224.** So if the branch is taken, the dial fails. It is
  only taken in the outer-VM shell namespace (where `/dev/vsock` exists), and
  even there `handleEnter` on the host (`cmd/thundersnapd/main.go:1747`) only
  knows how to enter **host** containers (it dials the host vshd unix socket at
  `main.go:1824`), not VMX containers — so `ts go` from an outer-VM shell into a
  VMX frame has no working path regardless of transport.
- **Deletable today; nothing live breaks.**

**B. `thunderclient.Dial()` vsock branch — `thunderclient/thunderclient.go:32-56`**
- A *duplicate* `inVM()` at `thunderclient.go:32` (I missed this one; DeepSeek
  found it).
- `Dial()` (`thunderclient.go:41`) branches: if `inVM()`, dials
  `vsock.Dial(hostCID, 5223)` (`thunderproto.Port`); else dials the unix socket
  with a CONNECT-5223 handshake.
- Callers: **all** control commands — `ts snap`, `ts frame`, `ts ping`, `ts ref`,
  `ts log`, `ts autorun`, etc. (~25 call sites in `cmd/ts/main.go` all go through
  `thunderclient.NewHTTPClient(*sockPath)` / `PostJSON`).
- **Status: LIVE for outer-VM shells, but only 3 endpoints work.** When `inVM()`
  is true (outer-VM shell, reachable via `vmx/<isolation>` SSH with no frame name
  → `runVMXOuterShell` → vshd `rootPrefix == ""` branch → VM init namespace where
  `/dev/vsock` exists), `ts ping` / `ts snap` / `ts fork` work over vsock 5223 →
  vm.go's `handleVsockConnection` → `makeVMXControlHandler`.
- **But `makeVMXControlHandler` (`cmd/thundersnapd/session.go:381-387`) only
  registers 3 endpoints** (`/ping`, `/snap`, `/fork`) vs ~25 on the host control
  server (`cmd/thundersnapd/main.go:1631-1655`). So `ts frame`, `ts ref`,
  `ts log`, `ts autorun`, `ts taint`, etc. all **404** even through the working
  vsock path.
- **Not deletable today** without a relay that proxies port 5223 to the host's
  full control server.

### Control commands inside VMX containers — completely broken, untested

This is the normal user case: SSH to `vmx/user@frame`, get a shell inside a VMX
container, run `ts snap`. It is **doubly broken**:

1. **No `/dev/vsock` in containers.** Containers are entered via `ts nsenter` +
   `ts join-and-run`, which does zero mount operations; `setupDev` (which
   creates `/dev/vsock` via `mountVsock`) runs only in the VM init (PID 1, via
   `drop-caps-and-run --vsock`). So `inVM()` is false and `thunderclient.Dial`
   takes the unix branch. (Correct per the desired state.)
2. **But the unix branch can't work.** `thunder.sock` would be a Unix socket
   file on virtiofs, and AF_UNIX sockets are kernel-local: the VM's kernel
   cannot `connect()` to a socket created by thundersnapd on the host kernel.
   virtiofs/FUSE socket files are inert.
3. **And in pure VMX mode nothing even creates `<frame>/id/thunder.sock`.** The
   control server is created only by `runContainerSession`
   (`cmd/thundersnapd/session.go:64`) and `handleEnter`
   (`cmd/thundersnapd/main.go:1811`); `runVMXSession` (`session.go:288-337`)
   never calls `getOrCreateControlServer`. (`prepareContainerRootFS` creates
   the `/id` subvolume at `cmd/thundersnapd/rootfs.go:205-221`; `prepareVMXRootFS`
   (`session.go:223`) does not.)

**Untested:** no e2e or not_e2e test runs `ts` control commands inside a VM.
`not_e2e`'s VM tests exercise shell sessions only.

### What must stay

**`--vsock` flag / `mountVsock` / the `/dev/vsock` bind-mount in `setupDev`
(`cmd/ts/container_setup.go:72-75`, `:642-653`): LIVE and load-bearing.** vshd
listens on AF_VSOCK port 5222 (`cmd/vshd/main.go:539`); this is the sole
host→VM session channel (`connectToVshd` dials it for every VMX session,
`cmd/thundersnapd/session.go:182-210`, used at `:321` and `:365`). Deleting
this breaks every VM session. It would require replacing vshd's transport
(e.g. SSH over passt) first. **Keep.**

### Stale comment

`cmd/thundersnapd/main.go:1687` has a comment "Port 5222: vshd proxy" but the
switch at `main.go:1704` handles only 5223/5224. The 5222-proxy design in
`ts-go-e2e-vsock.md` was superseded by the 5224 `/enter` protocol.

## Proposed plan

Order matters; each step should leave the tree building and tests green.

### Step 1 — delete the dead/broken branch (safe, no behavior change)

Remove `dialEnter()`'s vsock branch and the `inVM()` in `cmd/ts/main.go`
(`:2098-2118`). `ts go` / `ts undo` from inside a VM will then always use the
unix socket — which today means they fail in a VM (no relay yet), same as they
already fail via the broken vsock dial. No live caller regresses. This is pure
dead-code removal.

Also fix the stale comment at `cmd/thundersnapd/main.go:1687` (the switch it
describes is at `:1704`).

### Step 2 — build the in-VM `thunder.sock` relay

Add a relay process that runs inside the VM (alongside vshd, as part of the VM
init) and:

1. Creates `<vmxRootFS>/id/thunder.sock` (a real Unix socket in the VM's kernel).
2. Listens on it.
3. For each accepted connection, dials the host over `/dev/vsock` on the control
   port (5223 today) and bidirectionally copies bytes.

This makes `ts` inside the VM (outer shell **and** VMX containers, once Step 3
lands) reach the host control server through one local socket, matching the
desired state. `ts` no longer needs to know it is in a VM.

The relay must handle **both** protocols that currently go over the unix
socket today:
- **HTTP control** (port 5223 equivalent): raw HTTP request/response. Proxying
  raw bytes to the host's full control server means *all* endpoints work, not
  just the 3 `makeVMXControlHandler` registers — so `ts frame`/`ref`/`log`/…
  work in VMs for the first time. This likely lets us delete
  `makeVMXControlHandler` entirely.
- **The `/enter` session protocol** (port 5224 equivalent): the CONNECT-handshake
  + TLV stream used by `ts go`/`ts undo`. The relay proxies this too, so
  `handleEnter` on the host is reached. (This is also the point at which
  `handleEnter` would need to learn how to enter VMX containers, not just host
  containers, for `ts go` to fully work in a VM — see Open question below.)

Implementation sketch: a small Go program in `cmd/` (or a mode of vshd) started
by the VM init cmdline after vshd, listening on the unix socket and copying to a
vsock Dial to CID 2. The host side already accepts raw connections on the
port-specific unix sockets (`<vsockSock>_5223`, and would need `_5224`); or,
cleaner, the host side grows a single multiplexed vsock listener that demuxes by
the CONNECT port the same way the unix-socket path does today.

### Step 3 — make VMX containers get a working `thunder.sock`

Today VMX containers are entered via `ts nsenter` + `ts join-and-run` into a
container namespace under the VM. For `ts` inside that container to work, the
container's view of `/id/thunder.sock` must be the relay's socket from Step 2.
Two sub-options:
- The relay socket lives at a stable path under the virtiofs root that the
  container rootfs can see (e.g. bind-mounted or symlinked into each frame's
  `/id/thunder.sock`), or
- the relay socket is at the VM root and `join-and-run` arranges for
  `/id/thunder.sock` in the container to resolve to it.

After this, `inVM()` in `thunderclient/thunderclient.go` becomes dead (every
`ts` invocation, host or VM or VMX-container, uses a local unix socket) and can
be deleted along with the vsock import in `thunderclient`.

### Step 4 — drop `makeVMXControlHandler`

Once the relay proxies raw HTTP to the host's full control server (Step 2), the
3-endpoint VM-side mux is redundant. Delete `makeVMXControlHandler`
(`cmd/thundersnapd/session.go:381-387`) and the `ControlHandler`/`serveVsock`
machinery in `thundersnap/vm.go` that currently serves it
(`vm.go:34/58/338-350/451-510`). The vsock listener for vshd's port 5222
is unrelated and stays.

### Step 5 — cleanup

- Remove the now-unused `vsock` import from `cmd/ts/main.go` and
  `thunderclient/thunderclient.go`.
- Remove `VsockPort`/`VshPort` constants from `thundersnap/vm.go` if no longer
  referenced.
- Update `ts-go-e2e-vsock.md` and `docs/` to describe the relay architecture.

## Open questions to resolve before implementing

1. **`ts go` / `ts undo` into a VMX frame.** Even with the relay proxying
   `/enter` to the host, `handleEnter` (`cmd/thundersnapd/main.go:1747`) today
   dials the **host** vshd and enters a **host** container — it has no VMX
   awareness. So `ts go` from inside a VM into a VMX frame needs `handleEnter`
   to learn the VMX path (or the enter protocol needs to carry enough context
   to route to the right VMX session). Decide whether Step 2 covers this or
   whether it's a separate follow-up. (`ts go` from an outer-VM shell is a
   niche case; `ts go` from inside a VMX container into another VMX frame may
   not be a real use case today — confirm before over-investing.)

2. **Where the relay lives.** A separate `cmd/threlay` started by the VM init,
   or a mode of vshd? vshd already has the vsock dependency and runs as VM init;
   folding the relay into vshd avoids a second process and a second vsock
   listener. But vshd is currently the session TLV endpoint, not an HTTP proxy
   — mixing concerns. Lean toward a separate small program unless vshd already
   has the right shape.

3. **Multiplexing on the host side.** Today the host expects per-port unix
   sockets (`<vsockSock>_5223`, and `_5224` doesn't exist). The relay needs the
   host to accept its vsock connections and route to the right handler (5223
   HTTP control, 5224 /enter). Easiest is to mirror the existing unix-socket
   CONNECT demux (`cmd/thundersnapd/main.go:1697-1705`) on the vsock side: one
   vsock listener that reads the CONNECT port and dispatches. Or keep the
   per-port-socket convention and just add a `_5224` listener.

4. **Test coverage.** Before starting, add an e2e test that runs a `ts` control
   command (e.g. `ts ping`, then `ts snap`) *inside* a VMX container and watch
   it fail today; then it passes after Step 3. This is the real acceptance
   criterion and it's currently missing entirely.

## What does NOT change

- `--vsock` / `mountVsock` / `/dev/vsock` in the VM init: stays. vshd's AF_VSOCK
  listener on port 5222 is the host→VM session channel and is load-bearing.
- The container entry path (`ts nsenter` + `ts join-and-run`): unchanged. It
  already does zero mount ops and never exposes `/dev/vsock` to containers,
  which matches the desired state.
- `ts`'s default socket path (`/id/thunder.sock`): unchanged. After the relay
  exists, every `ts` invocation just connects to it locally.

## Cross-check

This analysis was cross-checked against an independent cross-family review
(`kimi-k3` was used after `deepseek-v4-flash` hit a rate limit; same
cross-family intent, run via `pi --print` per `README.codereview.md`).
The reviewer independently found the duplicate `inVM()` in `thunderclient`
that I had missed, confirmed the dead/broken status of `dialEnter`'s vsock
branch, and confirmed the 3-endpoint limitation of `makeVMXControlHandler`.
The plan above incorporates those findings.
