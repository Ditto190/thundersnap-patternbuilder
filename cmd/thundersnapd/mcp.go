// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

// mcp.go implements the thundersnap MCP server, exposing the six
// sandbox-equivalent tools (bash, view, create_file, str_replace,
// list_frames, list_refs) on the daemon's HTTP listener at /v1/mcp.
//
// This is a port of Aperture's chat/sandbox tool set (tool_bash.go,
// tool_view.go, tool_create_file.go, tool_str_replace.go) and the
// Tool→MCP adapter (proxy/mcp.go chatToolToMCPHandler), per
// sandbox-mcp-design.md. Per the project rule we do NOT import the
// aperture module: the self-contained command builders and the adapter
// are copied here.
//
// The exec primitive is mcpexec.CollectFrames (thundersnap/mcpexec), the
// vshdproto-frame port of Aperture's CollectExec. The launcher below
// (runInFrame) is the daemon-specific glue: it resolves a frame, prepares
// its rootfs, dials the host vshd, sends the VMX one-shot header, and
// collects the result — the same path runContainerSession uses for SSH,
// minus the PTY.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tailscale/thundersnap/frameid"
	"github.com/tailscale/thundersnap/mcpexec"
)

// --- Globals set by main() from flags (production-only; empty in test mode) --
//
// These are the production MCP knobs. In test mode (--test-listen/--test-user)
// none of them are set: the endpoint serves with testModeUser as the identity
// and no trusted-peer / self-registration behaviour. See resolveMCPUser.

// mcpTrustedAperture is the Tailscale identity (login name) of the Aperture
// node permitted to set X-Aperture-Login on proxied MCP calls. Set via
// --mcp-trusted-aperture. Empty in test mode (no tsnet, so no trusted peer).
var mcpTrustedAperture string

// mcpRegisterURL, when non-empty, is the Aperture /v1/mcp/register URL to
// self-register with. Set via --mcp-register-url. Empty → serve passively.
var mcpRegisterURL string

// mcpDaemonVersion is the version reported in the MCP server handshake. There
// is no build-time version injection in this repo today; TODO: wire to jj/git
// describe or an ldflag once one exists. Hardcoded for now so the handshake
// has a non-empty version string (the MCP spec requires Implementation.Version).
const mcpDaemonVersion = "dev"

// --- Identity ---------------------------------------------------------------
//
// Thundersnap keys frames by Tailscale login name (fs/<login>/<uuid>/). The
// MCP endpoint resolves the effective user per HTTP request:
//
//   - Test mode (--test-user): testModeUser is the identity for every
//     connection. There is no tsnet/WhoIs, and X-Aperture-Login is never
//     honoured (no trusted peer to authenticate it). This mirrors the SSH
//     --test-user seam and is what the e2e harness drives.
//   - Production: if X-Aperture-Login is present AND the peer's WhoIs matches
//     mcpTrustedAperture, use the header (so Aperture-fronted users keep
//     distinct frame dirs instead of collapsing into Aperture's own identity).
//     Otherwise fall back to the peer's own WhoIs login name (the direct-
//     tailnet case: harness → thundersnap, no Aperture).
//
// TODO: forward/record Aperture's stable UserID so a Tailscale rename doesn't
// orphan a user's frame directory. v1 uses login name to match SSH behaviour.

// mcpUserKey is the context key carrying the resolved MCP user from the HTTP
// auth middleware into the tool handlers. The streamable handler propagates
// req.Context() into the tool-call ctx, so a value set by the middleware is
// visible to every ToolHandler.
type mcpUserKey struct{}

// resolveMCPUser returns the effective Tailscale user for an MCP HTTP request.
func resolveMCPUser(r *http.Request) string {
	if testModeUser != "" {
		return testModeUser
	}
	// Production path: honour X-Aperture-Login only from the trusted Aperture
	// node, else fall back to the peer's own WhoIs login.
	if h := r.Header.Get("X-Aperture-Login"); h != "" && mcpTrustedAperture != "" && peerIsTrustedAperture(r) {
		return h
	}
	return peerWhoIsLogin(r)
}

