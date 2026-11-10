# the bwrap driver

status: implemented.

## purpose and limits

docker, podman and lxd each need a daemon or a server, and the machine pats runs on does not always have one: an
lxd container or vm made for a run, a ci runner with no docker, a laptop where a daemon is more than the job needs.
the bwrap driver runs each sandbox with [bubblewrap](https://github.com/containers/bubblewrap): unprivileged linux
namespaces over the pinned image's filesystem, started by the invoking user, with no daemon and no root at run time.
the agent sees the image, not the host; its egress is enforced the way docker's is, by a sandbox with no route out
but pats's proxy.

it is not for a task that needs containers or vms of its own (spread tests, an lxd or docker inside): a bwrap sandbox
has one uid, no capabilities for the agent, and `no_new_privs`, so nothing nested starts. such a task takes the lxd
driver with `security.nesting`, an lxd vm, or the host driver (`host-driver.md`).

the limits it accepts:

- **one uid.** an unprivileged user namespace maps one id: the user pats runs as. inside, every file of the rootfs
  belongs to whoever the current exec runs as: root for `prepare`, the agent's uid for the agent. so the agent owns
  the image's files and may change any of them (in its own overlay, never the stored rootfs), `chown` to any other
  id fails, and a package whose install chowns to a system user does not install.
- **no setuid, no sudo.** bwrap sets `no_new_privs`, so `sudo` and every setuid binary fail inside. `sudo: true` is a
  load error.
- **the host's kernel.** namespaces share the kernel, as a rootless container does; a kernel bug reachable from a
  user namespace reaches the host. a vm is the stronger boundary.
- **nothing outlives an exec.** each exec is its own bwrap with its own pid namespace (below): a daemon that `prepare`
  starts is gone when `prepare` ends, unlike in a container, where it keeps running into the agent's exec.
- **no resource limits** in v1 (below, "later").
- **gzip or plain layers.** a layer compressed with zstd is refused at pull: go's standard library has no zstd, and
  pats takes no new dependency for it.
- **the proxy is a child of pats**, as under the host driver: on linux the kernel ends it when pats dies.

## platform and preflight

linux only. it needs:

- `bwrap` 0.10 or later on `PATH`, for `--overlay`.
- unprivileged user namespaces allowed for `bwrap`, and unprivileged overlayfs (linux 5.11 or later).

preflight checks the first: `bwrap --version`, and a missing or older bwrap is a start error naming the version found
and the one needed. pats does not work around a host that falls short: the rest shows as bwrap's own error on the
first exec, which pats passes on with the sandbox's name. on macos, or any os but linux, a selected pair on a bwrap
sandbox is a start error; `pats.yaml` itself loads anywhere, so one config serves a mac with docker and a linux runner
with bwrap.

where it runs, as checked (ubuntu cloud images on lxd, bubblewrap from the archive), for the docs:

| pats runs in | bwrap |
|---|---|
| ubuntu 26.04 vm (bwrap 0.11.1) | works as shipped: apparmor's `bwrap-userns-restrict` profile lets it make namespaces |
| ubuntu 24.04 vm (bwrap 0.9.0) | no: no `--overlay`, and apparmor refuses the user namespace until the sysctl is lifted |
| an lxd container (26.04 in a 26.04 vm) | works with `security.nesting=true`; without it a fresh `/proc` cannot be mounted |
| an lxd container (24.04) | namespaces work with nesting, but its bwrap 0.9.0 has no `--overlay` |
| a docker container | only with `--security-opt seccomp=unconfined`, `apparmor=unconfined` and `systempaths=unconfined`, and the pats cache on a volume: overlayfs refuses an upper or lower dir on the container's own overlay root, even under `--privileged` |

## configuration

```yaml
sandboxes:
  kind: sandbox
  items:
    light:
      driver: bwrap
      image: ghcr.io/lczyk/pats/sandbox:26.04   # optional: the pats sandbox image by default, as under docker
      egress:
        allow:
          - .ubuntu.com
```

- `image`, `egress` (a mapping or `none`), `env` and `max-parallel` are allowed; a task's `prepare` runs as root (in
  the namespace, above).
- keys bwrap cannot honour are load errors, each naming the key: `build`, `base`, `provision`, `limits`, `sudo`
  (sandbox or task), `vm`, `remote`, `profiles`, `config`, `egress.image`.

## images

