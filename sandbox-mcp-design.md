# Thundersnap Sandbox MCP Design

This document describes how thundersnap exposes an MCP server that lets an
LLM harness (Aperture chat, Claude Code, pi, or any MCP client) drive
thundersnap frames the same way Aperture's built-in sandbox lets an agent
run shell commands, read and write files, etc. The goal is for thundersnap
to be usable as a drop-in alternative to Aperture's built-in sandbox, while
also being reachable directly by other harnesses.

## Context and scope

Aperture ships a "chat sandbox" — five tools (`bash`, `view`,
`create_file`, `str_replace`, `present_files`) that let an LLM act as if
it has an SSH session in a Linux VM: run commands, read files, write files.
The interesting fact about those tools is that **four of them are just
`bash` with a shell-script command builder in front of it.** `view` runs
`find`/`awk`; `create_file` pipes base64 through a heredoc; `str_replace`
pipes an embedded Python program through a heredoc. The whole sandbox
reduces to one primitive — *run a shell command and capture stdout/stderr/
exit* — plus one chat-UI-attached tool (`present_files`) that doesn't
travel outside Aperture.

Thundersnap already has that one primitive, two ways: vshd over a Unix
socket (the in-process path used by SSH and the control-socket `/enter`
endpoint), and SFTP. This design adds an MCP server on thundersnap's
existing HTTP listener that wraps the vshd primitive as the five
Aperture-equivalent tools (minus `present_files`), so any MCP client can
drive frames.

