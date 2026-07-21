// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package mcpexec

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/tailscale/thundersnap/vshdproto"
)

// frameStream is a tiny helper that concatenates vshdproto frames into a
// buffer so a test can hand CollectFrames an io.Reader of pre-encoded frames
// without spinning up a real vshd.
type frameStream struct{ buf bytes.Buffer }

func (s *frameStream) write(typ uint8, payload []byte) {
	if err := vshdproto.WriteFrame(&s.buf, typ, payload); err != nil {
		panic(err)
	}
}

func (s *frameStream) reader() io.Reader { return &s.buf }

func TestCollectFrames_EmptyExitZero(t *testing.T) {
	var s frameStream
	s.write(vshdproto.FrameExit, vshdproto.EncodeExit(0))
	r, err := CollectFrames(s.reader())
	if err != nil {
		t.Fatalf("CollectFrames: %v", err)
	}
	if r.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", r.ExitCode)
	}
	if r.Output != "" {
		t.Errorf("Output = %q, want empty", r.Output)
	}
	if r.Truncated {
		t.Errorf("Truncated = true, want false")
	}
	if got := FormatExit(r); got != "Exit code: 0 (no output)" {
		t.Errorf("FormatExit = %q, want %q", got, "Exit code: 0 (no output)")
	}
}

func TestCollectFrames_StdoutOnly(t *testing.T) {
	var s frameStream
	s.write(vshdproto.FrameStdout, []byte("hello\nworld\n"))
	s.write(vshdproto.FrameExit, vshdproto.EncodeExit(0))
	r, err := CollectFrames(s.reader())
	if err != nil {
		t.Fatalf("CollectFrames: %v", err)
	}
	if r.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", r.ExitCode)
	}
	if r.Output != "hello\nworld\n" {
		t.Errorf("Output = %q, want %q", r.Output, "hello\nworld\n")
	}
	if got := FormatExit(r); got != "hello\nworld\n" {
		t.Errorf("FormatExit = %q, want raw output", got)
	}
}

func TestCollectFrames_StdoutStderrInterleaved(t *testing.T) {
	// Frames arrive in order: out, err, out, err. CollectFrames must preserve
	// that arrival order in Output (it does NOT merge them onto one stream
	// ahead of time — the interleaving is observed from the frame sequence).
	var s frameStream
	s.write(vshdproto.FrameStdout, []byte("OUT1"))
	s.write(vshdproto.FrameStderr, []byte("ERR1"))
	s.write(vshdproto.FrameStdout, []byte("OUT2"))
	s.write(vshdproto.FrameStderr, []byte("ERR2"))
	s.write(vshdproto.FrameExit, vshdproto.EncodeExit(0))
	r, err := CollectFrames(s.reader())
	if err != nil {
		t.Fatalf("CollectFrames: %v", err)
	}
	if want := "OUT1ERR1OUT2ERR2"; r.Output != want {
		t.Errorf("Output = %q, want %q (arrival order preserved)", r.Output, want)
	}
}

func TestCollectFrames_NonZeroExit(t *testing.T) {
	var s frameStream
	s.write(vshdproto.FrameStdout, []byte("doing thing\n"))
	s.write(vshdproto.FrameStderr, []byte("oops\n"))
	s.write(vshdproto.FrameExit, vshdproto.EncodeExit(42))
	r, err := CollectFrames(s.reader())
	if err != nil {
		t.Fatalf("CollectFrames: %v", err)
	}
	if r.ExitCode != 42 {
		t.Errorf("ExitCode = %d, want 42", r.ExitCode)
	}
	want := "Exit code: 42\n\ndoing thing\noops\n"
	if got := FormatExit(r); got != want {
		t.Errorf("FormatExit = %q, want %q", got, want)
	}
}

func TestCollectFrames_TruncationAtCap(t *testing.T) {
	// Send one giant FrameStdout that blows past the 1 MiB cap. The collector
	// must stop at the cap, append the truncation marker, set Truncated, and
	// return immediately — without reading the FrameExit (which we do NOT
	// send, to prove the caller is expected to close the conn).
	var s frameStream
	// 2 MiB of 'A' in a single frame: exceeds maxOutputSize (1 MiB).
	s.write(vshdproto.FrameStdout, bytes.Repeat([]byte("A"), 2*MaxOutputSize()))
	r, err := CollectFrames(s.reader())
	if err != nil {
		t.Fatalf("CollectFrames: %v", err)
	}
	if !r.Truncated {
		t.Fatalf("Truncated = false, want true")
	}
	if !strings.HasSuffix(r.Output, TruncationMarker()) {
		t.Errorf("Output does not end with truncation marker; got %q... (len %d)",
			r.Output[:min(80, len(r.Output))], len(r.Output))
	}
	// Output is capped at exactly maxOutputSize + len(marker). The rune-trim
	// only fires on a partial final rune; 'A' is 1 byte so no trimming occurs.
	wantLen := MaxOutputSize() + len(TruncationMarker())
	if len(r.Output) != wantLen {
		t.Errorf("len(Output) = %d, want %d", len(r.Output), wantLen)
	}
	// ExitCode is 0 because we never got a FrameExit (caller would close).
	if r.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0 (no FrameExit seen)", r.ExitCode)
	}
}