an image is a registry ref, as under docker, and pinned the same way: `pats init` resolves it to a digest and records
the pin under the target `bwrap:<hostname>`. there is no daemon to hold it, so pats does:

- **fetch** the manifest for the host's architecture and its layers over the registry's http api, extending the
  client the lxd driver already fetches its proxy binary with (`registry.go`), with no new dependency. anonymous
  pulls only, as that client does: a public image.
- **unpack** the layers in order into the store, `<cache>/bwrap/rootfs/<digest>/`, as the invoking user: whiteouts
  and opaque dirs applied, the image config's `Env` kept beside it (`<digest>.env.json`), device nodes skipped
  (bwrap supplies `/dev`), setuid and setgid bits dropped, ownership not kept (one uid, above), and an entry whose
  path or link target leaves the rootfs refused. every entry is written through an `os.Root` of the rootfs: one that
  reaches past a symlink a lower layer left (a merged `/usr`'s `bin`) lands where that leads inside the rootfs, and
  one past a link that leaves it, or an absolute one, is refused. it unpacks into
  `<digest>.new` and renames when complete, holding `<digest>.lock` exclusively from the first byte to the rename:
  a second init waits and then finds the rootfs there, and clean removes a `.new` only when it gets that lock without
  waiting, so it never takes an unpack in progress, only a crashed one's.
- **stamps**: init runs `stampScript` (`stamps.go`) once in a throwaway session of the new rootfs, as docker's
  `Stamps` does in a throwaway container, and stores the harness versions beside it as `<digest>.json`; `Stamps`
  reads that file, so metadata's `components` and the pin drift warnings work as under docker.
- **in use**: every session holds a shared `flock` on `<digest>.lock` while it runs; `RemoveImage` takes it
  exclusively without waiting and refuses an image in use, as docker refuses removing a running container's image.
  a lock ends with its process, so a crash never leaves an image stuck.

on the `Driver` interface (`sandbox-seams.md`): `Resolve`, `Images`, `Pull`, `Stamps` and `RemoveImage` work on the
store; `Upstream` is `RegistryDigest`; `Build` is unsupported in v1; `Runs` lists nothing, since nothing of a run
outlives pats but files (below, "clean"). there is no proxy image: the proxy runs on the host, so init pins none for
a bwrap sandbox and `usePins` asks for none, as the host driver's `init` makes nothing. `ProxyImage` instead makes
the forwarder's binary present (below, "egress"), so a run never fetches.

## sessions

- **start** holds the process's lease as every driver does, takes the image's lock, makes the session dir under the
  pats cache (`MkTemp`: `upper/`, `work/`, `egress/`), and with `egress` starts the proxy (below).
- **exec** runs one `bwrap` per call, over the same rootfs and upper dir, so what one exec writes the next sees:
  - namespaces: user, mount, pid, ipc, uts, and net unless the sandbox is open; `--die-with-parent`, `--new-session`.
  - the filesystem: `--overlay-src <rootfs> --overlay upper work /`, a fresh `/proc`, bwrap's minimal `/dev`, no
    `/sys`; the workdir bound read-write at `/workspace`, each of the spec's mounts bound as asked, and the
    session's pushed files (below).
  - who: `Root` runs as uid 0 with the namespace's capabilities (`--uid 0 --gid 0 --cap-add ALL`); otherwise the
    agent runs as the host uid and gid, as under docker, with none.
  - the env: `--clearenv`, then the image config's `Env` (kept with the rootfs at unpack, so a `PATH` an image sets
    holds, as under docker) with pats's env over it goes in on stdin, and a shell reads it before the command runs
    (`envScript`, `envArgv`), as for a docker exec: a value never sits in bwrap's argv, which any local process can
    read.
  - for `prepare`, apt is told to stay root (`APT_CONFIG` names a pushed file setting `APT::Sandbox::User "root"`):
    apt's fetchers drop to `_apt`, a uid the namespace does not have, and fail otherwise.
  - one exec at a time per session: two overlays may not share an upper dir. the session serialises its execs; pats
    runs them one after another today.
  - cancel: bwrap is killed. it is pid 1 of the sandbox's pid namespace, so everything inside ends with it, and
    `--die-with-parent` ends it if pats dies. a context cancelled before bwrap started is `ErrLaunch`; one that killed
    it returns the killed result (exit -1, no error), and a bwrap that exited clean all the same reports the cancel,
    as the other drivers do.
