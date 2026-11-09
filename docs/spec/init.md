# pats init: build once, run many

status: implemented for docker, podman and lxd, the engine table included. nerdctl (an entry in that table and a ci
job) and bwrap come with those drivers.

pats gets three stages: `pats init` makes everything the project's sandboxes need, `pats run` uses
it, `pats score` scores. `report`, `list`, `egress` and `clean` stay as tools around them. this applies to every
driver: docker and podman today, lxd, and bwrap and nerdctl when they come. `lxd.md` is the lxd driver's design;
`lxd-testing.md` is how its tests get their seams.

## problem

`pats run` makes images on its way to the first pair. under docker and podman it builds `build:` dockerfiles and
`base:` / `provision:` images (cached, but still a build step), pulls the egress proxy image when absent, and lets
the first `docker run` pull the pinned sandbox image. under lxd it builds three images: the pats sandbox layer on
`ubuntu:26.04`, the project's `provision` layer on it, and the egress proxy; on the target server that is several
minutes, and the first instance from each fresh image is slow again while the pool unpacks it. every run recomputes
each image's key and builds on a miss, so any changed input rebuilds, unasked, at the start of a run:

- a refreshed base (`ubuntu:26.04` refreshes every week or two) rebuilds what sits on it;
- an edited `provision` script, its `bin/` or a dockerfile's context rebuilds the project layer;
- a new pats binary rebuilds the lxd proxy (keyed on its bytes): per release for users, per commit while developing;
- a change to what pats bakes into images (`provisionerVersion`) rebuilds everything.

superseded images are never deleted (on lxd about 2.5 GB per base refresh and 190 MB per pats binary), and two
projects share a layer only by accident of identical inputs.

## what we want

1. the slow work runs when the user says so, once, and runs reuse its result.
2. a run never builds or pulls. a missing image fails the run before the first pair, naming the command that makes
   it. the same on every driver.
3. a run that finds its images out of date -- a changed local input, a newer base or published image upstream --
   says so, names the command, and goes on with what it has.
4. projects on the same pats version share what does not depend on the project. a project's own layer is its own.
5. images can be removed without guessing what another project still uses.

## the stages

### `pats init`

`pats init [-S <suite>]... [-a <agent>]... [-t <task>]... [--sandbox <name>]... [--driver <d>] [--force]` makes what
the selected pairs' sandboxes need -- by default every sandbox the config's suites use -- and pins it. `-S`, `-a`
and `-t` are `run`'s selectors, so `pats init` and `pats run` with the same flags agree on what is needed;
`--sandbox` and `--driver` narrow further. the driver comes from each sandbox, so a project mixing docker and lxd
agents comes up with one command.

per driver:

- **container engines (docker, podman, nerdctl).** pull the published sandbox image for a sandbox with neither
  `image:` nor `build:`; pull an `image:` sandbox's image when absent; build `build:` dockerfiles and `base:` /
  `provision:` images as `run` does today; pull or build the egress proxy image for a sandbox with an egress
  policy. pin each by image id (and repo digest when pulled).
- **lxd.** set the server up (the `pats` project, the uplink, the docker bridge check that today first fails in a
  run), build the sandbox layer, the project layer and the proxy, pin each by fingerprint. details below.
- **bwrap**, when it exists: there are no images; init checks the host has what the sandbox needs (`bwrap`, the
  harness clis) and pins their versions, so the same staleness check applies.

it also stamps what is inside (see component versions) and writes logs to `.pats/init/<sandbox>.log`, with the
progress lines builds print today. a layer whose current key already has an image is reused and pinned, not rebuilt.
`--force` rebuilds regardless.

preflight stays in `run`: credentials expire between runs, images do not.

### `pats run`

before the first pair, for each sandbox it uses:

- **missing.** not pinned, the pinned image gone (a purge, a clean, a fresh machine or server), or the pin made
  against another daemon or server than the one the run reaches: the run stops before preflight, listing every
  missing sandbox and the one command that makes them: `pats init --sandbox mason --sandbox haiku-docker` (or bare
  `pats init`).
- **stale, local.** the provision material, the dockerfile context, the pats version or the pats build differs from
  the pin's: one warning per sandbox naming what changed and `pats init --sandbox <name>`, and the run goes on with
  the pinned image. hashing is local and is what the keys cost today.
- **stale, components.** the pinned image's stamped harness versions differ from what this pats release pins
  (component versions, below): same warning, same going on.
- **stale, upstream.** a newer base or published image exists: same warning, same going on. checked against the
  source, with a budget (below).
- **the proxy is the exception** (below): a pinned proxy from another pats version or build stops the run.

the run starts every instance and container from the pinned id, never from a name: `docker run <id>` neither pulls
when absent nor follows a re-pulled tag, so the id is what makes "run never builds or pulls" hold. the pin file takes
over what `buildImages` does today (writing the resolved image onto the sandbox for the rest of the run), and the id
is what `metadata.json` records. nothing in `run` writes pins.

## pins

one file, `.pats/images.json`, gitignored with `.pats/`, keyed by target and sandbox. a target is where images live,
resolved at init so a later change of default does not silently move a pin:

- containers: the engine and its daemon as resolved at init (`docker:<context>`, `podman:<connection>`), with the
  daemon's identity stored in the entry: docker's `docker info` id; podman has no such id, so its connection's uri
  (socket path or `ssh://` host, from `podman system connection list`) stands in. `DOCKER_HOST`, `docker context use`,
  colima or a remote daemon all change which daemon answers; a run whose daemon id differs from the pin's treats its
  images as missing, rather than trusting a pin made against another daemon.
- lxd: the resolved remote (`lxd:armstrong`); a config with no `remote:` pins under the default as resolved at init.
- bwrap: `bwrap:local`, pinning the host tools' versions.

```json
{
  "version": 1,
  "lxd:armstrong": {
    "mason": {
      "image": {"id": "<fingerprint>", "name": "pats-img-<key>", "built": true},
      "layers": ["<fingerprint>", "<fingerprint>"],
      "proxy": {"id": "<fingerprint>", "name": "pats-img-<key>", "built": true},
      "pats_version": "1.0.0-beta.4",
      "pats_build": "release",
      "built": "2026-11-07T11:00:00Z",
      "inputs": {"base": "<fingerprint of ubuntu:26.04 at init>", "provision": "<sha256>"},
      "components": {"claude": "2.3.1", "codex": "0.61.0", "opencode": "1.4.2"}
    }
  },
  "docker:default": {
    "haiku-docker": {
      "daemon": "<docker info id>",
      "image": {"id": "sha256:...", "name": "ghcr.io/lczyk/pats/sandbox:v1.0.0-beta.4", "digest": "sha256:...",
                "pulled": true},
      "proxy": {"id": "sha256:...", "name": "ghcr.io/lczyk/pats/egress-proxy:v1.0.0-beta.4", "digest": "sha256:...",
                "pulled": true},
      "pats_version": "1.0.0-beta.4",
      "pats_build": "release",
      "built": "2026-11-07T11:02:00Z",
      "inputs": {},
      "components": {"claude": "2.3.1", "codex": "0.61.0", "opencode": "1.4.2"}
    }
  }
}
```