// peerIsTrustedAperture reports whether the TCP peer's Tailscale identity
// matches mcpTrustedAperture. Requires tsnet/WhoIs; in test mode this is
// never called (resolveMCPUser returns early). TODO: implement against the
// tsnet LocalClient once the production HTTP listener is wired; for now it
// returns false so a bare header from any peer is ignored (fail-closed).
func peerIsTrustedAperture(r *http.Request) bool {
	// TODO: who = getWhoIs(r.Context(), globalLocalClient, r.RemoteAddr);
	// return who.UserProfile.LoginName == mcpTrustedAperture
	return false
}

// peerWhoIsLogin returns the Tailscale login name of the TCP peer. Requires
// tsnet/WhoIs; in test mode this is never called. TODO: implement; returns
// "unknown" so a misconfigured production deployment fails loudly rather than
// silently keying everyone to the same dir.
func peerWhoIsLogin(r *http.Request) string {
	// TODO: who = getWhoIs(r.Context(), globalLocalClient, r.RemoteAddr)
	// if who != nil && who.UserProfile != nil { return who.UserProfile.LoginName }
	return "unknown"
}

// mcpUserFromContext returns the MCP user stashed in ctx by the auth
// middleware, or "" if absent (which would indicate a wiring bug — the
// middleware must wrap the handler before mounting).
func mcpUserFromContext(ctx context.Context) string {
	if u, _ := ctx.Value(mcpUserKey{}).(string); u != "" {
		return u
	}
	return ""
}

// mcpAuthMiddleware resolves the effective user for each MCP HTTP request and
// stashes it in the request context, from which the streamable handler
// propagates it into every tool-call ctx. It does NOT reject requests: in the
// direct-tailnet deployment every authenticated peer (which tsnet guarantees)
// gets full tool access scoped to its own frames. Capability policy is a
// TODO (see sandbox-mcp-design.md §Capability policy).
//
// TODO: apply policy.jsonc / ResolveCap here — hide write tools for a
// read-only role, honour cap.Isolation when launching.
func mcpAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := resolveMCPUser(r)
		r = r.WithContext(context.WithValue(r.Context(), mcpUserKey{}, user))
		next.ServeHTTP(w, r)
	})
}

// errMCPCommandTimeout is returned by runInFrame when the per-call context
// deadline fires before the command produced a FrameExit. Handlers surface
// the partial output (with the timeout marker runInFrame appends) as an
// IsError=true result so the LLM sees the timeout rather than a silent
// success. It is distinct from a setup error (resolve/dial failure), where
// runInFrame returns a wrapped error and a nil result.
var errMCPCommandTimeout = errors.New("command timed out")

// --- The launcher: vshd one-shot exec in a frame ---------------------------

