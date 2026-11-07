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
  second Close with nothing left. the copy rules are also checked on a real docker where one runs.

per driver, on its fake: a whole session under each egress mode (`TestLXDSessionOnServer`,
`TestContainerSessionOnDaemon`), and the sweep: every cli call of Start under each egress mode, of Close, and for lxd
of the image builds and clean, faulted each way, after which nothing of the run is left. where the faulted call was
itself a removal, what it failed to remove must be what `pats clean` takes: for lxd the next clean once the run is
dead, for docker a name and the owner label `pats clean` finds.

the sweeps found, and the code now handles: two runs creating the lxd project at once; an lxd marker, a published
image or a run's alias left by a setup or build that failed; a marker removed under a live run; a docker network a
failed create made after all; and docker's Push and Pull nesting a directory one level deeper than the contract.

## not in this

- `internal/eval` still runs docker itself in three places: `pats clean`'s sweep of `sbx-*` containers and networks,
  the pre-pull of the sandbox and proxy images, and `docker info` for `-j auto`. they move into `src/sandbox` behind
  the runner when `pats init` (`init.md`) takes over pulls and builds; until then they stay e2e-only.
- bwrap: only the exec path is known to fit. its runner runs `bwrap` itself and `execCancellable`'s cancel step is a
  no-op, since ending bwrap ends its children. it has no daemon, no networks, no log follower and no cidfile, and
  Push and Pull are host copies; how egress works under it (the proxy, the self-test) is open.
- `pats clean` judges docker leftovers by owner files and labels, lxd leftovers by lease markers. when the docker
  sweep moves into `src/sandbox`, the two need one model or a stated reason to differ, not just a move.
- a typed client per daemon (the docker api, the lxd api) instead of the cli: the same argument as `lxd-testing.md`'s
  open question. the argv is the boundary, and the adapter is where the quoting and flag bugs live.

## risks

- the fakes answer by argv and can drift from the clis. the checks: the e2e jobs (`TestLXDE2E`, the docker session
  tests that skip without a daemon), lxd's contract scenarios (`TestLXDContract`, run against the fake and the ci
  job's lxd), and docker's copy rules on a real docker (`TestContainerCopyRules`). docker has no contract scenarios
  beyond those; the daemon behaviours `fakeDaemon` encodes (a taken name or subnet, a network with endpoints, `--rm`
  on kill) are checked only by the e2e paths that happen to hit them.
- `combined` and `output` trim and split output differently from the helpers they replace; each call site keeps the
  shape it had (`docker()` combined and trimmed, `dockerOut()` stdout with stderr in the error, `lxc()` combined with
  the args in the error).
