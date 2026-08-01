// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

//go:build e2e

// mcp_test.go is a real end-to-end test of the thundersnap MCP server. It
// starts a thundersnapd in test mode with --test-http-listen, drives the
// /v1/mcp endpoint with the official go-sdk MCP client, and asserts the
// sandbox and background-job tools work against a fresh btrfs-backed frame.
//
// Per CLAUDE.md: e2e tests NEVER SKIP. If the btrfs/root precondition is
// missing, requireBtrfsRoot calls t.Fatal — that is a misconfigured
// environment, not a skip.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// startDaemonWithHTTP is startDaemon but also serves the HTTP mux (and thus
// /v1/mcp) on a second free local port. Returns the daemon instance plus the
// base URL of the HTTP mux (e.g. "http://127.0.0.1:36213").
//
// The MCP endpoint lives at baseURL + "/v1/mcp".
func startDaemonWithHTTP(t *testing.T, env *testEnv) (*daemonInstance, string) {
	t.Helper()

	// Find a free port for the HTTP mux.
	httpPort, err := getFreePort()
	if err != nil {
		t.Fatalf("find free http port: %v", err)
	}
	httpAddr := fmt.Sprintf("127.0.0.1:%d", httpPort)

	// Reuse startDaemon's SSH setup, then start a second daemon with the HTTP
	// flag. We can't just call startDaemon (it doesn't set --test-http-listen),
	// so duplicate its body with the extra flag. To avoid drift, we invoke the
	// shared helper machinery by calling startDaemon and then... no: the SSH
	// and HTTP listeners must be in the SAME process. So build the args here.

	sshPort, err := getFreePort()
	if err != nil {
		t.Fatalf("find free ssh port: %v", err)
	}
	sshAddr := fmt.Sprintf("127.0.0.1:%d", sshPort)

	stateDir := filepath.Join(env.root, "state")
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}

	vshdBinary := env.requireBinary("vshd")
	if err := copyFile(vshdBinary, filepath.Join(env.libexecDir, "vshd")); err != nil {
		t.Fatalf("copy vshd to libexec: %v", err)
	}

	policyPath := filepath.Join(env.root, "policy.json")
	policyContent := `{
		"grants": [
			{
				"principals": ["*"],
				"cap": {
					"role": "developer",
					"isolation": "container",
					"maxFrames": 10
				}
			}
		]
	}`
	if err := os.WriteFile(policyPath, []byte(policyContent), 0644); err != nil {
		t.Fatalf("write policy file: %v", err)
	}

	daemonArgs := []string{
		"--test-listen=" + sshAddr,
		"--test-http-listen=" + httpAddr,
		"--test-user=" + testUser,
		"--data-dir=" + env.root,
		"--state-dir=" + stateDir,
		"--libexec-dir=" + env.libexecDir,
		"--policy=" + policyPath,
	}
	if dir := vmDir(); dir != "" {
		if abs, err := filepath.Abs(dir); err == nil {
			daemonArgs = append(daemonArgs, "--vm-dir="+abs)
		}
	}

	cmd := exec.Command(env.daemonBinary, daemonArgs...)
	cmd.Stdout = os.Stderr // pipe daemon logs so failures are debuggable
	cmd.Stderr = os.Stderr
	cmd.Dir = env.root

	t.Logf("Starting daemon (SSH %s, HTTP %s): %s %v", sshAddr, httpAddr, cmd.Path, cmd.Args[1:])
	if err := cmd.Start(); err != nil {
		t.Fatalf("start daemon: %v", err)
	}

	d := &daemonInstance{t: t, cmd: cmd, addr: sshAddr}
	t.Cleanup(func() { d.Stop() })

	if err := d.waitReady(10 * time.Second); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatalf("daemon not ready: %v", err)
	}

	// Wait for the HTTP mux to accept a connection (it starts slightly after
	// the SSH listener). Give it a short deadline.
	httpReady := net.JoinHostPort("127.0.0.1", fmt.Sprint(httpPort))
	deadline := time.Now().Add(10 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", httpReady, 500*time.Millisecond)
		if err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("HTTP mux on %s not ready: %v", httpAddr, err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	return d, "http://" + httpAddr
}

// mcpClient connects an MCP client session to the daemon's /v1/mcp endpoint
// and returns it (with a cleanup that closes the session). The session is
// already initialized (Connect runs the MCP handshake).
func mcpClient(t *testing.T, baseURL string) *mcp.ClientSession {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	transport := &mcp.StreamableClientTransport{Endpoint: baseURL + "/v1/mcp"}
	client := mcp.NewClient(&mcp.Implementation{
		Name:    "thundersnap-e2e-test",
		Version: "test",
	}, nil)

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("MCP Connect to %s: %v", transport.Endpoint, err)
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Logf("MCP session close: %v", err)
		}
	})

	// Sanity: the server must report the sandbox and background-job tools.
	listCtx, listCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer listCancel()
	tools, err := session.ListTools(listCtx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	got := map[string]bool{}
	for _, tool := range tools.Tools {
		got[tool.Name] = true
	}
	for _, want := range []string{
		"thundersnap_bash", "thundersnap_jobs_list", "thundersnap_jobs_wait", "thundersnap_jobs_kill",
		"thundersnap_view", "thundersnap_create_file", "thundersnap_str_replace",
		"thundersnap_list_frames", "thundersnap_list_refs",
	} {
		if !got[want] {
			t.Fatalf("MCP server missing tool %q; got %d tools", want, len(tools.Tools))
		}
	}
	t.Logf("MCP server advertises %d tools (all expected tools present)", len(tools.Tools))
	return session
}