// runInFrame runs `sh -c <command>` in the named frame for the given user,
// starting in workdir (defaulting to "/work"), and returns the collected
// output + exit code. This is the MCP analogue of runContainerSession: it
// resolves the frame via the user's ref store, prepares the rootfs, anchors a
// control server (so `ts` works inside the frame), dials the host vshd, and
// sends a non-PTY one-shot VMX request. The ctx deadline enforces the
// per-call timeout; closing the vshd socket on deadline/cap tears down the
// process group (the e2e cancellation spike, task T2, validates this).
//
// TODO: persistent shell per MCP session — v1 runs a fresh sh -c per call,
// matching Aperture; a persistent shell would carry cd/export across calls.
func runInFrame(ctx context.Context, user, frame, workdir, command string) (*mcpexec.ExecResult, error) {
	if user == "" {
		return nil, fmt.Errorf("no MCP user resolved for request")
	}
	if workdir == "" {
		workdir = "/work"
	}

	rootFS, _, err := resolveFrameRootFS(user, frame)
	if err != nil {
		return nil, fmt.Errorf("resolve frame: %w", err)
	}
	if err := prepareContainerRootFS(rootFS, ""); err != nil {
		return nil, fmt.Errorf("prepare frame rootfs: %w", err)
	}

	// Anchor a control server so `ts` subcommands work inside the frame (the
	// control socket lives at /id/thunder.sock in-container). This is the same
	// refcounted getOrCreate/release pair runContainerSession uses.
	if _, err := controlServers.getOrCreateControlServer(rootFS); err != nil {
		return nil, fmt.Errorf("start control socket: %w", err)
	}
	defer controlServers.releaseControlServer(rootFS)

	sockPath, err := hostVshd.ensure()
	if err != nil {
		return nil, fmt.Errorf("start host vshd: %w", err)
	}
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("dial host vshd: %w", err)
	}
	defer conn.Close()

	// VMX header: framePath relative to / (vshd reconstructs it as
	// filepath.Clean("/"+framePath)), target user "root" (matches `ssh
	// root@frame`), non-PTY, args ["sh", "-c", wrapped].
	absRootFS, err := filepath.Abs(rootFS)
	if err != nil {
		return nil, fmt.Errorf("abs rootfs path: %w", err)
	}
	framePathHdr := strings.TrimPrefix(absRootFS, "/")

	// Wrap the command to start in workdir. `cd <workdir> && <command>` matches
	// Aperture's local backend setting cmd.Dir = workdir: if the dir doesn't
	// exist, cd fails and && short-circuits so the command doesn't run.
	wrapped := "cd " + shellQuote(workdir) + " && " + command
	writeVshdRequest(conn, framePathHdr, "root", false, []string{"sh", "-c", wrapped})

	// Collect frames in a goroutine; on ctx cancel (timeout), close the conn to
	// unblock the collector's ReadFrame and tear down vshd's process group.
	type collectResult struct {
		res *mcpexec.ExecResult
		err error
	}
	ch := make(chan collectResult, 1)
	go func() {
		r, err := mcpexec.CollectFrames(conn)
		ch <- collectResult{r, err}
	}()

	select {
	case r := <-ch:
		return r.res, r.err
	case <-ctx.Done():
		// Close the conn to unblock the collector; the deferred Close is a
		// no-op afterwards. Closing the vshd socket mid-stream is identical to
		// an SSH client disconnecting, so vshd's existing reap path fires.
		conn.Close()
		r := <-ch
		// Surface partial output with a timeout marker (unless the cap already
		// produced a marker). The process group is reaped by vshd on disconnect.
		if r.res != nil && !r.res.Truncated {
			marker := "\n\n... command timed out ..."
			if r.res.Output == "" {
				r.res.Output = "... command timed out ..."
			} else if !strings.HasSuffix(r.res.Output, marker) {
				r.res.Output += marker
			}
		}
		return r.res, errMCPCommandTimeout
	}
}

