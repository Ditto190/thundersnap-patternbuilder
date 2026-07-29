// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

type processIdentity struct {
	pid       int
	startTime string
	cmdline   string
}

func processDescendants(root int) ([]processIdentity, error) {
	type proc struct {
		processIdentity
		ppid int
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	all := make(map[int]proc)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if err != nil {
			continue
		}
		closeParen := strings.LastIndexByte(string(stat), ')')
		if closeParen < 0 {
			continue
		}
		fields := strings.Fields(string(stat[closeParen+1:]))
		// fields starts at stat field 3 (state); starttime is field 22.
		if len(fields) < 20 {
			continue
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		cmdline, _ := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		all[pid] = proc{processIdentity: processIdentity{
			pid:       pid,
			startTime: fields[19],
			cmdline:   strings.ReplaceAll(string(cmdline), "\x00", " "),
		}, ppid: ppid}
	}

	owned := map[int]bool{root: true}
	changed := true
	for changed {
		changed = false
		for pid, p := range all {
			if !owned[pid] && owned[p.ppid] {
				owned[pid] = true
				changed = true
			}
		}
	}
	var result []processIdentity
	for pid, p := range all {
		if pid != root && owned[pid] {
			result = append(result, p.processIdentity)
		}
	}
	return result, nil
}

func processStillExists(p processIdentity) bool {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", p.pid))
	if err != nil {
		return false
	}
	closeParen := strings.LastIndexByte(string(stat), ')')
	if closeParen < 0 {
		return false
	}
	fields := strings.Fields(string(stat[closeParen+1:]))
	return len(fields) >= 20 && fields[19] == p.startTime
}

func assertCrashReapsDescendants(t *testing.T, d *daemonInstance, wantCmdlines ...string) {
	t.Helper()
	var owned []processIdentity
	var err error
	deadline := time.Now().Add(15 * time.Second)
	for {
		owned, err = processDescendants(d.cmd.Process.Pid)
		if err != nil {
			t.Fatalf("enumerate daemon descendants: %v", err)
		}
		allFound := true
		for _, want := range wantCmdlines {
			found := false
			for _, p := range owned {
				if strings.Contains(p.cmdline, want) {
					found = true
					break
				}
			}
			allFound = allFound && found
		}
		if allFound || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	var dump strings.Builder
	for _, p := range owned {
		fmt.Fprintf(&dump, "pid=%d start=%s cmd=%q\n", p.pid, p.startTime, p.cmdline)
	}
	t.Logf("daemon descendants before SIGKILL:\n%s", dump.String())
	for _, want := range wantCmdlines {
		found := false
		for _, p := range owned {
			found = found || strings.Contains(p.cmdline, want)
		}
		if !found {
			t.Fatalf("no daemon descendant contains %q; descendants:\n%s", want, dump.String())
		}
	}

	d.killAbruptly()
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		remaining := owned[:0]
		for _, p := range owned {
			if processStillExists(p) {
				remaining = append(remaining, p)
			}
		}
		owned = remaining
		if len(owned) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	dump.Reset()
	for _, p := range owned {
		fmt.Fprintf(&dump, "pid=%d start=%s cmd=%q\n", p.pid, p.startTime, p.cmdline)
	}
	t.Fatalf("daemon descendants survived SIGKILL:\n%s", dump.String())
}

func startHangingSession(t *testing.T, d *daemonInstance, user, command string, pty bool) (*ssh.Client, *ssh.Session) {
	t.Helper()
	client, err := ssh.Dial("tcp", d.addr, sshConfig(user))
	if err != nil {
		t.Fatalf("dial hanging session: %v", err)
	}
	session, err := client.NewSession()
	if err != nil {
		client.Close()
		t.Fatalf("new hanging session: %v", err)
	}
	if pty {
		if err := session.RequestPty("xterm", 24, 80, ssh.TerminalModes{}); err != nil {
			session.Close()
			client.Close()
			t.Fatalf("request pty: %v", err)
		}
	}
	if err := session.Start(command); err != nil {
		session.Close()
		client.Close()
		t.Fatalf("start hanging session: %v", err)
	}
	return client, session
}

func testContainerCrashLifecycle(t *testing.T, d *daemonInstance) {
	createFrameViaDaemon(t, d, "crashlife")
	installBusyboxAppletInFrame(t, d, "crashlife", "setsid")

	_, exit, err := sshExec(t, d, "root@crashlife", `ts autorun --ref crashlife /bin/sh -c 'while :; do :; done'`)
	if err != nil || exit != 0 {
		t.Fatalf("start crash autorun: err=%v exit=%d", err, exit)
	}
	client1, session1 := startHangingSession(t, d, "root@crashlife", `setsid /bin/sh -c 'while :; do :; done' & while :; do :; done`, false)
	defer client1.Close()
	defer session1.Close()
	client2, session2 := startHangingSession(t, d, "root@crashlife", `while :; do :; done`, true)
	defer client2.Close()
	defer session2.Close()
	time.Sleep(200 * time.Millisecond)

	assertCrashReapsDescendants(t, d, "vshd", "container-init", "session-serve")
}

func testVMCrashLifecycle(t *testing.T, d *daemonInstance) {
	createFrameViaDaemon(t, d, "vmcrashlife")
	client, session := startHangingSession(t, d, "vm/root@vmcrashlife", `while :; do :; done`, true)
	defer client.Close()
	defer session.Close()
	time.Sleep(200 * time.Millisecond)

	assertCrashReapsDescendants(t, d, "autoclean", "cloud-hypervisor", "virtiofsd", "passt")
}
