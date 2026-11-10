# pats as a snap

status: proposed. strict confinement. it depends on two specs: the build-tag seams (a spec of its own, to come), which
let a build leave drivers out, and `docs/spec/step-sandbox.md`, which lets a build with no host run steps. neither is
in code yet. `snap/snapcraft.yaml` is the target and packs only once the build tags exist. nothing in ci builds it,
and nothing is published.

## purpose and limits

`go install` needs a go toolchain, and a clone needs `make`. a snap installs pats in one command on a linux machine
with snapd, refreshes it, and lets a ci runner pin a channel. the snap is strict: it installs without `--classic`,
needs no classic review, and runs under apparmor with the access its plugs grant.

strict means a reduced pats. what it has:

- **drivers:** docker (the docker snap or an apt docker) and local lxd (the lxd snap), through clients the snap
  ships.
- **steps:** in step sandboxes only (`docs/spec/step-sandbox.md`).

what it does not have:

- **the host driver, and host steps.** a strict snap cannot run the user's tools. the build leaves them out
  (`nohost`), so a config that names a host sandbox, or a host step without a `step-sandbox`, is a load error.
- **bwrap.** it needs user namespaces, which a strict snap gets only through the `userns` plug. that plug is
  super-privileged (`allow-installation: false` in snapd's base declaration), and it would need a bwrap the snap
  ships. left out (`nobwrap`).
- **podman.** pats drives podman through its cli, which runs without a daemon. a strict snap cannot run the host's
  podman, and a shipped `podman-remote` would need the user's podman socket service and the `podman` plug, whose
  base declaration has `deny-connection: true`: it connects only with a store grant. left out (`nopodman`).
- **lxd remotes.** the user's remotes live in `~/snap/lxd/common/config`, which the `home` plug cannot read. local
  lxd only. remotes would mean a remote config of the snap's own.
- **the host's docker config.** the shipped cli reads `$SNAP_USER_COMMON/docker`, not `~/.docker`, so a host
  `docker login` to a private registry does not reach it. pats pulls images and builds them through that cli
  (`src/sandbox/container_driver.go`, `build.go`), so a private `image:` or a `build:` context that pulls from a
  private registry needs a login through the snap's own cli: `pats.docker login <registry>` (an app the snap
  exposes for that, below). pats's own registry requests (digest checks, the proxy binary) go through oras and are
  anonymous (`src/sandbox/registry.go`), as in a native build.
- **macos, ubuntu core.** no snap on macos. on ubuntu core the `docker` plug cannot connect
  (`deny-connection: on-classic: false`).

a project that needs any of these runs a native build.

## the build

- **tags:** `nohost`, `nobwrap`, `nopodman`, through the go plugin's `go-buildtags`. `pats --version` names the tags
  a build was made with.
- **static**, as before: under lxd, `proxyBinary` (`src/sandbox/lxd_egress.go`) copies `os.Executable()` into the
  proxy instance, and a dynamic binary would fail there. the part fails when `readelf -l` finds an `INTERP` header.
- **no upx.** the snap builds with the plugin, not `make`.
- **the clients the snap ships**, so pats keeps driving engines through their clis (`cliRunner`,
  `src/sandbox/process.go`) and gains no second way to drive one:
  - `lxc`: built from a pinned `canonical/lxd` tag, `CGO_ENABLED=0 go build ./lxc`. the go plugin builds `./...`,
    daemon and cgo included, so this part overrides the build.
  - `docker`: the docker cli from a pinned `docker/cli` tag, static. it has no plain `go.mod`, so if the go plugin
    cannot build it, the part dumps docker's published static tarball for the arch instead.
  - `docker-buildx`: the buildx plugin from a pinned `docker/buildx` release, static. pats runs `docker build`
    (`src/sandbox/build.go`), and without buildx the docker cli falls back to the deprecated legacy builder, which
    rejects buildkit-only dockerfiles (`RUN --mount`). a host's buildx is out of the snap's reach, so the snap ships
    its own. the cli finds plugins under `$DOCKER_CONFIG/cli-plugins`. the snap sets `DOCKER_CONFIG` to
    `$SNAP_USER_COMMON/docker`, and a command-chain script links the revision's buildx there on every launch.
    a private-registry login made with the host's `docker login` (`~/.docker/config.json`) is not seen (limits).
  - both clients land in the snap's `bin/`, first on `PATH`. their versions are pinned in `snapcraft.yaml` and bumped by
    hand.
- **version and grade:** unchanged from the classic design. a clean tree at the tag `v$VERSION` builds with
  `-buildvcs=false` and is that release, keyed like a `go install` of it in `releaseBuild`
  (`src/sandbox/images.go`). any other tree carries its commit, is `VERSION+git<sha7>` and counts as a dev build.
  `grade` is `devel` for a pre-release.

`platforms` builds amd64 and arm64, natively, matching the published images.

## plugs

- **`network`, `network-bind`:** registries, providers, the egress proxy's listener. connect automatically.
- **`home`:** the project, its runs, the real home's non-hidden files. connects automatically on classic distros.
- **`docker`:** the docker socket, apt docker or the docker snap. connected by hand until the store grants
  auto-connect.
