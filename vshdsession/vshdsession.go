// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

// Package vshdsession implements the guest-side server for a vshd session: it
// runs a command and bridges it to a client over the vshdproto TLV stream.
//
// It is the single source of session-serving logic shared by two callers:
//
//   - cmd/vshd, for sessions that run directly in the VM/host filesystem (no
//     container): vshd is the TLV endpoint and serves the session itself.
//   - cmd/ts ("session-serve"), for container sessions: vshd splices the raw
//     TLV bytes through to an inner `ts` that has already nsenter'd + chroot'd
//     into the container, and that inner `ts` serves the session here. Because
//     the PTY is opened from inside the container's mount namespace (after
//     chroot), the slave lands in the container's own devpts instance and so is
//     visible as /dev/pts/N inside the container.
//
// For a PTY session the command is started on a pty (creack/pty) whose size
// tracks FrameWinsize frames from the client; for a non-PTY session stdin is fed
// from FrameStdin frames and stdout/stderr are framed separately. In both cases
// the child's exit code is sent as a FrameExit frame before Serve returns.
package vshdsession

import (
	"io"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/tailscale/thundersnap/vshdproto"
)

// Logf is an optional logging hook (e.g. log.Printf). It may be nil.
type Logf func(format string, args ...any)

func (l Logf) logf(format string, args ...any) {
	if l != nil {
		l(format, args...)
	}
}

// Serve runs cmd, proxying it to the client over conn/reader using vshdproto TLV
// framing. wantPTY selects a pty session (FrameStdin/FrameWinsize -> pty, pty
// output -> FrameStdout) versus a pipe session (FrameStdin -> stdin, stdout and
// stderr framed separately). postStart, when non-nil, is invoked with the
// started child's PID immediately after the command starts (used to apply
// cgroup limits in host mode). logf, when non-nil, receives diagnostic logs.
func Serve(conn io.Writer, reader io.Reader, cmd *exec.Cmd, wantPTY bool, postStart func(pid int), logf Logf) {
	if wantPTY {
		servePTY(conn, reader, cmd, postStart, logf)
	} else {
		servePipe(conn, reader, cmd, postStart, logf)
	}
}

// servePTY starts cmd on a pty and bridges it to the TLV stream:
// FrameStdin -> pty, FrameWinsize -> pty.Setsize, pty output -> FrameStdout.
func servePTY(conn io.Writer, reader io.Reader, cmd *exec.Cmd, postStart func(pid int), logf Logf) {
	ptmx, err := pty.Start(cmd)
	if err != nil {
		logf.logf("failed to start pty: %v", err)
		vshdproto.WriteFrame(conn, vshdproto.FrameStderr, []byte("vshd: failed to start shell: "+err.Error()+"\n"))
		vshdproto.WriteFrame(conn, vshdproto.FrameExit, vshdproto.EncodeExit(1))
		return
	}
	defer ptmx.Close()
	if postStart != nil {
		postStart(cmd.Process.Pid)
	}
	logf.logf("pty session started with PID %d", cmd.Process.Pid)

	// Client -> child: decode TLV frames, route stdin to the pty and winsize to
	// the pty size. Runs until the client closes (EOF) or sends a malformed frame.
	go func() {
		for {
			typ, payload, err := vshdproto.ReadFrame(reader)
			if err != nil {
				if err != io.EOF {
					logf.logf("read frame: %v", err)
				}
				return
			}
			switch typ {
			case vshdproto.FrameStdin:
				if _, werr := ptmx.Write(payload); werr != nil {
					return
				}
			case vshdproto.FrameWinsize:
				ws, derr := vshdproto.DecodeWinsize(payload)
				if derr != nil {
					logf.logf("bad winsize: %v", derr)
					continue
				}
				pty.Setsize(ptmx, &pty.Winsize{Rows: ws.Rows, Cols: ws.Cols, X: ws.X, Y: ws.Y})
			}
		}
	}()

	// Child -> client: frame pty output as FrameStdout.
	done := make(chan struct{})
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, rerr := ptmx.Read(buf)
			if n > 0 {
				if werr := vshdproto.WriteFrame(conn, vshdproto.FrameStdout, buf[:n]); werr != nil {
					break
				}
			}
			if rerr != nil {
				break
			}
		}
		close(done)
	}()

	<-done
	logf.logf("signaling pty session to exit")
	cmd.Process.Signal(syscall.SIGHUP)
	code := waitExitCode(cmd)
	vshdproto.WriteFrame(conn, vshdproto.FrameExit, vshdproto.EncodeExit(code))
	logf.logf("pty session exited (code %d)", code)
}

