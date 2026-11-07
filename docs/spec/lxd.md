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

this builds on the 1.x egress work as shipped, all five tiers: one binary with `pats egress run`, the private-range
guard and `PATS_EGRESS_REFUSE`, the subnet pool, the preflight canary, the per-run mitm CA and merged trust bundle,
the `methods` posture, the audit events and `pats clean`. the driver-neutral half of it (`material` in
`src/sandbox/egress.go`: policy, CA, bundle, proxy env, agent env, audit folding) is reused as is. lxd adds only how
the sandbox reaches the proxy and how it is fail-closed.

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
- `profiles` and `config` pass through, but a profile can attach a nic and undo the egress topology, and a nic that
  is up at boot is already in use before anything could check it. so every instance is made with `lxc init`, its
  expanded config (`lxc config show --expanded`) checked while stopped, and only then started: a nic other than the
  slot's refuses the run, under `egress: none` any nic does, and so does `raw.lxc` / `raw.qemu*` under `none` and
  proxy, since those can add devices the expanded list does not show.

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
- everything lives in an lxd project `pats` (instances, images, bridges, acls), created on first use with
  `features.networks=true` (the default leaves bridges in the default project) and, where the admin restricts the
  project, `restricted.snapshots=allow` for the vm pool and `restricted.containers.nesting=allow` for nested lxd. a
  restricted client certificate cannot create projects: it gets a clear error with the admin commands to run once.
  everything pats makes (workers, builders, proxies, bridges, acls) carries `user.pats.run=<token>`, a random token
  rather than the run dir since laptops and config dirs collide, and `user.pats.owner=<hostname>`. see cleanup.
- the agent runs as a `pats` user at the next free uid (ubuntu images already have `ubuntu` at 1000), never as plain
  root: harnesses refuse their permission-bypass flags as root (claude code unless `IS_SANDBOX=1`, to verify).
