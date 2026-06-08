// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2022 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/container"
	"gitea.com/gitea/runner/act/model"

	logrustest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestJobExecutor(t *testing.T) {
	// Dryrun only checks syntax/planning; all cases resolve locally, so this runs offline.
	tables := []TestJobFileInfo{
		{workdir, "uses-and-run-in-one-step", "push", "Invalid run/uses syntax for job:test step:Test", platforms, secrets},
		{workdir, "uses-github-empty", "push", "Expected format {org}/{repo}[/path]@ref", platforms, secrets},
		{workdir, "uses-github-noref", "push", "Expected format {org}/{repo}[/path]@ref", platforms, secrets},
		{workdir, "uses-github-root", "push", "", platforms, secrets},
		{workdir, "uses-docker-url", "push", "", platforms, secrets},
		{workdir, "job-nil-step", "push", "invalid Step 0: missing run or uses key", platforms, secrets},
	}
	// These tests are sufficient to only check syntax.
	ctx := common.WithDryrun(context.Background(), true)
	for _, table := range tables {
		t.Run(table.workflowPath, func(t *testing.T) {
			table.runTest(ctx, t, &Config{})
		})
	}
}

type jobInfoMock struct {
	mock.Mock
}

func (jim *jobInfoMock) matrix() map[string]any {
	args := jim.Called()
	return args.Get(0).(map[string]any)
}

func (jim *jobInfoMock) steps() []*model.Step {
	args := jim.Called()

	return args.Get(0).([]*model.Step)
}

func (jim *jobInfoMock) startContainer() common.Executor {
	args := jim.Called()

	return args.Get(0).(func(context.Context) error)
}

func (jim *jobInfoMock) stopContainer() common.Executor {
	args := jim.Called()

	return args.Get(0).(func(context.Context) error)
}

func (jim *jobInfoMock) closeContainer() common.Executor {
	args := jim.Called()

	return args.Get(0).(func(context.Context) error)
}

func (jim *jobInfoMock) interpolateOutputs() common.Executor {
	args := jim.Called()

	return args.Get(0).(func(context.Context) error)
}

func (jim *jobInfoMock) result(result string) {
	jim.Called(result)
}

type jobContainerMock struct {
	container.Container
	container.LinuxContainerEnvironmentExtensions
}

func (jcm *jobContainerMock) ReplaceLogWriter(_, _ io.Writer) (io.Writer, io.Writer) {
	return nil, nil
}

type stepFactoryMock struct {
	mock.Mock
}

func (sfm *stepFactoryMock) newStep(model *model.Step, rc *RunContext) (step, error) {
	args := sfm.Called(model, rc)
	return args.Get(0).(step), args.Error(1)
}

