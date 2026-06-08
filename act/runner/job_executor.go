// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2022 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/container"
	"gitea.com/gitea/runner/act/model"
)

const maxJobSummaryBytes = 1024 * 1024

// jobSummaryTruncationMarker is appended to a summary that exceeded the size limit
// so the rendered output makes the truncation visible instead of silently cutting off.
const jobSummaryTruncationMarker = "\n\n---\n\n*Job summary truncated: it exceeded the maximum allowed size.*\n"

var (
	jobSummaryUploadRetryDelay = time.Second
	// jobSummaryUploadRequestTimeout bounds a single step upload request. It is kept
	// below jobSummaryUploadPhaseTimeout so one slow or unreachable request times out
	// and lets the remaining steps still upload within the phase budget, instead of a
	// single stuck request consuming the whole phase.
	jobSummaryUploadRequestTimeout = 5 * time.Second
	// jobSummaryUploadPhaseTimeout bounds the total time spent uploading all step
	// summaries. The uploads run inside the job cleanup budget that is also used to
	// stop and remove the container, so a slow or unreachable endpoint must not be
	// allowed to consume it; this keeps the remaining budget available for teardown.
	jobSummaryUploadPhaseTimeout = 15 * time.Second
)

type jobInfo interface {
	matrix() map[string]any
	steps() []*model.Step
	startContainer() common.Executor
	stopContainer() common.Executor
	closeContainer() common.Executor
	interpolateOutputs() common.Executor
	result(result string)
}

// reportStepError emits the GitHub Actions ##[error] annotation and records
// the error against the job so the job is reported as failed.
func reportStepError(ctx context.Context, err error) {
	common.Logger(ctx).Errorf("##[error]%v", err)
	common.SetJobError(ctx, err)
}