`layers` lists every layer under the image, bottom first: two for a project layer on the pats sandbox layer, one for
a project layer on a custom `base:` or for the pats sandbox layer alone. `inputs` holds what the staleness checks
compare: the base it was built from, and the hash of whatever local material went in (provision material, a
dockerfile's context).

- **by id.** a name (an lxd alias, a docker tag) is mutable; an image id or fingerprint is the image. the name stays
  for people and for the cli.
- **who writes it.** `pats init` only. writes take a lock on `.pats/images.lock`, re-read the file, change only
  their own entries, and replace it by rename. each init also drops entries for sandboxes no longer in the config,
  so a removed sandbox's images become candidates for removal (below).
- the file also holds the `superseded` list init retries (removing superseded images, below).
- `proxy` only for a sandbox with an egress policy. a sandbox with `image:` is pinned too (its id at init), so
  `run` can tell missing from present, and the upstream check covers it.
- `version` is the file's own format; a pats that does not know it says to rerun `pats init`.

### release and dev builds

`pats_build` is `release` for a tagged module install, else the build's commit, with `-dirty` when its tree was. a
module install of a tagged version (the documented way to get pats) carries no vcs stamp, so
`version.Info.CommitSHA == ""` with a release-shaped version is a release; a clone build carries `vcs.revision` and
counts as dev even at the tag. when unsure, dev. the version string alone cannot tell them apart: the `VERSION` file
names the last release on every commit after it.

`sandbox/images.go` picks the published images by the version string alone, so a clone build at the tip of
1.0.0-beta.3 pulls the published `egress-proxy:v1.0.0-beta.3`, whose proxy code may differ from the build's. that
stays: an install of a beta pulls that beta's images, which is what matters, and a clone at the tip of a beta is a
development build whose images are allowed to lag it. `pats_build` still tells the two apart, for the lxd proxy key
(bytes for a dev build) and for the proxy check below.

### the proxy exception

the proxy is pats's own code on the other end of the env protocol (`PATS_EGRESS_*`), and a proxy from another pats
may not speak this one's. so a pinned proxy is used only when both the run's `pats_version` and its `pats_build` match
the pin's: an upgrade from 1.0.0-beta.3 to beta.4 (both `release`) changes the version, a rebuild of a clone
changes the build, and either stops the run, saying `pats init` is needed and what it rebuilds. after an upgrade
that is the version-keyed layers and the proxy; after a dev rebuild only the proxy (the sandbox layers' keys carry
the version, not the commit). a developer runs `pats init` after rebuilding pats; it takes about a minute.

## upstream checks

`run` checks for newer upstream versions of what its pins came from, in parallel with preflight, with a 5s budget
for all of them together; a check that does not answer in time is skipped silently. never fatal, never written.

- lxd bases: `ubuntu:` and `images:` are simplestreams servers, which `lxc query` (the lxd rest api) cannot reach.
  the base's current fingerprint comes from `lxc image list <remote>: <alias> --format=json`, which works over both
  remote types, against `inputs.base`. a contract test covers it on both remote types.
- container bases and published images: the tag's manifest digest from the registry, by a HEAD through the registry
  client pats already has (no `docker pull`), against the pinned digest. the published sandbox and proxy images are
  versioned tags and never change; the check matters for `image:` sandboxes and the `FROM` images of `build:` and
  `base:`. those are often short (`ubuntu:26.04`, `python:3`), which the client's ref parser refuses today: they
  are normalised to `docker.io/library/...` first. the client gets ghcr's anonymous token from `<host>/token`;
  docker hub takes its token from the realm in `WWW-Authenticate`, so the client learns to follow that.
- any check that fails -- slow, refused (a private registry's 401), an unknown host -- is skipped silently.
- `--offline` on `run` skips them all, for a machine without a network.
- a newer pats release: not checked by `run`; out of scope here.

## component versions

`pats init` stamps what is inside each image: each harness cli's version (`claude --version`, `codex --version`,
`copilot --version`, `opencode --version`), and later anything else worth knowing. they go into the pin and into
each pair's `metadata.json`, so a score can be read against the harness that produced it. an lxd image carries them
too, as `pats.component.<cli>` properties its builder reads before publishing (`provisionerVersion` went up with
them, so every pats-made layer has them); init reads them back. a docker image cannot take a label once built, so
init runs the stamp in a throwaway container of the image and the pin is where they live.

today the harnesses install at `@latest` unless `CLAUDE_CLI_VERSION` and friends are set, so two images of one pats
version built a week apart carry different harnesses, and the published docker image and an lxd image provisioned from
the same script disagree. proposed (`harness-pins.md`): each pats release pins its harness versions in one file both
the dockerfile and the provision script read, bumped at each release. then "same pats version" means the same
harnesses, the stamps confirm it, and a newer harness arrives with a newer pats. a project wanting another harness
version sets it in its `provision` layer, and the stamp records what it got.

a run warns when a pinned image's stamps differ from what this pats release pins (an image built by an older pats,
or a provision layer that changed a harness): same warning, same going on.