We are building **the MCP-connector path first.** A later phase can make
thundersnap a drop-in replacement for Aperture's built-in `Backend` (which
would also bring upload/download/`present_files` parity with the Aperture
chat UI). That later phase is sketched under [Future work](#future-work)
but not designed here, because the `Backend`/sandbox APIs are expected to
change before we get to them.

## Decisions summary

| Topic | Decision |
|---|---|
| Endpoint path | `/v1/mcp` on the existing port-7575 tsnet listener |
| Tool prefix | `thundersnap_` (e.g. `thundersnap_bash`) |
| Tool set | `bash`, `view`, `create_file`, `str_replace`, `list_frames`, `list_refs`; **not** `present_files`; no `snap`/`undo`/`ref` MCP tools |
| Frame selection | per-call `frame` arg on every tool; empty = auto-create default frame (same as `ssh root@@host`) |
| Default workdir | `/work`; `bash` takes an optional `workdir` override (other tools don't) |
| Shell model | fresh `sh -c` per call; no persistent shell/env across calls (matches Aperture) |
| Identity | `X-Aperture-Login` header → frame-dir key; fallback to peer's Tailscale `WhoIs` login name (which is Aperture's own identity when brokered → shared across users) |
| Bash timeout | agent-supplied `timeout` (seconds); default 600 s; hard ceiling **120 min** |
| Output cap | 1 MiB per call, stream cancelled at the cap (matches Aperture) |
| Streaming to LLM | none in v1 — one accumulated `CallToolResult` at exit; internal streaming only (for cap/timeout cancellation). TODO: MCP progress notifications |
| Auto-register | optional, behind `--mcp-register-url`; otherwise serve passively |
| Capability policy | TODO — no caps enforced in v1 |
| Aperture code | copy-and-paste ~400 lines (tool defs, command builders, `CollectExec`, `Tool`→MCP adapter); do **not** depend on the aperture module |

## The Aperture reference (what we're matching)

Source: `../aperture`, files under `chat/sandbox/` and `proxy/`.

**The five tools** (`chat/sandbox/tool_*.go`):

- `bash` — `execInSandbox(cmd, "/home/aperture", timeout)`, returns
  `Exit code: N\n\n<output>` on non-zero, raw output on zero.
- `view` — builds a shell program: `find -maxdepth 2` for dirs, `awk
  'NR>=S && NR<=E'` for text with line numbers, a stat-line for images.
  30 s timeout, 16 KB output cap.
- `create_file` — `mkdir -p $(dirname) && base64 -d > path <<'B64EOF'…`
  (base64+heredoc dodges `ARG_MAX`).
- `str_replace` — an embedded Python program (with
  `errors='surrogateescape'` so non-UTF-8 files survive) read over a
  heredoc; old/new strings are base64-encoded on stdin. Errors if the
  target string appears 0 or >1 times.
- `present_files` — HEADs each path under `/mnt/workspace/outputs/` via
  the backend, returns JSON with a `download_url` of the form
  `/chat-api/conversations/{convID}/download_file?path=…`. **This tool is
  welded to Aperture's chat UI and `Backend.HeadOutput`/`OpenOutput`; it
  does not travel.**

**The exec primitive** (`chat/sandbox/backend.go`, `exec.go`): a
`Backend` interface with an `Exec(ctx, instanceID, convID, req, onEvent)`
method that streams `ExecEvent`s (`output`/`exited`/`error`).
`CollectExec` accumulates frames into one `ExecResult{Output, ExitCode}`,
cancelling the stream at 1 MiB and replacing invalid UTF-8 with U+FFFD.
The `local` backend runs each `Exec` as a **fresh** `sh -c` with workdir
reset to `/home/aperture` — there is no persistent shell; "the VM
persists" is purely a filesystem statement, not a process/env one.

**MCP exposure** (`proxy/mcp.go`, `internal/mcpserver`): `mcp.NewServer`
+ `mcp.NewStreamableHTTPHandler` is mounted at **`/v1/mcp`** on the
proxy's HTTP mux. `chatToolToMCPHandler` is the ~12-line adapter that
turns a `tool.Tool{Execute: func(ctx, input) (string, error)}` into an
`mcp.ToolHandler` returning `CallToolResult{Content:[TextContent]}`.

Note for context (not part of this phase): Aperture's built-in sandbox
tools are injected into the *chat turn's* tool set with bare names, only
when a `SandboxClient` is wired by the supervisor. They are **not**
registered on `/v1/mcp`. That distinction is what makes "drop-in
replacement" a separate, later piece of work — see
[Future work](#future-work).

## Thundersnap primitives we build on

**vshd** (`cmd/vshd`, `vshdproto`, `vshdsession`) is the in-frame command
runner. A session is: dial the host vshd Unix socket → send the null-
delimited request header (`VMX\0<framePath>\0<user>\0<pty>\0<argc>\0
<args…>`) → switch to `vshdproto` TLV framing. Frame types:

| Frame | Dir | Payload |
|---|---|---|
| `FrameStdin` (1) | host→guest | stdin bytes |
| `FrameStdout` (2) | guest→host | stdout bytes |
| `FrameStderr` (3) | guest→host | stderr bytes |
| `FrameWinsize` (4) | host→guest | 8-byte winsize (PTY only) |
| `FrameExit` (5) | guest→host | 4-byte int32 exit code |

vshd anchors shared PID/mount/UTS namespaces per rootfs via
`containerns.Manager`; sessions join them via the in-binary `ts nsenter`
and then chroot + drop caps. This is the same path SSH and the
control-socket `/enter` endpoint (port 5224) use, byte-identical on host
and in-VM. For the MCP tools we use **non-PTY** one-shot execs: send the
header with `pty=0`, no `FrameWinsize`, then read `FrameStdout`/
`FrameStderr` until `FrameExit`.

**Frames and refs** (`cmd/thundersnapd/session.go`, `refs_handlers.go`):
frames live at `fs/<tailscaleUser>/<uuid>`. `resolveFrameForUser(user,
name)` treats `name == ""` (or `name == user`) as "default": it looks up
the `default` ref, and if none is bound, **hands back a fresh auto-created
frame** — exactly the `ssh root@@host` double-@ behaviour. This means the
`frame` arg can be optional with auto-create semantics, so the LLM can
bootstrap without first listing frames.

**The in-container control socket** lives at `<rootFS>/id/thunder.sock`,
which is present inside the container as `/id/thunder.sock`. The `ts` CLI
defaults to that path. Crucially, `handleListFrames`/`handleListRefs`
scope to the **user's whole frame/ref store**, not just the current frame.
So once the LLM is inside *any* frame, `ts frames`, `ts refs`, `ts frame`,
`ts snap`, `ts undo`, `ts ref` all work and see the whole user universe.
This is why we do **not** need `snap`/`undo`/`ref`/`create_frame` as MCP
tools — they're all one `bash` call away once you're in a frame.

**The HTTP listener** (`cmd/thundersnapd/main.go:639`) is a tsnet
listener on `:7575` with an `http.ServeMux` that already serves `/ts/ping`,
`/ts/servers.json`, `/` (mesh UI), `/bupdate/` (file server), and
Prometheus metrics. The MCP endpoint mounts alongside these. (In test
mode this mux is currently never instantiated — see
[e2e plan](#e2e-plan).)

## Identity and frame scoping

Thundersnap keys frames by **Tailscale login name**
(`fs/<who.UserProfile.LoginName>/`). When an MCP client connects directly
on the tailnet, thundersnap's own `WhoIs` on the TCP peer already gives
the right user. When Aperture is the broker, the peer is Aperture's node,
so `WhoIs` returns Aperture's identity — every Aperture-fronted user
would collapse into one frame directory.

We solve only the direct case for free and add one Aperture-side
convention for the brokered case:

- **`X-Aperture-Login` header.** When present and the connection is
  trusted (see below), thundersnap uses it as the frame-dir key instead
  of the peer's `WhoIs` login. Absent header → fall back to peer
  `WhoIs` login (which is Aperture's own identity when brokered, i.e.
  shared across all users — acceptable degraded mode, documented to the
  operator).
- **Trust boundary.** The header is only honoured when the peer is the
  configured Aperture node. Concretely: thundersnap records the expected
  Aperture node identity (from config / `--mcp-trusted-aperture`), and
  `WhoIs(peer)` must match it before `X-Aperture-Login` is used. A bare
  header from any other peer is ignored. The trust anchor is Tailscale
  identity (tsnet authenticates the peer); no separate shared secret is
  needed.
- **Key choice: login name, not stable user ID.** Aperture also has an
  immutable `UserID` that survives renames, which would be more correct
  (a renamed user keeps their frames). We use login name for v1 because
  it matches today's SSH behaviour and needs zero thundersnap change.
  `TODO: forward/record stable UserID so a future migration can re-key
  frames without orphaning them on rename.`

> **Aperture-side dependency:** honouring `X-Aperture-Login` requires
> Aperture to *send* it on proxied MCP calls. Today Aperture forwards
> **no** user-identity header to connectors — `perUserAuth` backends
> learn the user implicitly via a per-user OAuth token; shared-auth
> backends see only Aperture's shared credential. So this design assumes
> a small Aperture change: a new "trust-proxy-identity" connector
> option (or auth mode) that causes Aperture to set
> `X-Aperture-Login: <login>` (and optionally `X-Aperture-User-ID`) on
> every proxied call to that connector. This is the one cross-repo
> dependency of the MCP-first phase. The direct-tailnet deployment
> (harness → thundersnap, no Aperture) needs no Aperture change.

## Tool set

All tools take an optional **`frame`** string (UUID, ref name, or empty
for default-auto-create). Every tool that runs a command resolves the
frame via the existing `resolveFrameForUser(user, frame)` and launches a
non-PTY vshd one-shot. `list_frames` and `list_refs` are the only tools
that do **not** launch a container — they read the user's stores
directly, so the LLM can pick an existing frame for its first real
`bash` instead of auto-creating a throwaway just to run `ts frames`.

### `thundersnap_bash`

```
{
  "command": string,            // required
  "frame":   string,            // optional; "" = default (auto-create)
  "workdir": string,            // optional; default "/work"
  "timeout": integer            // optional, seconds; default 600; max 7200
}
```

Runs `sh -c <command>` in the frame via vshd, workdir = `workdir` or
`/work`. Returns the accumulated stdout+stderr (interleaved as vshd
emits them) and, on non-zero exit, `Exit code: N\n\n<output>` (matching
Aperture). The `timeout` is enforced at the MCP wrapper via
`context.WithTimeout`; on expiry the vshd socket is closed, which the
inner layer treats identically to an SSH-client disconnect (existing
process-group reap fires).

Default 600 s; hard ceiling **120 min (7200 s)**. Both numbers are
stated in the tool description so the LLM knows the budget. A
`timeout` greater than the ceiling is clamped to the ceiling. Note: this
is a *per-call* ceiling; there is no per-session/per-turn budget in v1
(consistent with "no caps yet").

### `thundersnap_view`

```
{ "path": string, "view_range": [start, end], "frame": string }
```

Byte-identical to Aperture's `view`: the same `find`/`awk`/image-stat
shell program, same 2-level directory listing, same line-numbering, same
16 KB output cap. `path` is an in-frame absolute path; default workdir
(`/work`) is irrelevant since `view` uses absolute paths. 30 s timeout.

### `thundersnap_create_file`

```
{ "path": string, "file_text": string, "frame": string }
```

Byte-identical to Aperture's: `mkdir -p $(dirname) && base64 -d > path
<<'B64EOF'…`. 30 s timeout.

### `thundersnap_str_replace`

```
{ "path": string, "old_str": string, "new_str": string, "frame": string }
```

Byte-identical to Aperture's: the embedded Python program over a
heredoc, base64 old/new on stdin, `surrogateescape` for non-UTF-8 files,
errors on 0 or >1 occurrences. 30 s timeout.

### `thundersnap_list_frames`

```
{ }   // (frame arg N/A — this is the frame picker)
```

Calls the same logic as `handleListFrames`: enumerate the user's frame
store, annotate each with its bound ref name (or UUID if none), report
active-session count. Returns JSON:
`{ "frames": [{ "name": "...", "status": "stopped"|"N" }] }`.

### `thundersnap_list_refs`

```
{ }
```

Calls the same logic as `handleListRefs`: enumerate the user's ref
store. Returns `{ "refs": [{ "name", "uuid", "autorun" }] }`.

### Notably absent

- **`present_files`** — welded to Aperture's chat UI (`HeadOutput` +
  `download_file` URLs). Dropped for v1; chat-UI attachment-row parity is
  a [future work](#future-work) item that requires either the drop-in
  `Backend` path or an Aperture change to generalize the outputs
  handler.
- **`snap` / `undo` / `ref` / `create_frame` MCP tools** — all reachable
  as `ts` commands inside `bash` once you're in a frame (the control
  socket is at `/id/thunder.sock` in-container and scopes to the user's
  whole store). Adding them as MCP tools would duplicate `ts` for no
  gain, per the "don't add things that are easy from inside the sandbox"
  rule.
- **A glob tool** — Aperture doesn't have one; `view`-on-a-directory
  already lists 2 levels, and `find`/`fd`/shell globs are one `bash`
  call away.

## Exec, output, and cancellation

The exec collector is a port of Aperture's `CollectExec`
(`chat/sandbox/exec.go`), adapted to read vshdproto frames instead of
`Backend.Exec` events:

1. Dial host vshd, send the VMX header (`framePath`, user, `pty=0`,
   argc=1, arg=`sh -c <command>`). No `FrameWinsize` (non-PTY).
2. Read frames in a loop:
   - `FrameStdout`/`FrameStderr` → append `Data` to a `strings.Builder`
     (interleaved, matching Aperture's single-stream accumulation).
   - At 1 MiB: trim back to a UTF-8 rune boundary, append
     `"\n\n... output truncated (exceeded 1 MiB) ..."`, set `truncated`,
     and **cancel the context** so vshd tears down the process group.
   - `FrameExit` → record `ExitCode`.
3. Finalize: `strings.ToValidUTF8(output, "")` to clean up any rune
   split across frames.
4. Return `ExecResult{Output, ExitCode}`.

The wrapper applies its own `context.WithTimeout` from the `timeout`
arg (default 600 s, clamped to 7200 s). On either timeout or the 1 MiB
cap, the wrapper closes the vshd socket. From vshd's perspective this is
identical to an SSH client disconnecting mid-stream, so the existing
container-init reap path fires. **The first e2e task is a spike
confirming that closing the vshd socket mid-stream reliably reaps the
whole process group** (a `sleep 7200 &` backgrounded child is the test
case) — this is the one assumption in the whole plan that isn't already
proven by the SSH path, because the SSH path always runs to a clean
`FrameExit` or a PTY close rather than a raw socket drop on a non-PTY
one-shot.

### Streaming to the LLM (not in v1)

MCP has a `notifications/progress` primitive that a server *could* use to
push partial stdout chunks to the client mid-call. **We do not use it in
v1.** Reasons:

- Aperture doesn't either — `chatToolToMCPHandler` returns one
  accumulated `CallToolResult` and no mainstream MCP client (Aperture
  chat UI, Claude Code) renders progress notifications as live terminal
  output today. "Claude Code streams bash line-by-line" is a *harness*
  feature (Claude Code runs its own Bash subprocess locally and writes to
  your terminal); when Claude Code calls an MCP server's bash tool it
  gets one accumulated result, same as us.
- The internal streaming **does** matter in v1, for a different reason:
  it's what makes the 1 MiB cap and the timeout cheap (we stop reading
  and tear down the moment either trips, instead of buffering a
  `yes`-style firehose for the full deadline).

So: copy `CollectExec`'s streaming-collector design verbatim; return one
`CallToolResult`. `TODO: emit MCP progress notifications with stdout
chunks for live-streaming clients once any mainstream client renders
them.`

## Multiple commands per turn (concurrency)

Yes — and this is a genuine ergonomic win over Aperture's built-in
sandbox, which auto-provisions one VM per conversation. An MCP assistant
turn can include multiple `tool_use` blocks; the harness issues them as
separate `tools/call` requests. Thundersnap is well-placed to run them
**concurrently**:

- Each `thundersnap_bash` call is independent and stateless (per-call
  `frame`, fresh `sh -c`, no shared shell state), so there is no
  shared-mutable-state hazard between concurrent calls into *different*
  frames.
- Concurrent calls into the *same* frame are just two vshd one-shots
  against the same rootfs — exactly how two SSH sessions into one frame
  coexist today (`containerns.Manager` anchors the shared namespaces and
  multiple sessions join them).
- vshd is connection-per-exec on a Unix socket, so concurrent dials are
  cheap; there is no single-shared-shell serialization bottleneck.

Two caveats to document in the tool descriptions:

1. **Concurrency is harness-controlled, not thundersnap-controlled.** If
   a harness serializes tool calls within a turn, thundersnap can't
   force parallelism — it just won't block it.
2. **Same-frame concurrent calls share a filesystem.** Two `bash` calls
   into the same frame that write the same file race (last-writer-wins),
   exactly like two SSH sessions. The tool descriptions should tell the
   LLM to use distinct frames for independent work.

## Auto-registration

Aperture's `/v1/mcp/register` is a self-registration protocol: an MCP
server `POST {"url":"http://thundersnap:7575/v1/mcp"}` and then holds the
response open as a long-lived keepalive stream (1-second tickers). While
the connection is alive, Aperture assigns the server a `serverID`
(`auto<N>`), fetches its capabilities, starts a 5-minute poll, and
exposes its tools (prefixed `auto<N>_`) on `/v1/mcp` and through
`mcpToolsFn` into chat. Closing the POST unregisters. Gated by
`mcp.accept_registrations: true` in Aperture config (403 otherwise).

Thundersnap uses this as an **opt-in** flag:

- `--mcp-register-url <aperture>/v1/mcp/register` (or config equivalent)
  → start a `maintainRegistration` goroutine (the ~40-line loop from
  `internal/mcpserver`, copied) that keeps the POST open and reconnects
  after drops.
- Flag absent → just serve `/v1/mcp` passively and wait for clients
  (direct-tailnet harnesses, or an Aperture admin who config-registers
  thundersnap as a connector).

Auto-registration is the zero-config dev path; it yields an unstable
`auto<N>` id with no labels and no grant handle. For production/trust
(the deployment where Aperture sends `X-Aperture-Login`), a
**config-registered** connector entry in Aperture (`connectors.servers
["thundersnap"] = {url, labels}`) is better: stable id, grants,
labels, and a stable trust anchor for the identity header. Both are
supported; the flag picks auto-register, Aperture config picks
config-register.

## What we copy from Aperture (copy-and-paste, no module dependency)

Per the project rule, we do not import the aperture module. We
copy-and-paste the self-contained pieces, trimming the rest:

| Piece | Lines | Verdict |
|---|---|---|
| `tool.Tool` type + `chatToolToMCPHandler` adapter | ~30 | **Copy.** Trivial, proven, exactly the shape the MCP SDK wants. (Or build `*mcp.Tool`+handlers directly — equivalent; copying is slightly less work.) |
| `tool_bash.go` | ~50 | **Copy** the shell, retune: drop `(instanceID, convID)` plumbing, add `frame`/`workdir` handling, raise ceiling to 7200 s. |
| `tool_view.go` (incl. `buildViewCommand`, `truncateUTF8`) | ~100 | **Copy verbatim.** The `find`/`awk`/image-stat program and the rune-boundary truncation are fiddly to re-derive. |
| `tool_create_file.go` (`buildCreateFileCommand`) | ~40 | **Copy verbatim.** The base64+heredoc ARG_MAX dodge is the whole point. |
| `tool_str_replace.go` (`strReplaceScript`, `buildStrReplaceCommand`) | ~70 | **Copy verbatim.** The surrogateescape Python is the whole point. |
| `tool_present_files.go` | — | **Don't copy.** Welded to Aperture chat UI. |
| `exec.go` (`CollectExec`, 1 MiB cap, UTF-8 cleanup, cancel-on-cap) | ~90 | **Copy** the logic; swap the `Backend.Exec` event source for a vshdproto frame reader. |
| `backend.go` (`Backend` interface, sentinels, `WorkspaceSource`, metrics) | ~400 | **Don't copy.** Far wider than we need (uploads/outputs/lifecycle/restoration/metrics). We only need `ExecRequest`/`ExecEvent`/`ExecResult` shapes, which are ~30 lines. |
| `local/local.go` | ~400 | **Don't copy.** It's Aperture's test double; thundersnap's own frame exec via vshd is the real local backend. Lift only the *discipline* (fresh shell per call, workdir reset). |
| `internal/mcpserver` (`New`, `ListenAndServe`, `maintainRegistration`) | — | **Don't import.** It opens its own listener; we mount on the existing 7575 mux. Copy the ~40-line `maintainRegistration` loop if we do auto-register. |

Net: ~400 lines copied, all self-contained and well-commented. The
concept that does the most work is **"file tools = shell scripts over a
single exec primitive"** — once the vshd-backed exec exists,
`view`/`create_file`/`str_replace` come along for the ride with no
file-specific backend code.

## Capability policy

`TODO — no caps enforced in v1.` Thundersnap has a `policy.jsonc` +
`ResolveCap(who, policy)` model (role + isolation: `container`/`vmx`/
`none`) used by SSH. The MCP endpoint should eventually apply the same
policy (e.g. hide write tools for a read-only role, honour
`cap.Isolation` when launching). Left as a TODO so v1 ships; the design
assumes every authenticated peer gets full tool access, scoped only by
the identity model above (you can drive your own frames, not others').

## e2e plan

The e2e package is built with the `e2e` tag and run via `make e2e`; tests
must never skip, and run simplest-to-hardest in tiers.

1. **Serve the HTTP mux in test mode (pre-existing gap, fix first).**
   `runTestMode` today starts only SSH on a local TCP port; the entire
   port-7575 HTTP mux (mesh, `/bupdate/`, metrics, and now MCP) is never
   instantiated in tests. Refactor `runTestMode` to also build and serve
   the HTTP mux on a local port and plumb MCP onto it. This fixes a
   pre-existing coverage gap (the HTTP handlers have no e2e coverage at
   all today) and is a prerequisite for everything below.

2. **Cancellation/reap spike.** Confirm that closing the vshd socket
   mid-stream on a non-PTY one-shot reaps the whole process group,
   including a backgrounded child (`sleep 7200 &`). This is the one
   assumption not already proven by the SSH path. If reap is unreliable,
   the timeout/cap behaviour degrades to "socket closes but zombies
   linger" — fix before relying on it.

3. **Output truncation.** Mirror Aperture's `CollectExec` cancellation
   test: a `yes`-style infinite stream must hit the 1 MiB cap, return a
   truncated result with the marker, and tear down promptly (not run for
   the full deadline).

4. **Timeout.** A command sleeping past the (test-shrunken) timeout must
   return a timeout result and leave no process behind.

5. **Tool round-trips.** `bash` (zero and non-zero exit), `view` (file,
   directory, image, `view_range`), `create_file`, `str_replace`
   (success, not-found, not-unique), `list_frames`, `list_refs` — each
   against a freshly-created frame and against a named ref.

6. **`frame` arg semantics.** `frame=""` auto-creates a default frame;
   `frame=<uuid>` and `frame=<refname>` resolve correctly; bad frame
   errors cleanly.

7. **Concurrency.** Two `bash` calls into the same frame in one turn
   share the FS (assert both see a file the first writes); two calls
   into different frames are isolated (assert neither sees the other's
   writes); outputs/exit codes map to the right calls (no
   cross-contamination).

8. **Identity header.** Direct-tailnet connection uses peer `WhoIs`;
   a trusted-peer connection with `X-Aperture-Login` keys frames by the
   header; a header from a non-trusted peer is ignored (falls back to
   peer `WhoIs`).

9. **Auto-register (if enabled).** With `accept_registrations: true` on
   a test aperture, thundersnap's tools appear under `auto<N>_` and
   disappear when the registration connection closes.

## Future work (out of scope for this phase)

- **Drop-in `Backend` replacement.** Implement Aperture's
  `chatsandbox.Backend` as an HTTP client that talks a thundersnap
  sandbox protocol, plus a supervisor config knob to wire it instead of
  fargate/vercel. This brings chat-UI parity: Aperture's
  `sandbox_push`/`present_files`/`download_file`/drain machinery works
  unmodified, uploads land at `/mnt/workspace/uploads/` and outputs at
  `/mnt/workspace/outputs/` inside the frame. Requires the
  `(instanceID, convID) → (user, frame)` mapping, which ties back to
  the identity model. Deferred because the `Backend`/sandbox APIs are
  expected to change.
- **`present_files` / chat-UI attachment rows for the connector path.**
  Either generalize Aperture's `SandboxOutputsHTTPHandler`/
  `download_file` to a non-built-in sandbox source, or have thundersnap
  serve its own download URLs over the 7575 listener and teach the chat
  UI to render connector-sourced attachment chips.
- **Stable user-ID frame key.** Forward and record Aperture's immutable
  `UserID` so a Tailscale rename doesn't orphan a user's frame
  directory. v1 uses login name to match today's SSH behaviour.
- **Capability policy.** Apply `policy.jsonc`/`ResolveCap` to MCP tool
  access and frame launch isolation.
- **Persistent shell per MCP session.** v1 runs a fresh `sh -c` per call
  (matches Aperture); a persistent shell would let the LLM carry
  `cd`/`export` across calls but adds session state to manage. Note in
  source where the fresh-shell discipline is implemented.
- **MCP progress notifications** for live stdout streaming, once a
  mainstream client renders them.

## Open parameter

None remaining — the bash timeout ceiling is **120 min (7200 s)**, decided.