func newJobExecutor(info jobInfo, sf stepFactory, rc *RunContext) common.Executor {
	steps := make([]common.Executor, 0)
	preSteps := make([]common.Executor, 0)
	var postExecutor common.Executor

	steps = append(steps, func(ctx context.Context) error {
		logger := common.Logger(ctx)
		if len(info.matrix()) > 0 {
			logger.Infof("Matrix: %v", info.matrix())
		}
		return nil
	})

	infoSteps := info.steps()

	if len(infoSteps) == 0 {
		return common.NewDebugExecutor("No steps found")
	}

	preSteps = append(preSteps, func(ctx context.Context) error {
		// Have to be skipped for some Tests
		if rc.Run == nil {
			return nil
		}
		rc.ExprEval = rc.NewExpressionEvaluator(ctx)
		// evaluate environment variables since they can contain
		// GitHub's special environment variables.
		for k, v := range rc.GetEnv() {
			rc.Env[k] = rc.ExprEval.Interpolate(ctx, v)
		}
		return nil
	})

	for i, stepModel := range infoSteps {
		if stepModel == nil {
			return func(ctx context.Context) error {
				return fmt.Errorf("invalid Step %v: missing run or uses key", i)
			}
		}
		if stepModel.ID == "" {
			stepModel.ID = strconv.Itoa(i)
		}
		stepModel.Number = i

		step, err := sf.newStep(stepModel, rc)
		if err != nil {
			return common.NewErrorExecutor(err)
		}

		stepIdx := stepModel.Number
		preExec := step.pre()
		preSteps = append(preSteps, useStepLogger(rc, stepModel, stepStagePre, func(ctx context.Context) error {
			rc.CurrentStepIndex = stepIdx
			preErr := preExec(ctx)
			if preErr != nil {
				reportStepError(ctx, preErr)
			} else if ctx.Err() != nil {
				reportStepError(ctx, ctx.Err())
			}
			return preErr
		}))

		stepExec := step.main()
		steps = append(steps, useStepLogger(rc, stepModel, stepStageMain, func(ctx context.Context) error {
			rc.CurrentStepIndex = stepIdx
			err := stepExec(ctx)
			if err != nil {
				reportStepError(ctx, err)
			} else if ctx.Err() != nil {
				reportStepError(ctx, ctx.Err())
			}
			return nil
		}))

		postFn := step.post()
		postExec := useStepLogger(rc, stepModel, stepStagePost, func(ctx context.Context) error {
			rc.CurrentStepIndex = stepIdx
			err := postFn(ctx)
			if err != nil {
				reportStepError(ctx, err)
			} else if ctx.Err() != nil {
				reportStepError(ctx, ctx.Err())
			}
			return err
		})
		if postExecutor != nil {
			// run the post executor in reverse order
			postExecutor = postExec.Finally(postExecutor)
		} else {
			postExecutor = postExec
		}
	}

	postExecutor = postExecutor.Finally(func(ctx context.Context) error {
		jobError := common.JobError(ctx)
		var err error
		if rc.Config.AutoRemove || jobError == nil {
			// always allow 1 min for stopping and removing the runner, even if we were cancelled
			ctx, cancel := context.WithTimeout(common.WithLogger(context.Background(), common.Logger(ctx)), time.Minute)
			defer cancel()

			logger := common.Logger(ctx)
			tryUploadJobSummary(ctx, rc)
			// For Gitea
			// We don't need to call `stopServiceContainers` here since it will be called by following `info.stopContainer`
			// logger.Infof("Cleaning up services for job %s", rc.JobName)
			// if err := rc.stopServiceContainers()(ctx); err != nil {
			// 	logger.Errorf("Error while cleaning services: %v", err)
			// }

			logger.Infof("Cleaning up container for job %s", rc.JobName)
			if err = info.stopContainer()(ctx); err != nil {
				logger.Errorf("Error while stop job container: %v", err)
			}

			// For Gitea
			// We don't need to call `NewDockerNetworkRemoveExecutor` here since it is called by above `info.stopContainer`
			// if !rc.IsHostEnv(ctx) && rc.Config.ContainerNetworkMode == "" {
			// 	// clean network in docker mode only
			// 	// if the value of `ContainerNetworkMode` is empty string,
			// 	// it means that the network to which containers are connecting is created by `runner`,
			// 	// so, we should remove the network at last.
			// 	networkName, _ := rc.networkName()
			// 	logger.Infof("Cleaning up network for job %s, and network name is: %s", rc.JobName, networkName)
			// 	if err := container.NewDockerNetworkRemoveExecutor(networkName)(ctx); err != nil {
			// 		logger.Errorf("Error while cleaning network: %v", err)
			// 	}
			// }
		}
		setJobResult(ctx, info, rc, jobError == nil)
		setJobOutputs(ctx, rc)

		return err
	})

	pipeline := make([]common.Executor, 0)
	pipeline = append(pipeline, preSteps...)
	pipeline = append(pipeline, steps...)

	return common.NewPipelineExecutor(info.startContainer(), common.NewPipelineExecutor(pipeline...).
		Finally(func(ctx context.Context) error {
			var cancel context.CancelFunc
			if ctx.Err() == context.Canceled {
				// in case of an aborted run, we still should execute the
				// post steps to allow cleanup.
				ctx, cancel = context.WithTimeout(common.WithLogger(context.Background(), common.Logger(ctx)), 5*time.Minute)
				defer cancel()
			}
			return postExecutor(ctx)
		}).
		Finally(info.interpolateOutputs()).
		Finally(info.closeContainer()))
}

func setJobResult(ctx context.Context, info jobInfo, rc *RunContext, success bool) {
	logger := common.Logger(ctx)

	// Matrix combinations share one *model.Job and run in parallel; serialize the
	// read-modify-write of the job result so a failing combination is not lost-updated by a
	// concurrent succeeding one.
	job := rc.Run.Job()
	jobResult := func() string {
		defer lockJob(job)()
		result := "success"
		// we have only one result for a whole matrix build, so we need
		// to keep an existing result state if we run a matrix
		if len(info.matrix()) > 0 && job.Result != "" {
			result = job.Result
		}
		if !success {
			result = "failure"
		}
		info.result(result)
		return result
	}()

	if rc.caller != nil {
		// set reusable workflow job result
		rc.caller.setReusedWorkflowJobResult(rc.JobName, jobResult) // For Gitea
		return
	}

	jobResultMessage := "succeeded"
	if jobResult != "success" {
		jobResultMessage = "failed"
	}

	logger.WithField("jobResult", jobResult).Infof("Job %s", jobResultMessage)
}

