// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !(WITHOUT_DOCKER || !(linux || darwin || windows || netbsd))

package container

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"gitea.com/gitea/runner/act/common"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
)

const (
	jobLabel      = "com.gitea.runner.job"
	maxCreateBody = 8 << 20

	dockerProxyProbeTimeout = 5 * time.Second
)

var (
	createPath    = regexp.MustCompile(`^(/v[0-9.]+)?/(containers|networks|volumes)/create$`)
	rawStreamPath = regexp.MustCompile(`^(/v[0-9.]+)?/(containers/[^/]+/attach|exec/[^/]+/start)$`)
)

func NewDockerProxy(ctx context.Context, job string) *DockerProxy {
	if host := os.Getenv("DOCKER_HOST"); runtime.GOOS != "linux" || host != "" && !strings.HasPrefix(host, "unix://") {
		return nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, dockerProxyProbeTimeout)
	defer cancel()
	cli, err := GetDockerClient(probeCtx)
	if err != nil {
		return nil
	}
	defer cli.Close()
	daemonSocket, ok := strings.CutPrefix(cli.DaemonHost(), "unix://")
	if !ok {
		return nil
	}
	if info, err := os.Stat(daemonSocket); err != nil || info.Mode()&os.ModeSocket == 0 {
		return nil
	}
	dir, err := filepath.Abs(os.TempDir())
	if err != nil {
		common.Logger(ctx).Infof("docker proxy probe failed, jobs get the daemon socket directly: %v", err)
		return nil
	}
	seen, err := daemonSeesDir(probeCtx, cli, dir)
	if err != nil {
		common.Logger(ctx).Infof("docker proxy probe failed, jobs get the daemon socket directly: %v", err)
		return nil
	}
	if !seen {
		common.Logger(ctx).Infof("the docker daemon cannot reach the runner's temporary filesystem, jobs get the daemon socket directly")
		return nil
	}
	if ctx.Err() != nil {
		return nil
	}
	proxy, err := StartDockerProxy(daemonSocket, dir, job)
	if err != nil {
		common.Logger(ctx).Warnf("docker proxy not started, the job gets the daemon socket directly: %v", err)
	}
	return proxy
}

// daemonSeesDir reports whether the daemon opens the files the runner writes in dir,
// which is what a job's proxy socket mounted from there needs.
func daemonSeesDir(ctx context.Context, cli client.APIClient, dir string) (bool, error) {
	marker, err := os.CreateTemp(dir, "gitea-runner-probe-")
	if err != nil {
		return false, err
	}
	defer func() {
		if err := os.Remove(marker.Name()); err != nil {
			common.Logger(ctx).Warnf("removing the docker proxy probe marker failed: %v", err)
		}
	}()
	if err := marker.Close(); err != nil {
		return false, err
	}
	images, err := cli.ImageList(ctx, client.ImageListOptions{})
	if err != nil {
		return false, err
	}
	if len(images.Items) == 0 {
		return false, errors.New("no image available for the docker proxy probe")
	}
	created, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{Image: images.Items[0].ID, Cmd: []string{"true"}},
		HostConfig: &container.HostConfig{Mounts: []mount.Mount{
			{Type: mount.TypeBind, Source: marker.Name(), Target: "/gitea-runner-probe", ReadOnly: true},
		}},
	})
	if cerrdefs.IsInvalidArgument(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dockerProxyProbeTimeout)
	defer cancel()
	if _, err := cli.ContainerRemove(cleanupCtx, created.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil {
		return false, fmt.Errorf("removing the docker proxy probe container failed: %w", err)
	}
	return true, nil
}

