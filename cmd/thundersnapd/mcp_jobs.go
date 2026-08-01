// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tailscale/thundersnap/vshdproto"
)

const (
	apertureConversationIDMetaKey = "io.tailscale.aperture/conversation-id"
	mcpJobDefaultHardTimeout      = 2 * time.Hour
	mcpJobMaxHardTimeout          = 2 * time.Hour
	mcpJobWaitDefaultTimeout      = 30 * time.Second
	mcpJobWaitMaxTimeout          = 60 * time.Second
)

type mcpJobScopeKey struct {
	user, conversation string
}

type mcpJobState string

const (
	mcpJobRunning  mcpJobState = "running"
	mcpJobExited   mcpJobState = "exited"
	mcpJobTimedOut mcpJobState = "timed_out"
	mcpJobKilled   mcpJobState = "killed"
	mcpJobLost     mcpJobState = "lost"
)

type mcpJob struct {
	id             string
	label          string
	command        string
	workdir        string
	unixUser       string
	frame          string
	combinedLog    string
	stdoutLog      string
	stderrLog      string
	state          mcpJobState
	startedAt      time.Time
	endedAt        time.Time
	exitCode       *int
	combinedBytes  int64
	stdoutBytes    int64
	stderrBytes    int64
	outputRevision uint64
	endRevision    uint64
	conn           net.Conn
	stopReason     mcpJobState
	timer          *time.Timer
}

func (j *mcpJob) terminal() bool {
	return j.state == mcpJobExited || j.state == mcpJobTimedOut || j.state == mcpJobKilled || j.state == mcpJobLost
}

type mcpJobList struct {
	mu       sync.Mutex
	jobs     map[string]*mcpJob
	nextID   uint64
	revision uint64
	changed  chan struct{}
}

func newMCPJobList() *mcpJobList {
	return &mcpJobList{jobs: make(map[string]*mcpJob), changed: make(chan struct{})}
}

func (l *mcpJobList) notifyLocked() uint64 {
	l.revision++
	close(l.changed)
	l.changed = make(chan struct{})
	return l.revision
}

type mcpJobManager struct {
	mu    sync.Mutex
	lists map[mcpJobScopeKey]*mcpJobList
}

var mcpJobs = &mcpJobManager{lists: make(map[mcpJobScopeKey]*mcpJobList)}

func (m *mcpJobManager) list(key mcpJobScopeKey) *mcpJobList {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.lists[key]
	if l == nil {
		l = newMCPJobList()
		m.lists[key] = l
	}
	return l
}

func mcpJobScopeFromRequest(ctx context.Context, req *mcp.CallToolRequest) (mcpJobScopeKey, error) {
	user := mcpUserFromContext(ctx)
	if user == "" {
		return mcpJobScopeKey{}, fmt.Errorf("no MCP user resolved for request")
	}
	if req == nil || req.Params == nil {
		return mcpJobScopeKey{}, fmt.Errorf("missing Aperture conversation ID in MCP _meta")
	}
	v, ok := req.Params.Meta[apertureConversationIDMetaKey]
	conversation, okString := v.(string)
	if !ok || !okString || strings.TrimSpace(conversation) == "" {
		return mcpJobScopeKey{}, fmt.Errorf("missing Aperture conversation ID in MCP _meta (%s)", apertureConversationIDMetaKey)
	}
	return mcpJobScopeKey{user: user, conversation: conversation}, nil
}

func mcpJobScopeDir(key mcpJobScopeKey) string {
	sum := sha256.Sum256([]byte(key.user + "\x00" + key.conversation))
	return hex.EncodeToString(sum[:12])
}

type mcpJobStatus struct {
	ID            string      `json:"id"`
	Label         string      `json:"label,omitempty"`
	State         mcpJobState `json:"state"`
	Frame         string      `json:"frame"`
	Command       string      `json:"command"`
	Workdir       string      `json:"workdir"`
	User          string      `json:"user"`
	CombinedLog   string      `json:"combined_log"`
	StdoutLog     string      `json:"stdout_log"`
	StderrLog     string      `json:"stderr_log"`
	CombinedBytes int64       `json:"combined_bytes"`
	StdoutBytes   int64       `json:"stdout_bytes"`
	StderrBytes   int64       `json:"stderr_bytes"`
	ExitCode      *int        `json:"exit_code,omitempty"`
	StartedAt     string      `json:"started_at"`
	EndedAt       string      `json:"ended_at,omitempty"`
	ElapsedMS     int64       `json:"elapsed_ms"`
}

