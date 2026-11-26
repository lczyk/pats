# nested instances in a sandbox

status: proposed.

## problem

some tasks start instances of their own inside the sandbox. mason's rock tasks are the first: `rockcraft pack` builds
in an lxd container, and `rockcraft test` runs spread, whose backend in the rock projects (`spread/.extension`,
`allocate_lxdvm`) does `lxc launch --vm` and loops on `lxc exec` into it. so the agent's sandbox has to run lxd, and
that lxd has to run both containers and vms.

pats has no way to say so. a suite reaches for three knobs that each say part of it:

```yaml
rock-lxd:
  driver: lxd
  vm: true                    # what the sandbox is
  config:
    security.nesting: "true"  # raw lxd config: a container may run containers
  # and nothing at all for "may run vms"
```

which costs:

- **no check.** a sandbox that cannot run what the task needs (a container without nesting, a vm on a host without
  nested virtualisation) starts fine. the agent finds out mid-task, and the pair scores low for a reason that is not
  the agent's. mason's rock tasks: the pack scores, `lxc launch --vm` fails, the test never starts.
- **nothing recorded.** `metadata.json` does not say what a pair could run, so the same `pats.yaml` scores
  differently on a laptop, armstrong and a ci runner, and nothing says why.
- **mechanism, not intent.** `security.nesting` is meaningless on a vm (lxd refuses it there), and "may run vms" has
  no key at all: a container would need host devices passed in by a profile, with what that costs (below).

## the space

the levels: the host (L0, bare metal or itself a cloud vm), the sandbox (L1, what pats makes), the instances the agent
starts (L2), and what those start (L3, rare).

what the agent wants at L2, against what the sandbox is at L1:

L1 | L2 | works | needs | widening
---|---|---|---|---
container | nothing | yes | nothing | none
container | containers | yes | `security.nesting` | nesting: the container's root may make namespaces and mounts
container | raw qemu | yes | the host's `/dev/kvm` passed in | the host's kvm ioctls
container | lxd vms | yes | the host's `/dev/kvm` and `/dev/vhost-vsock` (the module loaded, the node there) | the host's kvm, and vsock (below)
vm | nothing | yes | nothing | none
vm | containers | yes | nothing: the vm's kernel is its own | none
vm | raw qemu, kvm-accelerated (tcg runs anywhere, slowly) | when the host nests | nothing from pats | none
vm | lxd vms | when the host nests | nothing from pats | none

the container rows that need host devices widen the sandbox past what pats promises:

