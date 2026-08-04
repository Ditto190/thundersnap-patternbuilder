// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/tailscale/thundersnap/tsm"
	"github.com/tailscale/thundersnap/vshdsession"
)

// cmdSessionServe is the in-container endpoint of a vshd session. vshd runs it
// (via nsenter + drop-caps-and-run --chroot) as:
//
//	ts session-serve <ptyFlag> <ptyOwner> <argc> <arg0> <arg1> ...
//
// where ptyFlag is "1" for a PTY session and "0" otherwise, ptyOwner is the
// resolved Unix username (or empty), and the args are the
// final command to run (e.g. "su - user" or a shell). It speaks the vshdproto
// TLV protocol on its own stdin/stdout, which vshd splices verbatim to/from the
// network connection. Crucially, because session-serve runs AFTER the chroot
// into the container rootfs, opening the pty here allocates the slave from the
// container's own devpts instance, so it is visible as /dev/pts/N inside the
// container (and `ps` shows the real pts as the controlling terminal).
func cmdSessionServe(args []string) {
	if len(args) < 3 {
		fmt.Fprintln(os.Stderr, "error: session-serve requires <ptyFlag> <ptyOwner> <argc> [args...]")
		os.Exit(1)
	}
	wantPTY := args[0] == "1"
	ptyOwnerUID := -1
	if wantPTY && args[1] != "" {
		if ui := tsm.LookupUser("/", args[1]); ui != nil {
			ptyOwnerUID = int(ui.UID)
		} else if args[1] == "root" {
			ptyOwnerUID = 0
		}
		// Unknown users are rejected by the eventual su command, preserving its
		// framed non-zero exit and diagnostic. Only known users need a chown.
	}
	argc, err := strconv.Atoi(args[2])
	if err != nil || argc < 0 {
		fmt.Fprintf(os.Stderr, "error: session-serve: invalid argc %q\n", args[2])
		os.Exit(1)
	}
	rest := args[3:]
	if len(rest) < argc {
		fmt.Fprintf(os.Stderr, "error: session-serve: expected %d args, got %d\n", argc, len(rest))
		os.Exit(1)
	}
	argv := rest[:argc]
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "error: session-serve: empty command")
		os.Exit(1)
	}

	// Resolve the command via PATH so a bare "su" works in minimal containers.
	exe, err := findExecutable(argv[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: session-serve: %v\n", err)
		os.Exit(1)
	}

	cmd := exec.Command(exe, argv[1:]...)
	cmd.Env = os.Environ()

	// vshd splices our stdin/stdout to the client connection; serve the session
	// over them. logf is nil (diagnostics, if any, go to our stderr which vshd
	// surfaces in its own log).
	vshdsession.Serve(os.Stdout, os.Stdin, cmd, wantPTY, ptyOwnerUID, nil, nil)
}

// cmdAutorunRun runs one attempt and, only after a non-zero exit, leaves a
// conspicuously named retry process in the same container session. The daemon
// reconnects after this command exits, so the retry delay is visible in ps axf.
func cmdAutorunRun(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "error: autorun-run requires <retry-duration> <program> [args...]")
		os.Exit(1)
	}
	delay, err := time.ParseDuration(args[0])
	if err != nil || delay < 0 {
		fmt.Fprintf(os.Stderr, "error: autorun-run: invalid retry duration %q\n", args[0])
		os.Exit(1)
	}
	exe, err := findExecutable(args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "autorun: %v\n", err)
		retryOnFail(delay)
		os.Exit(1)
	}
	cmd := exec.Command(exe, args[2:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = os.Environ()
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "autorun: %v\n", err)
		retryOnFail(delay)
		os.Exit(1)
	}
}

func cmdRetryOnFail(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "error: retry-on-fail requires <duration>")
		os.Exit(1)
	}
	delay, err := time.ParseDuration(args[0])
	if err != nil || delay < 0 {
		fmt.Fprintf(os.Stderr, "error: retry-on-fail: invalid duration %q\n", args[0])
		os.Exit(1)
	}
	sleepRetry(delay)
}

func retryOnFail(delay time.Duration) {
	self, err := os.Executable()
	if err == nil {
		cmd := exec.Command(self, "retry-on-fail", delay.String())
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		cmd.Env = os.Environ()
		if cmd.Run() == nil {
			return
		}
	}
	// Keep the retry delay even if re-exec is unavailable; only the diagnostic
	// process name is lost.
	sleepRetry(delay)
}

func sleepRetry(delay time.Duration) {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	<-timer.C
}
