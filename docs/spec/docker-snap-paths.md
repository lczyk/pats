# docker snap paths

status: implemented (`src/sandbox/cliroot.go`).

## problem

a pats not packed as a snap (`go install`, a clone's `make install`) cannot run a docker sandbox when the host's
docker is the docker snap and its paths are the default ones. found testing 1.0.0 on ubuntu 26.04 arm64 with the
docker snap 29.8.0. the pats snap is not affected (below).

the docker snap confines its cli, `/snap/bin/docker`, and not only its daemon, and the two see different things:

- **the cli** has the snap's own `/tmp` and its `home` plug. that reaches the home, less the hidden entries at its
  top: `~/.cache` is out, `~/proj/.build` is in. it reaches nothing outside the home (`/srv`), and no other snap's
  `~/snap/<name>`. of what it reaches it opens only the user's own files: a root-owned file in a dir of the user's is
  refused, though the user may read it.
- **the daemon** sees the whole home, hidden entries included: a bind mount of a path under `~/.cache` works. a bind
  under `/tmp` mounts an empty dir, and one under `/srv` is refused.

which of the two opens a path depends on the operation. a bind mount is a string the daemon resolves. a cidfile, an
iidfile, a build and a `docker cp` are the cli's own reads and writes. so what breaks is every path the cli opens
itself:

- `docker run --cidfile /tmp/...`: `failed to create the container ID file: open /tmp/pats-cid-.../cid: no such file
  or directory`. the start of an agent's sandbox and of a step sandbox passes one (`src/sandbox/sandbox.go`, from
  `os.MkdirTemp("", ...)`), so no pair runs. the egress proxy's own `docker run` passes none.
- `docker build`: the cli reads the context, the dockerfile pats generates for a `base:` / `provision:` sandbox
  (`-f`, in a `pats-build-` dir), each placed harness cli's tree (`--build-context`, a `pats-harness-` dir) and writes
  the `--iidfile` (a `pats-iid-` dir). all of them are under `~/.cache/pats/tmp`: `failed to read dockerfile: open
  Dockerfile: no such file or directory`, or, with a context in the project, `writing image ID file: ... permission
  denied`.
- `docker cp <host path> ...` from the temp root (a step's script, a `prepare`'s material): a file fails with `Can't
  add file ... permission denied`. a directory's contents (`dir/.`, what `Push` asks for a directory) copy nothing and
  exit 0, with one line on stderr, so the first sign is the script that is not there.
- `docker cp <sandbox>:... <host dir>` would fail by the same rule. nothing on the docker driver asks for it today.

with `TMPDIR` and `XDG_CACHE_HOME` both moved to non-hidden dirs under the home, the same build runs the smoke
project's docker pair end to end, so these are the only blockers on the docker side.

the comment on `MkTemp` (`src/sandbox/tmp.go`) and `docs/spec/cache-dir.md` say a confined daemon sees `$HOME`, so
a temp root under the cache is safe. that holds for the daemon's bind mounts. resolved: both now say the cli
sees less, and `tmp.go` names `MkTempFor` for the dirs it opens. `cache-dir.md` points here.

the pats snap is not affected: it ships its own docker client (`snap/snapcraft.yaml`), which runs in the pats snap's
mount namespace and talks to the docker snap's daemon over the socket, so the docker snap's cli is never in the path.
hence `hack/snap_smoke.sh docker` passes with the docker snap. ci does not catch this case, since its runners have
docker from the deb and no job runs a non-snap pats against the docker snap.

## proposal

- **a cli root.** every dir pats makes for the docker cli to open goes under one root, the cli root: the cidfile's,
  the iidfile's, the generated dockerfile's, a placed harness cli's tree, a `provision` or `prepare`'s material, and
  a step sandbox's staging dir, where its scripts are copied from. a bind mount's source stays in the temp root: the
  daemon sees it there.
- **which root.** when the docker cli is the docker snap, the cli root is
  `~/snap/docker/common/pats-tmp/<client-id>`. else it is the temp root as today (`<cache>/tmp`), so nothing moves for
  apt docker, docker desktop, podman or the pats snap (whose `docker` is its own client, never `/snap/bin/docker`).
- **why that dir.** it is the docker snap's own `$SNAP_USER_COMMON`: its cli reads and writes it, and pats, not
  confined, reads what the cli writes there (the cidfile, the iidfile). pats makes `pats-tmp/<client-id>` itself,
  mode 0700, whether or not the user has run the snap's docker before.
