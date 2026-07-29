// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestAutocleanLifecycleEOF(t *testing.T) {
	if os.Getenv("TS_AUTOCLEAN_TEST") == "1" {
		cmdAutoclean([]string{"--lifecycle-fd=3", "--", "/bin/sh", "-c", "trap '' HUP TERM; echo $$ >&4; while :; do :; done"})
		return
	}

	lifecycleR, lifecycleW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	pidR, pidW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestAutocleanLifecycleEOF")
	cmd.Env = append(os.Environ(), "TS_AUTOCLEAN_TEST=1")
	cmd.ExtraFiles = []*os.File{lifecycleR, pidW}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	lifecycleR.Close()
	pidW.Close()
	waited := false
	defer func() {
		if !waited {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	}()

	var childPID int
	if _, err := fmtFscanWithTimeout(pidR, &childPID, 5*time.Second); err != nil {
		t.Fatalf("read child pid: %v", err)
	}
	if err := syscall.Kill(childPID, 0); err != nil {
		t.Fatalf("child %d not alive: %v", childPID, err)
	}

	lifecycleW.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		waited = true
	case <-time.After(5 * time.Second):
		t.Fatal("autoclean did not exit after lifecycle EOF")
	}
	if err := syscall.Kill(childPID, 0); err != syscall.ESRCH {
		t.Fatalf("child %d survived lifecycle EOF: kill(0)=%v", childPID, err)
	}
}

func fmtFscanWithTimeout(r *os.File, value *int, timeout time.Duration) (int, error) {
	type result struct {
		n   int
		err error
	}
	ch := make(chan result, 1)
	go func() {
		var buf [64]byte
		n, err := r.Read(buf[:])
		if err == nil {
			_, err = fmt.Sscanf(string(buf[:n]), "%d", value)
		}
		ch <- result{n, err}
	}()
	select {
	case res := <-ch:
		return res.n, res.err
	case <-time.After(timeout):
		return 0, os.ErrDeadlineExceeded
	}
}
