// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !windows

package container

import "os"

// processKiller is a no-op on non-Windows platforms. The Job Object based
// tree-kill is only wired in on Windows (see exec()); elsewhere the default
// exec.CommandContext cancellation and Setpgid handling apply.
type processKiller struct{}

func newProcessKiller(_ *os.Process) (*processKiller, error) { return &processKiller{}, nil }

func (k *processKiller) Kill() error { return nil }

func (k *processKiller) Close() error { return nil }
