// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// passt's installed AppArmor policy permits socket creation beneath /tmp but
// not /run, so keep all of our runtime artifacts in one unmistakable subtree.
const runtimeRoot = "/tmp/thundersnapd"

// initRuntimeDir reclaims runtime directories whose owner PID is definitely
// gone, then creates a directory unique to this PID incarnation. We use a
// timestamp in the name to avoid PID-reuse collisions without depending on
// /proc. EPERM means the owner is alive but inaccessible and is preserved.
func initRuntimeDir() error {
	if err := os.MkdirAll(runtimeRoot, 0755); err != nil {
		return err
	}
	entries, err := os.ReadDir(runtimeRoot)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pidText, _, ok := strings.Cut(entry.Name(), "-")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(pidText)
		if err != nil || pid <= 0 {
			continue
		}
		err = syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			if err := os.RemoveAll(filepath.Join(runtimeRoot, entry.Name())); err != nil {
				return fmt.Errorf("remove stale runtime directory %s: %w", entry.Name(), err)
			}
		}
	}

	name := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	runtimeDir = filepath.Join(runtimeRoot, name)
	return os.Mkdir(runtimeDir, 0755)
}
