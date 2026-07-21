// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

// Package mcpexec implements the command-execution primitive that backs the
// thundersnap MCP tools. It is a port of Aperture's
// `chat/sandbox/exec.go` `CollectExec`, adapted to read vshdproto TLV frames
// (FrameStdout/FrameStderr/FrameExit) from a vshd one-shot session instead of
// Aperture's Backend.Exec event stream.
//
// The collector is deliberately daemon-agnostic: it reads frames from any
// io.Reader (a dialled vshd Unix socket in production, an in-memory pipe in
// unit tests). The launcher that dials host vshd, sends the VMX request
// header, and tears down the process group on cap/timeout lives in the
// thundersnapd package (cmd/thundersnapd/mcp.go); this package only owns the
// frame-accumulation discipline.
//
// Behaviour ported verbatim from Aperture's CollectExec:
//   - 1 MiB output cap (maxOutputSize).
//   - On hitting the cap, trim back to a UTF-8 rune boundary so a multi-byte
//     rune isn't split across the truncation marker, append the marker, and
//     stop reading (the caller closes the vshd socket to tear down the
//     process group, exactly as Aperture cancels the backend stream).
//   - Final strings.ToValidUTF8 cleanup, because vshd can split a rune across
//     adjacent frames.
//
// One intentional difference from Aperture: stdout and stderr are interleaved
// into a single Output string in arrival order (vshd frames them separately
// but on one stream; Aperture's Backend.Exec similarly folds both event types
// into one builder). The caller can't tell which bytes came from which stream
// — matching Aperture's `CombinedOutput`-style accumulation.
package mcpexec

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/tailscale/thundersnap/vshdproto"
)

// maxOutputSize caps the collected exec output at 1 MiB; further bytes are
// dropped with a truncation notice. Mirrors Aperture's chat/sandbox/exec.go
// constant of the same name. Tools surface the result to the LLM where larger
// outputs would be both expensive and unhelpful.
const maxOutputSize = 1 << 20

// truncationMarker is appended to Output when the cap is hit, matching
// Aperture's wording so an LLM trained on Aperture's truncation message sees a
// familiar string.
const truncationMarker = "\n\n... output truncated (exceeded 1 MiB) ..."

// ExecResult is the collected output of a command execution. It is the
// vshd-frame analogue of Aperture's sandbox.ExecResult.
type ExecResult struct {
	Output    string
	ExitCode  int
	Truncated bool
}

// CollectFrames reads vshdproto TLV frames from r until a FrameExit is seen,
// the 1 MiB output cap is hit, or the reader returns an error (EOF or
// otherwise). It accumulates FrameStdout and FrameStderr into Output
// (interleaved in arrival order), records the exit code from FrameExit, and
// applies the cap/UTF-8 discipline described in the package doc.
//
// CollectFrames does not take a context: it returns as soon as the stream ends
// or the cap is hit. The caller is responsible for closing the underlying
// vshd connection to tear down the remote process group when CollectFrames
// returns before FrameExit (cap hit, or a context-driven conn close that
// surfaces here as a read error). This mirrors Aperture's cancel-on-cap: the
// collector decides "stop now" and the caller enforces teardown.
//
// A stream that ends (EOF) without a FrameExit yields ExitCode 0 and a nil
// error — this is the normal result of the caller closing the conn after a
// timeout/cap. A non-EOF read error is returned to the caller so it can
// distinguish "we tore down" from "the stream broke".
func CollectFrames(r io.Reader) (*ExecResult, error) {
	var output strings.Builder
	result := &ExecResult{}

	for {
		typ, payload, err := vshdproto.ReadFrame(r)
		if err != nil {
			// io.EOF / io.ErrUnexpectedEOF / conn closed: the stream ended.
			// Treat as a normal end (the caller likely closed the conn after
			// a timeout/cap). Finalize and return.
			result.Output = finalizeOutput(output.String())
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return result, nil
			}
			return result, err
		}

		switch typ {
		case vshdproto.FrameStdout, vshdproto.FrameStderr:
			if result.Truncated {
				// Already capped: drop further output. The caller will close
				// the conn and end the loop via a read error.
				continue
			}
			if output.Len()+len(payload) > maxOutputSize {
				remaining := maxOutputSize - output.Len()
				if remaining > 0 {
					// Trim back to a UTF-8 rune boundary so a multi-byte rune
					// isn't split across the truncation marker. Ported
					// verbatim from Aperture's CollectExec.
					for remaining > 0 {
						rn, size := utf8.DecodeLastRune(payload[:remaining])
						if rn != utf8.RuneError || size > 1 {
							break
						}
						remaining--
					}
					output.Write(payload[:remaining])
				}
				output.WriteString(truncationMarker)
				result.Truncated = true
				// Return immediately so the caller can close the vshd socket
				// and tear down a firehose (e.g. `yes`) that would otherwise
				// keep us looping here dropping frames forever. This is the
				// direct analogue of Aperture's cancel()-on-cap: the collector
				// decides "stop now", the caller enforces teardown.
				result.Output = finalizeOutput(output.String())
				return result, nil
			}
			output.Write(payload)

		case vshdproto.FrameExit:
			if code, derr := vshdproto.DecodeExit(payload); derr == nil {
				result.ExitCode = int(code)
			}
			// FrameExit is the authoritative end of the stream; finalize and
			// return immediately (do not wait for EOF).
			result.Output = finalizeOutput(output.String())
			return result, nil
		}
	}
}

// finalizeOutput replaces invalid UTF-8 sequences with U+FFFD. The per-frame
// rune-boundary trim above keeps us rune-aligned within the truncation marker,
// but vshd can split a rune across adjacent frames; ToValidUTF8 cleans up any
// seam-time invalid bytes. Matches Aperture's CollectExec finalize step.
func finalizeOutput(s string) string {
	return strings.ToValidUTF8(s, "")
}

// MaxOutputSize is exported for tests and for the launcher, which uses it to
// decide when to stop reading. It is intentionally read-only via this getter
// rather than exported as a var so the cap can't drift between the collector
// and its callers.
func MaxOutputSize() int { return maxOutputSize }

// TruncationMarker is exported so tests can assert the marker appears in
// truncated output without duplicating the literal.
func TruncationMarker() string { return truncationMarker }

// FormatExit mirrors Aperture's tool_bash.go formatting: on a zero exit with
// no output it returns "Exit code: 0 (no output)", on a zero exit with output
// it returns the raw output, and on a non-zero exit it returns
// "Exit code: N\n\n<output>". Centralised here so every tool that wraps a
// command (bash, and indirectly view/create_file/str_replace when they want
// the bash-style envelope) formats identically.
func FormatExit(result *ExecResult) string {
	if result.ExitCode == 0 {
		if result.Output == "" {
			return "Exit code: 0 (no output)"
		}
		return result.Output
	}
	return fmt.Sprintf("Exit code: %d\n\n%s", result.ExitCode, result.Output)
}
