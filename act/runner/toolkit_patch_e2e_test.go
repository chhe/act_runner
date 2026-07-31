// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitea.com/gitea/runner/act/artifactcache"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// actionsCacheRef pins the actions/cache release this is verified against. Bump it
// deliberately: a new release is exactly what can stop the patch matching.
const actionsCacheRef = "v6.1.0"

// bundleFromGitHub downloads one entrypoint, keeping it in the user cache dir so repeated runs
// cost nothing. The bundles are megabytes, too large to vendor.
func bundleFromGitHub(t *testing.T, repo, ref, path string) string {
	t.Helper()

	cacheDir, err := os.UserCacheDir()
	require.NoError(t, err)
	dir := filepath.Join(cacheDir, "gitea-runner-test", strings.ReplaceAll(repo, "/", "-")+"-"+ref)
	bundle := filepath.Join(dir, strings.ReplaceAll(path, "/", "-"))
	if _, err := os.Stat(bundle); err == nil {
		return bundle
	}
	require.NoError(t, os.MkdirAll(dir, 0o755))

	url := "https://raw.githubusercontent.com/" + repo + "/" + ref + "/" + path
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Skipf("cannot reach %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Skipf("GET %s: %s", url, resp.Status)
	}
	file, err := os.Create(bundle)
	require.NoError(t, err)
	_, err = io.Copy(file, resp.Body)
	require.NoError(t, file.Close())
	require.NoError(t, err)
	return bundle
}

// runCacheAction runs one entrypoint the way a job would: a real Gitea server URL, and a results
// URL that points at Gitea rather than at the runner. Nothing about the environment is rewritten,
// so only the patch can make the client choose v2 and find the cache server.
func runCacheAction(t *testing.T, script, workspace, runnerTemp, cacheURL, token, key string) string {
	t.Helper()
	state := filepath.Join(runnerTemp, "state")
	output := filepath.Join(runnerTemp, "output")
	for _, name := range []string{state, output} {
		require.NoError(t, os.WriteFile(name, nil, 0o600))
	}

	cmd := exec.CommandContext(t.Context(), "node", script)
	cmd.Dir = workspace
	cmd.Env = append(os.Environ(),
		"INPUT_PATH=to-cache",
		"INPUT_KEY="+key,
		"ACTIONS_RUNTIME_TOKEN="+token,
		"ACTIONS_CACHE_URL="+cacheURL+"/",
		// Unreachable on purpose: the artifact service lives here, the cache service must not.
		"ACTIONS_RESULTS_URL=https://gitea.example",
		"ACTIONS_CACHE_SERVICE_V2=true",
		"GITHUB_SERVER_URL=https://gitea.example.com",
		"GITHUB_REF=refs/heads/main",
		"GITHUB_EVENT_NAME=push",
		"GITHUB_WORKSPACE="+workspace,
		"RUNNER_TEMP="+runnerTemp,
		"GITHUB_STATE="+state,
		"GITHUB_OUTPUT="+output,
	)
	out, err := cmd.CombinedOutput()
	t.Logf("%s:\n%s", filepath.Base(filepath.Dir(script)), out)
	require.NoError(t, err, "%s failed", script)
	return string(out)
}

// tempDirPath is TempDir with symlinks resolved, because macOS hands out /var paths that resolve
// to /private/var and the client derives archive paths relative to the workspace.
func tempDirPath(t *testing.T) string {
	t.Helper()

	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return dir
}