- `/dev/kvm` in a container is the host kernel's kvm. a bug in it is a host kernel bug reachable from the agent.
- `/dev/vhost-vsock` in a container is the host's vsock: a channel off the bridge, which the egress acl does not see,
  and (before linux's per-netns vsock) not namespaced. it reaches vsock listeners on the host and in other vms, lxd's
  own included. that bypasses the egress policy without any exploit.

a vm sandbox has both of its own: with nested virtualisation on the host, its guest kernel makes `/dev/kvm` and
`/dev/vhost-vsock`, and a vm the nested lxd boots talks vsock to the sandbox vm's kernel, not the host's.

one channel a vm sandbox has whatever `nested` says: its own guest-to-host vsock, which the outer lxd gives every vm
for `lxd-agent`. code in the sandbox can connect to any vsock listener on the host, off the bridge, unseen by the
acl. lxd's own listener authenticates, so it is not an egress path by itself, but any other host service on vsock
would be. this predates `nested` and holds for every `vm: true` pair today. this spec does not close it (an open
question below), and the egress claims below are about the network only.

the other dimensions every row is crossed with:

- **the host.** bare metal gives a vm sandbox's guests two levels (host, sandbox vm, test vm), with
  `kvm_intel.nested=1` / `kvm_amd.nested=1`. a cloud ci runner is already a guest, so the test vm is the third level.
- **egress.** a nested guest's traffic leaves through the sandbox's nic (below).
- **the arch.** amd64 nests with `vmx` / `svm`. arm64 nests only on newer cpus and kernels, and its failures read
  differently.
- **who drives L2.** the agent through the nested lxd's socket (the `lxd` group), through `sudo`, or root in
  `prepare`. the nested lxd runs qemu as root either way.
- **required or not.** a suite that must have it, against one that degrades on purpose.

## proposal

one key for the intent, beside `vm` for what the sandbox is:

```yaml
lxd:
  kind: sandbox
  items:
    rock-lxd:
      driver: lxd
      vm: true
      nested: vms
      provision: provision --rock
```

### values

- **`none`** (the default): no declaration, so no check and no record. it does not mean nesting is off: a profile or
  `config: {security.nesting: ...}` still turns it on, unchecked and unrecorded, as today.
- **`containers`**: the agent may start containers inside the sandbox.
  - on a container sandbox, pats sets `security.nesting=true` itself. a suite drops the raw config key.
  - on a vm sandbox, nothing to set: the vm's kernel is its own.
- **`vms`**: the agent may start vms, and containers, inside the sandbox.
  - on a vm sandbox, pats checks it can (below).
  - on a container sandbox, a load error: `nested: vms needs vm: true: a container would get the host's kvm and vsock
    (docs/spec/nested.md)`. the container rows with host devices are later work, as a loud opt-in (below).

each value takes a long form for a suite that degrades on purpose:

```yaml
nested: {run: vms, required: false}
```

with `required: false` the check still runs and is recorded, and a pair whose sandbox cannot do it runs anyway.

`vm` stays a key of its own. a suite picks `vm: true` for other reasons too (its own kernel, stronger isolation), and
`nested: vms` on a container is clearer as an error than as a silent switch to a vm.

`nested` is a sandbox key, so every pair on the sandbox, and its in-sandbox steps (`prepare`,
`docs/spec/step-sandbox.md`), get the same instance.

### host devices, whatever `nested` says

`checkDevices` (`src/sandbox/lxd_session.go`) refuses extra nics and `raw.*` config today, not char devices, so a
profile can hand a container sandbox the host's `/dev/kvm` or `/dev/vhost-vsock` with `nested: none`, unrecorded.
phase 1 extends it: on a container sandbox, an expanded `unix-char` / `unix-block` device whose `source` or `path`
names `/dev/kvm`, `/dev/vhost-vsock`, `/dev/vhost-net` or `/dev/vsock`, or whose `major` / `minor` keys name one of
the fixed misc nodes (10:232 kvm, 10:238 vhost-net, 10:241 vhost-vsock), fails the start (`/dev/vsock` has a dynamic
minor, so by name only, and it carries no channel of its own: af_vsock sockets need no node), for every `nested` value, naming the later
`host-devices` opt-in. a `gpu` or `usb` device passes as now, since lxd does not map these nodes through them. it
breaks a profile that passes them today, on purpose: that sandbox's egress was never what the record said. pats
reads only the expanded config, often of a remote server, and resolves nothing on the server's filesystem: a node an
admin made under another name and passed by path alone is not caught. profiles are the admin's, so that is a
choice the admin made, not a way round it for the agent. so the check catches the known names and numbers, and the record does not claim more.

### the check for `vms`

`pats nested-probe`, a hidden subcommand, run as root in the sandbox vm after `waitReady` and before the binds' pushes
(the workdir, the mounts), so a failing start costs a boot and no more. its own binary is a single-file push just
before it (below). it checks what the nested lxd will do, as root, since that is who runs qemu:

1. **kvm runs a vcpu.** opens `/dev/kvm`, `KVM_GET_API_VERSION` (12), `KVM_CREATE_VM`, `KVM_CREATE_VCPU`, maps a
   page of guest memory, sets the registers, and runs a few bytes of guest code to a userspace exit, under a
   timeout (a `SIGALRM` breaking `KVM_RUN` with `EINTR`). on amd64, real mode, an `out` of a fixed byte to a fixed
   port: `KVM_EXIT_IO` with that port and byte. on arm64, `KVM_ARM_PREFERRED_TARGET`, `KVM_ARM_VCPU_INIT`, the pc set
   to the code, and a store of a fixed value to an unmapped guest address: `KVM_EXIT_MMIO` with that address and
   value (`wfi` is not used: kvm handles it in the kernel and need not return to userspace). the probe checks the
   exit reason and the payload, not just that `KVM_RUN` returned. a node can exist and make a vm while a vcpu cannot
   run (a cpu model without the virtualisation flag), so running one is the check. the probe runs in the sandbox vm,
   so the vcpu it runs is already a guest of a guest: that is the nested run the nested lxd's qemu will make.
2. **vsock takes a guest.** `modprobe vhost_vsock` when the node is missing (it runs as root, and lxd itself loads
   the module when it starts a vm), opens `/dev/vhost-vsock`, `VHOST_SET_OWNER`, and `VHOST_VSOCK_SET_GUEST_CID` with a
   random cid, as lxd does for each vm, then closes. that is as far as it goes: `VHOST_VSOCK_SET_RUNNING` needs
   virtqueues a real guest sets up, so the probe shows the device takes a guest's cid, not that a transport runs. the
   record names it for that (`vhost_vsock_cid`, "a cid was taken", not a working transport). a cid in use (`EADDRINUSE`) is tried again with another, up to a
   few times: the sandbox vm's kernel is its own, so the only other holder is something in the image. without
   vhost-vsock (or `/dev/vsock`, below) the nested lxd (5.21 and 6.x, whose qemu driver uses vhost-vsock for its
   agent, and the versions this spec covers) reports vms as unsupported and `lxc launch --vm` fails, so a
   probe failure here is caught at start instead of mid-task. a vsock that takes a cid but whose transport does not run
   is the other failure, a booted vm whose `lxc exec` hangs, and the probe cannot see that one (above).
3. **vsock on the sandbox's own side.** `/dev/vsock` exists and opens: lxd checks it before it starts a vm, and listens
   on it for its vms' agents. in a vm sandbox it is the guest side of the vsock the outer lxd gave the vm. that the
   transport works end to end needs no check of its own: every `lxc exec` pats makes into the sandbox vm, the probe's
   included, goes over it to the vm's `lxd-agent`. the node check is for the nested lxd's own start-up test.
4. **vhost-net** is opened and recorded, not required: qemu runs without it, slower.

it does not boot a guest os or start the nested lxd. it is a capability check at start: kvm runs a vcpu and vsock
takes a guest then, not a promise that a later `lxc launch --vm` succeeds or that every image a task launches comes up: a nested image without `lxd-agent` still hangs `lxc exec`,
and that is the task's image. the official lxd images carry it.

**the binary** is chosen by the rules the lxd egress proxy's is (`proxyBinary`, `src/sandbox/lxd_egress.go`):
`PATS_LXD_PROXY_BIN`, else the running binary when it is linux and the server's arch, else the one in the published
`egress-proxy` image, fetched, for the arch the sandbox vm reports (`uname -m` over the exec pats already makes for
`waitReady`), not the server's arch that `serverArch` reads for the proxy today: `proxyBinary` is called with the vm's arch. that is the arch the nested lxd runs qemu for, so the probe checks the arch
that matters. lxd runs vms under kvm for the server's own architecture, so in practice the two agree, and pats's
images have no i686 or armhf personality to differ by. a sandbox that
did not need the proxy binary (open egress, `egress: none`) now fetches it when it sets `nested: vms`. when it cannot
be had (no linux binary here, the registry unreachable), `required` fails the start naming the probe, and `required:
false` records the result as unknown and runs. it goes to `/usr/local/lib/pats/` in the instance and is removed after
the probe, whatever it found: the removal is a root `rm -f` the start runs on every path after the probe's exec,
failed or not. a start cut short before it (a cancel, a lost server) leaves the file in an instance that is deleted
with the pair.

**the probe's protocol.** a dev build falls back to `egress-proxy:latest`, which may predate the probe or speak
another version of it (releases pin the image to their own version). the probe prints one line, `pats-nested-probe
1 <json>`, and pats reads that alone: an unknown subcommand, another version or no such line is "the probe binary
does not match", kept apart from a capability failure. under `required` it fails the start with that message and
`PATS_LXD_PROXY_BIN` as the fix. under `required: false` it is `ok: null`.

**the errors** under `required`, in pats's words, by what failed:

- no `/dev/kvm`: `nested: vms, but the vm has no /dev/kvm: is nested virtualisation on in the lxd server's host
  kernel?`, naming `kvm_intel.nested` / `kvm_amd.nested` on amd64.
- a vcpu does not run: `nested: vms, but kvm in the vm cannot run a vcpu`, naming the cpu flag on amd64 and the
  kernel and cpu generation on arm64.
- vhost-vsock: `nested: vms, but vhost-vsock in the vm cannot take a guest: the nested lxd would refuse to start
  vms`, with the modprobe's or the ioctl's error.
- vsock: `nested: vms, but the vm has no /dev/vsock: the nested lxd refuses to start vms without it (is the virtio
  vsock transport in the image?)`.

**the server.** before the sandbox vm exists, nothing about the host's nesting shows through lxd's api, so the probe
is the check. a server that cannot run vms at all fails `vm: true` at `lxc init`, as now.

**at image build.** `pats init` provisions a `vm: true` sandbox in a builder vm (`build`, `src/sandbox/lxd_image.go`),
and mason's `provision --rock` installs and inits lxd there, which may want nesting. a builder for a sandbox with
`nested: vms` gets the sandbox's `limits.*` and root disk size (today it gets lxd's defaults, 1 cpu and 1 GiB for a
vm), and runs the same probe before the provision script. under `required` a failed probe is an init error with the
probe's message, before any provisioning. the built image records nothing: the pair's own probe still runs at start,
since the image can run on another server.