// isWithinRootFS reports whether path is rootFS itself or a descendant of it
// after cleaning. It guards host-side file operations (str_replace) against
// path-traversal escapes from the frame's rootfs: a tool path like
// "/../../etc/shadow" must resolve inside the frame, not the host.
func isWithinRootFS(path, rootFS string) bool {
	absRoot, err := filepath.Abs(rootFS)
	if err != nil {
		return false
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// --- Command builders (ported from aperture chat/sandbox) -------------------
//
// These are ports of the aperture tool_*.go builders. The fiddly bits (base64+
// heredoc ARG_MAX dodge, surrogateescape Python, awk line-numbering, rune-
// boundary truncation) are proven; re-deriving them risks subtle regressions.

// shellQuote wraps a string in single quotes for safe shell interpolation.
// Ported from aperture chat/sandbox/tools.go.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// buildViewCommand returns the shell program that views path. Ported from
// aperture chat/sandbox/tool_view.go.
func buildViewCommand(path string, viewRange []int) (string, error) {
	const maxEndLine = 99_999_999
	startLine := 1
	endLine := maxEndLine
	if len(viewRange) == 2 {
		startLine = viewRange[0]
		if startLine < 1 {
			startLine = 1
		}
		endLine = viewRange[1]
		if endLine == -1 {
			endLine = maxEndLine
		}
		if endLine < startLine {
			return "", fmt.Errorf("invalid view_range: end (%d) is before start (%d)", endLine, startLine)
		}
	} else if len(viewRange) != 0 {
		return "", fmt.Errorf("view_range must have exactly 2 elements, got %d", len(viewRange))
	}

	qpath := shellQuote(path)
	return fmt.Sprintf(`path=%s
if [ -d "$path" ]; then
  find "$path" -maxdepth 2 -not -path '*/.*' -not -path '*/node_modules/*' | sort | head -200
elif [ -f "$path" ]; then
  case "$path" in
    *.jpg|*.jpeg|*.png|*.gif|*.webp|*.svg|*.bmp|*.ico) echo "[Image: $(basename "$path"), $(stat -c '%%s' "$path") bytes]" ;;
    *) awk 'NR>=%d && NR<=%d {printf "%%6d\t%%s\n", NR, $0}' "$path" ;;
  esac
else
  echo "Error: $path not found" >&2; exit 1
fi`, qpath, startLine, endLine), nil
}

// truncateUTF8 returns s capped at limit bytes, with marker appended if
// truncation occurred. Ported from aperture chat/sandbox/tool_view.go.
func truncateUTF8(s string, limit int, marker string) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 {
		r, size := utf8.DecodeLastRuneInString(s[:cut])
		if r != utf8.RuneError || size > 1 {
			break
		}
		cut--
	}
	return s[:cut] + marker
}

// buildCreateFileCommand returns the shell program that creates path containing
// fileText. Ported from aperture chat/sandbox/tool_create_file.go. The
// base64+heredoc dodge keeps the call bounded by the pipe buffer rather than
// ARG_MAX (~128 KiB on Linux).
func buildCreateFileCommand(path, fileText string) string {
	b64 := base64.StdEncoding.EncodeToString([]byte(fileText))
	qpath := shellQuote(path)
	return fmt.Sprintf(
		"mkdir -p \"$(dirname %s)\" && base64 -d > %s <<'B64EOF'\n%s\nB64EOF\n",
		qpath, qpath, b64,
	)
}

// str_replace has no command builder: it is implemented host-side (see
// mcpStrReplaceToolHandler) because thundersnap's default nil:nil:nil frames
// ship no python3 and the daemon has direct rootfs access. The other tools'
// builders (shellQuote, buildViewCommand, buildCreateFileCommand) remain
// exec-based below.

// --- Tool timeout constants (match aperture + design doc) ------------------

const (
	mcpBashDefaultTimeout = 600 * time.Second  // 10 min default; stated in tool description
	mcpBashMaxTimeout     = 7200 * time.Second // 120 min hard ceiling; clamp over this
	mcpViewTimeout        = 30 * time.Second
	mcpCreateFileTimeout  = 30 * time.Second
	mcpStrReplaceTimeout  = 30 * time.Second
	mcpMaxViewOutput      = 16_000
)

// --- Tool handlers ---------------------------------------------------------
//
// Each handler is an mcp.ToolHandler: func(ctx, *CallToolRequest) (*CallToolResult, error).
// Setup failures (resolve/dial) are returned as Go errors (protocol errors);
// command non-zero exits and "soft" failures (file not found, string not
// unique) are returned as CallToolResult with IsError=true, matching aperture's
// chatToolToMCPHandler convention (the LLM must see these to self-correct).

// textResult builds a single-TextContent CallToolResult. isError=true marks a
// tool-level failure (non-zero exit, file not found, string not unique,
// timeout, invalid input) so the LLM sees it and can self-correct; the output
// text is always preserved in Content. This mirrors aperture's
// chatToolToMCPHandler convention: protocol/transport errors come back as Go
// errors from the handler, while command/soft failures come back as
// IsError=true results so the LLM can read the output and retry.
func textResult(text string, isError bool) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
		IsError: isError,
	}, nil
}

