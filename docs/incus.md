# Running jobs in incus VMs

The runner can run each job in its own virtual machine instead of a Docker container.
It talks to an [incus](https://linuxcontainers.org/incus/) daemon, so the jobs get a real
kernel, systemd and network namespace per job rather than a shared host kernel.

Docker support is unchanged: jobs only get VMs when their labels ask for the `incus` scheme.

## Configuring the runner

```yaml
incus:
  # local daemon over its unix socket, or https://host:8443 for a remote one
  remote: ""
  project: ""
  # image the VMs boot from, an incus alias (e.g. images:ubuntu/24.04) or a fingerprint
  image: images:ubuntu/24.04
  # warm VMs kept ready for the next job, 0 starts a VM per job instead
  pool_size: 1
  # snapshot every job VM returns to, empty uses the one the pool takes, "none" deletes VMs instead
  snapshot: ""
  warmup_timeout: 5m
  agent_start_timeout: 2m
  instance_template:
    limits.cpu: "4"
    limits.memory: 8GiB
```

The runner user needs access to the incus socket, which on most setups means the `incus`
group. `remote` accepts an `https://` URL too, in which case the CLI config of the current
user supplies the client certificates.

`instance_template` keys go straight into the VM's incus config, so limits, devices and
`limits.cpu.allow` style knobs all work.

## Assigning jobs to VMs

A label with the `incus` schema picks the platform:

```yaml
labels:
  - "backend:incus"                  # the runner's configured image, pool VMs
  - "ubuntu-24:incus://images:ubuntu/24.04"   # this image only
  - "big:incus://images:ubuntu/24.04"
```

The image part after `incus://` overrides `incus.image` for the jobs that carry the label.
A job with a per-job image uses its own VM instead of a pool VM.

## What a job gets

- The job runs as root in its own VM, `service:` containers become sibling VMs the job
  reaches by hostname.
- `docker://` steps and Docker actions run against a docker daemon the runner starts
  inside the job VM. The VM image must ship the docker CLI and daemon, which the
  `images:` images do not, so use an image that has them.
- `container:` options, volumes and credentials are refused: a VM has no bind mounts and
  there is no host path to share.

## Pool lifecycle

With `pool_size` above zero the runner keeps that many VMs booted and ready. The pool is
topped up whenever a job starts, so a runner that restarted fills itself on first use. A job
claims a VM by renaming it, and when the job ends the VM is stopped, restored to its own
snapshot and renamed back into the pool. Every warm VM carries the snapshot taken when it was
created, so a job never sees what the previous one left behind.

A VM whose snapshot is missing, because its fill was cut short, is replaced rather than handed
out.

Job VMs left behind by a crashed runner are removed at idle cleanup, like orphan containers
are on the Docker path.

## Limitations

- One VM is claimed per job at a time. Extra jobs wait for a free VM until `warmup_timeout`.
- Snapshots cost disk in the pool VM that holds them; lower `pool_size` if that bites.
- No live e2e coverage: the incus path is covered by unit tests against a fake daemon.