- **`lxd`:** the lxd snap's socket. connected by hand until the store grants auto-connect.

- **docker:** `snap connect pats:docker` for an apt docker (`/run/docker.sock`), `snap connect pats:docker
  docker:docker-daemon` for the docker snap. the user must be able to use the socket, as today (the `docker` group).
- **lxd:** `snap connect pats:lxd lxd:lxd`. the snap sets `LXD_DIR=/var/snap/lxd/common/lxd` so its `lxc` finds the
  lxd snap's socket. the user must be in the `lxd` group, as today.
- **store requests:** auto-connect for `docker` and `lxd`, each a forum request, far smaller than a classic review.
  until then the snap works with the manual connects. keyless harnesses need no plug (below).
- a run whose engine plug is not connected fails at the engine's first call with the socket's permission error.
  preflight names the plug to connect instead.

## what pats can read and write

- **its own state:** `$SNAP_USER_COMMON` (`~/snap/pats/common`). the snap sets `XDG_CACHE_HOME` to
  `$SNAP_USER_COMMON/cache`, and `CacheDir` appends `pats` (`src/sandbox/cachedir.go`), so the cache
  (`docs/spec/cache-dir.md`) is `~/snap/pats/common/cache/pats`, not in the per-revision `$SNAP_USER_DATA` that a
  refresh copies. it survives refreshes.
- **projects:** under the real home (`$SNAP_REAL_HOME`) and not in a hidden directory, through `home`. the runs dir
  sits in the project (`.pats/runs`), which `home` reaches since only hidden directories at the top of the home are
  denied, not hidden ones below a project. not under `~/bin`, where `home` denies writes, so a project there can
  create no runs. not under `~/snap`, which `home` leaves out: another snap's data (`~/snap/code/common/...`) is
  out of reach, and only pats's own (`~/snap/pats`) is in. `home`'s rules are `owner` rules, so the project's files
  must belong to the user who runs pats: a checkout owned by another user or by root, even if readable to the group,
  is out of reach.
- **not:** a project under `/srv`, `/opt` or another place outside the home, or under `/tmp`, which a strict snap
  gets privately. a project in a hidden directory at the top of the home (`~/.work/...`). the home's dotfiles.
- **the docker snap's view.** the docker snap's daemon sees host paths under the home. pats binds its cache and the
  project into containers (`src/sandbox/tmp.go`), so both must be visible to it: the project under the home, the
  cache under `~/snap/pats/common`. whether the docker snap's daemon reaches `~/snap/pats/...` is not known yet. its
  own `home` plug denies `~/snap`, though a root daemon's view may differ (testing, below). if it does not, the cache
  moves to a non-hidden directory pats owns under the home.

## keyless harnesses

`claude-cli-keyless` and `codex-cli-keyless` copy the user's login (`~/.claude/.credentials.json`,
`~/.codex/auth.json`, `agent.HostCredsFile`) into the agent's home (`harnessHome`, `internal/eval/harness.go`). a
strict snap cannot read either: both sit in hidden directories, which `home` denies, and `HostCredsFile` looks under
`os.UserHomeDir()`, which under strict is `~/snap/pats/<rev>`. no plug is added for them. instead:

- **`PATS_CLAUDE_AUTH_PATH` and `PATS_CODEX_AUTH_PATH`** name the login file to copy, in every build, not only the
  snap. unset, pats reads today's default under the real home (`$SNAP_REAL_HOME` when set). set, pats reads that
  path and nothing else: a path that does not exist or cannot be read fails preflight, naming the variable, rather
  than falling back to the default. in the snap the path lies where `home` reaches (above), e.g.
  `~/pats-auth/codex.json`, `0600`. a symlink to the dotfile does not help, since apparmor checks the link's target,
  which is hidden.
- **claude has a second way in already:** `CLAUDE_CODE_OAUTH_TOKEN` (from `claude setup-token`) is forwarded to
  `claude-cli-keyless` (`credKeys`, `internal/agent/agent.go`), which keeps it keyless. it needs no file, so it
  suits the snap and ci best. `PATS_CLAUDE_AUTH_PATH` is there for parity with codex, whose subscription login has
  no environment form: `CODEX_API_KEY` and `OPENAI_API_KEY` would turn it into api billing.
- **a copy goes stale.** the host's claude or codex refreshes its own login and can rotate the refresh token, after
  which a copied file stops working. keeping it fresh is the user's: re-copy, or in ci write it from the secret on
  every run.
- **in the snap with neither set**, the default path is unreadable. preflight then fails the keyless harness,
  naming the variable to set, where a native build only warns that no creds were forwarded.

## the host's ca store

pats's own process makes tls connections to registries (`pats init`, oras). the credential preflight runs the
harness in a real sandbox (`internal/eval/preflight.go`), so it trusts what the sandbox trusts (below), not pats.
go reads the system roots from `/etc/ssl/certs`. in a strict snap that may be the base's bundle, not the host's,
which drops a ca a company installed on the host. go honours `SSL_CERT_FILE` for its system roots, so a host with its
own ca points `SSL_CERT_FILE` at a bundle under the home. which `/etc/ssl` a strict core24 snap sees is checked in
testing (below), and this section is settled by it.

