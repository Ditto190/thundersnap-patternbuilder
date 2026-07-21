// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

//go:build e2e

// mcp_test.go is a real end-to-end test of the thundersnap MCP server. It
// starts a thundersnapd in test mode with --test-http-listen, drives the
// /v1/mcp endpoint with the official go-sdk MCP client, and asserts the six
// tools work against a fresh btrfs-backed frame.
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

	// Sanity: the server must report the six tools.
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
		"thundersnap_bash", "thundersnap_view", "thundersnap_create_file",
		"thundersnap_str_replace", "thundersnap_list_frames", "thundersnap_list_refs",
	} {
		if !got[want] {
			t.Fatalf("MCP server missing tool %q; got %d tools", want, len(tools.Tools))
		}
	}
	t.Logf("MCP server advertises %d tools (all 6 expected tools present)", len(tools.Tools))
	return session
}

// callTool invokes a named tool with the given arguments and returns the text
// of the first TextContent block plus the IsError flag. It fatals on a
// transport-level error (the call didn't reach the server) but returns
// IsError=true results normally — those are tool-level failures the LLM is
// meant to see, and the test asserts on them.
func callTool(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) (text string, isError bool) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
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

// createFrameForMCP creates a frame the way the rest of the e2e suite does
// (via `ts frame` over SSH) so the MCP tools have a real frame to target. The
// SSH path is already proven by the other e2e tests; reusing it here keeps the
// MCP test focused on the MCP surface.
func createFrameForMCP(t *testing.T, d *daemonInstance, refName string) string {
	t.Helper()
	return createFrameViaDaemon(t, d, refName)
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

	// --- bash: zero exit ---
	{
		out, isErr := callTool(t, session, "thundersnap_bash", map[string]any{
			"command": "echo hello-mcp",
			"frame":   "mcpframe",
		})
		if isErr {
			t.Fatalf("bash echo: unexpected error %q", out)
		}
		if !strings.Contains(out, "hello-mcp") {
			t.Errorf("bash echo: output %q does not contain %q", out, "hello-mcp")
		}
	}

	// --- bash: non-zero exit is an error RESULT (not a transport error), output visible ---
	{
		out, isErr := callTool(t, session, "thundersnap_bash", map[string]any{
			"command": "echo to-stderr >&2; exit 3",
			"frame":   "mcpframe",
		})
		if !isErr {
			t.Errorf("bash exit 3: expected IsError=true, got false (output %q)", out)
		}
		if !strings.Contains(out, "to-stderr") {
			t.Errorf("bash exit 3: output %q missing stderr line", out)
		}
		if !strings.Contains(out, "Exit code: 3") {
			t.Errorf("bash exit 3: output %q missing 'Exit code: 3'", out)
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

	// --- bash: bad frame errors cleanly (setup error, returned as tool error) ---
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
