# sandbox drivers: one way to put them together

status: implemented for docker, podman and lxd: the rule, the shared pieces, the timings, and the tests (shared
fakes, a state-tracking fake per daemon, the cross-driver tables, the sweeps). the shape every sandbox driver
(docker, podman, lxd; bwrap and nerdctl to come) is built in, so each one is tested the same way and without its
daemon. `lxd-testing.md` is the lxd plan this grew out of; its seams 1 to 3a are this design applied to lxd, and its
fake server and fault sweep (seams 4 and 5) sit on top of it.

## the rule

a driver is built from three values and touches nothing else:

- **a process**: what one pats process holds across its sandboxes. its run token and owner stamp (lxd's
  `user.pats.owner`, docker's `pats.owner` label), a clock, the timings it waits by, the egress subnet counter, and
  per-server state a driver sets up once, in a field of that driver's own (`process.lxd`: each server's setup and
  lease renewal, its proxy image). production uses one, `defaultProcess`; a test makes its own, and two of them are
  two pats processes.
- **a runner** for the driver's cli: `Run(ctx, stdin, stdout, stderr, args...)` to completion, or `Start(stdin,
  stdout, stderr, args...)` for a `proc` the caller waits on and signals. production wraps `os/exec` around one binary
  (`lxc`, `docker`, `podman`); a test passes a function.
- **Options**: what the caller wants. the environment reaches a driver only through it: `Options.WithEnv()` fills
  `EgressPool`, `Uplink` and `ProxyBin` from `PATS_EGRESS_POOL`, `PATS_LXD_UPLINK` and `PATS_LXD_PROXY_BIN`, and
  `internal/eval` calls it where it builds a driver.

no driver reads its configuration from the environment, keeps state in a package global, waits on the wall clock or
runs `exec.Command`. what stays outside the three, on purpose:

- the host's identity: `os.Getuid` and `os.Getgid` (docker runs the agent as the host user), the hostname in the
  owner stamp. tests compute the same values.
- host-side files: the temp root (`MkTemp`, under the user's cache dir), the mitm ca's material, staged provisioning
  material, the build context a hash reads, docker's `--cidfile` (written by the cli, read by the driver: a fake cli
  writes it as the real one does), `os.Executable` for the proxy binary.
- the clock of record: the mitm ca's validity, which is about the real world.
- the registry: `fetchImageFile` (lxd's proxy binary) uses `http.DefaultClient`. an injected client and base url is
  `lxd-testing.md` seam 7, and applies to any driver that fetches from a registry.

`New(o)` builds the production driver from `defaultProcess`, the cli's runner and `o`. the unexported constructors
(`newLXD(p, r, o)`, `newContainer(p, r, o)`) are what tests use.

## the shared pieces

the patterns every cli driver repeats live once, on the runner and the process:

- **`combined(ctx, r, args)`**: run to completion, stdout and stderr together, trimmed. most calls.
- **`output(ctx, r, args)`**: run to completion, stdout alone, stderr in the error. for reading data (a file out of
  an image, an image id).
- **`p.execCancellable(ctx, r, stdout, stderr, cancel, args)`**: an exec whose context may end first. then `cancel`
  runs (what stops the work inside: lxd kills the agent's processes or stops the instance, docker kills the
  container, since ending the cli client leaves the process inside running), the client gets sigterm, and sigkill
  after `execGrace`. a process that finished as the context ended keeps its result. `exitResult` turns the error into
  an exit code: anything with `ExitCode() int` (an `*exec.ExitError`, or a fake's) is the command's result, anything
  else is the exec failing.
- **`follow(r, stdout, args)`**: a long-running process whose stdout goes to the caller and whose stderr carries a
  `listening ` line: lxd's egress proxy, docker's `logs -f` follower of its proxy sidecar. `ready` waits for the
  line up to a timeout on the clock; `stop` gives it a grace to exit on its own, then kills it.
- **the egress material**, already shared and the largest piece: `prepare` (the policy, the mitm ca, the merged
  trust bundle), the agent's and the proxy's env, the canary script and its verdict, the reachability probe. a
  driver supplies only the topology around it.

a killed exec reports its exit status, not an error (`Session.Exec`'s contract, what `internal/eval` relies on: it
tells a timeout by its context). one that exits 0 once cancelled reports the context's error, as `os/exec` does. an exec
on a context already done fails without starting. the cancel step runs
before the signals, on a fresh context: a daemon that hangs there hangs the exec, as before this design.

## timings

one struct on the process, every wait and bound a driver has: lease renewal and staleness, the instance-gone bound
and its retry, instance readiness and its poll, the online probe's retry, the proxy's listening line, the exec grace,
the follower grace, the bound on removing a container whose `run` failed (its cidfile), the proxy binary fetch. the
other cleanup calls on a fresh context (the exec's cancel step, `Close`'s removes) stay unbounded, as before.
production uses `defaultTimings`; tests shrink them. the waits between tries go by the clock (`Now`, `After`,
`NewTicker`), so a fake clock that moves only when a test says can replace the shrunk waits without touching a
driver. the bounds put on a context (instance-gone, the cleanup call, the fetch) stay wall time, as a context
deadline is; a fake clock would not move them. an `After` of zero fires at once, on any clock (a follower stopped
with no grace). the exec runner's own `WaitDelay` (how long it copies output after the process is gone) is production
plumbing, not a driver's wait.

## tests

one set of test pieces in `src/sandbox`, shared by every driver's tests (`fake_test.go`):

- `runFunc`: a runner from a function; `Start` runs it in a goroutine whose context a signal ends.
- `procRunner`: the same, keeping the procs it started, each with an ordered log of the signals it got; one can be
  told to ignore a signal (a client that outlives sigterm).
- `callLog`: the calls a fake saw, under a lock: `record`, `all`, `reset`, `call(prefix)`, `find(substring)`.
- `testProcess(t)`: a process with short timings, shut down when the test ends.
- `manualClock`: a clock that moves only when the test advances it.
- `faults`: at most one fault in a fake cli's calls, of four kinds: the daemon refuses the call, or does it and the
  client fails anyway, or the context ends before or after the daemon acts.

per daemon, a fake that tracks state and answers argv as the daemon does, flags parsed by name, refusing what the
daemon refuses, failing the test on argv it does not know, and answering probes by name (`probes.go`):
`fakeServer` (`lxdfake_test.go`, `lxd-testing.md` seam 4) and `fakeDaemon` (`dockerfake_test.go`: containers,
networks and their addresses, labels, the cidfile, docker cp's rules, `logs -f`). each records into a `callLog`.
the older argv-pinning fakes (`fakeLXC`, `fakeDocker`) stay for the tests that assert the argv itself.

behaviour every driver must share gets one table run against each driver's fake. a new driver joins each table and
passes before it ships:

- `TestExecCancel`: the cancel step, then sigterm, then sigkill after the grace; the killed client's status as the
  result; a clean exit after the cancel as the cancel; a context already done; a clean finish not signalled.
- `TestSessionConformance`: what `Session` promises of Push, Pull and Exec (a directory's contents to a new or an
  existing path, a file to a path, a directory's contents out into hostDir, a non-zero exit as the command's), and a
  second Close with nothing left. the drivers in it: lxd, docker and podman. the copy rules are also checked on a
  real docker and podman where they run.

per driver, on its fake: a whole session under each egress mode (`TestLXDSessionOnServer`,
`TestContainerSessionOnDaemon`), and the sweep: every cli call of Start under each egress mode, of Close, and for lxd
of the image builds and clean, faulted each way, after which nothing of the run is left. where the faulted call was
itself a removal, what it failed to remove must be what `pats clean` takes: for lxd the next clean once the run is
dead, for docker and podman an `sbx-` name and the owner label. the latter is a stand-in for `internal/eval`'s clean,
which stays out of `src/sandbox` (see below) and is not run by the sweep.

the sweeps found, and the code now handles: two runs creating the lxd project at once; an lxd marker, a published
image or a run's alias left by a setup or build that failed; a marker removed under a live run, or made again after
shutdown; a docker network a failed create made after all, and a taken name read as a taken range; an agent
container without the owner label; and docker's Push and Pull nesting a directory one level deeper than the
contract.

## drivers

status: implemented (as built, below). sessions have one shape (the rule above); everything else pats does with a daemon
or a server has none, and it grew two ways:

- lxd-specific exports in `src/sandbox`, which `internal/eval` picks by driver name: `LXDImage`, `LXDFingerprint`,
  `LXDCopyImage`, `LXDRemote`, `LXDProxyImage`, `LXDRemoteFingerprint`, `LXDImageStamps`, `LXDImageUsage`,
  `LXDDeleteImage`, `LXDInfo`, `ServerCPUs`, `CleanLXD`, `ShutdownLXD`. each builds a driver on `defaultProcess`
  inside, so a caller cannot hand it a process of its own.
- docker and podman argv in `internal/eval`, through `exec.Command`: `hosts.go`'s `containerHost` (`context show`,
  `system connection list`, `info`, `image inspect` in two formats, `images -a`, `pull`, `rmi` from `remove.go`),
  `clean.go`'s sweep, `build.go`'s `proxyImageID`, `run.go`'s `docker info` for `-j auto`; plus the exported
  `BuildContainerImage` and `ContainerStamps`, which take a driver name.

so eval knows docker's cli and which lxd functions exist, the docker half of image handling and clean has no argv
test, and a new driver means another set of exports and another switch in eval.

### the shape

a driver, by the rule above, grows from making sessions to every concern pats has of one place sandboxes run: a
docker or podman daemon, an lxd server, later a bwrap host. it is still built from a process, a runner and its options
(`newDriver(p, r, o)` in tests, `sandbox.Open(o)` in production on `defaultProcess`). a driver value is bound to its
place, as a database driver opened against one dsn is: two lxd remotes are two drivers of one kind, and `driver:` in
the config picks the kind `Open` builds:

```go
type Driver interface {
	// where
	Target(ctx context.Context) (Target, error) // pin key (docker:<ctx>, lxd:<remote>), daemon id, server version
	CPUs(ctx context.Context) int               // for -j auto; 0 when unknown

	// images
	Resolve(ctx context.Context, ref string) (Image, error) // id, digest; ErrNoImage when absent
	Images(ctx context.Context) (map[string]bool, error)    // every image id held, tagged or not
	Pull(ctx context.Context, ref string, force bool, log io.Writer) (img Image, fetched bool, err error)
	Build(ctx context.Context, spec BuildSpec, log io.Writer) (img Image, base string, built bool, err error)
	ProxyImage(ctx context.Context, ref string, force bool, log io.Writer) (img Image, fetched bool, err error)
	Stamps(ctx context.Context, id string) (map[string]string, error)
	RemoveImage(ctx context.Context, id string) error
	Upstream(ctx context.Context, ref string) (string, error) // the source's current id or digest, for staleness

	// sandboxes
	Sandbox(o SandboxOptions) (Sandbox, error) // today's Sandbox and Session, unchanged

	// leftovers (see clean)
	Runs(ctx context.Context) ([]RunRecord, error) // what the driver holds of each run, unjudged
	Remove(ctx context.Context, r Resource) error
}

