// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package container

import (
	"os"

	"golang.org/x/sys/windows"
)

// processKiller terminates a step process together with its entire descendant
// tree via a Windows Job Object.
//
// Background: a step often launches a process tree (a shell that starts a
// child which in turn spawns further GUI or background processes). The default
// exec.CommandContext cancellation only kills the direct child, so cancelling a
// job left the rest of the tree running. Because those orphans inherited the
// step's stdout/stderr pipe, cmd.Wait() also blocked forever and the runner hung.
//
// Assigning the step process to a Job Object lets us kill the whole tree
// atomically on cancellation (TerminateJobObject), which also closes the
// inherited pipe handles so cmd.Wait() can return.
type processKiller struct {
	job windows.Handle
}

// newProcessKiller creates a Job Object and assigns p (an already-started
// process) to it. Children spawned by p afterwards are automatically part of
// the job. The job does NOT use JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, so closing
// the handle on normal completion does not kill legitimate background
// processes; the tree is only torn down by an explicit Kill (cancellation).
func newProcessKiller(p *os.Process) (*processKiller, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}

	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(p.Pid))
	if err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	defer windows.CloseHandle(h)

	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		windows.CloseHandle(job)
		return nil, err
	}

	return &processKiller{job: job}, nil
}

// Kill terminates every process currently assigned to the job (the step process
// and all of its descendants).
func (k *processKiller) Kill() error {
	if k == nil || k.job == 0 {
		return nil
	}
	return windows.TerminateJobObject(k.job, 1)
}

// Close releases the job handle. It does not terminate the processes.
func (k *processKiller) Close() error {
	if k == nil || k.job == 0 {
		return nil
	}
	h := k.job
	k.job = 0
	return windows.CloseHandle(h)
}
