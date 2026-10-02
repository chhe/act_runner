// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package container

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	incus "github.com/lxc/incus/v6/client"
	"github.com/lxc/incus/v6/shared/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeServer records the calls a pool makes, its state is the VMs the daemon knows.
// It embeds the real interface nil so only the methods the tests reach need bodies.
type fakeServer struct {
	incus.InstanceServer

	instances  map[string]api.Instance
	state      map[string]api.InstanceState
	snapshots  map[string][]string
	calls      []string
	agentReady bool
}

func newFakeServer() *fakeServer {
	return &fakeServer{instances: map[string]api.Instance{}, state: map[string]api.InstanceState{}, snapshots: map[string][]string{}}
}

func (s *fakeServer) record(call string) { s.calls = append(s.calls, call) }

func (s *fakeServer) startedInstance(name string) {
	s.instances[name] = api.Instance{Name: name, CreatedAt: time.Now()}
	s.state[name] = api.InstanceState{Status: "Running"}
	s.snapshots[name] = []string{"pristine"} // the pool fills each warm VM with the snapshot its job restores
}

func (s *fakeServer) UseProject(string) incus.InstanceServer { return s }

func (s *fakeServer) GetInstanceNames(all api.InstanceType) ([]string, error) {
	s.record("GetInstanceNames")
	names := make([]string, 0, len(s.instances))
	for name := range s.instances {
		names = append(names, name)
	}
	slices.Sort(names)
	return names, nil
}

func (s *fakeServer) GetInstances(instanceType api.InstanceType) ([]api.Instance, error) {
	s.record("GetInstances")
	names, _ := s.GetInstanceNames(instanceType)
	instances := make([]api.Instance, 0, len(names))
	for _, name := range names {
		instances = append(instances, s.instances[name])
	}
	return instances, nil
}

func (s *fakeServer) RenameInstance(name string, post api.InstancePost) (incus.Operation, error) {
	s.record("RenameInstance " + name + " " + post.Name)
	instance := s.instances[name]
	delete(s.instances, name)
	instance.Name = post.Name
	s.instances[post.Name] = instance
	state := s.state[name]
	delete(s.state, name)
	s.state[post.Name] = state
	s.snapshots[post.Name] = s.snapshots[name] // a rename keeps the VM's snapshots, what a copy would not
	delete(s.snapshots, name)
	return s.newOp(), nil
}

func (s *fakeServer) newOp() incus.Operation {
	return &fakeOperation{}
}

func (s *fakeServer) CreateInstanceFromImage(incus.ImageServer, api.Image, api.InstancesPost) (incus.RemoteOperation, error) {
	s.record("CreateInstanceFromImage")
	return nil, errors.New("not implemented in this test")
}

func (s *fakeServer) UpdateInstance(name string, put api.InstancePut, etag string) (incus.Operation, error) {
	s.record("UpdateInstance " + name + " restore=" + put.Restore)
	return s.newOp(), nil
}

func (s *fakeServer) ExecInstance(name string, post api.InstanceExecPost, args *incus.InstanceExecArgs) (incus.Operation, error) {
	s.record(fmt.Sprintf("ExecInstance %s %v", name, post.Command))
	if post.WaitForWS && args.Stdout != nil {
		if script := stdinOf(args.Stdin); script != "" {
			_, _ = io.WriteString(args.Stdout, script)
		} else {
			_, _ = io.WriteString(args.Stdout, fakeExecOutput(post.Command))
		}
	}
	return s.newOp(), nil
}

func stdinOf(stdin io.Reader) string {
	if stdin == nil {
		return ""
	}
	content, _ := io.ReadAll(stdin)
	return string(content)
}

// fakeExecOutput answers what Start and the archive tests poll the VM for.
func fakeExecOutput(command []string) string {
	if len(command) == 4 && command[0] == "sh" && strings.Contains(command[2], "tar") {
		return ""
	}
	if len(command) >= 2 && command[0] == "sh" && strings.Contains(command[1], "uname") {
		return "x86_64\n/usr/local/bin:/usr/bin:/bin"
	}
	return ""
}

func (s *fakeServer) GetInstanceState(name string) (*api.InstanceState, string, error) {
	state, ok := s.state[name]
	if !ok {
		return nil, "", api.StatusErrorf(http.StatusNotFound, "not found")
	}
	if !s.agentReady {
		state.Network = map[string]api.InstanceStateNetwork{}
	} else {
		state.Network = map[string]api.InstanceStateNetwork{"eth0": {Addresses: []api.InstanceStateNetworkAddress{{Family: "inet", Scope: "global"}}}}
	}
	return &state, "", nil
}

