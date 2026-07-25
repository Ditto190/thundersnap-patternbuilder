// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tailscale/thundersnap/tsm"
	"golang.org/x/sys/unix"
)

// runAsSu implements a minimal `su` sufficient for vshd to switch to a non-root
// user inside a container that has no real su binary. ts is symlinked to
// /bin/su by thundersnapd (see copyTsBinary) when the frame rootfs lacks one,
// mirroring the /bin/sh -> ts trick that lets containers work with no
// userspace tools at all.
//
// Supported forms (the subset vshd's buildSessionCmd emits):
//
//	su - <user>              # interactive login shell as <user>
//	su - <user> -c <cmd>     # run <cmd> as <user> via a login shell
//	su -                     # interactive login shell as root
//	su -c <cmd>              # run <cmd> as root
//	su <user>                # non-login shell as <user>
//
// It looks up <user> in /etc/passwd (we run after chroot, so "/" is the
// rootfs), drops privileges to that user's uid/gid (CAP_SETUID/CAP_SETGID are
// retained by drop-caps-and-run, so this works as root), chdirs to the user's
// home (falling back to "/" if it is missing rather than aborting like busybox
// su does), and execs the user's login shell (defaulting to /bin/sh, which is
// ts itself in a minimal frame). For a login shell argv[0] is "-sh" so the
// shell enters login mode.
//
// Anything beyond this subset (e.g. -s /bin/bash, -w, -m) is accepted but
// ignored, matching the leniency of a rescue su.
func runAsSu(args []string) {
	login := false
	user := "root"
	var cmd string
	var shellOverride string

	i := 0
	if i < len(args) {
		switch args[i] {
		case "-":
			login = true
			i++
		case "-l", "--login":
			login = true
			i++
		}
	}
	// First non-flag argument is the target username.
	if i < len(args) && !strings.HasPrefix(args[i], "-") {
		user = args[i]
		i++
	}
	// Remaining options.
	for i < len(args) {
		switch args[i] {
		case "-c", "--command":
			i++
			if i < len(args) {
				cmd = args[i]
				i++
			} else {
				fatalSu("-%s requires an argument", "c")
			}
		case "-s", "--shell":
			i++
			if i < len(args) {
				shellOverride = args[i]
				i++
			}
		case "-m", "-p", "--preserve-environment":
			i++ // accepted, ignored
		default:
			i++ // ignore unknown options
		}
	}

	// Resolve the target user. We run after chroot, so "/" is the container
	// rootfs. If the user is missing from /etc/passwd, fall back to root so a
	// bare `su -` still works in a frame with no passwd entry.
	ui := tsm.LookupUser("/", user)
	uid, gid := uint32(0), uint32(0)
	home := "/root"
	shell := "/bin/sh"
	if ui != nil {
		uid, gid = ui.UID, ui.GID
		if ui.Home != "" {
			home = ui.Home
		}
		if ui.Shell != "" {
			shell = ui.Shell
		}
	}
	if shellOverride != "" {
		shell = shellOverride
	}
	// A non-existent shell is useless; fall back to /bin/sh (ts) so a passwd
	// entry naming /bin/bash on a frame without bash still logs in.
	if _, err := os.Stat(shell); err != nil {
		shell = "/bin/sh"
	}

	// Drop privileges to the target user. We must set groups+gid before uid
	// (once uid is non-zero we lose the ability to change gid). These calls
	// require CAP_SETGID/CAP_SETUID, which drop-caps-and-run retains in the
	// bounding set; we are still root here, so they succeed. Switching to root
	// (uid 0) is a no-op setuid(0) which is also fine.
	if gid != 0 {
		if err := unix.Setgroups([]int{int(gid)}); err != nil {
			fatalSu("setgroups: %v", err)
		}
		if err := unix.Setgid(int(gid)); err != nil {
			fatalSu("setgid: %v", err)
		}
	}
	if uid != 0 {
		if err := unix.Setuid(int(uid)); err != nil {
			fatalSu("setuid: %v", err)
		}
	}

	// chdir to the user's home. A missing home is NOT fatal (busybox su makes
	// it fatal, which breaks login in a nil:nil:nil frame whose /home exists
	// but /home/user does not); fall back to "/" so the shell still starts.
	if home != "" {
		if err := os.Chdir(home); err != nil {
			os.Chdir("/")
		}
	} else {
		os.Chdir("/")
	}

	// Build the environment for the new session. Preserve a few safe vars from
	// the caller; set the identity vars to the target user.
	env := identityEnv(user, home, shell)

	// Build argv for the exec. For a login shell, argv[0] is "-" + the shell
	// basename (e.g. "-sh") so the shell enters login mode; otherwise it is the
	// bare basename. With -c, run "shell -c cmd".
	shellBase := filepath.Base(shell)
	argv0 := shellBase
	if login {
		argv0 = "-" + shellBase
	}
	var argv []string
	if cmd != "" {
		argv = []string{shellBase, "-c", cmd}
	} else {
		argv = []string{argv0}
	}

	if err := unix.Exec(shell, argv, env); err != nil {
		fatalSu("exec %s: %v", shell, err)
	}
}

// identityEnv returns a minimal environment for a switched-to user: HOME, USER,
// LOGNAME, SHELL, and PATH. It carries TERM (so a PTY shell renders correctly)
// from the current environment but otherwise starts clean so a login shell
// re-reads its profile rather than inheriting the caller's session vars.
func identityEnv(user, home, shell string) []string {
	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	env := []string{
		"HOME=" + home,
		"USER=" + user,
		"LOGNAME=" + user,
		"SHELL=" + shell,
		"PATH=" + path,
	}
	if term := os.Getenv("TERM"); term != "" {
		env = append(env, "TERM="+term)
	}
	return env
}

// fatalSu prints a su-style error and exits non-zero, mirroring how a real su
// reports a failure to switch users.
func fatalSu(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "su: ")
	fmt.Fprintf(os.Stderr, format, args...)
	fmt.Fprintln(os.Stderr)
	os.Exit(1)
}