// mcpBashToolHandler is the ToolHandler for thundersnap_bash.
func mcpBashToolHandler(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var params struct {
		Command string `json:"command"`
		Frame   string `json:"frame"`
		Workdir string `json:"workdir"`
		Timeout int    `json:"timeout"`
	}
	if req.Params != nil && len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &params); err != nil {
			return textResult(fmt.Sprintf("invalid input: %v", err), true)
		}
	}
	if params.Command == "" {
		return textResult("command is required", true)
	}

	timeout := mcpBashDefaultTimeout
	if params.Timeout > 0 {
		timeout = time.Duration(params.Timeout) * time.Second
		if timeout > mcpBashMaxTimeout {
			timeout = mcpBashMaxTimeout
		}
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	res, err := runInFrame(ctx, mcpUserFromContext(ctx), params.Frame, params.Workdir, params.Command)
	if err != nil {
		if errors.Is(err, errMCPCommandTimeout) && res != nil {
			// Timeout: runInFrame already appended a marker to res.Output;
			// surface the partial output as an error result.
			return textResult(mcpexec.FormatExit(res), true)
		}
		return textResult(fmt.Sprintf("exec failed: %v", err), true)
	}
	// A non-zero exit is a tool-level failure (the LLM should see the output
	// and self-correct), not a protocol error.
	return textResult(mcpexec.FormatExit(res), res.ExitCode != 0)
}

// mcpViewToolHandler is the ToolHandler for thundersnap_view.
func mcpViewToolHandler(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var params struct {
		Path      string `json:"path"`
		ViewRange []int  `json:"view_range"`
		Frame     string `json:"frame"`
	}
	if req.Params != nil && len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &params); err != nil {
			return textResult(fmt.Sprintf("invalid input: %v", err), true)
		}
	}
	if params.Path == "" {
		return textResult("path is required", true)
	}
	cmd, err := buildViewCommand(params.Path, params.ViewRange)
	if err != nil {
		return textResult(err.Error(), true)
	}

	ctx, cancel := context.WithTimeout(ctx, mcpViewTimeout)
	defer cancel()

	res, err := runInFrame(ctx, mcpUserFromContext(ctx), params.Frame, "", cmd)
	if err != nil {
		if errors.Is(err, errMCPCommandTimeout) && res != nil {
			return textResult(truncateUTF8(res.Output, mcpMaxViewOutput, "\n\n... output truncated ..."), true)
		}
		return textResult(fmt.Sprintf("view failed: %v", err), true)
	}
	// A non-zero exit (e.g. path not found) is a tool-level failure. The view
	// command writes its own "Error: <path> not found" to stderr, so the
	// output already carries the message; just mark it IsError.
	return textResult(truncateUTF8(res.Output, mcpMaxViewOutput, "\n\n... output truncated ..."), res.ExitCode != 0)
}

// mcpCreateFileToolHandler is the ToolHandler for thundersnap_create_file.
func mcpCreateFileToolHandler(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var params struct {
		Path     string `json:"path"`
		FileText string `json:"file_text"`
		Frame    string `json:"frame"`
	}
	if req.Params != nil && len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &params); err != nil {
			return textResult(fmt.Sprintf("invalid input: %v", err), true)
		}
	}
	if params.Path == "" {
		return textResult("path is required", true)
	}
	cmd := buildCreateFileCommand(params.Path, params.FileText)

	ctx, cancel := context.WithTimeout(ctx, mcpCreateFileTimeout)
	defer cancel()

	res, err := runInFrame(ctx, mcpUserFromContext(ctx), params.Frame, "", cmd)
	if err != nil {
		if errors.Is(err, errMCPCommandTimeout) && res != nil {
			return textResult(res.Output, true)
		}
		return textResult(fmt.Sprintf("create_file failed: %v", err), true)
	}
	if res.ExitCode != 0 {
		return textResult(res.Output, true)
	}
	return textResult(fmt.Sprintf("Created %s (%d bytes)", params.Path, len(params.FileText)), false)
}