- **push** is a root exec that binds the host path read-only at a staging path and copies it to its path inside
  (`cp -a`), so what lands is an ordinary file of the overlay: `prepare` removes its material afterwards
  (`rm -rf /pats-prepare`), which a bind would refuse. **pull** of
  a path under a bind copies from the bind's host side, and of any other path is an error (only lxd's sessions pull
  today). **handover** and **sync** are no-ops: ownership already follows the one uid, and every writable path is
  bound.
- **close** kills a running exec, stops the proxy, releases the image's lock, and removes the session dir, making it
  writable first: the kernel leaves overlay's `work/work` mode 000. safe to call twice.

## egress

- **a policy** (`egress` as a mapping): the sandbox has its own net namespace, loopback only, no route and no
  resolver. pats starts `pats egress run` on the host as a child process, listening on a unix socket in the session's
  `egress/` dir (a new `--listen unix:<path>` form), and waits for its `listening` line, as the host driver does. the
  dir is bound into the sandbox at `/run/pats-egress`, and each exec's command runs under `pats egress forward`,
  which listens on `127.0.0.1:8080` inside, relays each connection to the socket, runs the command as its child and
  exits with its status. the agent gets the usual contract: `HTTP(S)_PROXY` at `127.0.0.1:8080`, the trust bundle at
  `/sbx-ca/bundle.pem`, the trust env, the policy merged with the harness's hosts.
- **the forwarder's binary** is a static linux pats, bound read-only into the sandbox: the running pats when it is
  one (no ELF interpreter), else the published proxy image's for this version, which `pats init` fetches as lxd
  fetches its proxy binary (`proxyBinary`) and keeps in the store, `<cache>/bwrap/pats-<version>-<arch>`. a run
  never fetches it: a missing one is a start error saying to run `pats init`, so an offline run after init works. an
  image need not carry anything of pats's.
- **fail-closed** by construction: the namespace has no interface but loopback. the self-test still runs, the same
  probe as under every driver.
- the agent can end the forwarder (it is in the agent's pid namespace and runs as the agent's uid); that only loses
  its egress.
- **`egress: none`**: the net namespace and nothing in it.
- **open** (no `egress`): the host's network namespace, with the host's `/etc/resolv.conf` bound over the image's.
  that includes the host's loopback: a service on the host's `127.0.0.1` is reachable from an open sandbox, as it is
  not from a container on a bridge. a limit the docs state; a policy closes it.
- **chained egress** (`chained-egress.md`) works unchanged: the proxy runs on the host, so even a loopback upstream
  is reachable, and an open sandbox shares the host's network and gets the machine's proxy env.

## records and the cli

- `metadata.json` records `driver: bwrap` (`host-driver.md` adds `driver` on every pair), and `egress.enforced:
  true`.
- `pats init` fetches and unpacks, printing progress per layer, and writes the pin. `pats clean` sweeps session dirs
  (with their leases, as for every temp dir, making them writable before removing them) and `.new` rootfs dirs left
  by a crash; init removes rootfs no pin names, unless in use.

## later

- **provision and base**: `provision` run as root over the base's rootfs, its upper dir kept as a layer, sessions
  stacking it as a second lower. `build:` dockerfiles stay docker's.
- **limits**: cpu and memory through `systemd-run --user --scope` where a user manager delegates the controllers.
- **more than one uid**: `newuidmap` and a subuid range, so `chown` and system users work. needs the setuid helper
  and a range per user.
- **private registries**: credentials for the registry client.

## not doing

- containers, vms or a docker daemon inside a bwrap sandbox: the lxd driver or the host driver.
- macos: no user namespaces.
- `build:` dockerfiles.

## tests

- argv, on a fake runner: root and agent execs; open, `none` and policy; no env value in argv; the forwarder wrapping
  the command under a policy.
- unpack, on crafted layers: whiteouts and opaque dirs, hardlinks, a symlink pointing out of the rootfs and a file
  written through it refused, a file written past a link that stays inside landing there, `../` entries refused,
  device nodes skipped, setuid dropped, a crash mid-unpack leaving only `.new`.
- store: two pulls of one digest converge; removing an image a session holds is refused; a crashed holder releases.
- sessions, on linux ci with bwrap: what `prepare` writes as root the agent reads; a background child dies with its
  exec; ctrl-c kills the exec; close removes the session dir; the self-test passes under a policy and a direct dial
  fails; a request through the forwarder reaches the proxy and leaves an audit line.
- preflight: a missing bwrap and an older one are start errors naming the version needed.