func setJobOutputs(ctx context.Context, rc *RunContext) {
	if rc.caller != nil {
		// map outputs for reusable workflows
		callerOutputs := make(map[string]string)

		ee := rc.NewExpressionEvaluator(ctx)

		for k, v := range rc.Run.Workflow.WorkflowCallConfig().Outputs {
			callerOutputs[k] = ee.Interpolate(ctx, ee.Interpolate(ctx, v.Value))
		}

		// Matrix combinations of a reusable-workflow caller share the caller's *model.Job;
		// serialize the write so parallel combos don't race on its Outputs field.
		callerJob := rc.caller.runContext.Run.Job()
		defer lockJob(callerJob)()
		callerJob.Outputs = callerOutputs
	}
}

func tryUploadJobSummary(ctx context.Context, rc *RunContext) {
	if rc == nil || rc.JobContainer == nil || rc.Config == nil {
		return
	}
	// Bound the whole upload phase so a slow or unreachable endpoint cannot consume
	// the job cleanup budget reserved for stopping and removing the container.
	ctx, cancel := context.WithTimeout(ctx, jobSummaryUploadPhaseTimeout)
	defer cancel()
	env := rc.GetEnv()
	caps := strings.TrimSpace(env["GITEA_ACTIONS_CAPABILITIES"])
	if !hasJobSummaryCapability(caps) {
		// Server did not advertise support. Do not attempt upload.
		return
	}
	runtimeURL := strings.TrimSpace(env["ACTIONS_RUNTIME_URL"])
	runtimeToken := strings.TrimSpace(env["ACTIONS_RUNTIME_TOKEN"])
	runID := strings.TrimSpace(env["GITEA_RUN_ID"])
	if runtimeURL == "" || runtimeToken == "" || runID == "" {
		return
	}
	if rc.Run == nil || rc.Run.Job() == nil {
		return
	}
	// The numeric ActionRunJob ID is not exposed in the proto Task message or task context,
	// but the server signs it into the ACTIONS_RUNTIME_TOKEN JWT claims. We decode the
	// unverified claims to retrieve it; the server re-verifies the token on the request.
	jobID := extractJobIDFromRuntimeToken(runtimeToken)
	if jobID <= 0 {
		return
	}

	base := strings.TrimRight(runtimeURL, "/") + "/_apis/pipelines/workflows/" + runID +
		"/jobs/" + strconv.FormatInt(jobID, 10) + "/steps/"
	actPath := rc.JobContainer.GetActPath()
	// Reuse a single client across all step uploads so connections can be pooled.
	client := &http.Client{Timeout: jobSummaryUploadRequestTimeout}
	for i := range rc.Run.Job().Steps {
		summaryPath := path.Join(actPath, "workflow", "step-summary-"+strconv.Itoa(i)+".md")
		body, ok := readSingleFileFromContainerArchive(ctx, rc.JobContainer, summaryPath, maxJobSummaryBytes)
		if !ok || len(body) == 0 {
			continue
		}
		uploadJobSummary(ctx, client, base+strconv.Itoa(i)+"/summary", runtimeToken, body)
	}
}