this covers pats's own process only. the sandbox, the preflight's included, and the egress proxy each run in a
container or instance of their own, and trust what pats gives them there today: the sandbox image's roots, plus the
machine proxy's trust when a machine proxy is set (`src/sandbox/machineproxy.go`, which reads `SSL_CERT_FILE` only
then). a provider or other destination behind a company ca with no machine proxy needs that ca in the sandbox image,
in the snap as in a native build.

## the environment under snap run

strict changes more than classic did:

- `HOME` is `~/snap/pats/<rev>`. pats uses the real home (`$SNAP_REAL_HOME`) wherever it means the user's home: the
  keyless logins, the step-sandbox tree walk's guard, the docker snap's visibility rule.
- `XDG_RUNTIME_DIR` is `/run/user/<uid>/snap.pats`. nothing pats ships reads it: rootless podman is out.
- `TMPDIR` and `/tmp` are the snap's own. pats already keeps temp dirs under the cache, not `/tmp`
  (`src/sandbox/tmp.go`), since a confined daemon cannot see the host's `/tmp` either.
- `SNAP_*` reach the shipped clients and nothing else: steps run in sandboxes, which get only what pats sets.

## pats clean across installs

a snap and a native install on one machine have separate caches (`~/snap/pats/common/cache/pats` and
`~/.cache/pats`), so separate client ids (`src/sandbox/clientid.go`). the owner label on every container carries
the client id (`pats.owner=<host>/<uid>/<client>`, `docs/spec/cache-dir.md`), so it depends on the driver:

- **docker:** each install's `pats clean` sees the other's containers as another client's, as it sees another
  machine's, and leaves them alone. `pats clean --all` takes them, as it takes another machine's.
- **lxd:** a run's instances are judged by the lease the run keeps on the lxd server (`src/sandbox/lxd_clean.go`,
  `clean.go`), which every client reads, before ownership. so either install's `pats clean` removes the other's
  runs whose lease is stale, and never one whose lease is fresh, `--all` or not. a dead run is dead whoever made it.

two installs are two clients, and the lxd server's lease is the one judge both share.

## refreshes

- snapd's refresh app awareness holds a refresh while a `pats` process runs, up to snapd's limit (14 days by
  default), when `snap run` can track the process in its own cgroup. where it cannot, the refresh is not held, and
  `snap run` says so only under `--debug-log`. a ci runner that must not be refreshed mid-run holds refreshes
  itself (`snap refresh --hold pats`).
- a run that outlives a refresh keeps its own revision's mount, `/snap/pats/<rev>`, while snapd keeps that revision
  (`refresh.retain`), and the lxd proxy binary it pushed came from there.
- the cache lives in `$SNAP_USER_COMMON` (above), shared by every revision, and its format is the same across
  versions.

## distribution

- **name:** `pats`. no published snap of that name exists. whether it is registered is known only when a
  registration is tried.
- **channels:** track `latest`. a `1.0.0-beta.N` tag goes to `beta`, and `stable` starts with 1.0. 0.x is not
  snapped.
- **store requests** (plugs, above) are separate from publishing: a strict snap releases without them, with manual
  connects.
- **publishing** runs on tag push, after the images, with a store credential in the repo's secrets. that job and the
  readme's install line come after the build tags and the step sandbox land.

## testing

a smoke test of a built snap, in an ephemeral ubuntu 26.04 lxd vm, through pats only:

- `snapcraft pack`, then `snap install --dangerous` of the result, and the plugs connected by hand.
- `pats --version` names the build tags. `readelf -l /snap/pats/current/bin/pats` shows no `INTERP`.
- **load errors:** a config naming a host, bwrap or podman sandbox, or a host step with no `step-sandbox`, fails at
  load with its line.
- **docker**, once with an apt docker and once with the docker snap: `pats init` pins a sandbox and a step sandbox.
  one real pair on the cheapest model, with a provider key from a ci secret, runs `stage` and `collect` in the step
  sandbox and is then scored by a script scorer in it. this answers the docker snap's view of `~/snap/pats/...`.
- **lxd:** the same pair under the lxd snap, with an `egress:` policy. `pats init` builds the proxy image from the
  snap's own binary.
- **keyless:** `codex-cli-keyless` with `PATS_CODEX_AUTH_PATH` at a copy under the home passes preflight. unset, it
  fails preflight naming the variable. `claude-cli-keyless` with `CLAUDE_CODE_OAUTH_TOKEN` passes with no file.
- **ca store:** a host ca added to the vm's store, and a registry behind it. the result settles the ca section.
- **clean:** `pats clean -n` after the runs, and once more with a native build's run alive beside the snap's.

## to decide later

- a `pats.lxc` app for remotes, if local lxd is not enough.
- podman, once the store grants the plug or pats has a reason to ship `podman-remote`.
- shell completions, once pats has them.
