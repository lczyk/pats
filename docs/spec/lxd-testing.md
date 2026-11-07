# lxd driver: testing seams

status: proposed. a plan for the seams that let the lxd path be tested without a server, written after a day of
bugs that only a real server showed. `lxd.md` is the driver's design; this is how to test it.

## why

the bugs found on the target server, and what would have caught each without it:

- a proxy container, a builder and a killed create left behind after a failed or cut-short start. a fake server that
  tracks what exists, plus a sweep that fails each lxc call in turn, finds every such path.
- a delete that raced a create its client stopped waiting for (`fizz/lxd_delete.fizz`). a fake whose operations
  outlive their client finds it.
- a live run removable by `pats clean` (`fizz/lxd_lease.fizz`, accepted). a fake clock lets the renewal loop and the
  cleaner run against each other in a test.
- preflight's one budget for start and agent, and a build that printed nothing for minutes. ordinary unit tests once
  the time and the progress output are injectable.
- docker dropping pats's bridges, and the bridge acl filtering between two nics under `br_netfilter`. host networking:
  out of reach of any fake, e2e only.

what the tests have today: `lxd.run`, the function that executes the cli, is swappable. `fakeLXC` records every call
and answers a handful by substring, and tests wrap it for the rest; they assert on the text of the calls, not on what
the server would hold after them. and the swap changes production paths, in four places: with `l.run` set,
`prepareServer` skips the per-process server cache and the lease renewal goroutine, `proxyImage` skips its cache,
`Exec` skips the child process whose cancel kills the agent's processes (or stops the instance for a root step), and
`runProxy` skips the child process whose stderr it waits on for the proxy's listening line. those run only for real.

## goals

- every failure path of `Start`, `Close`, the image builds and `clean` testable for leftovers, without a server.
- no real sleeps in unit tests (`src/sandbox` takes about 20s today, most of it in retry loops).
- tests run the same code as production: no `l.run != nil` branches.
- two pats processes and a cleaner against one server in one test.

not goals: modelling lxd's networking (acls, nat, `br_netfilter`, dnsmasq), which stays with the e2e; a typed client
with a method per lxc operation (see open questions).

## seams

### 1. options instead of environment

`PATS_LXD_UPLINK` and `PATS_LXD_PROXY_BIN` are read deep in `lxd_image.go` and `lxd_egress.go`, and
`PATS_EGRESS_POOL` at package init in `egress.go`, so no test can change it at all. read them once where the driver
is built (`internal/eval` and the e2e) into `Options.Uplink`, `Options.ProxyBin` and the egress pool (seam 3). tests
stop needing `t.Setenv`, which forbids `t.Parallel`.

### 2. a clock

```go
type clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
	NewTicker(d time.Duration) ticker // C() and Stop()
}
```

on `lxd` (and through it `lxdServer.renew`, `clean`, `waitReady`, `online`, `deleteInstance`). production gets the
wall clock. tests get a fake whose time moves only when the test says, so a retry loop of three rounds is three
`Advance` calls, not six seconds, and a lease trace from the fizz model can be replayed step by step. the timing
constants (`leaseEvery`, `leaseStale`, `instanceGone`, the probe and ready deadlines) move into one struct the fake can
shrink.

### 3. process state in one place

per-process globals today: `lxdRunToken`, `lxdServers` (setup once, lease renewal), `proxyImages`, the egress pool
and its slot counter in `egress.go`. gather them into a `process` value (token, owner, server cache, proxy image
cache, pool and subnet allocator, clock, timings) that `newLXD` takes; production uses one package-level instance,
tests make their own. two `process` values on one fake server are two pats runs. `ShutdownLXD` becomes a method.

### 3a. a runner that can start processes

two of the four `l.run` branches exist because `Exec` and `runProxy` need a child process, not a function call:
`Exec` hangs its cancel on it, `runProxy` streams its stderr. so the seam becomes a runner with both shapes:

```go
type runner interface {
	Run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error
	Start(stdin io.Reader, stdout, stderr io.Writer, args ...string) (proc, error)
}
type proc interface {
	Wait() error
	Signal(os.Signal) error
}
```

production wraps `os/exec`; the fake answers `Start` with an in-process proc (the proxy's listening line on its
stderr, an agent that runs until signalled). `Exec`'s cancel (pkill or stop, then SIGTERM) and `runProxy`'s
readiness wait then run in tests. a test helper binary as `l.bin` would do it too, but costs a build per test run
and puts the fake's state in another process. with 3 and 3a the four branches go.

### 4. a fake lxd server

a test-only type that answers the runner by parsing the argv the driver uses, flags by name (`--project`, `-c`, `-d`,
`-n`, `-p`, `--vm`, `--force`, `--storage`), not by position. it holds:

