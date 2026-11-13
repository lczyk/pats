# docker snap paths

status: proposed.

## problem

a pats not packed as a snap (`go install`, a clone's `make install`) cannot run a docker sandbox when the host's
docker is the docker snap. found testing 1.0.0 on ubuntu 26.04 arm64 with the docker snap 29.8.0. the pats snap is
not affected (below).

the docker snap confines its cli, `/snap/bin/docker`, and not only its daemon. the cli has the snap's own `/tmp` and
its `home` plug, which reaches the home dir but nothing hidden in it. the daemon sees more: a bind mount of a path
under `~/.cache` works. so what breaks is every path the cli opens itself, on this box:

- `docker run --cidfile /tmp/...`: `failed to create the container ID file: open /tmp/pats-cid-.../cid: no such file
  or directory`. every `docker run` pats makes passes one (`src/sandbox/sandbox.go`, from `os.MkdirTemp("", ...)`), so
  nothing starts: not the agent's sandbox, not a step sandbox, not the egress proxy.
- `docker cp <host path> ...` from the temp root under `~/.cache/pats/tmp` (a step's script, a push into a
  sandbox): `Can't add file ... permission denied`.
- `docker build <context>` with the context under `~/.cache/pats/tmp` (`src/sandbox/build.go`): `failed to read
  dockerfile: open Dockerfile: no such file or directory`. the `--iidfile` beside it has the same problem.
- `docker cp <sandbox>:... <host dir>` into the temp root, by the same rule.

with `TMPDIR` and `XDG_CACHE_HOME` both moved to non-hidden dirs under the home, the same build runs the smoke
project's docker pair end to end, so these are the only blockers on the docker side.

the comment on `MkTemp` (`src/sandbox/tmp.go`) and `docs/spec/cache-dir.md` say the docker snap sees `$HOME`, so a
temp root under the cache is safe. that holds for the daemon's bind mounts and not for the cli.

the pats snap is not affected: it ships its own docker client (`snap/snapcraft.yaml`), which runs in the pats snap's
mount namespace and talks to the docker snap's daemon over the socket, so the docker snap's cli is never in the path.
hence `hack/snap_smoke.sh docker` passes with the docker snap. ci does not catch this case, since its runners have docker from the deb and no
job runs a non-snap pats against the docker snap.

## proposal

- **one docker scratch root.** every path pats hands the docker cli to open goes under one root, the cli root: the
  cidfile, the iidfile, a build context pats stages, and the host side of a `docker cp` either way. bind mounts keep
  their paths: the daemon sees them.
- **which root.** when the docker cli is the docker snap, the cli root is `~/snap/docker/common/pats-tmp`: the
  docker snap's own `$SNAP_USER_COMMON`, which its cli always reads and writes, under a dir pats owns. else it is the
  temp root as today (`<cache>/pats/tmp`), so nothing moves for apt docker, docker desktop, podman or the pats snap
  (whose `docker` is its own client, never `/snap/bin/docker`).
- **detecting the docker snap.** the `docker` on `PATH` that pats runs resolves (symlinks followed) to
  `/usr/bin/snap`, or sits in `/snap/bin`. detected once per process. a `DOCKER_HOST` pointing at a remote daemon
  changes nothing here: the cli is still the snap's.
- **the cidfile** moves off `os.MkdirTemp("")` into a `MkTemp` dir under the cli root, so it carries an owner file and
  `pats clean` sweeps it like any other temp dir.
- **`pats clean`** runs `CleanTemp` over the cli root too, when it is not the temp root, with the same rules, `-a`
  and `-f`.
- **the docs.** the `MkTemp` comment and `cache-dir.md` stop saying the docker snap sees all of `$HOME`. they say the
  daemon sees the home and the cli sees only non-hidden paths there. `snap.md` is right as written.
- **a project under a hidden dir.** a `build:` context in the project is passed to the cli as is. under a hidden dir
  (`~/.local/src/x`) or `/tmp`, with the docker snap, the build fails as above. pats does not copy it elsewhere. init
  names the cause when the cli is the docker snap and the context path has a hidden component or is under `/tmp`.

## alternatives

- **a non-hidden root for every install** (`~/pats/tmp`). simpler, no detection, but a new dir in every user's home
  for the sake of one docker packaging.
- **document `XDG_CACHE_HOME` / `TMPDIR` only.** no code, but the failure names neither variable, so a user meets
  `failed to create the container ID file` and has to guess.

## not doing

- **no copy of a project's build context** out of a hidden dir. a build context can be large, and the error is clear.
- **no change to bind mounts.** the daemon sees them where they are.
- **no change to the pats snap.**

## tests

- detection: a `docker` that resolves to `/usr/bin/snap`, one in `/snap/bin`, and an apt one, with a fake `PATH`.
- the cli root is `~/snap/docker/common/pats-tmp` for the docker snap and the temp root otherwise; the cidfile, the
  iidfile, a staged build context and both `docker cp` directions land under it (a fake runner records the args).
- `pats clean` sweeps a dead run's dir from the cli root and keeps a live one's; with the docker snap absent it looks
  at the temp root only.
- init names a hidden or `/tmp` build context when the cli is the docker snap.
- ci: one job installs the docker snap and runs `make test` and the smoke project's docker pair with the clone's
  `bin/pats`.

## test-only fixes found alongside

these failed on the same box. none is a product bug:

- **`TestStampScript`** puts `/usr/bin:/bin` on `PATH`, so a host `copilot` (here the vscode insiders shim, which
  exits 127) makes the stamp script exit 3. the test wants a `PATH` with only its own dir and `sh`'s.
- **`TestContainerRunMountsAndEnv`** wants `NoNewPrivs: 1`. on the docker snap pats leaves `no-new-privileges` off on
  purpose (`noNewPrivs`, `snap.md`), so the test asks the same probe and wants `0` there.
- **the escape tests** (`TestUnpackRefusesEscapes`, `TestUnpackThroughLinks`, `TestCopyTreeStaysInside`,
  `TestReadTarSafety`) fail on go 1.26.3, where `os.Root.MkdirAll` on a link out of the root says `file exists`, not
  `path escapes from parent`. nothing is written outside, only the error differs. they pass on 1.26.11. either `go.mod`
  asks for 1.26.11 or `refused` takes the `EEXIST` from a link too.