func jobStatusLocked(j *mcpJob, now time.Time) mcpJobStatus {
	end := j.endedAt
	if end.IsZero() {
		end = now
	}
	s := mcpJobStatus{
		ID: j.id, Label: j.label, State: j.state, Frame: j.frame,
		Command: j.command, Workdir: j.workdir, User: j.unixUser,
		CombinedLog: j.combinedLog, StdoutLog: j.stdoutLog, StderrLog: j.stderrLog,
		CombinedBytes: j.combinedBytes, StdoutBytes: j.stdoutBytes, StderrBytes: j.stderrBytes,
		ExitCode: j.exitCode, StartedAt: j.startedAt.UTC().Format(time.RFC3339Nano),
		ElapsedMS: end.Sub(j.startedAt).Milliseconds(),
	}
	if !j.endedAt.IsZero() {
		s.EndedAt = j.endedAt.UTC().Format(time.RFC3339Nano)
	}
	return s
}

func selectJobsLocked(l *mcpJobList, ids []string) ([]*mcpJob, error) {
	if len(ids) == 0 {
		ids = make([]string, 0, len(l.jobs))
		for id := range l.jobs {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool {
			var a, b uint64
			fmt.Sscanf(ids[i], "j%d", &a)
			fmt.Sscanf(ids[j], "j%d", &b)
			return a < b
		})
	}
	jobs := make([]*mcpJob, 0, len(ids))
	seen := make(map[string]bool)
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		j := l.jobs[id]
		if j == nil {
			return nil, fmt.Errorf("unknown job ID %q in this conversation", id)
		}
		jobs = append(jobs, j)
	}
	return jobs, nil
}

func statusesLocked(jobs []*mcpJob) []mcpJobStatus {
	now := time.Now()
	out := make([]mcpJobStatus, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, jobStatusLocked(j, now))
	}
	return out
}

func jsonToolResult(v any, isError bool) (*mcp.CallToolResult, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return textResult(fmt.Sprintf("marshal result: %v", err), true)
	}
	return textResult(string(b), isError)
}

func mcpBashToolHandler(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	key, err := mcpJobScopeFromRequest(ctx, req)
	if err != nil {
		return textResult(err.Error(), true)
	}
	var params struct {
		Command     string `json:"command"`
		Frame       string `json:"frame"`
		Workdir     string `json:"workdir"`
		Label       string `json:"label"`
		User        string `json:"user"`
		HardTimeout int    `json:"hard_timeout"`
	}
	if len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &params); err != nil {
			return textResult(fmt.Sprintf("invalid input: %v", err), true)
		}
	}
	if params.Command == "" {
		return textResult("command is required", true)
	}
	if params.Workdir == "" {
		params.Workdir = "/work"
	}
	if params.User == "" {
		params.User = "user"
	}
	if params.User != "user" && params.User != "root" {
		return textResult("user must be either \"user\" (recommended) or \"root\"", true)
	}
	hardTimeout := mcpJobDefaultHardTimeout
	if params.HardTimeout > 0 {
		hardTimeout = time.Duration(params.HardTimeout) * time.Second
		if hardTimeout > mcpJobMaxHardTimeout {
			hardTimeout = mcpJobMaxHardTimeout
		}
	}

	rootFS, uuid, err := resolveFrameRootFS(key.user, params.Frame)
	if err != nil {
		return textResult(fmt.Sprintf("resolve frame: %v", err), true)
	}
	if err := prepareContainerRootFS(rootFS, ""); err != nil {
		return textResult(fmt.Sprintf("prepare frame rootfs: %v", err), true)
	}
	workdirHost := filepath.Join(rootFS, strings.TrimPrefix(filepath.Clean("/"+params.Workdir), "/"))
	if !isWithinRootFS(workdirHost, rootFS) {
		return textResult(fmt.Sprintf("workdir %q escapes the frame", params.Workdir), true)
	}
	if info, err := os.Stat(workdirHost); err != nil || !info.IsDir() {
		if err == nil {
			err = fmt.Errorf("not a directory")
		}
		return textResult(fmt.Sprintf("invalid workdir %q: %v", params.Workdir, err), true)
	}
	if _, err := controlServers.getOrCreateControlServer(rootFS); err != nil {
		return textResult(fmt.Sprintf("start control socket: %v", err), true)
	}
	releaseControl := true
	defer func() {
		if releaseControl {
			controlServers.releaseControlServer(rootFS)
		}
	}()

	l := mcpJobs.list(key)
	l.mu.Lock()
	l.nextID++
	id := fmt.Sprintf("j%d", l.nextID)
	l.mu.Unlock()

	logDirInFrame := filepath.Join("/.thundersnap/jobs", mcpJobScopeDir(key), id)
	logDirHost := filepath.Join(rootFS, strings.TrimPrefix(logDirInFrame, "/"))
	if !isWithinRootFS(logDirHost, rootFS) {
		return textResult("internal job log path escaped frame", true)
	}
	if err := os.MkdirAll(logDirHost, 0700); err != nil {
		return textResult(fmt.Sprintf("create job log directory: %v", err), true)
	}
	openLog := func(name string) (*os.File, error) {
		return os.OpenFile(filepath.Join(logDirHost, name), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	}
	combined, err := openLog("combined.log")
	if err != nil {
		return textResult(fmt.Sprintf("create combined log: %v", err), true)
	}
	stdout, err := openLog("stdout.log")
	if err != nil {
		combined.Close()
		return textResult(fmt.Sprintf("create stdout log: %v", err), true)
	}
	stderr, err := openLog("stderr.log")
	if err != nil {
		combined.Close()
		stdout.Close()
		return textResult(fmt.Sprintf("create stderr log: %v", err), true)
	}
	closeLogs := func() { combined.Close(); stdout.Close(); stderr.Close() }

	sockPath, err := hostVshd.ensure()
	if err != nil {
		closeLogs()
		return textResult(fmt.Sprintf("start host vshd: %v", err), true)
	}
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		closeLogs()
		return textResult(fmt.Sprintf("dial host vshd: %v", err), true)
	}
	absRootFS, err := filepath.Abs(rootFS)
	if err != nil {
		conn.Close()
		closeLogs()
		return textResult(fmt.Sprintf("abs rootfs: %v", err), true)
	}
	wrapped := "cd " + shellQuote(params.Workdir) + " && " + params.Command
	writeVshdRequest(conn, strings.TrimPrefix(absRootFS, "/"), params.User, false, []string{"sh", "-c", wrapped})

	j := &mcpJob{
		id: id, label: params.Label, command: params.Command, workdir: params.Workdir, unixUser: params.User,
		frame: uuid.String(), state: mcpJobRunning, startedAt: time.Now(), conn: conn,
		combinedLog: filepath.Join(logDirInFrame, "combined.log"),
		stdoutLog:   filepath.Join(logDirInFrame, "stdout.log"),
		stderrLog:   filepath.Join(logDirInFrame, "stderr.log"),
	}
	l.mu.Lock()
	l.jobs[id] = j
	revision := l.notifyLocked()
	status := jobStatusLocked(j, time.Now())
	l.mu.Unlock()

	j.timer = time.AfterFunc(hardTimeout, func() { requestMCPJobStop(l, j, mcpJobTimedOut) })
	releaseControl = false
	go collectMCPJob(l, j, conn, combined, stdout, stderr, func() { controlServers.releaseControlServer(rootFS) })

	return jsonToolResult(map[string]any{"job_id": id, "revision": revision, "job": status}, false)
}

