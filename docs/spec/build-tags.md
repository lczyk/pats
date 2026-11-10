# drivers behind build tags

status: proposed.

## problem

the strict snap (`docs/spec/snap.md`) is a pats with the host, bwrap and podman drivers left out. go leaves code out by
build tags, and a tag can only leave out whole files. today the drivers are not whole files: each driver's file holds
code the others use, `driver.go` names every driver, and callers outside `src/sandbox` branch on driver names. this
spec draws the seams so that one tag line per driver file is the whole difference between builds.

## the tags

- `nohost` leaves out the host driver and, with it, host steps (`docs/spec/step-sandbox.md`): no user code runs in
  pats's own environment.
- `nobwrap` leaves out the bwrap driver.
- `nopodman` leaves out podman.

one tag per driver, named for what is gone, not a single `strict` profile: each reads on its own, and the snap sets
all three. a native build sets none and is what pats is today. `pats --version` names the tags a build was made
with.

## the seams

each item is a `refactor:` that changes no behaviour, so the tree passes `make verify` after every one, before any tag
exists.

1. **shared code leaves the driver files.** after it, `host.go`, `bwrap.go` and `bwrap_image.go` hold their driver
   alone:
   - `Paths`, `PathsOf`, `HomeMount`, `PrepareMount` (`host.go`), which eval uses for every driver, to `paths.go`.
   - the `copy*` helpers (`host.go`), which bwrap uses too, to `copy.go`.
   - `proxyChild`, `startProxyChild` (`host.go`), which bwrap uses too, to `proxy_child.go`.
   - `HostLookPath` (`host.go`) to `paths.go`, with what it calls: `lookIn`, `envValue`, `hostEnv` and
     `hostPassed`. it stays the same lookup (relative and empty `PATH` entries dropped), not `exec.LookPath`.
   - `LXDDriver` (`bwrap.go`) to `lxd.go`.
   - `removeTree` (`bwrap_image.go`), which `clean.go` and `tmp.go` use, to `tmp.go`.
2. **a driver registry.** each driver file registers itself from `init()`: its name, its constructor, its traits
   (below), an optional clean hook, and how it reopens a pin target. the last keeps `OpenTarget`'s split: lxd takes
   the target's suffix as its remote and does not ask for a target back, while the engines and bwrap open with the
   current options and must answer with the pinned target key and daemon. `newDriver` and `OpenTarget`
   (`src/sandbox/driver.go`) look names up instead of switching on them. bwrap's leftover sweep in `clean.go`
   (`newBwrapStore().sweepNew`) becomes its clean hook. `sandbox.Drivers()` lists what is registered.
3. **podman as an engine entry.** podman is a variant of the container driver, not a driver of its own. its entry in
   `engines` (`engines.go`), its `Target` branch (`container_driver.go`) and its `CONTAINER_HOST` /
   `CONTAINER_CONNECTION` env (`engineRunner`, `driver.go`) move to `engine_podman.go`, through two new `engine`
   fields: a target func and an env func. `config.IsContainer` asks the engine table, not the name.
4. **traits, not names.** the `IsHost()` callers ask what they mean:
   - `Unboxed`: the agent runs on the machine (`preflight.go`, `hosts.go`, `pair.go`, `init.go`,
     `cmd/pats/egress.go`).
   - `NoImages`: nothing to pin or build (`init.go`).
   - `HostCLIs`: the harness cli comes from the host's `PATH` (`preflight.go`).

   `sandbox.TraitsOf(driver)` answers from the registry, since eval asks before any driver is open. `config.IsHost`
   goes.
5. **config asks the registry.** `validate.go` checks a sandbox's driver against `sandbox.Drivers()`. a known driver
   left out of this build reads `sandbox "x": driver "bwrap" is not in this build`, and an unknown one keeps today's
   error. the per-driver key checks (`validateBwrap`, `validateHost`) stay, as schema. `internal/config` imports
   only `src/sandbox/proxy` today, and `src/sandbox` does not import config, so there is no cycle.
6. **tests follow the registry.** the cross-driver tables (`conformance_test.go`, `exec_test.go` and the rest) take
   their rows from what is registered, and each driver's own test files carry its tag line. driver-specific test code
   in shared files moves into a test file with the driver's tag: the bwrap adapter in `conformance_test.go`
   (`bwrapConform`), the host subtests in `exec_test.go` (`hostOn`), the layer fuzz target in `fuzz_test.go`
   (`unpackLayer`). shared helpers stay untagged. a tagged build runs the tests it can.

## the tag commit

one `feat:` after the seams:

- `//go:build !nohost` on `host.go` and its tests, `!nobwrap` on `bwrap*.go` and theirs, `!nopodman` on
  `engine_podman.go` and its tests.
- `nohost` also rejects host steps at load (`docs/spec/step-sandbox.md`), so the rule lives in config behind the
  same tag: one small file per side of it.
- ci gains a job that vets and tests with `-tags nohost,nobwrap,nopodman`, so a tagged build cannot rot.
- `pats --version` prints the tags, from a constant each tag file sets.

## not in this

- the snap itself, the step sandbox, and any driver behaviour. a native build behaves as today.
- a tag for docker or lxd. nothing needs a build without them yet.