// StartDockerProxy serves a job's docker socket in dir, labelling what the job creates through it.
func StartDockerProxy(daemonSocket, dir, job string) (*DockerProxy, error) {
	info, err := os.Stat(daemonSocket)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return nil, errors.New("docker daemon path is not a Unix socket")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	instance, err := os.MkdirTemp(dir, "p-")
	if err != nil {
		return nil, err
	}
	socket := filepath.Join(instance, "docker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return nil, errors.Join(err, os.RemoveAll(instance))
	}
	if err := copyDockerSocketPermissions(socket, info); err != nil {
		return nil, errors.Join(err, listener.Close(), os.RemoveAll(instance))
	}
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", daemonSocket)
	}
	transport := &http.Transport{DialContext: dial}
	forward := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Scheme = "http"
			r.Out.URL.Host = "docker"
		},
		Transport: transport,
	}
	streams, cancelStreams := context.WithCancel(context.Background())
	creates, cancelCreates := context.WithCancel(context.Background())
	var admission sync.Mutex
	var handlers sync.WaitGroup
	server := &http.Server{ReadHeaderTimeout: 30 * time.Second, ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
		return context.WithValue(ctx, dockerProxyConnKey{}, conn)
	}, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		admission.Lock()
		if streams.Err() != nil {
			admission.Unlock()
			http.Error(w, "docker proxy is closing", http.StatusServiceUnavailable)
			return
		}
		handlers.Add(1)
		admission.Unlock()
		defer handlers.Done()
		creating := r.Method == http.MethodPost && createPath.MatchString(r.URL.Path)
		parent, lifetime := r.Context(), streams
		if creating {
			parent, lifetime = context.WithoutCancel(parent), creates
		}
		ctx, cancel := context.WithCancel(parent)
		defer cancel()
		stop := context.AfterFunc(lifetime, func() {
			cancel()
			if !creating {
				if conn, ok := parent.Value(dockerProxyConnKey{}).(net.Conn); ok {
					_ = conn.Close()
				}
			}
		})
		defer stop()
		r = r.WithContext(ctx)
		if creating {
			r.Body = http.MaxBytesReader(w, r.Body, maxCreateBody)
			if err := addLabel(r, job); err != nil {
				status := http.StatusBadRequest
				if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
					status = http.StatusRequestEntityTooLarge
				}
				http.Error(w, err.Error(), status)
				return
			}
		} else if r.Method == http.MethodPost && rawStreamPath.MatchString(r.URL.Path) {
			tunnel(w, r, dial, forward)
			return
		}
		forward.ServeHTTP(w, r)
	})}
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = server.Serve(listener)
	}()
	return &DockerProxy{Socket: socket, close: func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		admission.Lock()
		listenerErr := listener.Close()
		cancelStreams()
		admission.Unlock()
		<-served
		shutdownErr := server.Shutdown(ctx)
		cancelCreates()
		serverErr := server.Close()
		handlers.Wait()
		transport.CloseIdleConnections()
		return errors.Join(ctx.Err(), listenerErr, shutdownErr, serverErr, os.RemoveAll(instance))
	}}, nil
}

func addLabel(r *http.Request, job string) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		body = []byte("{}")
	}
	var fields map[string]json.RawMessage
	var config struct{ Labels map[string]string }
	if err := json.Unmarshal(body, &fields); err != nil {
		return fmt.Errorf("invalid create request: %w", err)
	}
	if err := json.Unmarshal(body, &config); err != nil {
		return fmt.Errorf("invalid create labels: %w", err)
	}
	if fields == nil {
		fields = make(map[string]json.RawMessage)
	}
	maps.DeleteFunc(fields, func(name string, _ json.RawMessage) bool {
		return strings.EqualFold(name, "Labels")
	})
	if config.Labels == nil {
		config.Labels = make(map[string]string)
	}
	config.Labels[jobLabel] = job
	if fields["Labels"], err = json.Marshal(config.Labels); err != nil {
		return err
	}
	if body, err = json.Marshal(fields); err != nil {
		return err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.TransferEncoding = nil
	return nil
}

type dockerProxyConnKey struct{}

type dockerProxyResponse struct {
	response *http.Response
}

