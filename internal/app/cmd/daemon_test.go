// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cmd

import (
	"testing"

	"gitea.com/gitea/runner/internal/pkg/config"

	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

func TestGetDockerSocketPathUsesConfigAndEnvironment(t *testing.T) {
	got, err := getDockerSocketPath("tcp://docker.example:2376")
	require.NoError(t, err)
	require.Equal(t, "tcp://docker.example:2376", got)

	t.Setenv("DOCKER_HOST", "unix:///tmp/docker.sock")
	got, err = getDockerSocketPath("-")
	require.NoError(t, err)
	require.Equal(t, "unix:///tmp/docker.sock", got)
}

func TestInitLoggingSetsLevelAndCaller(t *testing.T) {
	oldLevel := log.GetLevel()
	oldReportCaller := log.StandardLogger().ReportCaller
	t.Cleanup(func() {
		log.SetLevel(oldLevel)
		log.SetReportCaller(oldReportCaller)
	})

	cfg := &config.Config{}
	cfg.Log.Level = "debug"
	initLogging(cfg)

	require.Equal(t, log.DebugLevel, log.GetLevel())
	require.True(t, log.StandardLogger().ReportCaller)
}