func TestNewJobExecutor(t *testing.T) {
	table := []struct {
		name          string
		steps         []*model.Step
		preSteps      []bool
		postSteps     []bool
		executedSteps []string
		result        string
		hasError      bool
	}{
		{
			name:          "zeroSteps",
			steps:         []*model.Step{},
			preSteps:      []bool{},
			postSteps:     []bool{},
			executedSteps: []string{},
			result:        "success",
			hasError:      false,
		},
		{
			name: "stepWithoutPrePost",
			steps: []*model.Step{{
				ID: "1",
			}},
			preSteps:  []bool{false},
			postSteps: []bool{false},
			executedSteps: []string{
				"startContainer",
				"step1",
				"stopContainer",
				"interpolateOutputs",
				"closeContainer",
			},
			result:   "success",
			hasError: false,
		},
		{
			name: "stepWithFailure",
			steps: []*model.Step{{
				ID: "1",
			}},
			preSteps:  []bool{false},
			postSteps: []bool{false},
			executedSteps: []string{
				"startContainer",
				"step1",
				"interpolateOutputs",
				"closeContainer",
			},
			result:   "failure",
			hasError: true,
		},
		{
			name: "stepWithPre",
			steps: []*model.Step{{
				ID: "1",
			}},
			preSteps:  []bool{true},
			postSteps: []bool{false},
			executedSteps: []string{
				"startContainer",
				"pre1",
				"step1",
				"stopContainer",
				"interpolateOutputs",
				"closeContainer",
			},
			result:   "success",
			hasError: false,
		},
		{
			name: "stepWithPost",
			steps: []*model.Step{{
				ID: "1",
			}},
			preSteps:  []bool{false},
			postSteps: []bool{true},
			executedSteps: []string{
				"startContainer",
				"step1",
				"post1",
				"stopContainer",
				"interpolateOutputs",
				"closeContainer",
			},
			result:   "success",
			hasError: false,
		},
		{
			name: "stepWithPreAndPost",
			steps: []*model.Step{{
				ID: "1",
			}},
			preSteps:  []bool{true},
			postSteps: []bool{true},
			executedSteps: []string{
				"startContainer",
				"pre1",
				"step1",
				"post1",
				"stopContainer",
				"interpolateOutputs",
				"closeContainer",
			},
			result:   "success",
			hasError: false,
		},
		{
			name: "stepsWithPreAndPost",
			steps: []*model.Step{{
				ID: "1",
			}, {
				ID: "2",
			}, {
				ID: "3",
			}},
			preSteps:  []bool{true, false, true},
			postSteps: []bool{false, true, true},
			executedSteps: []string{
				"startContainer",
				"pre1",
				"pre3",
				"step1",
				"step2",
				"step3",
				"post3",
				"post2",
				"stopContainer",
				"interpolateOutputs",
				"closeContainer",
			},
			result:   "success",
			hasError: false,
		},
	}

	contains := func(needle string, haystack []string) bool {
		return slices.Contains(haystack, needle)
	}

	for _, tt := range table {
		t.Run(tt.name, func(t *testing.T) {
			fmt.Printf("::group::%s\n", tt.name) //nolint:forbidigo // pre-existing issue from nektos/act

			ctx := common.WithJobErrorContainer(context.Background())
			jim := &jobInfoMock{}
			sfm := &stepFactoryMock{}
			rc := &RunContext{
				JobContainer: &jobContainerMock{},
				Run: &model.Run{
					JobID: "test",
					Workflow: &model.Workflow{
						Jobs: map[string]*model.Job{
							"test": {},
						},
					},
				},
				Config: &Config{},
			}
			rc.ExprEval = rc.NewExpressionEvaluator(ctx)
			executorOrder := make([]string, 0)

			jim.On("steps").Return(tt.steps)

			if len(tt.steps) > 0 {
				jim.On("startContainer").Return(func(ctx context.Context) error {
					executorOrder = append(executorOrder, "startContainer")
					return nil
				})
			}

			for i, stepModel := range tt.steps {
				sm := &stepMock{}

				sfm.On("newStep", stepModel, rc).Return(sm, nil)

				sm.On("pre").Return(func(ctx context.Context) error {
					if tt.preSteps[i] {
						executorOrder = append(executorOrder, "pre"+stepModel.ID)
					}
					return nil
				})

				sm.On("main").Return(func(ctx context.Context) error {
					executorOrder = append(executorOrder, "step"+stepModel.ID)
					if tt.hasError {
						return errors.New("error")
					}
					return nil
				})

				sm.On("post").Return(func(ctx context.Context) error {
					if tt.postSteps[i] {
						executorOrder = append(executorOrder, "post"+stepModel.ID)
					}
					return nil
				})

				defer sm.AssertExpectations(t)
			}

			if len(tt.steps) > 0 {
				jim.On("matrix").Return(map[string]any{})

				jim.On("interpolateOutputs").Return(func(ctx context.Context) error {
					executorOrder = append(executorOrder, "interpolateOutputs")
					return nil
				})

				if contains("stopContainer", tt.executedSteps) {
					jim.On("stopContainer").Return(func(ctx context.Context) error {
						executorOrder = append(executorOrder, "stopContainer")
						return nil
					})
				}

				jim.On("result", tt.result)

				jim.On("closeContainer").Return(func(ctx context.Context) error {
					executorOrder = append(executorOrder, "closeContainer")
					return nil
				})
			}

			executor := newJobExecutor(jim, sfm, rc)
			err := executor(ctx)
			assert.NoError(t, err) //nolint:testifylint // pre-existing issue from nektos/act
			assert.Equal(t, tt.executedSteps, executorOrder)

			jim.AssertExpectations(t)
			sfm.AssertExpectations(t)

			fmt.Println("::endgroup::") //nolint:forbidigo // pre-existing issue from nektos/act
		})
	}
}