// ImageUsage is what lxd's server records of an image's use; a container
// daemon records none, and init judges container images by the projects'
// pins instead (init.md). eval asks for it by type assertion.
type ImageUsage interface {
	Usage(ctx context.Context, id string) (ImageUse, error)
}
```

- **two levels of options.** the place: the kind (`driver:`), remote, uplink, proxy binary, egress pool, podman's
  connection (`CONTAINER_HOST`, `CONTAINER_CONNECTION`, filled by `WithEnv` like the others, so no driver reads the
  environment), and the instance type (`vm`). the sandbox: image, the pinned proxy image, limits, profiles, config.
  `vm` is the place's because every lxd image method depends on it (the image key, the builder and stamp instances,
  the upstream image picked by type) as much as the sandbox does: a server serving both types is two drivers,
  sharing the process's per-server state, which therefore stays independent of the instance type (the proxy image is
  always a container). clean opens one driver per server, of either type: what it lists and removes does not depend
  on it. a container driver ignores it. today's `ForceImages`, `Progress` and
  `Warn` become call parameters (`force`, `BuildSpec`'s, the `log` writer and its progress lines).
- **one implementation per driver family.** `containerDriver` (docker, podman, nerdctl through the engine table),
  `lxdDriver`, later `bwrapDriver`, whose image methods say there are none and whose `Runs` lists nothing: its only
  leftovers are the temp dirs and lease files the shared clean already sweeps. what differs by engine stays in the
  engine table.
- **what the image methods mean on each.**
  - `Pull` makes `ref` present: fetched when absent or forced, a no-op otherwise, `fetched` saying whether it
    brought something (what init records as pulled), `img` what is now present. docker pulls; lxd copies a remote
    image into the project, and `img` is the fingerprint it copied: a later `Resolve` of the ref asks the image
    remote, whose alias may have moved since.
  - `Build` builds one layer and returns the base it built on, as resolved for the key: an alias moving during a
    long provision cannot make the pin name a base the image never saw. eval composes lxd's two-layer chain and
    container `base:` / `provision:` from it. for a container `build:` dockerfile, `base` is the resolved
    `PATS_SANDBOX` or empty: its `FROM` lines stay eval's to track, around the build, with `Resolve` and `Pull`.
  - `ProxyImage` makes the egress proxy image the driver's sandboxes use present: a container driver pulls `ref`
    (the published image), `fetched` saying, as for `Pull`, whether the image is pats's or was there already (a
    user's own build of it, which init must never remove); lxd builds its own (the binary, the netplan, the key rules
    stay inside) and caches it per process.
  - `Upstream` is, for a container driver, `RegistryDigest` of the ref; for lxd, the image server's current
    fingerprint for the driver's instance type.
  - `Usage` is lxd's only (`ImageUsage`, above).
- **`BuildSpec`** carries what init builds on any driver: a base, provision material and argv, or a dockerfile and
  its context (container drivers only; lxd refuses it), the role and sandbox for lxd's properties, force, and the
  progress and warn callbacks. the per-driver keys, the claim protocol and the re-pointing stay inside each driver.
- **eval holds policy only:** pins, staleness, what to build, pull or remove and when, the order of a run, what clean
  reports. it declares the narrow interfaces it uses, as `imageHost` does today, and a driver satisfies them; init's
  `fakeHost` keeps testing init's logic. eval runs no cli and names no engine; it only opens the driver.
- **the process stays one per pats.** a driver holds the process it was built on; leases, the subnet counter and
  per-server setup stay on the process, shared by every driver it opened. `ShutdownLXD` becomes `sandbox.Shutdown`,
  ending every lease the process holds, on any driver.
- **pure helpers stay functions:** `SandboxImage`, `ProxyImage`'s ref resolution (`sandbox.ProxyImage` as a name),
  `NormalizeRef`, `Repository`, `RegistryDigest`, `ContextHash`, `DockerfileFroms`, `MkTemp` and friends touch no
  daemon.
- **an alternative weighed.** moving only the container argv into `src/sandbox` behind the runner, and keeping eval's
  per-concern interfaces (`imageHost`, `puller`, the removers), buys the argv tests and a cli-free eval without `Open`
  or the options split. `Driver` adds one construction seam (no `newLXD(defaultProcess, ...)` rebuilt per call)
  and the home clean's `Runs` and `Remove` need, which is why it is the proposal.

### tests

each driver's argv is tested on its fake, like its sessions: `fakeDaemon` learns images (inspect, the image list,
pull, rmi, the target commands), and `fakeServer` already holds what lxd's image methods use. a driver conformance
table, beside the session one, runs what every driver promises against each fake: resolve after pull and after
build, a second pull fetching nothing, a missing ref as `ErrNoImage`, an image in use refused removal, `Images` after
a removal, `Target` stable across calls. `Usage` has rows for lxd only. eval's tests keep their method-level fakes.

### the move

one cut, not a staged migration: 1.x is still moving and may break between commits. behaviour changes it brings along
are taken, not avoided: `-j auto` on podman asks podman for its cpus, where it asked docker. `Driver`, `Open` and the
two option levels land together with docker and podman's argv moving into `containerDriver`, the lxd exports and
`BuildContainerImage` / `ContainerStamps` going, and eval's `hostFor` and driver switches replaced. clean follows on
`Runs` and `Remove`, and with it the docker sweep's bar for a faulted removal (above, "tests") becomes the new clean's.
`src/sandbox`'s exported surface is then `Open`, `Shutdown`, the types and the pure helpers.

### as built

- the implementations are the `container` and `lxd` types that already made the sessions: `Sandbox` returns a copy
  carrying the `SandboxOptions`, and there are no separate `containerDriver` / `lxdDriver` types. every lxd method
  works on a copy it prepares (`ready`), so one driver serves concurrent callers.
- lxd's proxy image build reports its steps as the `==> ` lines of the log it is given (warnings as
  `==> warning: `); eval turns them into progress lines (`stepLog`). `BuildSpec` keeps `Progress` and `Warn`.
- a container base / provision build generates its dockerfile outside the context and uses the material dir as the
  context (`COPY . /pats-provision`), so nothing is copied to build it. `BuildSpec.SandboxImage` gives the pats
  sandbox, asked for only when the build uses `PATS_SANDBOX`.
- `Target.Version` is docker's `ServerVersion` and lxd's `server_version`; podman gives none. `CPUs` reads the
  engine table's info template (`{{.NCPU}}`, podman's `{{.Host.CPUs}}`). podman's `CONTAINER_HOST` and
  `CONTAINER_CONNECTION`, set in `Options`, reach the cli as its environment as well as naming the target.
- a container `Resolve` of an image id gives its first registry digest, with the repository it belongs to
  (`Image.Repo`), for the proxy image a pair's metadata records. lxd's `Pull` of a ref on the server itself only
  resolves it. a remote image not listed for the server's arch and type is `ErrNoImage`.
- eval's interfaces are `imageHost`, `puller`, `builder` and `host` (`internal/eval/hosts.go`); `hostFor`,
  `hostForTarget`, `newSandbox` and clean's `openDriver` are its seams.
- tests: `TestDriverConformance` (pull then resolve, a second and a forced pull, a missing ref, remove, an image in
  use, build then resolve, the target) on the lxd, docker and podman fakes; `TestContainerTarget`,
  `TestContainerResolveDigest`, `TestProvisionDockerfile`, `TestEngineRunnerEnv`. `fakeDaemon` learned images,
  pulls, builds and the target commands (`dockerimages_test.go`).

## clean

status: implemented (as built, below). `pats clean` removes what dead runs left, on every driver, by one rule: a run is
alive while its lease is fresh, wherever the lease is kept. before it, lxd judged by a lease and docker and podman by
an owner file's pid; both are liveness records, kept in different places. images are not clean's but `pats init`'s
(`init.md`), with one exception below.

- **stamps.** everything a run makes carries its run token and its owner: lxd's `user.pats.run` and
  `user.pats.owner` (as today), `pats.run` and `pats.owner` labels on docker and podman containers and networks (set
  at creation; labels never change, and neither do these), the token in a temp dir's owner file. that includes what
  `pats init` runs, the docker `pats-stamp-*` containers among them, which today carry neither and outlive a killed
  init.
- **the lease.** one record per run, renewed every `leaseEvery` (2m), stale after `leaseStale` (10m), by the
  process's one renewal loop, made before anything else of the run (lxd's marker already is). lxd keeps it as the
  marker acl's `user.pats.lease`. docker and podman keep it as the modification time of `run-<token>.lease` beside the
  temp root, written before the first `docker run`. the loop wakes every 30s and renews once `leaseEvery` has passed
  on the wall clock, so a machine waking from sleep renews within 30s, not a whole period later. the lease file
  replaces the pid check and with it pid reuse. the record names the host and pid that wrote it, and the pid is an
  early answer one way only: on that host, a pid that is gone makes the run dead at once; a pid that exists says
  nothing (it may be reused), and the lease decides.
- **who can judge a docker run.** a run's lease file is in its user's cache dir, so a run is judged by a clean that
  shares that cache dir: the same user, the same `$HOME` and `XDG_CACHE_HOME`, the same machine. another user's
  runs on a shared daemon, a `sudo pats clean`, another client of a `DOCKER_HOST` daemon all see an unreadable record.
  a `$HOME` shared over the network by two machines brings two clocks to one record, and with them lxd's accepted
  skew (below).
- **the verdict, per run.** a stale lease, or a gone pid on the record's host: dead. no record, and everything of the
  run older than `leaseStale`: dead (a run that died before its record, or a clean that died after removing it;
  docker's networks have no creation time in `network ls`, so the driver inspects them). a fresh lease: alive. a
  record this clean cannot read: unknown.
- **removal.** dead runs go in one order on every driver: sandboxes, then networks and acls, then the run's private
  image aliases (`pats-img-<key>-<token>`), then the record last, so a clean that dies halfway leaves a run still
  judged. a private alias on an image that also carries another alias goes alone (the claim succeeded, its removal did
  not). an image whose only alias is the private one is either a build that died between its publish and its claim, or a
  claimed image a forced init re-pointed the public alias away from, which a pin may still name, used or not (stamps
  read its properties, creating no instance): nothing on the image tells the two apart. so it is judged as init judges a
  superseded lxd image: it goes, private alias and all, once nothing used it for 30 days (`last_used_at`, or
  `uploaded_at` for an image no instance was made from), and until then the private alias stays, marking it for the next
  clean. init.md's accepted failure applies unchanged: a pin dormant for over 30 days finds its image gone. no other
  image: one a run built still carries its token after a re-pointed alias left it, and `pats init` keeps it while pins
  elsewhere may name it. a removal that fails is left for the next clean (the bar the sweeps test). orphan lease files
  (stale, naming no run any driver lists and no temp dir) go with the temp dirs, matched as `run-*.lease` exactly: the
  client id and init's project registry sit beside them.
- **`--all`.** also removes unknown runs. an unknown run may be alive on another machine or under another user, and
  the output says so before `--all` takes it; a run whose fresh lease clean can read is never removed by it. what
  `--all` loses of lxd's today (live runs) comes back named: `pats clean --run <token>` removes that one run whatever
  its lease, for a wedged process whose renewal still ticks or an admin clearing a shared server. `--all` also removes
  shared resources no run uses (lxd's `patsup`), which the daemon refuses while an instance is on it, and an init
  build holds a live marker while it uses one. and it removes every image pats made on a place, which a driver lists
  apart from the runs: on docker and podman the built ones by their `pats.context-hash` label and those pulled from
  pats's registry, plus whatever the registered projects' pins say pats pulled or built on that daemon (an `image:`
  ref pats pulled has no mark of its own), on lxd every image in the pats project but a run's under its private
  alias. a build is tagged `pats-build:<hash>` as well, and the tag is the proof: an image carrying it is pats's. the
  label alone is inherited, so a labelled image without the tag whose layers start with another's of the same label
  (or equal them: a `FROM` that only sets metadata) is someone's own, and neither of two such twins is taken by label.
  the build cache's hit (`cachedImageID`) needs the tag and the matching label, so an image built FROM a pats build
  is never reused as one, even once the pats build is gone. an
  image that an image outside the removal set is built on (a layer prefix) stays, as does one a container uses
  unless forced; both are decided up front, so a dry run says what a real one does. images go in passes,
  so a parent goes once the children built on it have; one a container uses stays, and is said. a run's own image
  goes however recently used. pins naming a removed image are left for `pats run` to report. `--older-than` does not
  apply. `--force` is `--all` and also live runs, the containers using an image it removes, and every temp dir and
  lease file. clean says what it does as events (`CleanEvent`) its caller renders as log lines or, with `--json`,
  json lines; a dry run makes the same events, an image in use kept up front and the images said in listing order
  once every pass is done.
- **where it lives.** each driver lists what it holds of each run, unjudged (`Driver.Runs`: token, owner,
  resources with their creation times, and for lxd the marker's lease) and removes a resource (`Driver.Remove`).
  the verdict is the shared clean's, in `src/sandbox`: it reads the docker lease files from this host's cache dir,
  which a driver on a remote daemon could not, and the order and `--all` are shared with it.
  `internal/eval`'s clean loops over the drivers and the temp dirs, with no cli calls of its own. it takes `pats
  init`'s machine lock while it removes, so the two never act on one daemon at once. the docker clean gets the
  fake-daemon sweep lxd's has.
- **the clock.** docker's lease and its cleaner share one clock where they share one cache dir (above). lxd's are two
  machines' clocks, the run's writing the lease and the cleaner's reading it: a cleaner about 7.5m fast takes a live
  run. accepted (`fizz/lxd_lease.fizz`): ntp keeps real machines far closer, and the server's clock cannot be read
  cheaply (acls carry no server timestamp, `lxc query` does not show the `Date` header, and a server-stamped
  `created_at` needs a write per read). a machine asleep longer than `leaseStale` looks dead to a clean run before its
  first renewal after waking: with the 30s wake renewal that is a window of seconds, accepted and not modelled.
- **older runs.** no fallback: leftovers of a pats before this carry no `pats.run` and are not listed. the release
  notes say to remove `sbx-*` containers and networks by hand once, after upgrading.
- **a gap closed.** a docker agent whose workdir is outside the temp root has no `sbx-` name; it carries the token and
  the owner label, so this clean finds it by label, which today's clean, filtering by name, does not.

### as built

- a resource's kind is `sandbox` (a container or an instance: builders, stamp containers and the egress helpers that
  read the ca bundle and run the probes included), `network`, `acl`, `alias`, `image` or `marker`. an lxd run is
  `Held`: the server holds its record, so it is never unknown. what no run owns (lxd's uplink) goes after every run,
  so its users go first.
- the lease file is `run-<token>.lease` in the pats cache dir, beside the temp root, holding `<host> <pid>`. it is made
  before a run's first container and before its first temp dir (`MkTemp`), so `pats init` holds one too, and every
  command ends its leases on exit (`main` calls `sandbox.Shutdown`). a temp dir's owner file holds the token; one from
  before holds a pid (anything not shaped like a token), judged by it alone. lxd's marker renewal wakes every 30s like
  the file's.
- a removal that fails keeps the run's record (the marker, the lease file), so the next clean judges the run again.
  an unknown run is printed, with why, before `--all` takes it.
- `CleanTemp` leaves every lease file when a place could not be listed (a daemon down may still hold the run).
- tests: `TestJudge`, `TestHostLease`, `TestCleanTemp`; the container sweeps run a real clean after a faulted
  removal, and `TestContainerSweepClean` faults every call of one; the docker fake fails any container run without
  the run labels; `TestLXDClean` covers the image rules, `--all` and `--run`, `TestCleanCachedContainerImages` the cached images; eval's `TestCleanAcrossPlaces` runs
  the whole clean on fake places.

## not in this

- bwrap: only the exec path is known to fit. its runner runs `bwrap` itself and `execCancellable`'s cancel step is a
  no-op, since ending bwrap ends its children. it has no daemon, no networks, no log follower and no cidfile, and
  Push and Pull are host copies; how egress works under it (the proxy, the self-test) is open.
- a typed client per daemon (the docker api, the lxd api) instead of the cli: the same argument as `lxd-testing.md`'s
  open question. the argv is the boundary, and the adapter is where the quoting and flag bugs live.

## risks

- the fakes answer by argv and can drift from the clis. the checks: the e2e jobs (`TestLXDE2E`, the docker session
  tests that skip without a daemon), lxd's contract scenarios (`TestLXDContract`, run against the fake and the ci
  job's lxd), and the copy rules on a real docker and podman (`TestContainerCopyRules`). docker has no contract
  scenarios beyond those; the daemon behaviours `fakeDaemon` encodes (a taken name or subnet, a network with
  endpoints, `--rm` on kill) are checked only by the e2e paths that happen to hit them.
- `combined` and `output` trim and split output differently from the helpers they replace; each call site keeps the
  shape it had (`docker()` combined and trimmed, `dockerOut()` stdout with stderr in the error, `lxc()` combined with
  the args in the error).
