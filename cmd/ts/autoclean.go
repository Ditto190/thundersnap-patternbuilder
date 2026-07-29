// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// cmdAutoclean runs a foreground subprocess whose lifetime is tied to an
// inherited file descriptor. EOF on that descriptor immediately SIGKILLs the
// subprocess's process group. The subprocess also receives PDEATHSIG=SIGKILL,
// so it cannot survive autoclean itself being killed.
func cmdAutoclean(args []string) {
	fs := flag.NewFlagSet("autoclean", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	lifecycleFD := fs.Int("lifecycle-fd", -1, "file descriptor whose EOF kills the subprocess")
	var passFDs fdList
	fs.Var(&passFDs, "pass-fd", "inherited fd to pass to the subprocess (repeatable; assigned from fd 3 in order)")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	argv := fs.Args()
	if *lifecycleFD < 0 || len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "usage: ts autoclean --lifecycle-fd=N [--pass-fd=N ...] -- program [args...]")
		os.Exit(2)
	}

	lifecycle := os.NewFile(uintptr(*lifecycleFD), "autoclean-lifecycle")
	if lifecycle == nil {
		fmt.Fprintf(os.Stderr, "autoclean: invalid lifecycle fd %d\n", *lifecycleFD)
		os.Exit(2)
	}
	defer lifecycle.Close()

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: unix.SIGKILL,
	}
	for _, fd := range passFDs {
		f := os.NewFile(uintptr(fd), fmt.Sprintf("autoclean-pass-fd-%d", fd))
		if f == nil {
			fmt.Fprintf(os.Stderr, "autoclean: invalid pass fd %d\n", fd)
			os.Exit(2)
		}
		defer f.Close()
		cmd.ExtraFiles = append(cmd.ExtraFiles, f)
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "autoclean: start %s: %v\n", argv[0], err)
		os.Exit(1)
	}

	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	lifecycleGone := make(chan struct{})
	go func() {
		var b [1]byte
		_, _ = lifecycle.Read(b[:])
		close(lifecycleGone)
	}()

	select {
	case err := <-wait:
		exitLikeChild(err)
	case <-lifecycleGone:
		// The child leads a fresh process group. Kill the entire group so a
		// foreground daemon cannot leak helpers it happened to fork.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		err := <-wait
		exitLikeChild(err)
	}
}

func exitLikeChild(err error) {
	if err == nil {
		os.Exit(0)
	}
	if ee, ok := err.(*exec.ExitError); ok {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
			if ws.Signaled() {
				os.Exit(128 + int(ws.Signal()))
			}
			os.Exit(ws.ExitStatus())
		}
	}
	os.Exit(1)
}

type fdList []int

func (l *fdList) String() string {
	parts := make([]string, len(*l))
	for i, fd := range *l {
		parts[i] = strconv.Itoa(fd)
	}
	return strings.Join(parts, ",")
}

func (l *fdList) Set(s string) error {
	fd, err := strconv.Atoi(s)
	if err != nil || fd < 0 {
		return fmt.Errorf("invalid fd %q", s)
	}
	*l = append(*l, fd)
	return nil
}