// mcpStrReplaceToolHandler is the ToolHandler for thundersnap_str_replace.
//
// Unlike the other tools, str_replace does NOT run a command in the frame.
// Aperture's tool_str_replace.go pipes an embedded Python program over a
// heredoc because Aperture's sandbox is a remote HTTP backend with no direct
// filesystem access — it must run python3 *inside* the sandbox. Thundersnap's
// daemon, by contrast, has direct host access to the frame's rootfs (a local
// btrfs subvolume), so the read/count/replace/write is done in-process on the
// host. This is a deliberate, documented deviation from the design doc's
// "byte-identical to Aperture" note: thundersnap's default nil:nil:nil frames
// ship no python3 (and no /lib64 for a dynamic one), so the Python approach
// fails there. The host-side implementation is binary-safe (it operates on
// raw []byte, so non-UTF-8 files survive byte-for-byte — the same property
// Aperture's surrogateescape buys) and works in every frame, minimal or full.
// The contract is unchanged: error on 0 or >1 occurrences, replace exactly
// once, preserve the file's existing mode/owner.
func mcpStrReplaceToolHandler(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var params struct {
		Path   string `json:"path"`
		OldStr string `json:"old_str"`
		NewStr string `json:"new_str"`
		Frame  string `json:"frame"`
	}
	if req.Params != nil && len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &params); err != nil {
			return textResult(fmt.Sprintf("invalid input: %v", err), true)
		}
	}
	if params.Path == "" {
		return textResult("path is required", true)
	}
	if params.OldStr == "" {
		return textResult("old_str is required", true)
	}

	user := mcpUserFromContext(ctx)
	if user == "" {
		return textResult("no MCP user resolved for request", true)
	}

	ctx, cancel := context.WithTimeout(ctx, mcpStrReplaceTimeout)
	defer cancel()

	rootFS, _, err := resolveFrameRootFS(user, params.Frame)
	if err != nil {
		return textResult(fmt.Sprintf("resolve frame: %v", err), true)
	}
	if err := prepareContainerRootFS(rootFS, ""); err != nil {
		return textResult(fmt.Sprintf("prepare frame rootfs: %v", err), true)
	}

	// Resolve the in-frame path to a host path under rootFS, refusing to
	// escape the frame (a path like /../../etc/shadow must not reach the host).
	rel := strings.TrimPrefix(filepath.Clean("/"+params.Path), "/")
	hostPath := filepath.Join(rootFS, rel)
	if !isWithinRootFS(hostPath, rootFS) {
		return textResult(fmt.Sprintf("path %q escapes the frame", params.Path), true)
	}

	content, err := os.ReadFile(hostPath)
	if err != nil {
		if os.IsNotExist(err) {
			return textResult(fmt.Sprintf("Error: %s not found", params.Path), true)
		}
		return textResult(fmt.Sprintf("read %s: %v", params.Path, err), true)
	}

	oldBytes := []byte(params.OldStr)
	count := bytes.Count(content, oldBytes)
	if count == 0 {
		return textResult(fmt.Sprintf("Error: string not found in %s", params.Path), true)
	}
	if count > 1 {
		return textResult(fmt.Sprintf("Error: string appears %d times in %s (must be unique)", count, params.Path), true)
	}

	newContent := bytes.Replace(content, oldBytes, []byte(params.NewStr), 1)
	// Preserve the existing file mode; os.WriteFile truncates the existing
	// inode (no chown, no mode change for an existing file) so owner/perm
	// survive. Stat first to surface a directory/pipe/etc. as a clean error.
	info, err := os.Stat(hostPath)
	if err != nil {
		return textResult(fmt.Sprintf("stat %s: %v", params.Path, err), true)
	}
	if err := os.WriteFile(hostPath, newContent, info.Mode()); err != nil {
		return textResult(fmt.Sprintf("write %s: %v", params.Path, err), true)
	}

	if ctx.Err() == context.DeadlineExceeded {
		return textResult(fmt.Sprintf("str_replace timed out writing %s", params.Path), true)
	}
	return textResult(fmt.Sprintf("Replaced in %s", params.Path), false)
}

