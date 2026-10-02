// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package container

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"gitea.com/gitea/runner/act/common"

	"github.com/kballard/go-shellquote"
	incus "github.com/lxc/incus/v6/client"
	"github.com/lxc/incus/v6/shared/api"
)

// IncusOptions configures the VMs jobs run in.
type IncusOptions struct {
	Remote      string         // incus API endpoint, a unix socket path or a URL, empty is the local daemon
	Project     string         // project of job and pool VMs, empty is the remote's default
	PoolSize    int            // warm VMs kept ready, 0 starts a VM per job
	Snapshot    string         // snapshot a VM is restored to after its job, empty is the pool fill one, "none" deletes the VM instead
	Template    map[string]any // instance config merged over what the runner sets
	Image       string         // image the pool fills from and per-job VMs default to
	MaxLifetime time.Duration
	RunnerUUID  string
}

// IncusPool hands out warm VMs and takes them back after a job.
type IncusPool struct {
	options  IncusOptions
	server   incus.InstanceServer
	mutex    chan struct{} // held while filling, claiming or returning a VM
	image    string        // image the pool fills from, also a per-job VM's default
	claimed  string        // instance name of the claimed VM, empty when none is
	services []*incusContainer
}

func (p *IncusPool) poolImage() string {
	return cmp.Or(p.options.Image, p.image)
}

// NewIncusPool connects to the incus daemon the job VMs run on.
func NewIncusPool(ctx context.Context, options IncusOptions) (*IncusPool, error) {
	server, err := connectIncus(ctx, options.Remote)
	if err != nil {
		return nil, fmt.Errorf("connect to incus: %w", err)
	}
	if options.Project != "" {
		server = server.UseProject(options.Project)
	}
	return &IncusPool{options: options, server: server, mutex: make(chan struct{}, 1)}, nil
}

// connectIncus is a variable so tests can substitute a fake server.
var connectIncus = func(ctx context.Context, remote string) (incus.InstanceServer, error) {
	if strings.HasPrefix(remote, "http://") || strings.HasPrefix(remote, "https://") {
		return incus.ConnectIncusWithContext(ctx, remote, nil)
	}
	return incus.ConnectIncusUnixWithContext(ctx, remote, nil)
}

// Close drops the pool's idle connections, its VMs live on for the next job.
func (p *IncusPool) Close() {
	p.server.Disconnect()
}

// JobContainer prepares the VM a job runs in, claiming a warm one or creating it fresh.
func (p *IncusPool) JobContainer(ctx context.Context, input *NewContainerInput) (ExecutionsEnvironment, error) {
	if err := p.Fill(ctx); err != nil {
		common.Logger(ctx).Warnf("Filling the incus VM pool: %v", err)
	}
	select {
	case p.mutex <- struct{}{}:
		defer func() { <-p.mutex }()
	default:
		return nil, errors.New("another job is starting on this runner's incus pool")
	}
	var name string
	if p.options.PoolSize > 0 {
		vm, err := p.claimWarm(ctx)
		if err != nil {
			return nil, err
		}
		name = vm
	} else {
		name = "gitea-runner-job-" + p.suffix()
		if err := p.createVM(ctx, name, cmp.Or(input.Image, p.options.Image)); err != nil {
			return nil, err
		}
	}
	p.claimed = name
	return &incusContainer{pool: p, job: true, input: input}, nil
}