func (s *fakeServer) CreateInstanceSnapshot(name string, post api.InstanceSnapshotsPost) (incus.Operation, error) {
	s.record("CreateInstanceSnapshot " + name + " " + post.Name)
	s.snapshots[name] = append(s.snapshots[name], post.Name)
	return s.newOp(), nil
}

func (s *fakeServer) GetInstanceSnapshotNames(name string) ([]string, error) {
	s.record("GetInstanceSnapshotNames " + name)
	return s.snapshots[name], nil
}

func (s *fakeServer) UpdateInstanceState(name string, put api.InstanceStatePut, etag string) (incus.Operation, error) {
	s.record("UpdateInstanceState " + name + " " + put.Action)
	state := s.state[name]
	switch put.Action {
	case "stop":
		state.Status = "Stopped"
	case "start":
		state.Status = "Running"
	}
	s.state[name] = state
	return s.newOp(), nil
}

func (s *fakeServer) DeleteInstance(name string) (incus.Operation, error) {
	s.record("DeleteInstance " + name)
	delete(s.instances, name)
	delete(s.state, name)
	return s.newOp(), nil
}

func (s *fakeServer) Disconnect() {}

func (s *fakeServer) GetImageAlias(name string) (*api.ImageAliasesEntry, string, error) {
	return nil, "", api.StatusErrorf(http.StatusNotFound, "not found")
}

func (s *fakeServer) GetImage(fingerprint string) (*api.Image, string, error) {
	return nil, "", api.StatusErrorf(http.StatusNotFound, "not found")
}

var _ incus.InstanceServer = (*fakeServer)(nil)

// fakeOperation embeds the real interface nil so only the methods the tests reach need bodies.
type fakeOperation struct {
	incus.Operation
}

func (op *fakeOperation) Wait() error                       { return nil }
func (op *fakeOperation) WaitContext(context.Context) error { return nil }

func TestIncusPoolClaimsWarmVMs(t *testing.T) {
	server := newFakeServer()
	server.agentReady = true
	server.startedInstance("gitea-runner-warm-aaaaaaaa")
	server.startedInstance("other-vm")
	restore := connectIncus
	connectIncus = func(context.Context, string) (incus.InstanceServer, error) { return server, nil }
	t.Cleanup(func() { connectIncus = restore })

	pool, err := NewIncusPool(context.Background(), IncusOptions{PoolSize: 1})
	require.NoError(t, err)

	env, err := pool.JobContainer(context.Background(), &NewContainerInput{Image: "ubuntu:24.04"})
	require.NoError(t, err)
	assert.NotNil(t, env)
	assert.Regexp(t, `^gitea-runner-job-[0-9a-z]{8}$`, pool.claimed)
	assert.Equal(t, "Running", server.state[pool.claimed].Status)
	assert.Contains(t, server.calls, "RenameInstance gitea-runner-warm-aaaaaaaa "+pool.claimed)

	// while the VM is claimed no warm one is left, so a second job waits and then reports it
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = pool.JobContainer(ctx, &NewContainerInput{})
	require.ErrorContains(t, err, "no warm incus VM became free")

	// the job ends: the VM is restored to its snapshot and started again, claimable by the next job
	jobVM := pool.claimed
	require.NoError(t, env.Remove()(context.Background()))
	assert.Contains(t, server.calls, "UpdateInstance "+jobVM+" restore="+jobVM+"/pristine")
	assert.Contains(t, server.calls, "UpdateInstanceState "+jobVM+" stop")

	again, err := pool.JobContainer(context.Background(), &NewContainerInput{})
	require.NoError(t, err)
	assert.NotNil(t, again)
	assert.Regexp(t, `^gitea-runner-job-[0-9a-z]{8}$`, pool.claimed)
	assert.NotEqual(t, jobVM, pool.claimed, "the restored VM went back to the pool and the job claimed a VM again")
	assert.True(t, slices.ContainsFunc(server.calls, func(call string) bool {
		return strings.HasPrefix(call, "UpdateInstanceState gitea-runner-warm-") && strings.HasSuffix(call, " start")
	}), "the recycle started the VM again under its pool name")
}
