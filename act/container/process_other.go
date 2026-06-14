// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build plan9

package container

import "os"

// processKiller falls back to single-process termination on platforms without
// a process-group / Job Object tree-kill. The Job Object (Windows) and process
// group (Unix) based tree-kills live in process_windows.go / process_unix.go;
// here we just kill the direct child, matching the previous default behaviour.
type processKiller struct {
	p *os.Process
}

func newProcessKiller(p *os.Process) (*processKiller, error) {
	return &processKiller{p: p}, nil
}

func (k *processKiller) Kill() error {
	if k == nil || k.p == nil {
		return nil
	}
	return k.p.Kill()
}

func (k *processKiller) Close() error { return nil }