func requestMCPJobStop(l *mcpJobList, j *mcpJob, reason mcpJobState) {
	l.mu.Lock()
	if j.terminal() || j.stopReason != "" {
		l.mu.Unlock()
		return
	}
	j.stopReason = reason
	conn := j.conn
	l.notifyLocked()
	l.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

func collectMCPJob(l *mcpJobList, j *mcpJob, conn net.Conn, combined, stdout, stderr *os.File, release func()) {
	defer release()
	defer conn.Close()
	defer combined.Close()
	defer stdout.Close()
	defer stderr.Close()
	gotExit := false
	for {
		typ, payload, err := vshdproto.ReadFrame(conn)
		if err != nil {
			if err != io.EOF && err != net.ErrClosed {
				// The terminal state below records this as lost unless a stop was requested.
			}
			break
		}
		switch typ {
		case vshdproto.FrameStdout, vshdproto.FrameStderr:
			_, _ = combined.Write(payload)
			if typ == vshdproto.FrameStdout {
				_, _ = stdout.Write(payload)
			} else {
				_, _ = stderr.Write(payload)
			}
			l.mu.Lock()
			j.combinedBytes += int64(len(payload))
			if typ == vshdproto.FrameStdout {
				j.stdoutBytes += int64(len(payload))
			} else {
				j.stderrBytes += int64(len(payload))
			}
			j.outputRevision = l.notifyLocked()
			l.mu.Unlock()
		case vshdproto.FrameExit:
			if code, err := vshdproto.DecodeExit(payload); err == nil {
				c := int(code)
				l.mu.Lock()
				j.exitCode = &c
				l.mu.Unlock()
			}
			gotExit = true
			break
		}
		if gotExit {
			break
		}
	}
	if j.timer != nil {
		j.timer.Stop()
	}
	l.mu.Lock()
	if j.stopReason != "" {
		j.state = j.stopReason
	} else if gotExit {
		j.state = mcpJobExited
	} else {
		j.state = mcpJobLost
	}
	j.conn = nil
	j.endedAt = time.Now()
	j.endRevision = l.notifyLocked()
	l.mu.Unlock()
}

func mcpJobsListToolHandler(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	key, err := mcpJobScopeFromRequest(ctx, req)
	if err != nil {
		return textResult(err.Error(), true)
	}
	var params struct {
		JobIDs []string `json:"job_ids"`
	}
	if len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &params); err != nil {
			return textResult(fmt.Sprintf("invalid input: %v", err), true)
		}
	}
	l := mcpJobs.list(key)
	l.mu.Lock()
	jobs, err := selectJobsLocked(l, params.JobIDs)
	if err != nil {
		l.mu.Unlock()
		return textResult(err.Error(), true)
	}
	result := map[string]any{"revision": l.revision, "jobs": statusesLocked(jobs)}
	l.mu.Unlock()
	return jsonToolResult(result, false)
}