- instances (project, config, devices, profiles, state, files), networks (config), acls (config, rules), images
  (fingerprint, aliases, properties), projects, profiles (the default one's root pool), pool usage. an instance's
  `expanded_devices` and `expanded_config` merge its profiles, or the refusal of a foreign nic or a `raw.*` key that
  a profile brings (`checkDevices`) is not tested at all;
- operations: an `init` or `launch` can be made slow, so the client's call returns when its context ends while the
  operation finishes on a later call, as on the target server (a delete meanwhile says not found);
- the `query` paths in use: `/1.0`, `/1.0/resources`, `/1.0/instances?..&recursion=1`, `/1.0/instances/<name>`,
  `/1.0/networks?..`, `/1.0/networks/<name>/state`, `/1.0/network-acls?..`, `/1.0/images?..`, `/1.0/operations?..`,
  `/1.0/storage-pools/<pool>/resources`.

a subcommand or path it does not know fails the test, so the fake's coverage is never assumed. `exec` goes to a small
per-instance file tree for the tar push and pull, to named probes once seam 6 lands (substring matching until then),
and otherwise succeeds with no output. errors use lxd's real wording where
the driver reads it ("not found", "already exists").

### 5. fault sweep

on top of the fake: `failAt(n)` makes the n-th lxc call fail, `cancelAt(n)` ends the context there (a deadline or
ctrl+c). for each entry point (`Start` with egress none, open and proxy, container and vm; `Close`; the agent image
build; the proxy image build; `clean`), run it with n = 0, 1, 2, ... until a run completes untouched, and after each
shut the test's process down (seam 3: this is `ShutdownLXD`, which removes the run marker) and assert what the
server holds: nothing of the run's token, only caches (`pats-img-<key>` images, `patsup`, the project). a failure
that leaves something names the call and the resource.

one leftover the sweep will show and that stays: `pats clean` deletes a dead run's instance once, without the retry
`deleteInstance` does for a create still running on the server, because clean is not to wait. such an instance
survives that clean and goes in the next one. the sweep's clean entry exempts it.

### 6. named probes

the shell run inside instances whose output the driver reads (ready, online, canary, reach, hostile, the agent uid)
is spread over string literals that fakes match by substring. make each a value: a name, the script, and the parse
of its output. the fake answers by name, each parse gets a table test, and a script edit no longer breaks a fake
silently. scripts whose output nobody reads (sysctl, chown, the build epilogue) stay as they are; the fake's
default success covers them.

### 7. registry over an injected client

`fetchImageFile` uses `http.DefaultClient` and builds `https://` urls from the ref. give it a client and a base url so
an `httptest` registry can serve it: an index without the server's arch, a manifest or layer whose digest is wrong, a
layer without the file, a token endpoint that refuses. there are no registry tests today, and the digest checks are
the point of that code.

### 8. contract tests

the behaviours the fake encodes about lxd (delete during a create says not found; `acl create` of an existing name
fails; `publish` then `alias create` of a taken alias fails; an operation names its instance as
`/1.0/instances/<name>?project=<p>`; `lxc image info` prints a `Fingerprint:` line, the one place the driver
parses text meant for people) get one scenario function each, run against the fake in unit tests and against a
real server under `PATS_LXD_E2E=1`. a newer lxd that changes one fails the contract run, not a production run.

### 9. eval

`internal/eval` already swaps `newSandbox` and `lxdCPUs`. add `lxdImageFn` for `buildImages`, so the image-build
progress, warnings and log-tail errors are tested without a server.

## tests this enables

- the leftover sweep (seam 5) across every entry point.
- a killed create on the agent, the proxy and the builder, each removed by its `Close`.
- the lease: renewal on a fake clock against `clean`, asserting the requirements that hold (on-time renewals keep a
  live run; a dead run goes) and pinning the accepted gap with the fizz trace as an expected removal.
- two processes and a cleaner on one server: a live run's resources survive the other's clean; two runs missing the
  same image build it once and both use it; clean of one run's leftovers leaves the other's.
- concurrent pairs in one process: distinct instance, bridge and subnet per pair.
- `Exec` cancelled mid-agent: the agent's processes killed, the instance kept; a root step cancelled: the instance
  stopped. the proxy's readiness wait, with and without its listening line.
- `src/sandbox` unit tests with no real sleeps.

## order

1. seams 1, 2, 3 and 3a, and the `l.run` branches out. medium: 3a reshapes `Exec` and `runProxy`; no new tests
   beyond what moves.
2. seam 4 and 5: the fake server and the sweep. the bulk, a few hundred lines of fake. existing tests move onto it as
   they are touched, not in one go.
3. seams 6 to 9, each on its own.

## risks

- the fake drifts from lxd. contract tests (seam 8) are the check; the fake also records the lxd version it mimics
  (5.21).
- argv parsing ties the fake to the cli's syntax, which is the real boundary pats talks across, so that is the right
  thing to be tied to. flags parsed by name keep argument order free.
- a sweep over every call of every path is slow if each run is slow; the fake clock keeps each in milliseconds.
- the fake is shared by goroutines (concurrent pairs, the renewal loop, two processes): it locks, and each test
  shuts its processes down so no renewal goroutine outlives it on the fake clock.
- `fingerprint` reading `lxc image info` text could instead read the alias over `lxc query` as json, which would
  take one contract scenario away; worth doing when seam 4 lands.

## open questions

- a typed client (one method per lxc operation) would make fakes simpler, but the adapter from methods to argv would
  then be untested, and that adapter is where quoting and flag bugs live. proposed: keep the argv seam and fake the
  server behind it.
- the fake lives in `src/sandbox` test files unless `internal/eval` needs it; then an internal package
  (`src/sandbox/lxdfake`).