func TestHasJobSummaryCapability(t *testing.T) {
	assert.True(t, hasJobSummaryCapability("cache,job-summary artifacts"))
	assert.True(t, hasJobSummaryCapability("cache,\njob-summary\tartifacts"))
	assert.False(t, hasJobSummaryCapability("not-job-summary,job-summary-v2"))
}

// fakeRuntimeToken builds a JWT-shaped string whose middle (claims) segment encodes
// the given JobID. The header and signature segments are filler — the runner does not
// verify the signature; the server does.
func fakeRuntimeToken(jobID int64) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	claims := base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, `{"JobID":%d}`, jobID))
	sig := base64.RawURLEncoding.EncodeToString([]byte("sig"))
	return header + "." + claims + "." + sig
}

func newJobSummaryRC(env map[string]string, jobContainer container.ExecutionsEnvironment, stepCount int) *RunContext {
	steps := make([]*model.Step, stepCount)
	for i := range steps {
		steps[i] = &model.Step{ID: strconv.Itoa(i)}
	}
	return &RunContext{
		Config:       &Config{},
		JobContainer: jobContainer,
		Env:          env,
		Run: &model.Run{
			JobID: "test",
			Workflow: &model.Workflow{
				Jobs: map[string]*model.Job{
					"test": {Steps: steps},
				},
			},
		},
	}
}

func TestTryUploadJobSummaryRetriesTransientFailure(t *testing.T) {
	oldDelay := jobSummaryUploadRetryDelay
	jobSummaryUploadRetryDelay = 0
	defer func() {
		jobSummaryUploadRetryDelay = oldDelay
	}()

	runtimeToken := fakeRuntimeToken(34)

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		assert.Equal(t, http.MethodPut, r.Method)
		assert.Equal(t, "/_apis/pipelines/workflows/12/jobs/34/steps/0/summary", r.URL.Path)
		assert.Equal(t, "Bearer "+runtimeToken, r.Header.Get("Authorization"))
		assert.Equal(t, "text/markdown; charset=utf-8", r.Header.Get("Content-Type"))
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		assert.Equal(t, []byte("# summary"), body)
		if requests == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	ctx := context.Background()
	cm := &containerMock{}
	cm.On("GetContainerArchive", mock.Anything, "/var/run/act/workflow/step-summary-0.md").Return(
		io.NopCloser(bytes.NewReader(tarArchive(t, tarEntry{name: "step-summary-0.md", body: "# summary"}))),
		nil,
	).Once()

	rc := newJobSummaryRC(map[string]string{
		"GITEA_ACTIONS_CAPABILITIES": "cache, job-summary",
		"ACTIONS_RUNTIME_URL":        server.URL,
		"ACTIONS_RUNTIME_TOKEN":      runtimeToken,
		"GITEA_RUN_ID":               "12",
	}, cm, 1)

	tryUploadJobSummary(ctx, rc)

	assert.Equal(t, 2, requests)
	cm.AssertExpectations(t)
}

