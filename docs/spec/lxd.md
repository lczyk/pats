# spec: an lxd backend

status: designed, not started. phases at the end; each is a PR or two.
scope: `src/sandbox` (a session-based driver seam, a docker driver on it, an lxd driver), the task steps (`stage` /
`prepare` / `collect`), the sandbox schema, `internal/eval`, `images/`. 1.x owes 0.x no backwards compatibility.

## why

pats runs every agent in a docker / podman container. an lxd backend gives what docker can't:

- a real system: systemd, snapd, nested lxd, spread tests inside the sandbox.
- a vm boundary for untrusted runs.
- no docker dependency, which also suits a future pats snap.
- an agent that can be given root inside the sandbox while the files it leaves stay the host user's.

the first target is remote lxd: pats on a laptop drives a remote server (e.g. lxd 5.21, x86_64, zfs, nftables, network
acls with logging, both container and vm drivers). local lxd on a linux host is the special case. canonical lxd only,
through the `lxc` cli; incus (the community fork) could come later as a second cli name, as podman sits next to docker.

this builds on tier 1 of the 1.x egress work: one binary with `pats egress run`, the private-range guard, the subnet
pool, the preflight canary. that work's driver seam must allow a proxy that runs next to a remote sandbox, not only
in-process on the host.

## schema

```yaml
lxd:
  kind: sandbox
  items:
    armstrong-vm:
      driver: lxd
      remote: armstrong          # an `lxc remote`; omitted -> the cli's current remote
      vm: true                   # omitted -> a system container
      base: ubuntu:26.04         # omitted -> the pats sandbox image
      provision-cmd: ./provision.sh   # or `provision: |` inline; once per image, as root
      sudo: false                # default; may the agent sudo. a task can override
      max-parallel: 2            # cap this sandbox's pairs whatever -j says
      limits:                    # driver-neutral; docker maps to --cpus / --memory
        cpu: 4
        memory: 8GiB
        disk: 20GiB
      profiles: [...]            # lxd passthrough
      config:                    # lxd passthrough, e.g. for spread with nested lxd
        security.nesting: true
      egress: ...                # the 1.x egress schema, unchanged
```

- `base` + `provision` (inline / `provision-cmd`, the same two-form rule as the task run fields) are the
  driver-neutral way to make an image. the script sees only itself and the host `bin/` on its `PATH`; anything bigger
  it downloads. on docker it compiles to a generated dockerfile (`FROM <base>` + the script). `build:` (a real
  dockerfile) stays as a docker / podman escape hatch. `image:` names a prebuilt image in the driver's own namespace.
- no `base` means the pats sandbox image (ubuntu + the harness clis), and the sandbox's own `provision` runs on top:
  two cached images in a chain, the pats image's hash feeding the user's.
- the published docker sandbox image keeps being published. its install steps move into
  `images/provision-sandbox.sh`, which `Dockerfile.sandbox-26.04` runs and lxd provisioning runs too.
- `provision` (once per image, inside, as root) is deliberately not named after a task step (once per pair).
- validation per driver: `vm`, `remote`, `profiles`, `config` only under `lxd`; `build` only under docker / podman;
  `sudo: true` under docker is an error for now, since the docker agent runs as the host uid with no passwd entry.

## task steps, aligned with spread

| step | where | when |
|---|---|---|
| `stage` / `stage-cmd` | host | before the sandbox starts: ground truth, seeded files, fetches the agent may not make |
| `prepare` / `prepare-cmd` | inside, as root | after the push, before the agent: per-task system state |
| the prompt | inside, as the agent | spread's `execute` |
| `collect` / `collect-cmd` | host | after the pull |