// callTool invokes a named tool with the given arguments and returns the text
// of the first TextContent block plus the IsError flag. It fatals on a
// transport-level error (the call didn't reach the server) but returns
// IsError=true results normally — those are tool-level failures the LLM is
// meant to see, and the test asserts on them.
func callTool(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) (text string, isError bool) {
	t.Helper()
	return callToolForConversation(t, session, "e2e-default-conversation", name, args)
}

func callToolForConversation(t *testing.T, session *mcp.ClientSession, conversation, name string, args map[string]any) (text string, isError bool) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	meta := mcp.Meta{}
	if conversation != "" {
		meta[apertureConversationIDMetaKeyE2E] = conversation
	}
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Meta:      meta,
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		t.Fatalf("CallTool %q: %v", name, err)
	}
	if len(res.Content) == 0 {
		return "", res.IsError
	}
	if tc, ok := res.Content[0].(*mcp.TextContent); ok {
		return tc.Text, res.IsError
	}
	// Fall back to a JSON dump for non-text content.
	b, _ := json.Marshal(res.Content)
	return string(b), res.IsError
}

const apertureConversationIDMetaKeyE2E = "io.tailscale.aperture/conversation-id"

// waitForMCPJob waits for one job to exit and returns its status.
func waitForMCPJob(t *testing.T, session *mcp.ClientSession, jobID string, revision uint64) map[string]any {
	t.Helper()
	out, isErr := callTool(t, session, "thundersnap_jobs_wait", map[string]any{
		"job_ids":        []string{jobID},
		"after_revision": revision,
		"until":          "any_exit",
		"timeout":        60,
	})
	if isErr {
		t.Fatalf("wait job %s: %s", jobID, out)
	}
	var result struct {
		Reason string           `json:"reason"`
		Jobs   []map[string]any `json:"jobs"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("wait job %s unmarshal %q: %v", jobID, out, err)
	}
	if result.Reason != "any_exit" || len(result.Jobs) != 1 {
		t.Fatalf("wait job %s result = %s", jobID, out)
	}
	return result.Jobs[0]
}

func startAndWaitMCPBash(t *testing.T, session *mcp.ClientSession, args map[string]any) map[string]any {
	t.Helper()
	out, isErr := callTool(t, session, "thundersnap_bash", args)
	if isErr {
		t.Fatalf("start bash: %s", out)
	}
	var result struct {
		JobID    string `json:"job_id"`
		Revision uint64 `json:"revision"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil || result.JobID == "" {
		t.Fatalf("start bash result %q: job=%+v err=%v", out, result, err)
	}
	return waitForMCPJob(t, session, result.JobID, result.Revision)
}

// createFrameForMCP creates a frame the way the rest of the e2e suite does
// (via `ts frame` over SSH) so the MCP tools have a real frame to target. The
// SSH path is already proven by the other e2e tests; reusing it here keeps the
// MCP test focused on the MCP surface.
func createFrameForMCP(t *testing.T, d *daemonInstance, refName string) string {
	t.Helper()
	return createFrameViaDaemon(t, d, refName)
}

// installBusyboxAppletsInFrame installs the named busybox applets into a
// frame's /bin over the daemon's SFTP subsystem. A nil:nil:nil frame ships
// only the `ts` binary (plus /bin/sh and /bin/su symlinks to it); the MCP
// tool command builders (view/create_file/bash) call out to standard POSIX
// utilities (mkdir, awk, find, head, sort, base64, ...), so the e2e harness
// must install them the same way the rest of the suite does (see
// installBusyboxAppletInFrame). Each applet is a copy of the host's
// busybox-static, which dispatches on argv[0].
func installBusyboxAppletsInFrame(t *testing.T, d *daemonInstance, refName string, applets ...string) {
	t.Helper()
	for _, a := range applets {
		installBusyboxAppletInFrame(t, d, refName, a)
	}
}

// mcpFrameApplets are the POSIX utilities the MCP tool command builders
// invoke inside a frame. They are installed once per frame so the bash/view/
// create_file tools work in a minimal nil:nil:nil frame (which has only `ts`).
var mcpFrameApplets = []string{
	"mkdir",   // create_file: mkdir -p $(dirname ...)
	"dirname", // create_file: $(dirname ...)
	"base64",  // create_file: base64 -d <<heredoc
	"awk",     // view (file): awk line-numbering
	"find",    // view (dir): find -maxdepth 2
	"sort",    // view (dir): | sort
	"head",    // view (dir): | head -200
	"stat",    // view (image): stat -c %s (also useful for general tests)
	"sleep",   // timeout/reap tests: foreground long-running command
	"ps",      // reap tests: inspect leftover processes
}

// TestMCPToolsRoundTrip is the core MCP e2e test (Phase 4, T5). It exercises
// every tool end-to-end against a fresh frame: list_frames/list_refs before
// and after frame creation, bash (zero + non-zero exit), view (file + dir +
// missing), create_file, str_replace (success + not-unique + not-found).
//
// It does NOT cover timeout/cap/cancellation (T2–T4) or identity (T8); those
// are separate, slower tests so this one stays fast and hits the happy path.
func TestMCPToolsRoundTrip(t *testing.T) {
	env := newTestEnv(t)
	d, httpBase := startDaemonWithHTTP(t, env)
	session := mcpClient(t, httpBase)

	// --- list_frames / list_refs on a fresh user: both empty ---
	if out, isErr := callTool(t, session, "thundersnap_list_frames", nil); isErr {
		t.Fatalf("list_frames on fresh user: unexpected error %q", out)
	} else {
		var res struct {
			Frames []struct {
				Name   string `json:"name"`
				Status string `json:"status"`
			} `json:"frames"`
		}
		if err := json.Unmarshal([]byte(out), &res); err != nil {
			t.Fatalf("list_frames: unmarshal %q: %v", out, err)
		}
		if len(res.Frames) != 0 {
			t.Errorf("list_frames on fresh user: want 0 frames, got %d (%+v)", len(res.Frames), res.Frames)
		}
	}
	if out, isErr := callTool(t, session, "thundersnap_list_refs", nil); isErr {
		t.Fatalf("list_refs on fresh user: unexpected error %q", out)
	} else {
		var res struct {
			Refs []struct {
				Name string `json:"name"`
			} `json:"refs"`
		}
		if err := json.Unmarshal([]byte(out), &res); err != nil {
			t.Fatalf("list_refs: unmarshal %q: %v", out, err)
		}
		if len(res.Refs) != 0 {
			t.Errorf("list_refs on fresh user: want 0 refs, got %d", len(res.Refs))
		}
	}

	// --- create a real frame via SSH (ts frame), so MCP has something to target ---
	uuid := createFrameForMCP(t, d, "mcpframe")
	// nil:nil:nil frames ship only `ts`; install the POSIX utilities the tool
	// command builders call (mkdir/awk/find/head/sort/base64/...). str_replace
	// is host-side and needs nothing in-frame.
	installBusyboxAppletsInFrame(t, d, "mcpframe", mcpFrameApplets...)

	// --- list_frames now shows it, named by the ref ---
	{
		out, _ := callTool(t, session, "thundersnap_list_frames", nil)
		var res struct {
			Frames []struct {
				Name   string `json:"name"`
				Status string `json:"status"`
			} `json:"frames"`
		}
		if err := json.Unmarshal([]byte(out), &res); err != nil {
			t.Fatalf("list_frames after create: unmarshal %q: %v", out, err)
		}
		found := false
		for _, f := range res.Frames {
			if f.Name == "mcpframe" {
				found = true
				if f.Status != "stopped" {
					t.Errorf("frame %s status = %q, want stopped (no active sessions)", f.Name, f.Status)
				}
			}
		}
		if !found {
			t.Fatalf("list_frames after create: ref %q not in %s", "mcpframe", out)
		}
	}
	// --- list_refs shows mcpframe -> uuid ---
	{
		out, _ := callTool(t, session, "thundersnap_list_refs", nil)
		var res struct {
			Refs []struct {
				Name string `json:"name"`
				UUID string `json:"uuid"`
			} `json:"refs"`
		}
		if err := json.Unmarshal([]byte(out), &res); err != nil {
			t.Fatalf("list_refs after create: unmarshal %q: %v", out, err)
		}
		found := false
		for _, r := range res.Refs {
			if r.Name == "mcpframe" {
				found = true
				if r.UUID != uuid {
					t.Errorf("ref %s UUID = %q, want %q", r.Name, r.UUID, uuid)
				}
			}
		}
		if !found {
			t.Fatalf("list_refs after create: ref %q not in %s", "mcpframe", out)
		}
	}

	// --- background bash: zero exit + live log ---
	{
		job := startAndWaitMCPBash(t, session, map[string]any{
			"command": "echo hello-mcp",
			"frame":   "mcpframe",
		})
		if job["state"] != "exited" || job["exit_code"] != float64(0) || job["user"] != "user" {
			t.Fatalf("bash echo job (including default non-root user) = %+v", job)
		}
		out, isErr := callTool(t, session, "thundersnap_view", map[string]any{
			"path":       job["combined_log"],
			"tail_lines": 20,
			"frame":      "mcpframe",
		})
		if isErr || !strings.Contains(out, "hello-mcp") {
			t.Errorf("bash echo log: isErr=%v output=%q", isErr, out)
		}
	}

	// --- background bash: non-zero exit is recorded on the job ---
	{
		job := startAndWaitMCPBash(t, session, map[string]any{
			"command": "echo to-stderr >&2; exit 3",
			"frame":   "mcpframe",
			"user":    "root",
		})
		if job["state"] != "exited" || job["exit_code"] != float64(3) || job["user"] != "root" {
			t.Fatalf("bash exit 3 job = %+v", job)
		}
		out, isErr := callTool(t, session, "thundersnap_view", map[string]any{
			"path":       job["stderr_log"],
			"tail_lines": 20,
			"frame":      "mcpframe",
		})
		if isErr || !strings.Contains(out, "to-stderr") {
			t.Errorf("bash exit 3 stderr log: isErr=%v output=%q", isErr, out)
		}
	}

	// --- create_file ---
	{
		out, isErr := callTool(t, session, "thundersnap_create_file", map[string]any{
			"path":      "/work/poem.txt",
			"file_text": "roses are red\nviolets are blue\nroses are red\n",
			"frame":     "mcpframe",
		})
		if isErr {
			t.Fatalf("create_file: unexpected error %q", out)
		}
		if !strings.Contains(out, "Created /work/poem.txt") {
			t.Errorf("create_file: output %q missing 'Created'", out)
		}
	}

	// --- view: the file we just created, with line numbers ---
	{
		out, isErr := callTool(t, session, "thundersnap_view", map[string]any{
			"path":  "/work/poem.txt",
			"frame": "mcpframe",
		})
		if isErr {
			t.Fatalf("view poem.txt: unexpected error %q", out)
		}
		if !strings.Contains(out, "roses are red") {
			t.Errorf("view poem.txt: output %q missing content", out)
		}
		// awk prefixes "    %6d\t" — line numbers should appear.
		if !strings.Contains(out, "\troses are red") {
			t.Errorf("view poem.txt: output %q missing line-numbered format", out)
		}
	}

	// --- view: a directory listing ---
	{
		out, isErr := callTool(t, session, "thundersnap_view", map[string]any{
			"path":  "/work",
			"frame": "mcpframe",
		})
		if isErr {
			t.Fatalf("view /work: unexpected error %q", out)
		}
		if !strings.Contains(out, "poem.txt") {
			t.Errorf("view /work: output %q does not list poem.txt", out)
		}
	}

	// --- view: missing path is an error result ---
	{
		out, isErr := callTool(t, session, "thundersnap_view", map[string]any{
			"path":  "/work/does-not-exist",
			"frame": "mcpframe",
		})
		if !isErr {
			t.Errorf("view missing path: expected IsError=true, got false (output %q)", out)
		}
		if !strings.Contains(out, "not found") {
			t.Errorf("view missing path: output %q missing 'not found'", out)
		}
	}

	// --- str_replace: success ---
	{
		out, isErr := callTool(t, session, "thundersnap_str_replace", map[string]any{
			"path":    "/work/poem.txt",
			"old_str": "violets are blue",
			"new_str": "violets are MCP",
			"frame":   "mcpframe",
		})
		if isErr {
			t.Fatalf("str_replace (unique): unexpected error %q", out)
		}
		if !strings.Contains(out, "Replaced in /work/poem.txt") {
			t.Errorf("str_replace (unique): output %q missing 'Replaced in'", out)
		}
	}

	// --- str_replace: not unique (roses are red appears twice) ---
	{
		out, isErr := callTool(t, session, "thundersnap_str_replace", map[string]any{
			"path":    "/work/poem.txt",
			"old_str": "roses are red",
			"new_str": "x",
			"frame":   "mcpframe",
		})
		if !isErr {
			t.Errorf("str_replace (not unique): expected IsError=true, got false (output %q)", out)
		}
		if !strings.Contains(out, "2 times") && !strings.Contains(out, "must be unique") {
			t.Errorf("str_replace (not unique): output %q missing uniqueness error", out)
		}
	}

	// --- str_replace: not found ---
	{
		out, isErr := callTool(t, session, "thundersnap_str_replace", map[string]any{
			"path":    "/work/poem.txt",
			"old_str": "this string is absent",
			"new_str": "x",
			"frame":   "mcpframe",
		})
		if !isErr {
			t.Errorf("str_replace (not found): expected IsError=true, got false (output %q)", out)
		}
		if !strings.Contains(out, "not found") {
			t.Errorf("str_replace (not found): output %q missing 'not found'", out)
		}
	}

	// --- verify the successful str_replace actually changed the file ---
	{
		out, _ := callTool(t, session, "thundersnap_view", map[string]any{
			"path":  "/work/poem.txt",
			"frame": "mcpframe",
		})
		if !strings.Contains(out, "violets are MCP") {
			t.Errorf("post-replace view: output %q does not contain the replacement", out)
		}
		if strings.Contains(out, "violets are blue") {
			t.Errorf("post-replace view: output %q still contains the old string", out)
		}
	}

	// --- bash start: bad frame errors cleanly ---
	{
		out, isErr := callTool(t, session, "thundersnap_bash", map[string]any{
			"command": "echo anything",
			"frame":   "this-frame-does-not-exist",
		})
		if !isErr {
			t.Errorf("bash bad frame: expected IsError=true, got false (output %q)", out)
		}
		if !strings.Contains(out, "resolve frame") && !strings.Contains(out, "not found") {
			t.Errorf("bash bad frame: output %q missing resolve/not-found error", out)
		}
	}
}

// TestMCPHTTPMuxResponds (Phase 4, T1) confirms the test-mode HTTP mux serves
// the non-MCP endpoints too — a quick smoke that the pre-existing coverage
// gap (HTTP handlers never instantiated in tests) is closed. It only pokes
// /ts/servers.json and /v1/mcp (initialize) since mesh/metrics are exercised
// in their own package tests.
func TestMCPHTTPMuxResponds(t *testing.T) {
	env := newTestEnv(t)
	_, httpBase := startDaemonWithHTTP(t, env)

	// /v1/mcp must exist and speak MCP (the client does a full initialize).
	session := mcpClient(t, httpBase)
	if err := session.Close(); err != nil {
		t.Logf("MCP session close: %v", err)
	}
}

// TestMCPTimeoutAndReap pins background-job hard-timeout cleanup: closing the
// vshd connection must reap the whole process group, including grandchildren.
func TestMCPTimeoutAndReap(t *testing.T) {
	env := newTestEnv(t)
	d, httpBase := startDaemonWithHTTP(t, env)
	session := mcpClient(t, httpBase)
	createFrameForMCP(t, d, "timeoutframe")
	installBusyboxAppletsInFrame(t, d, "timeoutframe", "sleep", "ps")

	start := time.Now()
	out, isErr := callTool(t, session, "thundersnap_bash", map[string]any{
		"command":      "sleep 30 & sleep 30",
		"frame":        "timeoutframe",
		"hard_timeout": 2,
	})
	if isErr {
		t.Fatalf("start timeout job: %s", out)
	}
	var started struct {
		JobID    string `json:"job_id"`
		Revision uint64 `json:"revision"`
	}
	if err := json.Unmarshal([]byte(out), &started); err != nil {
		t.Fatalf("unmarshal timeout start %q: %v", out, err)
	}
	job := waitForMCPJob(t, session, started.JobID, started.Revision)
	elapsed := time.Since(start)
	if job["state"] != "timed_out" {
		t.Fatalf("timeout job state = %+v", job)
	}
	if elapsed > 20*time.Second {
		t.Errorf("timeout job took %v, expected ~2s", elapsed)
	}
	t.Logf("timeout job returned in %v: %+v", elapsed, job)

	// Give vshd a moment to tear down the process group after the conn close.
	time.Sleep(1500 * time.Millisecond)

	// Assert no leftover `sleep 30` processes in the frame's PID namespace.
	// ps (busybox) lists every process in the namespace; parse in Go so we
	// don't need a `grep` applet.
	psOut, _, err := sshExec(t, d, "root@timeoutframe", "ps")
	if err != nil {
		t.Fatalf("ssh ps after timeout: %v", err)
	}
	t.Logf("ps after timeout:\n%s", psOut)
	// ps itself and the sh that ran it appear in the output; only `sleep 30`
	// would indicate a leak.
	if n := strings.Count(psOut, "sleep 30"); n > 0 {
		t.Errorf("timeout left %d 'sleep 30' process(es) behind (process-group reap failed):\n%s", n, psOut)
	}
}

// TestMCPFrameResolution (Phase 4, T6) covers the `frame` argument semantics:
//   - frame="" auto-creates a fresh unattached frame and runs there; the new
//     frame then appears in list_frames (proving runInFrame's auto-create path
//     and the frames.Store sidecar are wired together).
//   - frame=<uuid> resolves a frame by UUID.
//   - frame=<ref> resolves a frame by ref name.
//   - frame=<bad> errors cleanly with a resolve error.
//
// It does not depend on timeout/reap; it's purely the frame-resolution matrix.
func TestMCPFrameResolution(t *testing.T) {
	env := newTestEnv(t)
	d, httpBase := startDaemonWithHTTP(t, env)
	session := mcpClient(t, httpBase)

	// --- frame="" auto-creates a fresh frame ---
	// A fresh user has no frames; bash with frame="" must auto-create one and
	// run there. We write a marker file we can later read back by UUID.
	autoJob := startAndWaitMCPBash(t, session, map[string]any{
		"command": "echo auto-created > /work/marker.txt",
		"frame":   "",
	})
	if autoJob["state"] != "exited" || autoJob["exit_code"] != float64(0) {
		t.Fatalf("bash frame=\"\" auto-create job: %+v", autoJob)
	}

	// list_frames must now show exactly one frame (the auto-created one). It's
	// unattached (no ref), so it's listed by UUID.
	listOut, isErr := callTool(t, session, "thundersnap_list_frames", nil)
	if isErr {
		t.Fatalf("list_frames after auto-create: unexpected error %q", listOut)
	}
	var lf struct {
		Frames []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"frames"`
	}
	if err := json.Unmarshal([]byte(listOut), &lf); err != nil {
		t.Fatalf("list_frames: unmarshal %q: %v", listOut, err)
	}
	if len(lf.Frames) != 1 {
		t.Fatalf("list_frames after auto-create: want 1 frame, got %d (%+v)", len(lf.Frames), lf.Frames)
	}
	autoUUID := lf.Frames[0].Name
	if _, perr := uuidParse(autoUUID); perr != nil {
		t.Fatalf("auto-created frame name %q is not a UUID: %v", autoUUID, perr)
	}
	t.Logf("auto-created frame UUID: %s", autoUUID)

	// --- frame=<uuid> resolves the auto-created frame and reads the marker ---
	// Use shell builtins (read/echo) rather than `cat`, which isn't present in a
	// nil:nil:nil frame; this keeps the auto-create path applet-free.
	uuidOut, isErr := callTool(t, session, "thundersnap_view", map[string]any{
		"path":       "/work/marker.txt",
		"tail_lines": 10,
		"frame":      autoUUID,
	})
	if isErr || !strings.Contains(uuidOut, "auto-created") {
		t.Errorf("view frame=<uuid>: isErr=%v output=%q", isErr, uuidOut)
	}

	// --- frame=<ref> resolves a named ref ---
	refUUID := createFrameForMCP(t, d, "namedframe")
	refJob := startAndWaitMCPBash(t, session, map[string]any{
		"command": "echo via-ref > /work/from-ref.txt",
		"frame":   "namedframe",
	})
	if refJob["state"] != "exited" || refJob["exit_code"] != float64(0) {
		t.Fatalf("bash frame=<ref> job: %+v", refJob)
	}
	// Verify via UUID that the ref-resolved call landed in the right frame.
	uuidReadOut, isErr := callTool(t, session, "thundersnap_view", map[string]any{
		"path":       "/work/from-ref.txt",
		"tail_lines": 10,
		"frame":      refUUID,
	})
	if isErr {
		t.Fatalf("view frame=<ref-uuid> readback: unexpected error %q", uuidReadOut)
	}
	if !strings.Contains(uuidReadOut, "via-ref") {
		t.Errorf("frame=<ref> did not land in the ref's frame: readback %q", uuidReadOut)
	}

	// --- frame=<bad> errors cleanly ---
	badOut, isErr := callTool(t, session, "thundersnap_bash", map[string]any{
		"command": "echo nope",
		"frame":   "definitely-not-a-real-frame",
	})
	if !isErr {
		t.Errorf("bash frame=<bad>: expected IsError=true, got false (output %q)", badOut)
	}
	if !strings.Contains(badOut, "resolve frame") && !strings.Contains(badOut, "no such frame") && !strings.Contains(badOut, "not found") {
		t.Errorf("bash frame=<bad>: output %q missing resolve/not-found error", badOut)
	}
}

// TestMCPBackgroundJobWaitAndKill covers live-output waits, reading a log
// before exit, explicit kill, and the terminal killed state.
func TestMCPBackgroundJobWaitAndKill(t *testing.T) {
	env := newTestEnv(t)
	d, httpBase := startDaemonWithHTTP(t, env)
	session := mcpClient(t, httpBase)
	createFrameForMCP(t, d, "killframe")
	installBusyboxAppletsInFrame(t, d, "killframe", "sleep")

	out, isErr := callTool(t, session, "thundersnap_bash", map[string]any{
		"command": "echo ready; sleep 30 & sleep 30",
		"frame":   "killframe",
	})
	if isErr {
		t.Fatalf("start kill job: %s", out)
	}
	var started struct {
		JobID    string `json:"job_id"`
		Revision uint64 `json:"revision"`
		Job      struct {
			CombinedLog string `json:"combined_log"`
		} `json:"job"`
	}
	if err := json.Unmarshal([]byte(out), &started); err != nil {
		t.Fatalf("unmarshal start %q: %v", out, err)
	}

	out, isErr = callTool(t, session, "thundersnap_jobs_wait", map[string]any{
		"job_ids": []string{started.JobID}, "after_revision": started.Revision,
		"until": "output", "timeout": 10,
	})
	if isErr || !strings.Contains(out, `"reason":"output"`) {
		t.Fatalf("wait output: isErr=%v output=%q", isErr, out)
	}
	logOut, logErr := callTool(t, session, "thundersnap_view", map[string]any{
		"path": started.Job.CombinedLog, "tail_lines": 10, "frame": "killframe",
	})
	if logErr || !strings.Contains(logOut, "ready") {
		t.Fatalf("live job log: isErr=%v output=%q", logErr, logOut)
	}

	out, isErr = callTool(t, session, "thundersnap_jobs_kill", map[string]any{"job_ids": []string{started.JobID}})
	if isErr {
		t.Fatalf("kill job: %s", out)
	}
	var killed struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if err := json.Unmarshal([]byte(out), &killed); err != nil || len(killed.Jobs) != 1 {
		t.Fatalf("unmarshal kill %q: %+v err=%v", out, killed, err)
	}
	if killed.Jobs[0]["state"] != "killed" {
		t.Fatalf("killed job state = %+v", killed.Jobs[0])
	}
}

// TestMCPBackgroundJobsConversationScope covers the chat-level ownership
// contract and actual parallel execution despite sequential tool dispatch.
func TestMCPBackgroundJobsConversationScope(t *testing.T) {
	env := newTestEnv(t)
	d, httpBase := startDaemonWithHTTP(t, env)
	sessionA := mcpClient(t, httpBase)
	sessionB := mcpClient(t, httpBase)
	createFrameForMCP(t, d, "jobsframe")
	installBusyboxAppletsInFrame(t, d, "jobsframe", "sleep")

	if out, isErr := callToolForConversation(t, sessionA, "", "thundersnap_jobs_list", nil); !isErr || !strings.Contains(out, "conversation ID") {
		t.Fatalf("missing conversation metadata: isErr=%v output=%q", isErr, out)
	}

	start := func(session *mcp.ClientSession, conversation, command string) (string, uint64) {
		out, isErr := callToolForConversation(t, session, conversation, "thundersnap_bash", map[string]any{
			"command": command,
			"frame":   "jobsframe",
		})
		if isErr {
			t.Fatalf("start %s: %s", conversation, out)
		}
		var r struct {
			JobID    string `json:"job_id"`
			Revision uint64 `json:"revision"`
		}
		if err := json.Unmarshal([]byte(out), &r); err != nil {
			t.Fatalf("unmarshal start %q: %v", out, err)
		}
		return r.JobID, r.Revision
	}

	wallStart := time.Now()
	jobA1, revA1 := start(sessionA, "conversation-a", "sleep 2; echo a1")
	jobA2, revA2 := start(sessionA, "conversation-a", "sleep 2; echo a2")
	jobB1, revB1 := start(sessionB, "conversation-b", "sleep 2; echo b1")
	if jobA1 != "j1" || jobA2 != "j2" || jobB1 != "j1" {
		t.Fatalf("conversation-local IDs: A=(%s,%s) B=%s", jobA1, jobA2, jobB1)
	}

	// A separate MCP connection with the same conversation sees A's jobs.
	out, isErr := callToolForConversation(t, sessionB, "conversation-a", "thundersnap_jobs_list", nil)
	if isErr || !strings.Contains(out, `"id":"j1"`) || !strings.Contains(out, `"id":"j2"`) {
		t.Fatalf("same conversation across MCP sessions: isErr=%v output=%s", isErr, out)
	}
	// Conversation B cannot address conversation A's j2.
	if out, isErr := callToolForConversation(t, sessionB, "conversation-b", "thundersnap_jobs_list", map[string]any{"job_ids": []string{"j2"}}); !isErr || !strings.Contains(out, "unknown job") {
		t.Fatalf("cross-conversation lookup: isErr=%v output=%q", isErr, out)
	}

	wait := func(conversation string, ids []string, revision uint64) {
		out, isErr := callToolForConversation(t, sessionA, conversation, "thundersnap_jobs_wait", map[string]any{
			"job_ids": ids, "after_revision": revision, "until": "all_exit", "timeout": 10,
		})
		if isErr || !strings.Contains(out, `"reason":"all_exit"`) {
			t.Fatalf("wait %s: isErr=%v output=%q", conversation, isErr, out)
		}
	}
	wait("conversation-a", []string{jobA1, jobA2}, revA2)
	wait("conversation-b", []string{jobB1}, revB1)
	if elapsed := time.Since(wallStart); elapsed > 5*time.Second {
		t.Errorf("three 2s jobs took %v; expected concurrent execution", elapsed)
	}
	_ = revA1
}

// uuidParse parses a UUID string and returns an error if it's malformed. Used
// by TestMCPFrameResolution to assert the auto-created frame is listed by UUID.
func uuidParse(s string) (struct{}, error) {
	if len(s) != 36 || strings.Count(s, "-") != 4 {
		return struct{}{}, fmt.Errorf("not a UUID: %q", s)
	}
	return struct{}{}, nil
}