func TestTryUploadJobSummaryStopsAtPhaseTimeout(t *testing.T) {
	oldPhase := jobSummaryUploadPhaseTimeout
	jobSummaryUploadPhaseTimeout = 100 * time.Millisecond
	defer func() {
		jobSummaryUploadPhaseTimeout = oldPhase
	}()

	runtimeToken := fakeRuntimeToken(34)

	// The server blocks until either the request context is cancelled (the behaviour
	// under test: the phase timeout aborts the in-flight upload) or the test tears it
	// down. Without the phase timeout the upload would hang until the 30s client
	// timeout instead of releasing the cleanup budget. The release channel guarantees
	// the handler always returns so server.Close() cannot itself hang.
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)

	ctx := context.Background()
	cm := &containerMock{}
	cm.On("GetContainerArchive", mock.Anything, "/var/run/act/workflow/step-summary-0.md").Return(
		io.NopCloser(bytes.NewReader(tarArchive(t, tarEntry{name: "step-summary-0.md", body: "# summary"}))),
		nil,
	).Once()

	rc := newJobSummaryRC(map[string]string{
		"GITEA_ACTIONS_CAPABILITIES": "job-summary",
		"ACTIONS_RUNTIME_URL":        server.URL,
		"ACTIONS_RUNTIME_TOKEN":      runtimeToken,
		"GITEA_RUN_ID":               "12",
	}, cm, 1)

	done := make(chan struct{})
	go func() {
		defer close(done)
		tryUploadJobSummary(ctx, rc)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("tryUploadJobSummary did not honour the phase timeout")
	}
	cm.AssertExpectations(t)
}

func TestTryUploadJobSummaryUploadsEachStepIndependently(t *testing.T) {
	runtimeToken := fakeRuntimeToken(34)

	type upload struct {
		path string
		body string
	}
	var got []upload
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		got = append(got, upload{r.URL.Path, string(body)})
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	ctx := context.Background()
	cm := &containerMock{}
	// Three steps: 0 has content, 1 has empty content (skipped), 2 has content.
	cm.On("GetContainerArchive", mock.Anything, "/var/run/act/workflow/step-summary-0.md").Return(
		io.NopCloser(bytes.NewReader(tarArchive(t, tarEntry{name: "step-summary-0.md", body: "first"}))),
		nil,
	).Once()
	cm.On("GetContainerArchive", mock.Anything, "/var/run/act/workflow/step-summary-1.md").Return(
		io.NopCloser(bytes.NewReader(tarArchive(t, tarEntry{name: "step-summary-1.md", body: ""}))),
		nil,
	).Once()
	cm.On("GetContainerArchive", mock.Anything, "/var/run/act/workflow/step-summary-2.md").Return(
		io.NopCloser(bytes.NewReader(tarArchive(t, tarEntry{name: "step-summary-2.md", body: "third"}))),
		nil,
	).Once()

	rc := newJobSummaryRC(map[string]string{
		"GITEA_ACTIONS_CAPABILITIES": "job-summary",
		"ACTIONS_RUNTIME_URL":        server.URL,
		"ACTIONS_RUNTIME_TOKEN":      runtimeToken,
		"GITEA_RUN_ID":               "12",
	}, cm, 3)

	tryUploadJobSummary(ctx, rc)

	assert.Equal(t, []upload{
		{"/_apis/pipelines/workflows/12/jobs/34/steps/0/summary", "first"},
		{"/_apis/pipelines/workflows/12/jobs/34/steps/2/summary", "third"},
	}, got)
	cm.AssertExpectations(t)
}

func TestTryUploadJobSummaryRequiresExactCapability(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	rc := newJobSummaryRC(map[string]string{
		"GITEA_ACTIONS_CAPABILITIES": "not-job-summary,job-summary-v2",
		"ACTIONS_RUNTIME_URL":        server.URL,
		"ACTIONS_RUNTIME_TOKEN":      fakeRuntimeToken(34),
		"GITEA_RUN_ID":               "12",
	}, &containerMock{}, 1)

	tryUploadJobSummary(context.Background(), rc)

	assert.Equal(t, 0, requests)
}

func TestTryUploadJobSummarySkipsWhenJobIDMissingFromToken(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	rc := newJobSummaryRC(map[string]string{
		"GITEA_ACTIONS_CAPABILITIES": "job-summary",
		"ACTIONS_RUNTIME_URL":        server.URL,
		"ACTIONS_RUNTIME_TOKEN":      "not-a-jwt",
		"GITEA_RUN_ID":               "12",
	}, &containerMock{}, 1)

	tryUploadJobSummary(context.Background(), rc)

	assert.Equal(t, 0, requests)
}