### the check for `containers`

none in the first cut, and the value promises less than `vms`: on a container it means "pats set `security.nesting`",
on a vm nothing. it is not a promise that every nested workload starts. docker on overlayfs in a container may also
need lxd's syscall interception (`security.syscalls.intercept.mknod`, `.setxattr`), and a nested runtime cannot load
kernel modules the host has not. a suite that needs those sets them in `config:`, as now. the record says what was
asked, not what works. a probe (an unprivileged user namespace and a mount in it) can come when a suite finds a case
it misses.

### egress of nested guests

a nested instance's traffic leaves through the sandbox's nic. pats's checks see the sandbox's devices, not the nested
lxd's. the topology under a policy (`src/sandbox/lxd_egress.go`): the pair's bridge has no host address and routes
nothing, the agent and its proxy sit on it at static addresses, and the bridge acl, both defaults reject, has one
allow rule, from the agent's address to the proxy's port. so this is what holds:

- **open egress** (mason's rock sandbox today): nested guests get out as the sandbox does.
- **an `egress:` policy**: a proxied sandbox has no default route (`waitReady` waits for none, `src/sandbox/lxd.go`).
  a nested lxd's default network (`lxdbr0`, `lxd init --auto`) masquerades its guests to the sandbox's address, and
  the proxy is on the sandbox's own subnet, so a nated guest reaches the proxy with no extra route, and nothing else.
  to use it, a guest needs the proxy env and pats's ca in its trust store (the proxy inspects tls), and the nested lxd
  needs `core.proxy_https` for its image downloads. pats does not reach into nested instances to set any of it. a
  nested network bridged onto the sandbox's nic or a macvlan gives guests addresses of their own, which the acl's one
  rule does not match: they reach nothing, the proxy included. that fails closed, and is unsupported. spread's
  connection from the sandbox to its guest stays inside the sandbox and is untouched. the policy still holds: a
  nested guest has no network way out the sandbox lacks. the readme says that `nested` under a policy is the suite's
  to plumb, and phase 1's e2e covers a nated guest fetching through the proxy.
- **`egress: none`**: no network way out, for anything (the vsock above aside). pats gives the sandbox no nic, and `checkDevices`
  (`src/sandbox/lxd_session.go`) refuses the start, before the instance first boots, when the expanded devices (a profile's included) hold any nic pats
  did not give it, which under `none` is every nic. a nested guest has nothing to leave through.
- the threat model does not change: a nested guest is something the agent made, inside the sandbox, behind its nic.

### metadata

each pair on a sandbox that set `nested` gets, in `metadata.json`:

```json
"nested": {"asked": "vms", "required": true, "ok": true,
           "kvm": "ok", "vhost_vsock_cid": "ok", "vsock": "ok", "vhost_net": "missing"}
```

- `asked`: `containers` or `vms`. `required`: as set.
- `ok`: `true` when every required check passed, `false` when the probe ran to the end and one failed, `null` when the
  probe could not run (no binary, a timeout, the exec failed). the probe runs every check whatever an earlier one
  found, so on a probe that ran each field is a verdict, never `unknown`. absent for `containers`, which has no
  check yet: there the object records the request (`{"asked": "containers", "set": "security.nesting"}` on a
  container, `"set": null` on a vm) and nothing else, and the report shows it as asked, never as available.
- `kvm`, `vhost_vsock_cid`, `vsock`, `vhost_net`: each `ok`, `missing` (no node), `failed` (the node is there and the
  ioctls fail), or `unknown` (only when `ok` is `null`: the probe did not run).
- `reason`: the probe's message when `ok` is not `true`.

the vm's devices are its own kernel's and pats changes nothing in them, so the record holds for the pair, short of a
root step removing a module. it is "at start", and it is a device check: `ok: true` says kvm ran a vcpu and
vhost-vsock took a cid, not that the nested lxd is installed, initialised, reachable by the agent's user, or boots a
guest that answers `lxc exec`. the report words it "nested devices ok", not "nested vms available". a workload smoke
test (launch the guest, wait for its agent, get its address, reach it, delete it on every path) after `prepare` is a
later option, `nested: {run: vms, smoke: true}`, not in phase 1: it costs a guest boot per pair and needs an image.

a start failure is a launch error as now: `runPair` (`internal/eval/pair.go`) keeps a start error as `setupErr` and
writes the pair's metadata itself. the error is a `*sandbox.NestedError` (wrapping `ErrLaunch`) carrying the probe's
result, and that path takes it out with `errors.As` into the `nested` object. a pair that started reads it from the
session (`Session.Nested()`, nil where nothing was asked).

the report marks pairs that ran without what they asked for (`required: false`), as it marks unenforced egress.

### checks at load

- `nested` is `none`, `containers`, `vms`, or the long form with `run` and `required`. anything else is an error.
- `nested: vms` without `vm: true` is an error (above).
- `nested` with `config: {security.nesting: ...}` is an error on a vm (lxd refuses the key there). on a container,
  `nested: containers` or `vms` with `security.nesting: "false"` is an error (they contradict), and with `"true"` a
  warning that `nested` sets it. `nested: none` leaves the raw key alone, whatever it says.
- on drivers other than lxd: the mapping below. until a driver has its mapping, a value other than `none` is an error
  naming the driver.

## other drivers

`nested` is the intent, and each driver maps it to its own mechanism or refuses it. the probe is the same binary,
run in the sandbox as root, and the record is the same object. the pattern: **the key names what the agent may
start; the driver decides how, and says no when its only way is a widening pats does not take by default.**

driver | `containers` | `vms`
---|---|---
lxd, vm | nothing to set | the probe (above)
lxd, container | `security.nesting` | refused, later a host-device opt-in
docker, podman | rootful: `--privileged` or a runtime like sysbox, refused by default. rootless podman in podman works with user namespaces and `/dev/fuse` on a suitable host: a candidate mapping, to check | the host's `/dev/kvm` (and vsock for lxd): refused by default
bwrap | an unprivileged user namespace inside bwrap's own: may work as is, to check | the host's `/dev/kvm` by `--dev-bind`: refused by default
host | nothing: the agent is on the machine | nothing to set. the probe runs as the user pats runs as, not root, so it checks what that user can open (`/dev/kvm` through the `kvm` group, the nested lxd through the `lxd` group), skips `modprobe`, and records a root-only check it cannot make as `unknown`

the host row is what to set, not a sandbox: the host driver's egress is already recorded as not enforced, and
`nested` there records capability only.

the refusals are the same widening seen from different drivers: where the sandbox has no kernel of its own, running
vms means handing the agent the host's kvm, and lxd vms the host's vsock. a later opt-in key covers them all, along
the lines of:

```yaml
nested: {run: vms, host-devices: true}
```

which passes the host's devices in, marks the pair's egress as not enforced when vsock is among them (as the host
driver's is), and says so in the report. not in this spec's phases.

## alternatives

- **`kvm: true` on a vm**: what this spec started as. it names a mechanism, says nothing about containers, and leaves
  `security.nesting` raw.
- **one `instance:` key** (`container`, `vm`, `vm+nested`): mixes what the sandbox is with what it allows, and does
  not grow to nested containers or to other drivers.
- **derive `vm` from `nested: vms`**: a container suite asking for vms would silently become a vm suite, slower and on
  different hosts. an error is clearer.
- **change the rock projects' spread backend to containers**: `lxc launch` without `--vm` needs no kvm, but the
  backend is rockcraft's own template, and the tasks should test the real one.
- **on by default**: a check that fails pairs on hosts without nesting, for suites that never start an instance.

## open questions

- **three levels on a cloud runner.** whether a github-hosted (azure) runner that exposes nesting lets an lxd vm in it
  run a vm of its own. if not, mason's rock suite runs its test step on a self-hosted runner (bare metal, two levels),
  and `required: true` is what tells the suite so, at start.
- **`vhost_vsock` in the images.** whether the ubuntu images lxd uses for `vm: true` ship the module (the probe loads
  it, but only if it is there). to check on 24.04 and 26.04.
- **sizing.** the rock projects' spread backend (`tests/spread/.extension`) asks 4 cpus, 8 GiB and a 20 GiB disk per
  test vm by default, and a pats vm sandbox with no `limits` gets lxd's 1 cpu and 1 GiB. so `rock-lxd` must set
  `limits.cpu`, `limits.memory` and a root disk big enough for the test vm, the nested lxd's images and the pack, and
  the backend's `CPU` / `MEM` / `DISK` may need lowering. two such pairs do not fit a 4-cpu / 16 GiB runner: either
  `max-parallel: 1` there, a bigger runner, or smaller test vms. the numbers come from the spike.
- **the sandbox vm's own vsock.** whether to fence the guest-to-host vsock every vm sandbox has (above), and how,
  without breaking `lxd-agent`. its own spec.
- **arm64.** the probe's vcpu run there, and what a server without nesting support shows.

## phases

0. **spike**, by hand: on armstrong, an lxd vm running `lxc launch --vm` and `lxc exec` into it, and the module
   question. on a github runner with kvm, the same, for three levels. no code.
1. **lxd `nested: vms` on `vm: true`**: the host-device refusal in `checkDevices`, the schema and load checks (the value, the long form, the `vm` and
   `security.nesting` rules, the other drivers' refusals), `pats nested-probe`, its protocol line and its push, the builder's limits and probe, the start error and
   `NestedError`, `metadata.json`, the report mark. fake lxd tests, an e2e on a server with nesting. M.
2. **`nested: containers`** on lxd: `security.nesting` on a container, the record. S.
3. **mason**: `rock-lxd` becomes `vm: true`, `nested: vms`, with the sized `limits` (a mason change). done when a
   rock task's `rockcraft test` passes end to end on the target runner at the chosen parallelism. S.
4. **other drivers**: the table's mappings where they are no widening (host's check, bwrap's userns), as suites need
   them. S each.
5. **`host-devices`**: the opt-in for the refused rows, with its threat model and egress record. its own spec. M.