func (r dockerProxyResponse) RoundTrip(_ *http.Request) (*http.Response, error) {
	return r.response, nil
}

// tunnel splices attach and exec streams, which the daemon hijacks with or without an HTTP upgrade
func tunnel(w http.ResponseWriter, r *http.Request, dial func(context.Context, string, string) (net.Conn, error), forward *httputil.ReverseProxy) {
	upstream, err := dial(r.Context(), "", "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer upstream.Close()
	stop := context.AfterFunc(r.Context(), func() { _ = upstream.Close() })
	defer stop()
	if err := r.Write(upstream); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	reader := bufio.NewReader(upstream)
	var response *http.Response
	for {
		response, err = http.ReadResponse(reader, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if response.StatusCode >= 200 || response.StatusCode == http.StatusSwitchingProtocols {
			break
		}
		maps.Copy(w.Header(), response.Header)
		w.WriteHeader(response.StatusCode)
		clear(w.Header())
		_ = response.Body.Close()
	}
	defer func() {
		_ = upstream.Close()
		_ = response.Body.Close()
	}()
	mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if response.StatusCode != http.StatusSwitchingProtocols && (response.StatusCode != http.StatusOK || mediaType != "application/vnd.docker.raw-stream") {
		ordinary := *forward
		ordinary.Transport = dockerProxyResponse{response: response}
		ordinary.ServeHTTP(w, r)
		return
	}
	downstream, buffered, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	defer downstream.Close()
	if _, err := fmt.Fprintf(buffered, "%s %s\r\n", response.Proto, response.Status); err != nil {
		return
	}
	if err := response.Header.Write(buffered); err != nil {
		return
	}
	if _, err := buffered.WriteString("\r\n"); err != nil {
		return
	}
	if err := buffered.Flush(); err != nil {
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := io.Copy(upstream, io.MultiReader(io.LimitReader(buffered, int64(buffered.Reader.Buffered())), downstream)); err != nil { // Bypass net/http after the prefix so stdin EOF preserves output.
			_ = upstream.Close()
		} else if writer, ok := upstream.(interface{ CloseWrite() error }); ok {
			_ = writer.CloseWrite()
		} else {
			_ = upstream.Close()
		}
	}()
	_, _ = io.Copy(downstream, reader)
	_ = downstream.Close()
	_ = upstream.Close()
	<-done
}

func RemoveDockerJobResources(ctx context.Context, job string) error {
	cli, err := GetDockerClient(ctx)
	if err != nil {
		return err
	}
	defer cli.Close()
	return removeLabelled(ctx, cli, job)
}

func removeLabelled(ctx context.Context, cli client.APIClient, job string) error {
	logger := common.Logger(ctx)
	filters := make(client.Filters).Add("label", jobLabel+"="+job)
	containers, err := cli.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filters})
	errs := []error{err}
	for _, c := range containers.Items {
		logger.Infof("removing container %s the job left behind", strings.TrimPrefix(strings.Join(c.Names, ","), "/"))
		errs = append(errs, (&containerReference{cli: cli, id: c.ID}).remove()(ctx))
	}
	networks, err := cli.NetworkList(ctx, client.NetworkListOptions{Filters: filters})
	errs = append(errs, err)
	for _, n := range networks.Items {
		if _, err := cli.NetworkRemove(ctx, n.ID, client.NetworkRemoveOptions{}); err != nil && !cerrdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("failed to remove network %s: %w", n.Name, err))
		}
	}
	volumes, err := cli.VolumeList(ctx, client.VolumeListOptions{Filters: filters})
	errs = append(errs, err)
	for _, v := range volumes.Items {
		if _, err := cli.VolumeRemove(ctx, v.Name, client.VolumeRemoveOptions{}); err != nil && !cerrdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("failed to remove volume %s: %w", v.Name, err))
		}
	}
	return errors.Join(errs...)
}