// claimWarm takes an idle warm VM or waits for one, bounded by WarmupTimeout through ctx.
func (p *IncusPool) claimWarm(ctx context.Context) (string, error) {
	logger := common.Logger(ctx)
	for {
		instances, err := p.server.GetInstances(api.InstanceTypeVM)
		if err != nil {
			return "", fmt.Errorf("list incus VMs: %w", err)
		}
		for _, instance := range slices.Backward(instances) {
			if !strings.HasPrefix(instance.Name, "gitea-runner-warm-") || instance.Name == p.claimed || instance.CreatedAt.Equal(time.Time{}) {
				continue
			}
			if !p.hasSnapshot(instance.Name) {
				continue // its fill was cut short, ensureFilled replaces it
			}
			newName, ok := p.claim(instance.Name)
			if !ok {
				continue
			}
			logger.Debugf("Claimed warm incus VM %s", newName)
			return newName, nil
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("no warm incus VM became free: %w", ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

// claim marks a warm VM as a job's, losing the race leaves it for another.
func (p *IncusPool) claim(name string) (string, bool) {
	newName := "gitea-runner-job-" + p.suffix()
	op, err := p.server.RenameInstance(name, api.InstancePost{Name: newName})
	if err != nil {
		return "", false // another job claimed it, or it is stopping
	}
	return newName, op.Wait() == nil
}

// suffix disambiguates concurrent VM names, unique enough for one runner's jobs.
func (p *IncusPool) suffix() string {
	return strings.ToLower(rand.Text()[:8])
}

// ensureFilled tops the pool up to its size, counting the ready VMs the daemon already holds.
func (p *IncusPool) ensureFilled(ctx context.Context) error {
	instances, err := p.server.GetInstances(api.InstanceTypeVM)
	if err != nil {
		return fmt.Errorf("list incus VMs: %w", err)
	}
	warm := 0
	for _, instance := range instances {
		if strings.HasPrefix(instance.Name, "gitea-runner-warm-") && p.hasSnapshot(instance.Name) {
			warm++
		}
	}
	for range max(0, p.options.PoolSize-warm) {
		if err := p.fillOne(ctx); err != nil {
			return err
		}
	}
	return nil
}

// createVM creates and starts a VM from an incus image, waiting for its agent to answer.
func (p *IncusPool) createVM(ctx context.Context, name, image string) error {
	logger := common.Logger(ctx)
	logger.Debugf("Creating incus VM %s from image %s", name, image)
	source, err := p.sourceImage(image)
	if err != nil {
		return err
	}
	req := api.InstancesPost{
		Name:   name,
		Type:   api.InstanceTypeVM,
		Start:  true,
		Source: *source,
		Config: map[string]string{
			// the job's nested dockerd needs modprobe and overlay, the kernel modules come from the VM image
			"security.nesting": "true",
		},
	}
	for key, value := range p.options.Template {
		req.Config[key] = fmt.Sprint(value)
	}
	op, err := p.server.CreateInstanceFromImage(p.server, api.Image{Fingerprint: source.Fingerprint}, req)
	if err != nil {
		return fmt.Errorf("create VM %s: %w", name, err)
	}
	if err = op.Wait(); err != nil {
		return fmt.Errorf("create VM %s: %w", name, err)
	}
	return p.waitAgent(ctx, name)
}

// sourceImage resolves an incus image name to the source that creates a VM from it, through
// the daemon's cached remotes for images:* ones and its own storage otherwise.
func (p *IncusPool) sourceImage(image string) (*api.InstanceSource, error) {
	if remote, ref, found := strings.Cut(image, ":"); found && slices.Contains([]string{"images", "ubuntu"}, remote) {
		return &api.InstanceSource{Type: "image", Protocol: "simplestreams", Server: imageRemotes[remote], Alias: ref}, nil
	}
	if _, _, err := p.server.GetImageAlias(image); err == nil {
		return &api.InstanceSource{Type: "image", Alias: image}, nil
	}
	if _, _, err := p.server.GetImage(image); err != nil {
		return nil, fmt.Errorf("image %s: %w", image, err)
	}
	return &api.InstanceSource{Type: "image", Fingerprint: image}, nil
}

var imageRemotes = map[string]string{
	"images": "https://images.linuxcontainers.org",
	"ubuntu": "https://cloud-images.ubuntu.com/releases",
}

// waitAgent polls the VM state until the agent answers, which is what makes exec and files work.
func (p *IncusPool) waitAgent(ctx context.Context, name string) error {
	for {
		state, _, err := p.server.GetInstanceState(name)
		if err != nil {
			return fmt.Errorf("state of VM %s: %w", name, err)
		}
		if hasGlobalAddress(state.Network) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("incus agent of VM %s did not answer: %w", name, ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// hasGlobalAddress reports a VM state with an interface that got an address, which the agent being up implies.
func hasGlobalAddress(networks map[string]api.InstanceStateNetwork) bool {
	for _, network := range networks {
		for _, addr := range network.Addresses {
			if addr.Family == "inet" && addr.Scope == "global" {
				return true
			}
		}
	}
	return false
}

// Fill primes the pool with warm VMs, each holding the snapshot its job restores afterwards.
func (p *IncusPool) Fill(ctx context.Context) error {
	if p.options.PoolSize <= 0 {
		return nil
	}
	select {
	case p.mutex <- struct{}{}:
		defer func() { <-p.mutex }()
	default:
		return nil // another fill or job start is running
	}
	return p.ensureFilled(ctx)
}

// fillOne creates a warm VM and the snapshot a job restores it to. The snapshot lives on the VM
// itself: a snapshot taken on another instance is gone with that one.
func (p *IncusPool) fillOne(ctx context.Context) error {
	name := "gitea-runner-warm-" + p.suffix()
	if err := p.createVM(ctx, name, p.poolImage()); err != nil {
		return err
	}
	op, err := p.server.CreateInstanceSnapshot(name, api.InstanceSnapshotsPost{Name: p.snapshotName()})
	if err != nil {
		return fmt.Errorf("snapshot VM %s: %w", name, err)
	}
	if err := op.WaitContext(ctx); err != nil {
		return fmt.Errorf("snapshot VM %s: %w", name, err)
	}
	return nil
}

// hasSnapshot reports whether a VM holds the snapshot a job restores, so a VM whose fill was cut
// short is refilled instead of claimed.
func (p *IncusPool) hasSnapshot(name string) bool {
	snapshots, err := p.server.GetInstanceSnapshotNames(name)
	if err != nil {
		return false
	}
	return slices.Contains(snapshots, p.snapshotName())
}

// stopVM stops a VM and waits for it.
func (p *IncusPool) stopVM(ctx context.Context, name string) error {
	op, err := p.server.UpdateInstanceState(name, api.InstanceStatePut{Action: "stop", Force: true, Timeout: 60}, "")
	if err != nil {
		return fmt.Errorf("stop VM %s: %w", name, err)
	}
	if err := op.WaitContext(ctx); err != nil {
		return fmt.Errorf("stop VM %s: %w", name, err)
	}
	return nil
}

// restore returns a VM to its pristine snapshot, running again for the next job.
func (p *IncusPool) restore(ctx context.Context, name string) error {
	op, err := p.server.UpdateInstance(name, api.InstancePut{Restore: name + "/" + p.snapshotName()}, "")
	if err != nil {
		return err
	}
	return op.WaitContext(ctx)
}

// snapshotName is the snapshot a job VM returns to, configured or the one its fill took.
func (p *IncusPool) snapshotName() string {
	return cmp.Or(p.options.Snapshot, "pristine")
}

// Recycle puts a claimed VM back under its pool name, or deletes it when restores are off.
func (p *IncusPool) Recycle(ctx context.Context, name string) {
	logger := common.Logger(ctx)
	_ = p.stopVM(ctx, name)
	if p.snapshotName() == "none" {
		if err := p.deleteVM(context.WithoutCancel(ctx), name); err != nil {
			logger.Errorf("Recycling incus VM %s: %v", name, err)
		}
		return
	}
	if p.snapshotName() != "" { // restored to the fill snapshot, so the next job sees a clean VM
		if err := p.restore(ctx, name); err != nil {
			logger.Errorf("Recycling incus VM %s: %v", name, err)
		}
	}
	warm := "gitea-runner-warm-" + p.suffix()
	op, err := p.server.RenameInstance(name, api.InstancePost{Name: warm})
	if err != nil {
		logger.Errorf("Returning incus VM %s to the pool: %v", name, err)
		return
	}
	if err := op.WaitContext(context.WithoutCancel(ctx)); err != nil {
		logger.Errorf("Returning incus VM %s to the pool: %v", name, err)
		return
	}
	if start, err := p.server.UpdateInstanceState(warm, api.InstanceStatePut{Action: "start", Timeout: 60}, ""); err != nil {
		logger.Errorf("Starting returned incus VM %s: %v", warm, err)
	} else if err := start.WaitContext(context.WithoutCancel(ctx)); err != nil {
		logger.Errorf("Starting returned incus VM %s: %v", warm, err)
	}
}

// incusContainer is one job's VM, its services are sibling VMs on the same bridge.
type incusContainer struct {
	LinuxContainerEnvironmentExtensions
	pool         *IncusPool
	vm           string // the instance name, "job" for the job's, one per service otherwise
	job          bool
	input        *NewContainerInput
	arch, vmPath string
}

// IncusJobContainer returns the VM the job's steps run in.
func (p *IncusPool) IncusJobContainer(input *NewContainerInput) ExecutionsEnvironment {
	return &incusContainer{pool: p, vm: "job", job: true, input: input}
}

// IncusServiceContainer returns a sibling VM the job reaches at hostname IncusServiceName(id).
func (p *IncusPool) IncusServiceContainer(id string, input *NewContainerInput) ExecutionsEnvironment {
	service := &incusContainer{pool: p, vm: IncusServiceName(id), input: input}
	p.services = append(p.services, service)
	return service
}

// IncusServiceName returns the container name and hostname of service id, like KubernetesServiceName.
func IncusServiceName(id string) string {
	return KubernetesServiceName(id)
}

// jobName maps the job's or a service's container name to the VM's actual instance name, service
// instances prefixed so a crashed runner's leftovers are recognisable.
func (p *IncusPool) jobName(kind string) string {
	if kind == "job" {
		return p.claimed
	}
	return "gitea-runner-svc-" + kind + "-" + p.claimed
}

// deleteVM removes a VM, waiting for the operation.
func (p *IncusPool) deleteVM(ctx context.Context, name string) error {
	op, err := p.server.DeleteInstance(name)
	if err != nil {
		return fmt.Errorf("delete VM %s: %w", name, err)
	}
	if err := op.WaitContext(ctx); err != nil {
		return fmt.Errorf("delete VM %s: %w", name, err)
	}
	return nil
}

// ServiceAddresses returns the IPv4 address of every service VM the job can reach, keyed by the
// hostname the workflow uses, so the job VM gets hosts entries for them.
func (p *IncusPool) ServiceAddresses(ctx context.Context) (map[string]string, error) {
	addresses := map[string]string{}
	for _, service := range p.services {
		state, _, err := p.server.GetInstanceState(service.instance())
		if err != nil {
			return nil, fmt.Errorf("state of service VM %s: %w", service.instance(), err)
		}
		for _, network := range state.Network {
			for _, address := range network.Addresses {
				if address.Family == "inet" && address.Scope == "global" {
					addresses[service.vm] = address.Address
				}
			}
		}
		if addresses[service.vm] == "" {
			return nil, fmt.Errorf("service VM %s has no address", service.instance())
		}
	}
	return addresses, nil
}

func (c *incusContainer) instance() string {
	return c.pool.jobName(c.vm)
}

func (c *incusContainer) Create(_, _ []string) common.Executor {
	return func(ctx context.Context) error {
		if c.job {
			return nil // the VM is the claimed or freshly created one, its dockerd comes up with it
		}
		return c.pool.createVM(ctx, c.instance(), c.input.Image)
	}
}

func (c *incusContainer) Start(_ bool) common.Executor {
	return func(ctx context.Context) error {
		if !c.job {
			return nil
		}
		var stdout bytes.Buffer
		if err := c.exec(ctx, []string{"sh", "-c", `printf '%s\n%s' "$(uname -m)" "$PATH"`}, nil, &stdout, io.Discard); err != nil {
			return err
		}
		machine, vmPath, _ := strings.Cut(stdout.String(), "\n")
		c.arch, c.vmPath = goArchToActionArch(machine), vmPath
		// docker:// steps need a daemon, the VM image has to ship the docker CLI (nesting is set on the VM)
		return c.exec(ctx, []string{"sh", "-c", dockerdScript}, nil, io.Discard, io.Discard)
	}
}

func (*incusContainer) Pull(bool) common.Executor {
	return func(context.Context) error { return nil } // the image is baked into the VM
}

var (
	incusShellName    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	incusBashReadonly = []string{"BASHOPTS", "BASH_VERSINFO", "EUID", "PPID", "SHELLOPTS", "UID"} // bash as sh cannot export these
)

// dockerdScript starts the VM's own docker daemon when the image ships one and no daemon answers yet.
const dockerdScript = `command -v dockerd >/dev/null || exit 0
docker info >/dev/null 2>&1 && exit 0
nohup dockerd >/var/log/dockerd.log 2>&1 &
for i in $(seq 60); do docker info >/dev/null 2>&1 && exit 0; sleep 1; done
echo "dockerd did not come up, see /var/log/dockerd.log" >&2; exit 1`

// Exec feeds the command to sh on stdin, which exports the env so it stays out of the request URL and, where sh can export a name, out of process arguments.
func (c *incusContainer) Exec(command []string, env map[string]string, _, workdir string) common.Executor {
	return func(ctx context.Context) error {
		script, args := "", []string{"env", "--"}
		for name, value := range env {
			if incusShellName.MatchString(name) && !slices.Contains(incusBashReadonly, name) {
				script += "command export " + shellquote.Join(name+"="+value) + "\n"
			} else {
				args = append(args, name+"="+value)
			}
		}
		script += "exec " + shellquote.Join(append(args, command...)...)
		if workdir != "" {
			script = "cd " + shellquote.Join(workdir) + " || exit\n" + script
		}
		defer common.FlushWriter(c.input.Stdout)
		defer common.FlushWriter(c.input.Stderr)
		return c.exec(ctx, []string{"sh"}, strings.NewReader(script), c.input.Stdout, c.input.Stderr)
	}
}

// exec runs a command in the VM over the agent's exec API, returning its exit status as an error.
func (c *incusContainer) exec(ctx context.Context, command []string, stdin io.Reader, stdout, stderr io.Writer) error {
	instance := c.instance()
	post := api.InstanceExecPost{Command: command, WaitForWS: true, Environment: map[string]string{"TERM": "xterm"}}
	if c.job && c.vmPath != "" {
		post.Environment["PATH"] = c.vmPath
	}
	args := incus.InstanceExecArgs{Stdin: stdin, Stdout: stdout, Stderr: stderr, DataDone: make(chan bool)}
	op, err := c.pool.server.ExecInstance(instance, post, &args)
	if err != nil {
		return fmt.Errorf("exec in VM %s: %w", instance, err)
	}
	if err := op.WaitContext(ctx); err != nil {
		return fmt.Errorf("exec in VM %s: %w", instance, err)
	}
	opAPI := op.Get()
	if exit, ok := opAPI.Metadata["return"].(float64); ok && int(exit) != 0 {
		return ExitCodeError(int(exit))
	}
	return nil
}

func (c *incusContainer) extract(ctx context.Context, destPath string, write func(io.Writer) error) error {
	reader, writer := io.Pipe()
	defer reader.Close()
	go func() { writer.CloseWithError(write(writer)) }()
	var stderr bytes.Buffer
	if err := c.exec(ctx, []string{"sh", "-c", `mkdir -p "$1" && tar -xf - -C "$1"`, "sh", destPath}, reader, io.Discard, &stderr); err != nil {
		return fmt.Errorf("extract to %s: %w: %s", destPath, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (c *incusContainer) Copy(destPath string, files ...*FileEntry) common.Executor {
	return func(ctx context.Context) error {
		return c.extract(ctx, destPath, func(writer io.Writer) error { return writeFilesTar(ctx, writer, 0, 0, files...) })
	}
}

func (c *incusContainer) CopyDir(destPath, srcPath string, useGitIgnore, skipGitDir bool) common.Executor {
	return func(ctx context.Context) error {
		return c.extract(ctx, "/", func(writer io.Writer) error {
			return writeDirTar(ctx, writer, destPath, srcPath, useGitIgnore, skipGitDir, 0, 0)
		})
	}
}

// GetContainerArchive streams the archive, checking the path first as tar writes an empty archive for a missing one.
func (c *incusContainer) GetContainerArchive(ctx context.Context, srcPath string) (io.ReadCloser, error) {
	reader, writer := io.Pipe()
	go func() {
		var stderr bytes.Buffer
		err := c.exec(ctx, []string{"sh", "-c", `test -e "$1" -o -L "$1" && exec tar -cf - -C "$2" "$3"`, "sh", srcPath, path.Dir(srcPath), path.Base(srcPath)}, nil, writer, &stderr)
		if err != nil {
			err = fmt.Errorf("archive %s: %w: %s", srcPath, err, strings.TrimSpace(stderr.String()))
		}
		writer.CloseWithError(err)
	}()
	buffered := bufio.NewReader(reader)
	if _, err := buffered.Peek(1); err != nil {
		reader.Close()
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{buffered, reader}, nil
}

func (c *incusContainer) UpdateFromEnv(srcPath string, env *map[string]string) common.Executor {
	return parseEnvFile(c, srcPath, env)
}

func (c *incusContainer) UpdateFromImageEnv(env *map[string]string) common.Executor {
	return func(context.Context) error {
		(*env)["PATH"] = cmp.Or((*env)["PATH"], c.vmPath)
		return nil
	}
}

func (c *incusContainer) Inspect(ctx context.Context) (*Info, error) {
	instance := c.instance()
	state, _, err := c.pool.server.GetInstanceState(instance)
	if api.StatusErrorCheck(err, http.StatusNotFound) {
		return nil, fmt.Errorf("VM %s %w", instance, ErrContainerNotFound)
	} else if err != nil {
		return nil, fmt.Errorf("state of VM %s: %w", instance, err)
	}
	info := &Info{ID: instance, State: "created", Health: HealthNone, Ports: map[string]string{}}
	switch state.Status {
	case "Running":
		info.State = StateRunning
	case "Stopped", "Frozen":
		info.State = "exited"
	case "Error":
		info.State = "exited"
		info.ExitCode = 1
	}
	for port := range c.input.ExposedPorts {
		info.Ports[port.Port()] = port.Port() // services reach each other on the VM's own addresses
	}
	return info, nil
}

func (c *incusContainer) DumpLogs(ctx context.Context) error {
	instance := c.instance()
	defer common.FlushWriter(c.input.Stdout)
	logfiles, err := c.pool.server.GetInstanceLogfiles(instance)
	if err != nil {
		return fmt.Errorf("logs of VM %s: %w", instance, err)
	}
	for _, logfile := range logfiles {
		if !strings.HasSuffix(logfile, "console.log") && !strings.HasSuffix(logfile, "lxc.log") {
			continue
		}
		content, err := c.pool.server.GetInstanceLogfile(instance, path.Base(logfile))
		if err != nil {
			continue
		}
		_, _ = io.Copy(c.input.Stdout, content)
		content.Close()
	}
	return nil
}

func (c *incusContainer) Remove() common.Executor {
	return func(ctx context.Context) error {
		if !c.job {
			return c.pool.deleteVM(ctx, c.instance()) // a service VM lives for this job only
		}
		c.pool.Recycle(ctx, c.pool.claimed)
		c.pool.claimed = ""
		return nil
	}
}

// RemoveOrphanIncusVMs deletes this runner's job VMs older than createdBefore.
func RemoveOrphanIncusVMs(ctx context.Context, options IncusOptions, createdBefore time.Time) error {
	pool, err := NewIncusPool(ctx, options)
	if err != nil {
		return err
	}
	defer pool.Close()
	instances, err := pool.server.GetInstances(api.InstanceTypeVM)
	if err != nil {
		return fmt.Errorf("list incus VMs: %w", err)
	}
	var errs []error
	for _, instance := range instances {
		if !strings.HasPrefix(instance.Name, "gitea-runner-job-") && !strings.HasPrefix(instance.Name, "gitea-runner-svc-") && !strings.HasPrefix(instance.Name, "gitea-runner-template-") {
			continue
		}
		if !instance.CreatedAt.Before(createdBefore) {
			continue
		}
		op, err := pool.server.DeleteInstance(instance.Name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := op.WaitContext(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (c *incusContainer) Close() common.Executor {
	return func(context.Context) error {
		if !c.job {
			return nil
		}
		c.pool.server.Disconnect()
		return nil
	}
}

func (c *incusContainer) ReplaceLogWriter(stdout, stderr io.Writer) (io.Writer, io.Writer) {
	oldStdout, oldStderr := c.input.Stdout, c.input.Stderr
	c.input.Stdout, c.input.Stderr = stdout, stderr
	return oldStdout, oldStderr
}

func (c *incusContainer) GetRunnerContext(_ context.Context) map[string]any {
	return map[string]any{"os": "Linux", "arch": c.arch, "temp": "/tmp", "tool_cache": DefaultToolCache}
}
