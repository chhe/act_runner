// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !WITHOUT_DOCKER && (linux || darwin || netbsd)

package container

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

func copyDockerSocketPermissions(socket string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("docker socket ownership is unavailable")
	}
	if err := os.Chown(socket, int(stat.Uid), int(stat.Gid)); err != nil {
		return err
	}
	if err := os.Chown(filepath.Dir(socket), int(stat.Uid), -1); err != nil {
		return err
	}
	return os.Chmod(socket, info.Mode().Perm())
}