func TestCollectFrames_TruncationRuneBoundary(t *testing.T) {
	// Fill to just under the cap with ASCII, then push a frame whose first
	// bytes are a complete 3-byte UTF-8 rune (€ = 0xE2 0x82 0xAC) that would
	// straddle the cap. The trim must drop the partial rune rather than leave
	// an invalid prefix, so Output is valid UTF-8 before the marker.
	var s frameStream
	pad := MaxOutputSize() - 1 // 1 byte of headroom
	s.write(vshdproto.FrameStdout, bytes.Repeat([]byte("x"), pad))
	// Now a frame whose first byte is the start of a 3-byte rune; only the
	// first byte fits in the 1-byte headroom. The trim must drop it.
	s.write(vshdproto.FrameStdout, []byte("€€€")) // each € is 3 bytes
	r, err := CollectFrames(s.reader())
	if err != nil {
		t.Fatalf("CollectFrames: %v", err)
	}
	if !r.Truncated {
		t.Fatalf("Truncated = false, want true")
	}
	// Everything before the marker must be valid UTF-8 (the rune-boundary trim
	// is the whole point of this test). strings.ToValidUTF8 in finalizeOutput
	// would mask a bug here, so check the raw length: pad bytes of 'x' plus
	// nothing of the partial rune, plus the marker.
	pre := strings.TrimSuffix(r.Output, TruncationMarker())
	if len(pre) != pad {
		t.Errorf("pre-marker length = %d, want %d (partial rune should be dropped)", len(pre), pad)
	}
	if pre != strings.Repeat("x", pad) {
		t.Errorf("pre-marker content mismatch")
	}
}

func TestCollectFrames_SplitRuneAcrossFramesCleanedUp(t *testing.T) {
	// Two FrameStdout frames that each carry half of a 2-byte rune (ë =
	// 0xC3 0xAB). Neither frame alone is valid UTF-8 at the seam; the final
	// ToValidUTF8 cleanup must not panic and the bytes must round-trip to the
	// original rune (since both halves are present in Output).
	var s frameStream
	s.write(vshdproto.FrameStdout, []byte{0xC3})
	s.write(vshdproto.FrameStdout, []byte{0xAB})
	s.write(vshdproto.FrameExit, vshdproto.EncodeExit(0))
	r, err := CollectFrames(s.reader())
	if err != nil {
		t.Fatalf("CollectFrames: %v", err)
	}
	// finalizeOutput uses ToValidUTF8(s, "") which DROPS invalid sequences.
	// The two halves together ARE valid UTF-8 (ë), so the rune must survive.
	if r.Output != "ë" {
		t.Errorf("Output = %q, want %q (split rune reassembled)", r.Output, "ë")
	}
}

func TestCollectFrames_EOFAfterExitIsIgnored(t *testing.T) {
	// FrameExit ends the stream; trailing garbage after it is never read.
	var s frameStream
	s.write(vshdproto.FrameStdout, []byte("done\n"))
	s.write(vshdproto.FrameExit, vshdproto.EncodeExit(0))
	// Deliberately do NOT write more — the reader hits EOF after the exit
	// frame. CollectFrames returns at FrameExit, never observing the EOF.
	r, err := CollectFrames(s.reader())
	if err != nil {
		t.Fatalf("CollectFrames: %v", err)
	}
	if r.ExitCode != 0 || r.Output != "done\n" {
		t.Errorf("result = %+v, want ExitCode=0 Output=%q", r, "done\n")
	}
}

func TestCollectFrames_EmptyStreamEOF(t *testing.T) {
	// A reader that is immediately EOF (e.g. the caller closed the conn
	// before any frame arrived). CollectFrames returns a zero result, nil err.
	r, err := CollectFrames(bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("CollectFrames on empty stream: %v", err)
	}
	if r.ExitCode != 0 || r.Output != "" || r.Truncated {
		t.Errorf("result = %+v, want zero-valued", r)
	}
}

func TestCollectFrames_TruncatedDropsFurtherFrames(t *testing.T) {
	// If somehow CollectFrames kept reading after the cap (it doesn't in
	// production, but the Truncated flag exists to guard the loop), further
	// stdout frames are dropped. We simulate this by sending exactly-cap
	// output then one more stdout frame then FrameExit. Since we return at
	// the cap, the extra frame and exit are never read — but if a future
	// change makes the collector keep draining, the Truncated guard must
	// drop the post-cap frame. This test pins that contract by checking
	// the cap return happens before the extra frame.
	var s frameStream
	s.write(vshdproto.FrameStdout, bytes.Repeat([]byte("z"), MaxOutputSize()))
	s.write(vshdproto.FrameStdout, []byte("AFTER-CAP-MUST-NOT-APPEAR"))
	s.write(vshdproto.FrameExit, vshdproto.EncodeExit(7))
	r, err := CollectFrames(s.reader())
	if err != nil {
		t.Fatalf("CollectFrames: %v", err)
	}
	// Exactly-cap output does not trigger truncation (the > check is strict),
	// so the loop continues, reads AFTER-CAP... which DOES exceed the cap,
	// triggers truncation, and returns. The post-cap string must not appear.
	if !r.Truncated {
		t.Fatalf("Truncated = false, want true (AFTER-CAP frame should trip the cap)")
	}
	if strings.Contains(r.Output, "AFTER-CAP-MUST-NOT-APPEAR") {
		t.Errorf("post-cap frame leaked into Output (len %d)", len(r.Output))
	}
	// Exit code is 0 because we returned at the cap before FrameExit.
	if r.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0 (returned before FrameExit)", r.ExitCode)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