func mcpJobsWaitToolHandler(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	key, err := mcpJobScopeFromRequest(ctx, req)
	if err != nil {
		return textResult(err.Error(), true)
	}
	var params struct {
		JobIDs        []string `json:"job_ids"`
		AfterRevision uint64   `json:"after_revision"`
		Until         string   `json:"until"`
		Timeout       int      `json:"timeout"`
	}
	if len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &params); err != nil {
			return textResult(fmt.Sprintf("invalid input: %v", err), true)
		}
	}
	if params.Until == "" {
		params.Until = "any_exit"
	}
	if params.Until != "output" && params.Until != "any_exit" && params.Until != "all_exit" {
		return textResult(fmt.Sprintf("invalid until %q (want output, any_exit, or all_exit)", params.Until), true)
	}
	waitTimeout := mcpJobWaitDefaultTimeout
	if params.Timeout > 0 {
		waitTimeout = time.Duration(params.Timeout) * time.Second
		if waitTimeout > mcpJobWaitMaxTimeout {
			waitTimeout = mcpJobWaitMaxTimeout
		}
	}
	waitCtx, cancel := context.WithTimeout(ctx, waitTimeout)
	defer cancel()
	l := mcpJobs.list(key)
	for {
		l.mu.Lock()
		jobs, err := selectJobsLocked(l, params.JobIDs)
		if err != nil {
			l.mu.Unlock()
			return textResult(err.Error(), true)
		}
		satisfied := false
		switch params.Until {
		case "output":
			for _, j := range jobs {
				satisfied = satisfied || j.outputRevision > params.AfterRevision
			}
		case "any_exit":
			for _, j := range jobs {
				satisfied = satisfied || j.endRevision > params.AfterRevision
			}
		case "all_exit":
			satisfied = len(jobs) > 0
			for _, j := range jobs {
				satisfied = satisfied && j.terminal()
			}
		}
		if satisfied {
			result := map[string]any{"reason": params.Until, "revision": l.revision, "jobs": statusesLocked(jobs)}
			l.mu.Unlock()
			return jsonToolResult(result, false)
		}
		changed := l.changed
		l.mu.Unlock()
		select {
		case <-changed:
			continue
		case <-waitCtx.Done():
			l.mu.Lock()
			jobs, err := selectJobsLocked(l, params.JobIDs)
			if err != nil {
				l.mu.Unlock()
				return textResult(err.Error(), true)
			}
			result := map[string]any{"reason": "timeout", "revision": l.revision, "jobs": statusesLocked(jobs)}
			l.mu.Unlock()
			return jsonToolResult(result, false)
		}
	}
}

func mcpJobsKillToolHandler(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	key, err := mcpJobScopeFromRequest(ctx, req)
	if err != nil {
		return textResult(err.Error(), true)
	}
	var params struct {
		JobIDs []string `json:"job_ids"`
	}
	if len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &params); err != nil {
			return textResult(fmt.Sprintf("invalid input: %v", err), true)
		}
	}
	if len(params.JobIDs) == 0 {
		return textResult("job_ids is required for kill", true)
	}
	l := mcpJobs.list(key)
	l.mu.Lock()
	jobs, err := selectJobsLocked(l, params.JobIDs)
	l.mu.Unlock()
	if err != nil {
		return textResult(err.Error(), true)
	}
	for _, j := range jobs {
		requestMCPJobStop(l, j, mcpJobKilled)
	}
	// Do not report success while the process group may still be alive. Wait
	// for each collector to observe connection teardown and publish terminal
	// state. The request context remains the upper bound if vshd wedges.
	for {
		l.mu.Lock()
		allTerminal := true
		for _, j := range jobs {
			allTerminal = allTerminal && j.terminal()
		}
		if allTerminal {
			result := map[string]any{"revision": l.revision, "jobs": statusesLocked(jobs)}
			l.mu.Unlock()
			return jsonToolResult(result, false)
		}
		changed := l.changed
		l.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return textResult(fmt.Sprintf("kill did not complete: %v", ctx.Err()), true)
		}
	}
}