// servePipe runs cmd without a pty, feeding FrameStdin frames to the child's
// stdin and framing its stdout/stderr separately (FrameStdout/FrameStderr), then
// sending FrameExit.
func servePipe(conn io.Writer, reader io.Reader, cmd *exec.Cmd, postStart func(pid int), logf Logf) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		logf.logf("stdin pipe: %v", err)
		vshdproto.WriteFrame(conn, vshdproto.FrameStderr, []byte("vshd: "+err.Error()+"\n"))
		vshdproto.WriteFrame(conn, vshdproto.FrameExit, vshdproto.EncodeExit(1))
		return
	}
	// stdout and stderr are framed onto the same connection from independent
	// goroutines; a shared mutex keeps each frame's header+payload contiguous.
	var writeMu sync.Mutex
	cmd.Stdout = &frameWriter{conn: conn, typ: vshdproto.FrameStdout, mu: &writeMu}
	cmd.Stderr = &frameWriter{conn: conn, typ: vshdproto.FrameStderr, mu: &writeMu}

	if err := cmd.Start(); err != nil {
		logf.logf("start command: %v", err)
		vshdproto.WriteFrame(conn, vshdproto.FrameStderr, []byte("vshd: "+err.Error()+"\n"))
		vshdproto.WriteFrame(conn, vshdproto.FrameExit, vshdproto.EncodeExit(1))
		return
	}
	if postStart != nil {
		postStart(cmd.Process.Pid)
	}
	logf.logf("command started with PID %d", cmd.Process.Pid)

	// Client -> child stdin: decode FrameStdin frames. Other frame types (e.g.
	// stray winsize) are ignored in pipe mode. Close stdin on EOF or when
	// we receive an empty FrameStdin (the daemon's EOF marker).
	//
	// If the reader returns an error (the client disconnected / the conn
	// closed) we also signal the child to exit: a non-stdin-reading command
	// (e.g. `while true; do :; done`, as used by autorun) would otherwise
	// keep running forever, orphaned after its peer is gone. This mirrors the
	// SIGHUP servePTY sends when the pty output stream ends, and is the
	// standard "remote command dies on SSH disconnect" behaviour. An empty
	// FrameStdin is a normal stdin EOF (the daemon sends it as soon as its
	// own stdin ends for an exec session) and must NOT kill the child.
	go func() {
		defer stdin.Close()
		for {
			typ, payload, err := vshdproto.ReadFrame(reader)
			if err != nil {
				killChildOnDisconnect(cmd, logf)
				return
			}
			if typ == vshdproto.FrameStdin {
				// Empty FrameStdin is the EOF marker from the daemon
				if len(payload) == 0 {
					return
				}
				if _, werr := stdin.Write(payload); werr != nil {
					return
				}
			}
		}
	}()

	code := waitExitCode(cmd)
	vshdproto.WriteFrame(conn, vshdproto.FrameExit, vshdproto.EncodeExit(code))
	logf.logf("command exited (code %d)", code)
}

// killChildOnDisconnect signals cmd to exit because the client disconnected.
// It sends SIGHUP (the signal a terminal sends on hangup, matching servePTY),
// then escalates to SIGKILL after a 2s grace in case the child ignores or traps
// SIGHUP. Signalling an already-exited child is harmless (the kernel returns
// ESRCH, which we ignore). It runs the escalation in its own goroutine so the
// caller (the stdin reader loop) can return immediately; the main servePipe
// goroutine unblocks on cmd.Wait() once the child exits.
func killChildOnDisconnect(cmd *exec.Cmd, logf Logf) {
	if cmd.Process == nil {
		return
	}
	go func() {
		if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
			return // child already exited
		}
		logf.logf("client disconnected; sent SIGHUP to child PID %d", cmd.Process.Pid)
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		<-timer.C
		// Still running? Force kill. ProcessState is set once Wait reaps the
		// child; checking it races favorably enough here (worst case we SIGKILL
		// a just-exited process, which is a harmless no-op).
		if cmd.ProcessState == nil {
			_ = cmd.Process.Signal(syscall.SIGKILL)
			logf.logf("child PID %d did not exit on SIGHUP; sent SIGKILL", cmd.Process.Pid)
		}
	}()
}

// waitExitCode waits for cmd and returns its exit code (0 on success, 128+signal
// for a signalled child, the process exit status on a normal non-zero exit, or 1
// for other failures).
func waitExitCode(cmd *exec.Cmd) int32 {
	err := cmd.Wait()
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
			if ws.Signaled() {
				return int32(128 + int(ws.Signal()))
			}
			return int32(ws.ExitStatus())
		}
		return int32(ee.ExitCode())
	}
	return 1
}

// frameWriter wraps a connection so that each Write is emitted as one vshdproto
// frame of a fixed type. Used to frame a child's stdout/stderr in pipe mode.
type frameWriter struct {
	conn io.Writer
	typ  uint8
	mu   *sync.Mutex // optional; serialises frames sharing one conn
}

func (fw *frameWriter) Write(p []byte) (int, error) {
	if fw.mu != nil {
		fw.mu.Lock()
		defer fw.mu.Unlock()
	}
	if err := vshdproto.WriteFrame(fw.conn, fw.typ, p); err != nil {
		return 0, err
	}
	return len(p), nil
}