func TestExtractJobIDFromRuntimeToken(t *testing.T) {
	assert.Equal(t, int64(42), extractJobIDFromRuntimeToken(fakeRuntimeToken(42)))
	assert.Equal(t, int64(0), extractJobIDFromRuntimeToken("not-a-jwt"))
	assert.Equal(t, int64(0), extractJobIDFromRuntimeToken("a.b.c"))
	assert.Equal(t, int64(0), extractJobIDFromRuntimeToken(""))
}

func TestReadSingleFileFromContainerArchiveFindsMatchingRegularFile(t *testing.T) {
	ctx := context.Background()
	cm := &containerMock{}
	cm.On("GetContainerArchive", ctx, "/var/run/act/workflow/SUMMARY.md").Return(
		io.NopCloser(bytes.NewReader(tarArchive(t,
			tarEntry{name: "workflow", typeflag: tar.TypeDir},
			tarEntry{name: "other.md", body: "wrong"},
			tarEntry{name: "SUMMARY.md", body: "right"},
		))),
		nil,
	).Once()

	body, ok := readSingleFileFromContainerArchive(ctx, cm, "/var/run/act/workflow/SUMMARY.md", 1024)

	assert.True(t, ok)
	assert.Equal(t, []byte("right"), body)
	cm.AssertExpectations(t)
}

func TestReadSingleFileFromContainerArchiveTruncatesWhenTooLarge(t *testing.T) {
	logger, hook := logrustest.NewNullLogger()
	ctx := common.WithLogger(context.Background(), logger)
	cm := &containerMock{}
	content := strings.Repeat("a", 300)
	cm.On("GetContainerArchive", ctx, "/var/run/act/workflow/SUMMARY.md").Return(
		io.NopCloser(bytes.NewReader(tarArchive(t, tarEntry{name: "SUMMARY.md", body: content}))),
		nil,
	).Once()

	const maxBytes = 200
	body, ok := readSingleFileFromContainerArchive(ctx, cm, "/var/run/act/workflow/SUMMARY.md", maxBytes)

	// Oversized summaries are truncated to the limit (reserving room for the marker)
	// rather than dropped entirely, and the truncation marker is appended.
	assert.True(t, ok)
	assert.LessOrEqual(t, len(body), maxBytes)
	keep := maxBytes - len(jobSummaryTruncationMarker)
	assert.Equal(t, []byte(content[:keep]+jobSummaryTruncationMarker), body)
	if assert.Len(t, hook.Entries, 1) {
		assert.Contains(t, hook.Entries[0].Message, "job summary truncated")
	}
	cm.AssertExpectations(t)
}

func TestReadSingleFileFromContainerArchiveKeepsExactLimitWithoutWarning(t *testing.T) {
	logger, hook := logrustest.NewNullLogger()
	ctx := common.WithLogger(context.Background(), logger)
	cm := &containerMock{}
	cm.On("GetContainerArchive", ctx, "/var/run/act/workflow/SUMMARY.md").Return(
		io.NopCloser(bytes.NewReader(tarArchive(t, tarEntry{name: "SUMMARY.md", body: "abc"}))),
		nil,
	).Once()

	body, ok := readSingleFileFromContainerArchive(ctx, cm, "/var/run/act/workflow/SUMMARY.md", 3)

	// A summary that is exactly at the limit is kept whole and not flagged as truncated.
	assert.True(t, ok)
	assert.Equal(t, []byte("abc"), body)
	assert.Empty(t, hook.Entries)
	cm.AssertExpectations(t)
}

type tarEntry struct {
	name     string
	body     string
	typeflag byte
}

func tarArchive(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()

	buf := &bytes.Buffer{}
	tw := tar.NewWriter(buf)
	for _, entry := range entries {
		typeflag := entry.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		header := &tar.Header{
			Name:     entry.name,
			Typeflag: typeflag,
			Mode:     0o644,
			Size:     int64(len(entry.body)),
		}
		if typeflag == tar.TypeDir {
			header.Mode = 0o755
			header.Size = 0
		}
		require.NoError(t, tw.WriteHeader(header))
		if typeflag == tar.TypeReg {
			_, err := tw.Write([]byte(entry.body))
			require.NoError(t, err)
		}
	}
	require.NoError(t, tw.Close())
	return buf.Bytes()
}