- files: tar over exec, one stream per dir (`tar c -C <host> . | lxc exec <inst> -- tar x -C <path>`), pulled back
  the same way with `--no-same-owner`. the whole `/workspace` comes back, with a warning above a size threshold; a
  task-level filter (spread's `artifacts:`) is later work. no local bind-mount fast path at first.
- secrets go in as `lxc exec --env`, as `docker run -e` does today.
- `-j auto` uses the server's cpu count. vm size is lxd's default unless `limits:` says otherwise.

## slots and the vm pool

a slot is one worker instance plus its own bridge, that bridge's acl and (under a proxy policy) a proxy container,
created once per run. pool size per
sandbox is `min(max-parallel, -j, pairs)`. the slot model is the same for containers and vms; only vms snapshot.

- containers: a fresh ephemeral instance per pair, launched from the cached image (a zfs clone, about a second).
- vms: at run start each worker vm boots once and takes a stateful snapshot `ready` (memory included,
  `migration.stateful=true`, `lxc snapshot <worker> ready --stateful`). a disk-only snapshot wouldn't help, since
  restoring one restarts the vm. per pair: `lxc restore <worker> ready --stateful` (without the flag the restore is
  disk-only and the vm reboots), wait for `lxd-agent` to answer (exec and the tar sync both go through it), then
  fixups so pairs share nothing from the snapshot moment (mix host randomness into `/dev/urandom`, step the clock),
  then push, prepare, agent, pull. credentials and home are pushed
  after the resume, so no login state is baked in.
- preflight runs once per agent in the first slot, then the slot is restored.
- the pool lives for one run. snapshots cost about one ram size of disk each, which `-j` multiplies.
- if the server can't take stateful vm snapshots, pats warns and boots a fresh vm per pair.
- provisioning disables cloud-init in pats images, so a launch or restore doesn't rerun first-boot setup.
- a failed exec or transfer marks the pair as an error, with no retries.

## images

- cache key: `sha256(base image fingerprint, provision script, the host bin/ contents, instance type)`, published as
  `pats-img-<hash>` in the `pats` project. `bin/` is in the key because provisioning runs it. a refreshed base image
  therefore triggers reprovisioning. container and vm images are separate image types, hence the instance type in the
  key.
- a miss: launch the base as a builder, push the script and `bin/`, run it as root, stop, `lxc publish` under a
  run-private alias `pats-img-<hash>-<token>`, delete the builder, then create the real alias. if another run made
  it first (two laptops, one miss) its image is used and this one deleted: same key, same inputs. never
  `lxc publish --reuse`, which could swap an image out from under a running pair. output goes to
  `<run>/images/<hash>.log`, so the run dir is made before images are resolved (`internal/eval/run.go` builds first
  today); only a generous fixed time cap.
- cached images keep their harness cli versions until the base refreshes, the same staleness docker has; whatever
  pins them for docker applies here too.
- a custom `base` for a vm must run `lxd-agent`: exec and file transfer need it.
- all pats-built images live in the server's image store; nothing is kept on the machine pats runs on.

## egress

- `none`: no nic. the instance uses a pats profile with a root disk and nothing else. `lxc exec` still works on a
  vm, since the guest agent uses vsock.
- `open`: the slot's bridge with nat and an acl. assigning an acl makes lxd reject what no rule matches, so the acl
  sets `security.acls.default.egress.action=allow` and drops the private, local and multicast ranges, v4 and v6
  (rfc1918, loopback, link-local, `224.0.0.0/4`, `fc00::/7`, `fe80::/10`, `ff00::/8`). bridge acls apply only at
  the bridge-host boundary, which is exactly this case. that keeps the agent off a lan numbered out of those ranges
  and off other instances; a lan on global addresses is not covered, and `open` does not claim it is.
- proxy: fail-closed by topology first, acl second. a bridge acl cannot filter between instances on the same bridge
  or per nic (that needs ovn), and lxd's own dhcp and dns on a bridge come before any acl -- the way docker's internal
  bridge handed the agent the host (checked on the target server: an addressed bridge with `ipv4.dhcp=false`,
  `dns.mode=none` and a default-reject acl still answers on the host's port 53). so the slot's bridge gets
  `ipv4.address=none` and `ipv6.address=none` (lxd then starts no dnsmasq on it and adds no address, nat or
  forwarding rules), and nothing is on it but the agent and the proxy.
  - the bridge interface still exists on the host, and a root agent could aim a route at its mac; if anything else
    on the server turned `ip_forward` on, the host would route that. so the bridge also carries an acl with no rules
    and both default actions `reject`: it fences exactly the bridge-host boundary, which is all that is left.
  - both instances get static addresses out of the egress work's pats-managed pool. the lxd nic devices carry
    explicit names (`name=pats0` on the slot bridge for both, `name=uplink0` for the proxy's way out), so the address
    goes on the right nic whatever order a vm enumerates them in. the address is set with `ip` over exec after
    start, not netplan, so a custom base needs only iproute2; a restored vm keeps it in memory.
  - the proxy's `uplink0`, on the nat'd bridge, is the only way out.
  - a server that also runs docker drops bridged ipv4 between instances on any lxd bridge it was not told about
    (seen on the target server: a fresh bridge, addressed or not, carried v6 but no v4 between two containers, while
    `lxdbr0` worked; docker's `FORWARD` policy with `br_netfilter` is the usual cause). pats can't fix that from a
    remote client, so slot bridges are named `patsbr<n>` for one admin rule to cover all, and the preflight checks
    agent-to-proxy reachability and names the fix (lxd's docker coexistence how-to, scoped to `patsbr+`) when it
    fails.
  - the slot's proxy container (`ubuntu-minimal:26.04` plus the pats binary, published once per pats version as
    `pats-proxy-<version>`) sits on the bridge at a fixed ip with a second nic out; ip forwarding stays off in it.
    each pair starts a fresh `pats egress run` process in it with that pair's policy and
    `--listen <bridge ip>:8080` -- never `:8080`, which would serve the outbound bridge too -- and stops it after.
    its stdout is the audit stream, written to the pair's `egress.log`. the proxy is always a container, even when
    the agent is a vm.
  - the pats side is the egress work's `material`, unchanged: the per-run CA (its key reaches the proxy inline, never
    disk), the merged trust bundle, `PATS_EGRESS_REFUSE=<pool>`, the harness hosts merged per agent, the audit folded
    into the pair's metadata. the lxd attach pushes `/sbx-ca` into the instance and gives the agent exec the same
    `HTTP_PROXY` and tls env the docker agent gets.
  - the binary is linux for the server's arch (instances always run natively). if the running pats is linux on that
    arch it pushes itself; otherwise pats fetches it from `egress-proxy:v<version>` over the registry's https api
    (token, index, per-arch manifest, top layer, untar in go), with no docker involved. an env var names a local file
    for unreleased dev builds.
  - the preflight canary is the egress work's: tcp4, tcp6, a udp dns query and a name lookup from inside must all
    fail before the proxy exists. it proves the topology per driver. lxd adds a hostile probe for `sudo: true`: as
    root, point a default route at the bridge's mac through a static neighbour entry and try again.
- egress is enforced outside the instance, so `sudo` can't undo it. a nic a profile adds is refused (see schema).

## cleanup

the owner-pid liveness `pats clean` uses for docker cannot be asked of a remote server. instead the first thing a
run creates is a marker, an empty acl `pats-run-<token>`, and pats renews `user.pats.lease=<unix time>` on it every two
minutes while the run lives. everything else the run makes (builders, workers, proxies, bridges, acls, run-private
image aliases) carries the token. a container slot has no worker between pairs while its proxy and bridge stay up, so
the lease can't live on a worker.

`pats clean` groups what it finds by token. a group whose marker's lease is more than ten minutes old goes, agents
before proxies before networks before the marker, as for docker. a group with no marker yet (a run that died between
creating something and its marker, or the instant before) is judged by its resources' creation time against the same
ten minutes. a fresh lease is left alone whoever owns it; `--force` takes everything in the project, for a laptop
that is gone. a run that ends, or is interrupted, removes its own.

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
   behaviour-identical. the egress work has landed, so this is the only rewrite of `src/sandbox`. M-L.
2. **lxd containers**: the project, `remote`, `base` / `provision` with the image cache (docker too), the shipped
   sandbox script, the `pats` user and `sudo`, tar sync, slots, limits, `max-parallel`, none and open egress, cleanup
   on cancel. L.
3. **lxd vms**: `vm: true`, the stateful-snapshot pool with fixups and the fresh-boot fallback, `profiles` / `config`.
   first check stateful vm snapshots on the target server. M-L.
4. **lxd egress**: the addressless bridge with its reject acl, named nics and static addressing, the proxy container
   per slot, the `open` acl, the registry fetch of the proxy binary, `material` reuse, the canary and the hostile
   probe, the docker coexistence check. L.
5. **cleanup and docs**: lease-based `pats clean` for the `pats` project; readme; mason migration notes; an lxd e2e
   job in ci (containers; vms if the runner exposes kvm). M.

## later, not scheduled

- publishing the pats images as rocks. pebble as the container's init could let an lxd container boot the published
  image without provisioning (pats converts the oci layers with its registry client). it wouldn't cover vms or the
  real-system cases, so provisioning stays.
- spread's `artifacts:` (pull back only some paths), an in-sandbox `debug` on agent failure, a system-state capture
  step after the agent.
- a local-lxd fast path: idmapped `disk` devices instead of tar sync when the remote is `local:`.
- `bypass_attempts` in the pair's metadata: on an addressless bridge a direct connect has no route and leaves no
  trace. if wanted, point the agent's default route at the proxy container and count what nftables drops there.
- a warm vm pool that survives between runs.
- incus as a second cli.

## verification

- unit: the lxd driver behind a fake `lxc` exec, as the container driver has `exec` / `execOut` swaps: launch args,
  tar sync, slot reuse, cleanup on cancel, ErrLaunch. config tests for the new keys and the `stage` rename.
- docker: the existing suite passes on sessions; one directory task with an in-sandbox `prepare` run end to end.
- e2e on a remote server: preflight passes; one mason task per group on an lxd container, then a vm, scored the same
  as on docker; `-j 4`; ctrl+c mid-run leaves nothing in the `pats` project; a second run reuses the cached image.
- egress: the canary fails closed; a denied host shows in `denied_egress`; from inside, the host's addresses, the
  server's lan, another slot's bridge and the proxy's outbound address are all unreachable; from the outbound bridge
  the proxy's port is closed.
- cleanup: `kill -9` of pats mid-run, then `pats clean` after the lease lapses, leaves the project empty; two runs
  from two laptops on one server do not sweep each other, including a container run between pairs; two runs
  missing the same image build it once or reuse each other's.
- checked on the target server before the vm phase: stateful vm snapshots, `lxc publish` of a vm.