- today's host-side `prepare` becomes `stage`. `prepare` takes spread's meaning: setup on the system under test.
- host steps pair as `stage` / `collect`, in-sandbox ones as `prepare` (spread's `restore` / `debug` later if wanted;
  `restore` isn't needed while sandboxes are fresh or snapshot-restored per pair).
- `stage` stays on the host for two reasons: it can hold ground truth the agent must never see, and it can fetch
  what the agent's egress policy denies (mason clones the very repo its agents may not reach).
- in-sandbox `prepare` gets the task env and `PATS_*` for in-sandbox paths (`/workspace`, `/pats-home`), no
  `PATS_OUTPUT_DIR`, and runs under the sandbox's egress policy. anything secret stays in `stage`.
- the agent can read everything in the sandbox, so the host `bin/` is pushed in for `prepare` and deleted before the
  agent starts.
- `sudo:` (sandbox default `false`, task override): provisioning gives the agent user a sudoers rule; when the
  effective value is false, pats removes it as root after `prepare`. one cached image serves both. each pair's
  metadata records whether the agent had sudo.
- a failed `stage` or `prepare` marks the pair as an error with that step as the cause; their output goes to
  `stage.log` / `prepare.log` in the pair dir.
- no in-sandbox step after the agent yet (system-state capture); added when an eval needs it.
- the task `timeout` covers the agent only. setup (start or restore, push, prepare) has its own budget, 10 minutes by
  default.

## the driver seam

every driver becomes session-based, which in-sandbox `prepare`, lxd's push / pull and the vm pool all need:

```go
type Driver interface {
    // once per run per sandbox: resolve or build the image, cached across runs.
    Image(ctx context.Context, spec ImageSpec) (Image, error)
    // once per pair.
    Start(ctx context.Context, img Image, spec Spec) (Session, error)
}

type Session interface {
    Push(ctx context.Context, hostDir, path string) error
    Pull(ctx context.Context, path, hostDir string) error
    Exec(ctx context.Context, cmd Cmd, stdout, stderr io.Writer) (int, error) // as root or the agent user
    Close() error
}
```

- the docker driver absorbs `buildImages`, `buildContextHash` and `cachedImageID` from `internal/eval/build.go`. it
  starts the container idle (`sleep infinity`), keeps its bind mounts (`Push` / `Pull` are no-ops for bound paths),
  and `docker exec`s prepare and the agent. a timed-out agent is stopped by `docker rm -f` on the container, since
  stopping a `docker exec` client doesn't kill the process inside.
- egress attach stays the egress work's seam; lxd adds its own.
- bwrap stays on the roadmap, so the seam must also suit a daemonless local driver.

## the lxd driver

- shells out to the `lxc` cli and reuses the configured remotes. the go client library is a possible later swap
  behind the same driver; a pats snap won't carry `lxc`, so by then it either bundles the client or moves.
- everything lives in an lxd project `pats` (instances, images, bridges, acls), created on first use. a restricted
  client certificate gets a clear error with the admin commands to run once. instances carry `user.pats.run=<id>`
  so a crashed run can be swept.
- the agent runs as a `pats` user at the next free uid (ubuntu images already have `ubuntu` at 1000), never as plain
  root: harnesses refuse their permission-bypass flags as root (claude code unless `IS_SANDBOX=1`, to verify).
- files: tar over exec, one stream per dir (`tar c -C <host> . | lxc exec <inst> -- tar x -C <path>`), pulled back
  the same way with `--no-same-owner`. the whole `/workspace` comes back, with a warning above a size threshold; a
  task-level filter (spread's `artifacts:`) is later work. no local bind-mount fast path at first.
- secrets go in as `lxc exec --env`, as `docker run -e` does today.
- `-j auto` uses the server's cpu count. vm size is lxd's default unless `limits:` says otherwise.

## slots and the vm pool

a slot is one worker instance plus its own bridge, acl and proxy container, created once per run. pool size per
sandbox is `min(max-parallel, -j, pairs)`. the slot model is the same for containers and vms; only vms snapshot.

- containers: a fresh ephemeral instance per pair, launched from the cached image (a zfs clone, about a second).
- vms: at run start each worker vm boots once and takes a stateful snapshot `ready` (memory included,
  `migration.stateful=true`). a disk-only snapshot wouldn't help, since restoring one restarts the vm. per pair:
  `lxc restore <worker> ready`, then fixups so pairs share nothing from the snapshot moment (mix host randomness into
  `/dev/urandom`, step the clock, renew the dhcp lease), then push, prepare, agent, pull. credentials and home are
  pushed after the resume, so no login state is baked in.
- preflight runs once per agent in the first slot, then the slot is restored.
- the pool lives for one run. snapshots cost about one ram size of disk each, which `-j` multiplies.
- if the server can't take stateful vm snapshots, pats warns and boots a fresh vm per pair.
- provisioning disables cloud-init in pats images, so a launch or restore doesn't rerun first-boot setup.
- a failed exec or transfer marks the pair as an error, with no retries.

## images

- cache key: `sha256(base image fingerprint, provision script, instance type)`, published as `pats-img-<hash>` in the
  `pats` project. a refreshed base image therefore triggers reprovisioning. container and vm images are separate
  image types, hence the instance type in the key.
- a miss: launch the base as a builder, push the script and `bin/`, run it as root, stop, `lxc publish` with the
  alias, delete the builder. output goes to `<run>/images/<hash>.log`; only a generous fixed time cap.
- cached images keep their harness cli versions until the base refreshes, the same staleness docker has; whatever
  pins them for docker applies here too.
- all pats-built images live in the server's image store; nothing is kept on the machine pats runs on.

## egress

- `none`: no nic. the instance uses a pats profile with a root disk and nothing else. `lxc exec` still works on a
  vm, since the guest agent uses vsock.
- `open`: the slot's bridge with nat, plus an acl dropping private ranges (rfc1918, link-local, loopback), so the
  agent reaches the internet but not the server's lan or other instances.
- proxy: the slot's bridge without nat, ipv6 off, and an acl on the agent's nic allowing only the proxy's ip:port.
  fail-closed by acl, not by convention. bridge subnets come from the egress work's pats-managed pool.
  - the slot's proxy container (`ubuntu-minimal:26.04` plus the pats binary, published once per pats version as
    `pats-proxy-<version>`) sits on the bridge at a fixed ip with a second nic out. each pair starts a fresh
    `pats egress run` process in it with that pair's policy and stops it after. its stdout is the audit stream,
    written to the pair's `egress.log`. the proxy is always a container, even when the agent is a vm.
  - the binary is linux for the server's arch (instances always run natively). if the running pats is linux on that
    arch it pushes itself; otherwise pats fetches it from `egress-proxy:v<version>` over the registry's https api
    (token, index, per-arch manifest, top layer, untar in go), with no docker involved. an env var names a local file
    for unreleased dev builds.
  - connections the acl drops are logged by lxd and reported as `bypass_attempts` in the pair's metadata, next to
    `denied_egress`.
  - the egress preflight canary (a direct tcp connect from inside must fail) proves the topology.
- egress is enforced outside the instance, so `sudo` can't undo it.

## metadata

each pair's `metadata.json` gains the remote, the instance type, the image fingerprint, the lxd server version and
whether the agent had sudo.

## mason

- its dockerfile becomes a `provision.sh` on top of the pats sandbox image: apt packages, chisel picked by
  `dpkg --print-architecture`, `git config --system protocol.version 1`.
- its host `prepare` scripts become `stage`; nothing else in the task directories changes.
- one sandbox per driver in its `pats.yaml`; agents pick theirs.
- a vm sandbox with `config: {security.nesting: true}` and `sudo: true` lets an agent run chisel-releases' spread
  tests against nested lxd.

## phases

1. **steps and seam**: rename `prepare` to `stage`, add in-sandbox `prepare`; the session-based `Driver` / `Session`;
   image building moved out of `internal/eval/build.go`; docker on sessions (idle container plus exec), otherwise
   behaviour-identical. after egress tier 1 lands, so two changes don't rewrite `src/sandbox` at once. M-L.
2. **lxd containers**: the project, `remote`, `base` / `provision` with the image cache (docker too), the shipped
   sandbox script, the `pats` user and `sudo`, tar sync, slots, limits, `max-parallel`, none and open egress, cleanup
   on cancel. L.
3. **lxd vms**: `vm: true`, the stateful-snapshot pool with fixups and the fresh-boot fallback, `profiles` / `config`.
   first check stateful vm snapshots on the target server. M-L.
4. **lxd egress**: proxy container per slot, the acl, the registry fetch of the proxy binary, `bypass_attempts`, the
   canary. depends on egress tier 1. L.
5. **cleanup and docs**: `pats clean` for the `pats` project; readme; mason migration notes; an lxd e2e job in ci
   (containers; vms if the runner exposes kvm). M.

## later, not scheduled

- publishing the pats images as rocks. pebble as the container's init could let an lxd container boot the published
  image without provisioning (pats converts the oci layers with its registry client). it wouldn't cover vms or the
  real-system cases, so provisioning stays.
- spread's `artifacts:` (pull back only some paths), an in-sandbox `debug` on agent failure, a system-state capture
  step after the agent.
- a local-lxd fast path: idmapped `disk` devices instead of tar sync when the remote is `local:`.
- a warm vm pool that survives between runs.
- incus as a second cli.

## verification

- unit: the lxd driver behind a fake `lxc` exec, as the container driver has `exec` / `execOut` swaps: launch args,
  tar sync, slot reuse, cleanup on cancel, ErrLaunch. config tests for the new keys and the `stage` rename.
- docker: the existing suite passes on sessions; one directory task with an in-sandbox `prepare` run end to end.
- e2e on a remote server: preflight passes; one mason task per group on an lxd container, then a vm, scored the same
  as on docker; `-j 4`; ctrl+c mid-run leaves nothing in the `pats` project; a second run reuses the cached image.
- egress: the canary fails closed; a denied host shows in `denied_egress`; a direct connection shows in
  `bypass_attempts`.
- checked on the target server before the vm phase: stateful vm snapshots, acl logging, `lxc publish` of a vm.