// extractJobIDFromRuntimeToken returns the JobID claim from an ACTIONS_RUNTIME_TOKEN JWT
// without verifying its signature. Returns 0 if the token is unparseable or has no JobID.
func extractJobIDFromRuntimeToken(token string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return 0
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0
	}
	var claims struct {
		JobID int64 `json:"JobID"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return 0
	}
	return claims.JobID
}

func hasJobSummaryCapability(caps string) bool {
	return slices.Contains(strings.FieldsFunc(caps, func(r rune) bool {
		return r == ',' || unicode.IsSpace(r)
	}), "job-summary")
}

func uploadJobSummary(ctx context.Context, client *http.Client, url, runtimeToken string, body []byte) {
	logger := common.Logger(ctx)

	var lastStatus int
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		status, err := putJobSummary(ctx, client, url, runtimeToken, body)
		if err == nil && status/100 == 2 {
			return
		}
		lastStatus = status
		lastErr = err
		if attempt == 1 || !isTransientJobSummaryUploadFailure(status, err) {
			break
		}
		timer := time.NewTimer(jobSummaryUploadRetryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			lastErr = ctx.Err()
			attempt = 1
		case <-timer.C:
		}
	}

	// Best-effort only; do not fail job, but log because capability was advertised.
	if lastErr != nil {
		logger.WithError(lastErr).Warn("job summary upload failed")
		return
	}
	logger.Warnf("job summary upload failed: status=%d", lastStatus)
}

func putJobSummary(ctx context.Context, client *http.Client, url, runtimeToken string, body []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+runtimeToken)
	req.Header.Set("Content-Type", "text/markdown; charset=utf-8")

	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

func isTransientJobSummaryUploadFailure(status int, err error) bool {
	return err != nil || status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status/100 == 5
}

func readSingleFileFromContainerArchive(ctx context.Context, env container.ExecutionsEnvironment, p string, maxBytes int64) ([]byte, bool) {
	rc, err := env.GetContainerArchive(ctx, p)
	if err != nil {
		return nil, false
	}
	defer rc.Close()

	tr := tar.NewReader(rc)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return nil, false
		}
		if err != nil {
			return nil, false
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		if !archiveEntryMatchesPath(header.Name, p) {
			continue
		}
		// Summaries larger than the limit are truncated rather than dropped, so the
		// user still gets the leading content (mirroring how GitHub caps oversized
		// step summaries instead of discarding them). Read one extra byte so an
		// over-limit file is detected from the actual stream rather than trusting
		// header.Size, then cap the returned content at maxBytes.
		b, err := io.ReadAll(io.LimitReader(tr, maxBytes+1))
		if err != nil {
			return nil, false
		}
		if int64(len(b)) > maxBytes {
			// Reserve room for the marker so the marked-up result still fits in maxBytes.
			marker := []byte(jobSummaryTruncationMarker)
			keep := max(maxBytes-int64(len(marker)), 0)
			b = append(b[:keep], marker...)
			common.Logger(ctx).Warnf("job summary truncated: path=%s max=%d", p, maxBytes)
		}
		return b, true
	}
}

func archiveEntryMatchesPath(entryName, requestedPath string) bool {
	entryName = path.Clean(strings.TrimPrefix(entryName, "/"))
	requestedPath = path.Clean(strings.TrimPrefix(requestedPath, "/"))
	return entryName == requestedPath || entryName == path.Base(requestedPath)
}

func useStepLogger(rc *RunContext, stepModel *model.Step, stage stepStage, executor common.Executor) common.Executor {
	return func(ctx context.Context) error {
		ctx = withStepLogger(ctx, stepModel.Number, stepModel.ID, rc.ExprEval.Interpolate(ctx, stepModel.String()), stage.String())

		rawLogger := common.Logger(ctx).WithField("raw_output", true)
		logWriter := common.NewLineWriter(rc.commandHandler(ctx), func(s string) bool {
			if rc.Config.LogOutput {
				rawLogger.Infof("%s", s)
			} else {
				rawLogger.Debugf("%s", s)
			}
			return true
		})

		oldout, olderr := rc.JobContainer.ReplaceLogWriter(logWriter, logWriter)
		defer rc.JobContainer.ReplaceLogWriter(oldout, olderr)

		return executor(ctx)
	}
}