// The whole chain against the pinned release, whose bundles ship unminified: patch them, run the
// real client with an ordinary Gitea server URL and a results URL that goes nowhere, and have it
// save and restore through this runner's cache server. The unreachable results URL is the point,
// it is what proves the cache reaches the runner without the runner fronting Gitea. If a release
// stops matching the patch the client falls back to v1 and this fails on the version line, which
// is the signal to look at the new bundle.
func TestCacheServiceV2EndToEnd(t *testing.T) {
	requireHostTools(t, "node")

	// A stand-in action directory, patched exactly as a downloaded one would be.
	actionDir := tempDirPath(t)
	scripts := map[string]string{}
	for _, stage := range []string{"restore", "save"} {
		body, err := os.ReadFile(bundleFromGitHub(t, "actions/cache", actionsCacheRef, "dist/"+stage+"/index.js"))
		require.NoError(t, err)
		scripts[stage] = filepath.Join(actionDir, stage+".js")
		require.NoError(t, os.WriteFile(scripts[stage], body, 0o600))
	}
	patchToolkit(t.Context(), actionDir, []string{scripts["restore"], scripts["save"]})

	handler, err := artifactcache.StartHandler(filepath.Join(t.TempDir(), "cache"), "127.0.0.1", 0, "", nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = handler.Close() })
	const token, repo = "e2e-runtime-token", "testuser/testrepo"
	handler.RegisterJob(token, repo)

	workspace, runnerTemp := tempDirPath(t), tempDirPath(t)
	require.NoError(t, os.MkdirAll(filepath.Join(workspace, "to-cache"), 0o755))
	content := []byte("cached through the patched gate")
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "to-cache", "data.txt"), content, 0o600))

	const key = "patched-gate-key"
	missed := runCacheAction(t, scripts["restore"], workspace, runnerTemp, handler.ExternalURL(), token, key)
	require.Contains(t, missed, "Cache service version: v2", "the patch did not take, the client stayed on v1")
	require.Contains(t, missed, "Cache not found for input keys: "+key)

	saved := runCacheAction(t, scripts["save"], workspace, runnerTemp, handler.ExternalURL(), token, key)
	require.Contains(t, saved, "Cache saved with key: "+key)

	restored := tempDirPath(t)
	hit := runCacheAction(t, scripts["restore"], restored, runnerTemp, handler.ExternalURL(), token, key)
	require.Contains(t, hit, "Cache restored from key: "+key)

	got, err := os.ReadFile(filepath.Join(restored, "to-cache", "data.txt"))
	require.NoError(t, err)
	assert.Equal(t, content, got)
}

// The gate and the URL getter are separate functions, and a bundler may put either first: the gap
// between them runs from 159 to 1179 bytes across these actions, which is why neither edit is
// anchored on that distance. One entrypoint from each of the families that bundle the cache
// toolkit, patched but not run, is what keeps a future release from quietly matching only one of
// the two shapes and leaving every cache on v1.
func TestToolkitPatchAcrossActions(t *testing.T) {
	for _, tc := range []struct{ repo, ref, path string }{
		{"actions/setup-go", "v7.0.0", "dist/setup/index.js"},
		{"actions/setup-node", "v6.0.0", "dist/cache-save/index.js"},
		{"actions/setup-python", "v6.0.0", "dist/setup/index.js"},
		{"ruby/setup-ruby", "v1.271.0", "dist/index.js"},
		{"pnpm/action-setup", "v6.0.9", "dist/index.js"},
		// The artifact toolkit, where the gate is a refusal and there is nothing to redirect.
		// v4.4.0 is the first release whose gate carries the localhost test this matches; the
		// releases before it refuse in a shape the runner leaves alone.
		{"actions/upload-artifact", "v4.4.0", "dist/upload/index.js"},
		{"actions/upload-artifact", "v7.0.1", "dist/upload/index.js"},
		{"actions/download-artifact", "v6.0.0", "dist/index.js"},
		{"oven-sh/setup-bun", "v2.2.0", "dist/setup/index.js"},
	} {
		t.Run(tc.repo+"@"+tc.ref, func(t *testing.T) {
			t.Parallel()

			data, err := os.ReadFile(bundleFromGitHub(t, tc.repo, tc.ref, tc.path))
			require.NoError(t, err)

			out, patched := patchedBundle(data)
			assert.True(t, patched, "the version gate was not patched")
			assert.NotContains(t, string(out), ".LOCALHOST", "a copy of the gate was missed")

			if !strings.Contains(string(data), CacheServiceV2Env) {
				return // the artifact toolkit: a refusal to open, and no URL to move
			}
			// Only the reads inside getCacheServiceURL are rewritten. The others, such as the
			// feature-availability check, must be left as they are.
			assert.NotZero(t, strings.Count(string(out), "(process.env."+cacheURLEnv+"||process.env"),
				"the cache service URL was not redirected")
			assert.Equal(t, strings.Count(string(data), resultsURLEnv), strings.Count(string(out), resultsURLEnv),
				"a read of the results URL was lost, it must stay as the fallback")
		})
	}
}