## container engines

docker, podman and nerdctl take one command line for what pats does, with differences in network setup. today docker
and podman are grouped as containers (`IsContainer`), share one implementation, and differ in six places, all in
egress network setup: docker's `inhibit_ipv4`, podman's `--disable-dns`, the default network (`bridge` | `podman`),
and docker-only steps in the self-test and the canary. those become one table per engine: the binary, the default
network, the internal-network and dns flags, the build command, what the self-test needs. nerdctl is then a table
entry and a ci job.

`driver:` keeps naming the engine (`docker | podman | nerdctl | lxd | bwrap`); the family (container, lxd, bwrap)
follows from it. a `driver: container` plus `engine:` would add a key without adding information.

## removing superseded images

`pats init` removes what it replaces. there is no sweep of a daemon or server and no `clean --images`: the only
candidates are images pats itself built or pulled (the pin entry says which: `built` or `pulled`; an image that was
already there, say an `image:` sandbox's the user pulled, is never one) that this project's own pins pointed at until
this init moved them (a new base, an edited provision, a pats upgrade) or dropped them (a sandbox gone from the
config). plain `pats clean` removes runs' leftovers, and of images only one a dead run left under its private alias
alone, once unused for 30 days, by the rule init applies to a superseded lxd image (`sandbox-seams.md`, clean).

a candidate that is not removed yet goes on the pin file's `superseded` list (target, id, since when), and every later
`pats init` tries the list again, on every driver.

a candidate goes only when no one else may still want it:

- **container engines.** every project on the machine registers itself: `pats init` adds the project's path, one
  line, to `~/.cache/pats/projects` (the pins stay in each project's `.pats/images.json`; the list only says where to
  find them), and drops listed paths that no longer exist. a candidate is removed when no listed project's pins name
  it, on any target. the engine itself refuses to remove an image a container (running or stopped) still uses, so a
  pair mid-run is safe; such a candidate stays on `superseded` until a later init, after `pats clean` has taken the
  stopped container.
- **lxd.** a server is shared across machines, whose pins no list can reach. a candidate is removed when this project
  no longer pins it, it is not run-private (`pats-img-<key>-<token>`), and nothing used it for 30 days:
  `last_used_at`, or `uploaded_at` for an image no instance was made from. an image this project used until today
  fails that test on the day it is replaced, so it waits on `superseded` and goes once 30 days pass without a use.

init prints what it removed and what it kept and why (`kept pats-img-ab12...: used 3 days ago, may serve another
machine`), so a missing image in another project can be traced to the init that took it.

accepted failures, each costing a rebuild or a re-pull at someone's next run, which stops with "missing, run `pats
init`":

- docker: a project moved or renamed (its old path drops off the list) or its `.pats/` deleted has its pins unseen,
  so another project's init may remove an image it shared.
- docker, a daemon several users share: another user's projects are not on this user's list.
- lxd: a colleague's project on another machine, dormant for over 30 days, finds its layers gone.
- lxd, rare: an instance created from a candidate in the very second init deletes it fails to launch. it needs an
  image unused for 30 days to be used at that moment.

## lxd specifics

- **names.** the alias stays the content key, so identical inputs share across projects without coordination, and the
  claim protocol (run-private alias, then `alias create`) stays. each image also gets properties `lxc image list`
  shows: `pats.role` (`sandbox` | `project` | `proxy`), `pats.version`, `pats.sandbox`, `pats.inputs`, and the
  component stamps.
- **keys.** the pats version goes into every layer's key (a project layer on a custom `base:` would not inherit it),
  so only the same pats version shares a layer. dev builds share the sandbox layer across commits; what pats bakes
  into every image (the epilogue, the `ProvisionMount` protocol) is covered by `provisionerVersion`, bumped by hand
  when it changes. the proxy key is the version and target arch for a release, the binary's bytes for a dev build.
- **`--force`.** the claim as it is would read the existing alias as "another run built it first" and keep the old
  image, so a forced build re-points the alias at the new fingerprint: `PUT /1.0/images/aliases/<name>` through `lxc
  query`, one call, no window without the alias. other checkouts' pins keep the old fingerprint until their own init.
- **lxd 6.** pats is developed against 5.21 (the current lts); 6.x is in `latest/stable` and `6/stable` and becomes
  the next lts in 2026. the rest api through `lxc query` is versioned and additive (low risk). `lxc image info` text
  is the one place pats reads output meant for people (medium): `pats-img-*` aliases read over `lxc query` as json and
  bases over `lxc image list --format=json` remove it, and are done anyway. image remotes (`ubuntu:26.04`) may resolve
  differently on a fresh 6.x client, whose docs describe "image registries" (medium, unknown until tried). acl and nic
  keys, `publish`, `alias create`, project features: low. observed behaviours the driver works around (delete during a
  create; acls under `br_netfilter`) may differ. no 6.x server is at hand: the ci lxd e2e job becomes a matrix over
  `5.21/stable` and `6/stable`, and the contract tests of `lxd-testing.md` run on both. where they differ, the driver
  asks the server version once at setup and branches there.
- **oci images and pebble.** canonical lxd cannot pull oci images (its sources are simplestreams servers and other lxd
  servers; incus runs oci application containers since its 6.3), so pats would convert the image itself with its
  registry client. pebble or `sleep infinity` as pid 1 is enough for `lxc exec`, but nothing would bring the network
  up: open pairs would need static addressing like proxy pairs have. vms and real-system cases are not covered. with
  init, provisioning runs once per pats version per server, so the gain is one build per release. not doing.

## tests

pins, the run's checks and init's removal are local logic: pins written, read and merged by two processes, a missing
pin stopping the run with the right command, each staleness source warning once, the proxy mismatch stopping the run,
the upstream budget skipping a slow check, `--force` re-pointing an lxd alias, and init's removal taking only its own
superseded images and keeping one another listed project pins, an lxd image used within 30 days (retried from
`superseded` at a later init) and a run-private one. the lxd parts take the fake server and clock from
`lxd-testing.md`; the container parts a fake engine cli. each e2e runs `pats init`, then `pats run` twice: the second
run must build and pull nothing.

## migration

`pats run` on a project with no `.pats/images.json` stops and names `pats init`, the same as any missing image. the
readme's quick start gains the step. ci jobs that run pats gain `pats init`. the lxd e2e drives the driver through
`go test` (`TestLXDE2E`), not `pats run`, so it calls the init path itself before starting sandboxes. nothing in
`pats.yaml` changes.

## decided after review (recommendations, open to change)

- no `pats run --init`: one more command on a cold machine, and a flag would bring the unasked build back into `run`.
  cheap to add later if cold starts hurt.
- the upstream budget stays 5s (the checks overlap preflight, which rarely finishes sooner), with `--offline` to
  skip them.
- the proxy stops the run on a version or build mismatch: the cost to a developer is one `pats init` of about a
  minute after rebuilding pats.
- harness versions are pinned per pats release: the stamp check needs something to compare against, and it makes
  "same pats version, same image" true.