- **by the sandbox's driver.** the cli root is the docker driver's. a dir made for a sandbox of another driver (lxd,
  podman, bwrap, the host) is in the temp root whatever `docker` is on `PATH`. `MkTemp` keeps its meaning, and the
  six places that make a dir the cli opens ask for one by driver: four are in the container driver, which is podman's
  too and knows which of the two it runs, and the two in `internal/eval` (the material, a step sandbox's staging)
  know their sandbox.
- **the client id** (`src/sandbox/clientid.go`, one per cache) is in the path because the leases stay in the cache.
  a temp dir's owner file names a run, and `pats clean` calls the run dead when its own cache has no lease for it.
  two caches in one dir (a run under another `XDG_CACHE_HOME`, the test suite) would each take the other's live
  dirs.
- **detecting the docker snap.** the `docker` on `PATH` that pats runs is a link to `/usr/bin/snap`, or sits in
  `/snap/bin`, itself or through links to one that does (a `/usr/local/bin/docker` pointing at the snap's). the walk
  ends at `/usr/bin/snap` and does not resolve it: on ubuntu 26.04 that is itself a link to `/usr/lib/snapd/snapd`.
  detected once per process.
- **it is about the cli, not the daemon.** the probe that leaves `no-new-privileges` off (`confined`,
  `src/sandbox/engines.go`) asks the daemon, and the two answers differ: the pats snap's client with the docker
  snap's daemon, or the docker snap's cli with a `DOCKER_HOST` elsewhere.
- **a wrapper around the snap's docker** (a script, not a link) is not detected. `XDG_CACHE_HOME` on a non-hidden
  dir under the home is then enough, since the cidfile is under the cli root too (next), which is then the temp
  root.
- **the cidfile** moves off `os.MkdirTemp("")` into a dir of the cli root, made as `MkTemp` makes one and removed
  with `RemoveTemp`, so it carries an owner file and `pats clean` sweeps it like any other temp dir. where the docker
  snap is not the cli, that moves it from `$TMPDIR` to the temp root.
- **`pats clean`** sweeps the temp root as today, and `~/snap/docker/common/pats-tmp/<client-id>` whenever it
  exists, whether or not the docker snap is detected now: a `PATH` that changed, or a docker snap swapped for the
  deb, leaves dirs there. same rules, `-a` and `-f`. it reads the temp dirs of both before it sweeps the leases,
  since a lease stays while a temp dir names it. it makes neither dir. a dir it may not look at is one that is not
  there: the pats snap reaches no other snap's `~/snap/<name>`, so its clean gets a permission error for the path,
  not a missing file, and says nothing. it has nothing there in any case, with a cache and so a client id of its
  own.
- **a link in the temp root.** `<cache>/tmp/docker-snap` is a link to the cli root, made when pats first makes the
  cli root and left there. it is for someone who looks for a run's dirs in the usual place and finds half of them.
  pats hands the cli the real path, never the link: the cli cannot pass through `~/.cache`. `pats clean` sweeps dirs
  only, so it leaves the link, dangling or not.
- **what the cli reads has to be the user's own.** pats writes what it stages as the user. a dir a sandbox's root
  wrote into (a workdir, a bind) is never a `docker cp` source or a build context on docker, and has to stay so.
- **the docs.** the `MkTemp` comment and `cache-dir.md` say what each of the two sees, as above. `snap.md` is right
  as written.
- **a project's build context.** a `build:` context in the project is passed to the cli as is, and pats does not
  copy it elsewhere. init names the cause when the cli is the docker snap and the context, its links resolved, is
  not under the home or is under a hidden dir at the home's top. the same words as `snap.md` has for a project the
  pats snap cannot reach.

## checked

on ubuntu 26.04 amd64, the docker snap 29.8.0, a user in the `docker` group who had never run docker:

- **the dir.** made by hand before the first `docker` run. the snap made `~/snap/docker/current` and its revision
  dir beside it and took no notice.
- **the cli, under it.** a cidfile, `docker cp` in (a file, a dir's contents) and out, and a build with its context,
  `-f`, `--iidfile` and a `--build-context` all there.
- **the daemon, under it.** binds work there too (read, written as the user and as root, a file bound read-only),
  which the proposal does not need.
- **the limits.** the cli refused a cidfile under `/tmp` and `~/.cache`, a context under `~/.cache`, `~/.work`,
  `~/snap/other`, `/srv` and `/tmp`, and a root-owned file under the dir. it took a context under `~/proj/.build`.
- **pats.** 1.0.0 as installed failed `pats init` on the iidfile. with `XDG_CACHE_HOME` and `TMPDIR` both under
  `~/snap/docker/common`, so that every dir the cli opens was there, the smoke project's docker suite passed: init of
  a `build:` sandbox and of one with a placed cli, the pair, its steps in a step sandbox, the scorer, and `pats clean
  -n` with nothing left.

on ubuntu 26.04 arm64, the same docker snap, by hand in a dir under `~/snap/docker/common`: a build with its
context and `--iidfile`, `docker cp` of a file in and out, a cidfile and a bind all worked. 1.0.0 as installed
failed a forced `pats init` on the iidfile there too. an init that finds its image built already (by the pats snap,
here) builds nothing, and the first failure is then the cidfile at the run.

as implemented, on the arm64 box: the detection finds the docker snap, and a clone's `bin/pats` with the default
cache runs the smoke project's docker suite (a forced init of a `build:` sandbox and of one with a placed cli, the
pair, its steps in a step sandbox, the scorer, `pats clean -n` with nothing left), its bind sources in the cache.
`make test` passes there with `TMPDIR` under the home.

two things the implementation adds. the client's dir goes once its last temp dir does, so a run leaves an empty
`pats-tmp` and no more, and the link in the temp root dangles until the next run. and `hack/snap_smoke.sh` takes
`PAIR_ONLY=1`, the entry that skips its checks of a reduced build.

not checked: the ci job, which has not run yet.

## alternatives

- **the whole temp root moves** when the docker cli is the docker snap. one switch, no call site choosing, and it
  works (checked, above). but it is per process, not per driver: an lxd or podman run on a box that has the docker
  snap would keep its workdirs in the docker snap's data, which `snap remove docker` takes.
- **a non-hidden root for every install** (`~/pats/tmp`). simpler, no detection, but a new dir in every user's home
  for the sake of one docker packaging.
- **document `XDG_CACHE_HOME` / `TMPDIR` only.** no code, but the failure names neither variable, so a user meets
  `failed to create the container ID file` and has to guess.
- **no paths at all: stdin.** `docker cp` takes a tar on stdin, and the lxd driver has the tar code. a dockerfile
  goes on stdin too (`-f -`). a `--build-context` has no such form, so the builds still need a dir the cli reads,
  and docker and podman would have a second way to copy.
- **a name for the cidfile.** a container named before `docker run` can be removed by name, with no file. it saves
  one path of the four kinds, and the root is needed for the others.

## not doing

- **no copy of a project's build context** out of a dir the cli cannot reach. a build context can be large, and the
  error is clear.
- **no setting for the root.** `XDG_CACHE_HOME` is the one there is.
- **no change to the pats snap.**

## tests

- detection: a `docker` that is a link to `/usr/bin/snap` (in `/snap/bin`, and in `/var/lib/snapd/snap/bin`), one in
  `/snap/bin` that is no link, a link of the user's to either, and an apt one, with a fake `PATH`.
- the cli root is `~/snap/docker/common/pats-tmp/<client-id>` for the docker snap and the temp root otherwise. the
  cidfile, the iidfile, the generated dockerfile, a placed cli's tree, the material and a pushed step script are
  under it, and a bind's source is not (a fake runner records the args). a start leaves no owner file behind. a
  podman or lxd sandbox's dirs are in the temp root with the docker snap on `PATH`.
- `pats clean` sweeps a dead run's dir from both roots and keeps a live one's; it sweeps the docker snap's root with
  no docker snap on `PATH`; it leaves another client id's dir alone; a dry run makes no dir; a root it may not read is
  passed over with no warning.
- making the cli root leaves the link `docker-snap` in the temp root, naming it; a second run leaves the link as it
  is; `pats clean` does not take it; with no docker snap there is no link.
- init names a build context under `/srv`, `~/.work` and `~/snap/other` when the cli is the docker snap, and not one
  under `~/proj/.build`.
- ci: one job installs the docker snap, checks that `docker` is the snap's and that `docker info` answers (a docker
  test skips when there is none), then runs `make test` with `TMPDIR` on a non-hidden dir under the home (the tests
  bind and build from `t.TempDir()`) and the smoke project's docker pair with the clone's `bin/pats`.
  `hack/snap_smoke.sh` refuses a pats without the snap's build tags, so the pair needs an entry that skips those
  checks.

## test-only fixes found alongside

these failed on the same box. none is a product bug:

- **`TestStampScript`** puts `/usr/bin:/bin` on `PATH`, so a host `copilot` (here the vscode insiders shim, which
  exits 127) makes the stamp script exit 3. the test wants a `PATH` of its own dir alone: the script uses the
  shell's builtins, and the test runs `sh` by its path.
- **`TestContainerRunMountsAndEnv`** wants `NoNewPrivs: 1`. on the docker snap pats leaves `no-new-privileges` off on
  purpose (`noNewPrivs`, `snap.md`), so the test asks the same probe and wants `0` there.
- **the escape tests** (`TestUnpackRefusesEscapes`, `TestUnpackThroughLinks`, `TestCopyTreeStaysInside`,
  `TestReadTarSafety`) fail on go 1.26.3, where `os.Root.MkdirAll` on a link out of the root says `file exists`, not
  `path escapes from parent`. nothing is written outside, only the error differs. they pass on 1.26.11. either `go.mod`
  asks for 1.26.11 or the tests take the `EEXIST` from a link too: `refused`, and the two asserts in
  `TestCopyTreeStaysInside` that name the error themselves.