// mcpListFramesToolHandler is the ToolHandler for thundersnap_list_frames. It
// does NOT launch a container: it reads the user's frame + ref stores directly,
// so the LLM can pick an existing frame for its first bash call without
// auto-creating a throwaway. Mirrors handleListFrames' logic.
func mcpListFramesToolHandler(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	user := mcpUserFromContext(ctx)
	if user == "" {
		return textResult("no MCP user resolved for request", true)
	}
	frameStore := userFrameStore(user)
	refStore := userRefStore(user)

	uuids, err := frameStore.List()
	if err != nil {
		return textResult(fmt.Sprintf("list frames: %v", err), true)
	}

	refByUUID := map[frameid.ID]string{}
	if names, err := refStore.List(); err == nil {
		for _, name := range names {
			if ref, err := refStore.Get(name); err == nil {
				refByUUID[ref.UUID] = name
			}
		}
	}

	type frameInfo struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	var frames []frameInfo
	for _, uuid := range uuids {
		name := uuid.String()
		if refName, ok := refByUUID[uuid]; ok {
			name = refName
		}
		sessionCount := getActiveFrameCount(framePathForUserUUID(user, uuid))
		status := "stopped"
		if sessionCount > 0 {
			status = fmt.Sprintf("%d", sessionCount)
		}
		frames = append(frames, frameInfo{Name: name, Status: status})
	}
	if frames == nil {
		frames = []frameInfo{}
	}
	out, err := json.Marshal(map[string]any{"frames": frames})
	if err != nil {
		return textResult(fmt.Sprintf("marshal frames: %v", err), true)
	}
	return textResult(string(out), false)
}

// mcpListRefsToolHandler is the ToolHandler for thundersnap_list_refs. It does
// NOT launch a container. Mirrors handleListRefs' logic.
func mcpListRefsToolHandler(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	user := mcpUserFromContext(ctx)
	if user == "" {
		return textResult("no MCP user resolved for request", true)
	}
	refStore := userRefStore(user)
	names, err := refStore.List()
	if err != nil {
		return textResult(fmt.Sprintf("list refs: %v", err), true)
	}
	type refEntry struct {
		Name    string   `json:"name"`
		UUID    string   `json:"uuid"`
		Autorun []string `json:"autorun,omitempty"`
	}
	var refs []refEntry
	for _, name := range names {
		ref, err := refStore.Get(name)
		if err != nil {
			continue
		}
		refs = append(refs, refEntry{Name: name, UUID: ref.UUID.String(), Autorun: ref.Autorun})
	}
	if refs == nil {
		refs = []refEntry{}
	}
	out, err := json.Marshal(map[string]any{"refs": refs})
	if err != nil {
		return textResult(fmt.Sprintf("marshal refs: %v", err), true)
	}
	return textResult(string(out), false)
}

// --- MCP server factory + HTTP mount ---------------------------------------

