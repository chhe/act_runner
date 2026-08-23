// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import (
	"testing"
)

func testActionsCacheRoundTrip(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		v2   bool
	}{
		{name: "cache_v2", v2: true},
		{name: "cache_v1", v2: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			api, repo, _ := startIsolatedScenario(t, "cache.yml", "e2e-cache", runnerOptions{cacheV2: &tc.v2})

			wfRun := waitForRun(t, api, repo)
			requireSuccess(t, api, repo, wfRun.ID)
		})
	}
}