// newMCPServer builds the MCP server with all six tools registered. It is
// called once per handler mount (production main() and test runTestMode each
// build their own). The server name/version are the thundersnap daemon's.
func newMCPServer() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{
		Name:    "thundersnap",
		Title:   "Thundersnap Sandbox",
		Version: mcpDaemonVersion,
	}, &mcp.ServerOptions{
		Instructions: "Thundersnap sandbox tools: run shell commands, view/edit " +
			"files, and list frames/refs inside thundersnap VM frames. Every " +
			"tool call runs `sh -c` in a fresh non-PTY one-shot inside the " +
			"named frame (defaulting to the user's current frame).",
	})

	// bash
	s.AddTool(&mcp.Tool{
		Name: "thundersnap_bash",
		Description: "Run a bash command in a thundersnap frame. The command " +
			"runs as `sh -c <command>` in /work (override with workdir) inside " +
			"the named frame, as root. Output (stdout+stderr interleaved) is " +
			"collected up to 1 MiB; the exit code is reported. Default timeout " +
			"600s, max 7200s (override with timeout in seconds). A " +
			"non-zero exit code is returned as an error result, not a protocol " +
			"error, so you can see the output and self-correct.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "The bash command to run.",
				},
				"frame": map[string]any{
					"type":        "string",
					"description": "Frame name or UUID to run in. Defaults to the user's current/only frame.",
				},
				"workdir": map[string]any{
					"type":        "string",
					"description": "Working directory inside the frame (default /work).",
				},
				"timeout": map[string]any{
					"type":        "integer",
					"description": "Timeout in seconds (default 600, max 7200).",
				},
			},
			"required": []string{"command"},
		},
	}, mcpBashToolHandler)

	// view
	s.AddTool(&mcp.Tool{
		Name: "thundersnap_view",
		Description: "View a file or directory listing in a thundersnap frame. " +
			"For a file, prints lines with line numbers (use view_range " +
			"[start, end] to limit; end=-1 means to EOF). For a directory, " +
			"lists up to 200 entries (maxdepth 2). Output is truncated to " +
			"16K bytes. A non-zero exit (e.g. file not found) is an error result.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Absolute path inside the frame to view.",
				},
				"view_range": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "integer"},
					"description": "Optional [start_line, end_line] to limit file output. end_line=-1 means EOF.",
				},
				"frame": map[string]any{
					"type":        "string",
					"description": "Frame name or UUID. Defaults to the user's current/only frame.",
				},
			},
			"required": []string{"path"},
		},
	}, mcpViewToolHandler)

	// create_file
	s.AddTool(&mcp.Tool{
		Name: "thundersnap_create_file",
		Description: "Create a file in a thundersnap frame with the given " +
			"content. Overwrites if the file exists. Parent directories are " +
			"created. The content is base64-encoded over a heredoc to avoid " +
			"ARG_MAX limits, so binary-safe up to the 1 MiB output cap.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Absolute path inside the frame to create.",
				},
				"file_text": map[string]any{
					"type":        "string",
					"description": "The full text content of the file.",
				},
				"frame": map[string]any{
					"type":        "string",
					"description": "Frame name or UUID. Defaults to the user's current/only frame.",
				},
			},
			"required": []string{"path", "file_text"},
		},
	}, mcpCreateFileToolHandler)

	// str_replace
	s.AddTool(&mcp.Tool{
		Name: "thundersnap_str_replace",
		Description: "Replace a unique string in a file in a thundersnap frame. " +
			"old_str must appear exactly once or the call fails (use " +
			"thundersnap_view first to find context). Uses Python with " +
			"surrogateescape so non-UTF-8 files survive byte-for-byte.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Absolute path inside the frame to edit.",
				},
				"old_str": map[string]any{
					"type":        "string",
					"description": "The exact string to replace. Must be unique in the file.",
				},
				"new_str": map[string]any{
					"type":        "string",
					"description": "The replacement string.",
				},
				"frame": map[string]any{
					"type":        "string",
					"description": "Frame name or UUID. Defaults to the user's current/only frame.",
				},
			},
			"required": []string{"path", "old_str", "new_str"},
		},
	}, mcpStrReplaceToolHandler)

	// list_frames
	s.AddTool(&mcp.Tool{
		Name: "thundersnap_list_frames",
		Description: "List the caller's thundersnap frames (does NOT launch a " +
			"container). Returns JSON {\"frames\":[{\"name\",\"status\"}]} where " +
			"status is \"stopped\" or the active session count. Use this to pick " +
			"a frame for the first bash/view/create_file/str_replace call.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}, mcpListFramesToolHandler)

	// list_refs
	s.AddTool(&mcp.Tool{
		Name: "thundersnap_list_refs",
		Description: "List the caller's thundersnap refs (named frame pointers) " +
			"(does NOT launch a container). Returns JSON " +
			"{\"refs\":[{\"name\",\"uuid\",\"autorun\"}]}.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}, mcpListRefsToolHandler)

	return s
}

// mcpHTTPHandler returns the http.Handler for the /v1/mcp endpoint: the
// streamable MCP handler wrapped in the auth middleware that resolves the
// per-request user. The getServer closure returns the singleton server built
// by newMCPServer(); the SDK calls it once per incoming request.
func mcpHTTPHandler() http.Handler {
	server := newMCPServer()
	streamable := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, nil)
	return mcpAuthMiddleware(streamable)
}

// mountMCP registers the /v1/mcp endpoint on the given mux. Called by both
// the production httpMux (in main()) and the test-mode mux (in runTestMode).
func mountMCP(mux *http.ServeMux) {
	mux.Handle("/v1/mcp", mcpHTTPHandler())
}
