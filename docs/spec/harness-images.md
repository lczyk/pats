# harnesses are placed in images at init

status: the artefacts, the checked fetch, the cache, the tree and `pats harness pin` are implemented
(`harnesses-1x.md`, step 5); placement and what follows from it are proposed. it changes `harness-pins.md` (a pin
gains a sha256 per arch, and lives in the machine config's defaults, not in a file a script reads), `init.md` (a
default sandbox is built, not only pulled; the pin entry gains what init placed) and `bwrap.md` (a harness is a lower
dir of its own). `machine-config.md` holds the `clis` table this spec fetches from. `harness-registry.md` is what a
harness is, and where a config names one.

## problem

- **every image holds every harness.** the published sandbox image and the lxd sandbox layer install all four clis.
  unpacked, at today's pins on amd64, they are 340 MB (claude), 459 MB (codex), 206 MB (copilot) and 214 MB
  (opencode): 1.2 GB, in every image, whichever harness the project's agents use. each harness pats learns adds to
  every project's image.
- **a pin is a version, not bytes.** `images/harness-versions` names versions, and `npm` resolves them when an image
  is built: a ranged dependency (copilot's `detect-libc`), each package's per-arch optional dependency and two
  postinstall scripts are whatever the registry serves that day. nothing pats holds says what the bytes should be.
- **another version is a side door.** a project that wants a harness version this pats does not pin installs it in
  its own `provision` layer. pats neither knows nor checks; only the stamp notices afterwards.
- **two ways to get the same clis.** the docker image is built in ci and pulled; lxd runs the provision script at
  init. bwrap pulls the docker image.

what makes a single way possible: each of the four clis is published as one file per architecture holding one
native executable (the per-arch package beside its npm package: `@anthropic-ai/claude-code-linux-x64`,
`@openai/codex@<v>-linux-x64`, `@github/copilot-linux-x64`, `opencode-linux-x64`). none needs node or a dependency
tree. pi and omp publish per-arch release files the same way.

## proposal

a harness cli gets into an image as a file pats fetched, checked against a sha256 it holds, and placed. the published
sandbox image holds no harness. which clis an image gets follows from the config.

### artefacts and pins

both are keys of a cli's entry in the `clis` table (`machine-config.md`), whose defaults pats embeds:

- **an artefact** is one file, an entry of the cli's `artefacts` keyed by its url (the vendor's own per-arch
  package, with `${version}` for the version). under the url: the `arch` it is for, its `sha256`, and how it is
  placed: with `bin` a gzipped tar and the path of the executable in it, without it the file itself.
- **a pin** is the cli's `version` and, per arch, the `sha256` of that arch's artefact.
  `images/harness-versions` goes, and the defaults hold the pins. a test holds that every cli of the defaults has
  one version and an artefact for `amd64` and for `arm64`. the per-release bump of `harness-pins.md` stays, on the
  defaults.
- **arches** are the two the sandbox image is published for. the arch that matters is that of the image the trees
  go into, not of the machine pats runs on, nor of the daemon (one on arm64 may hold an amd64 base it emulates):
  under docker and podman the base image's own (`image inspect --format {{.Architecture}}`, once it is pulled),
  under lxd the server's (`serverArch`), under bwrap the one its rootfs was fetched for (`GOARCH`). each maps to
  `amd64` or `arm64`; any other is an init error naming it.
- **a project may pin another version** of a cli, on a harness item (`harness-registry.md`): `version` and `sha256`, the
  latter a mapping from arch to sha. the two come together or not at all; a `version` with no sha is a load error, since
  a pin is always bytes. the url and the `bin` are still those of the machine's artefact for the arch, with the item's
  version in the url: a project says which bytes, the machine where from. the sha for the target's arch must be there at
  init, else init fails naming the harness, the version and the arch; a config used on both arches lists both. one
  sandbox holds one version of a cli: two harnesses of its agents that pin one cli differently are a load error naming
  both, and take a sandbox each.
- **`pats harness pin <cli> <version>`** fetches the artefact for both arches and prints the pin as the `version` and
  `artefacts` of a `clis` entry and as the `version` and `sha256` of a project's item. it is how a release bumps the
  defaults and how a user gets shas for a version. what it prints is what the source served at that moment: trust on
  first use, and it says so, naming where the vendor's own checksum is when there is one (npm's `dist.integrity`, a
  github release's digests).

a url is a location and a sha is what the bytes are. so an artefact's url can be changed on a machine (a mirror, a
vendor that repacked its release) without changing what any project runs: the wrong bytes fail the sha.

### the set

a sandbox's harness set is the clis of every agent in the config that resolves to it (`ResolveSandbox`), judges
included, since a judge is an agent. it is taken from the whole config, not from a command's `-S` / `-a` / `-t`: an
init or a run of part of the matrix sees the same image. a harness with no `cli`
(`harness-registry.md`) adds nothing. a sandbox no agent runs in (a step sandbox) has an empty set.

there is no key for it. an agent that names a harness is the statement that its sandbox needs it.

### which sandboxes

pats places harnesses in the images it composes: a sandbox with neither `image:` nor `build:` (the default one, and
any with `base:` or `provision:`). an `image:` or a `build:` is the user's whole statement of what is inside, and
gets nothing. the host driver places nothing, as now, and keeps its version warning.

one check covers both kinds: init stamps the finished image (`init.md`, component versions), and when the stamps
lack a cli of the sandbox's set, init fails naming the cli and the sandbox. for an `image:` or a `build:` that is a
cli the user did not bring; for a composed image, one its `provision` removed. init asks the image itself each
time, and never answers from stamps an earlier init kept: those were taken for the set of then, and would call a
cli that joined it since absent. where the answer is kept is each driver's (under bwrap the pin entry, below).

a `base:` sandbox so gets its harnesses from pats too, where today its `provision` had to install them.

### fetch

only `pats init` fetches. for each cli of the set, at the target's arch:

- the cache is `<cache>/harness/<sha256>` (`cache-dir.md`), one file per artefact, shared by every project, sandbox
  and driver on the machine.
- a file there is hashed again before each use; one that no longer matches is removed and fetched once more.
- a miss downloads to a temp file beside it, hashing as it writes, and renames on a match. a mismatch is an init
  error naming the cli, version, arch, url and both shas, and keeps nothing. a response over 2 GiB is refused.
- the request follows redirects (github's release files do) and takes the machine's proxy from the environment, as
  go's `net/http` does. the sha is what makes the bytes right, not the host that served them.
- with every artefact in the cache, init needs no network for harnesses.

a run never fetches: it starts from the pinned image, as now.

### the tree

a cli's tree is what goes into the image, the same on every driver:

- `opt/pats/harness/<cli>/`: the unpacked artefact, or the file itself as `<cli>` in it, mode 0755.
- `usr/local/bin/<cli>`: a symlink to the executable in it.

it is unpacked by the rules the bwrap store unpacks a layer with (`bwrap.md`): an entry whose path or link target
leaves the tree is refused, device nodes are skipped, setuid and setgid bits are dropped, everything is root's.

### placement

the order is base, harnesses, the project's `provision`: provision still finds the clis, and has the last word.

- **docker, podman.** `provisionDockerfile` gains one `COPY` per cli of the set, in name order, between `FROM` and
  the provision step; the trees are in the build context. the default sandbox, which today is only pulled, is so
  built too when its set is not empty: a pull of the base and a local copy. with an empty set it is pulled, as now.
- **lxd.** the layer under the project's is pats's own: the tools (when the base is pats's `ubuntu:26.04`) and the
  set's trees, pushed with the material and copied into place. a `base:` sandbox gets that layer too, the trees
  alone on its base. so two images as now, and none more.
- **bwrap.** a tree is unpacked once into the store, `<cache>/bwrap/harness/<tree>/`, and a session stacks the
  trees its pin entry records (`harnesses`, below), by tree id, as lower dirs over the image's rootfs: never the
  ones the config would resolve to now, so a stale pin runs what it pinned. bwrap needs no `Build` for it. a session
  holds the trees it uses as it holds its rootfs (`<tree>.lock`). a recorded tree that is gone is a missing
  image. init stamps a session of the rootfs with its trees, and keeps that in the pin entry; the stamps file beside
  a rootfs stays the bare rootfs's.

an artefact reaches a remote daemon or lxd server through this machine: fetched here, then sent with the build
context or the material. over a slow link that is the cost of one mechanism; a fetch on the target would be a second.

### keys, pins and the run's checks

- **the key** of an image pats composes covers its set as lines of cli, version, sha256 and `bin` for the target's
  arch: everything that decides what a tree holds, and not the trees' bytes. `pats run` recomputes it from the config
  and the machine's `clis` without reading an artefact. an artefact's url is not in it. the docker build hash, the lxd
  layer key and `inputs` in the pin entry all take those lines. a tree's id is the sha256 of its line, so a corrected
  `bin` is another tree and a stale image, though the artefact in the cache is the same file.
- **the pin entry** (`.pats/images.json`) gains `arch` and `harnesses`, what init placed:

  ```json
  "arch": "amd64",
  "harnesses": {"claude": {"version": "2.1.319", "sha256": "<64 hex>", "tree": "<64 hex>"}}
  ```

  `components` stays what the finished image itself says (`stampScript`), and the stamp asks the clis of the
  sandbox's set, not a fixed list of four: under the host driver too, where a cli no agent uses is no longer run.
  the sha is of what pats placed: a `provision` that then swaps a cli shows in `components`, not in `harnesses`.
  a pair's `metadata.json` copies the pin entry's `harnesses` whole as `placed`, beside `components`, a judge's
  sample included, on every driver that pins an image: under bwrap the trees are not part of the image id, and an
  agent can run any cli its sandbox holds, so this is the one record of what a pair ran over. the host driver pins
  nothing (`host-driver.md`) and has no `placed`.
- **missing.** a selected pair whose agent's cli is not in its sandbox's `components` stops the run before
  preflight, with the other missing images, naming `pats init --sandbox <name>`. under every kind of sandbox that has an
  image: this is an agent added with a harness new to it. under the host driver a missing cli stays what preflight
  reports now, with how to install it: no init can place one there.
- **stale.** a changed set or pin moves the key, which is the local staleness warning of today (`init.md`), naming
  `pats init --sandbox <name>`; the run goes on with the pinned image. the component warning stays too, and
  compares the stamps with the sandbox's own resolved pins (an item's pin over the machine's), not with the
  defaults alone.
- **superseded.** an image a changed set replaces goes the way any superseded image goes (`init.md`).
- the artefact cache and bwrap's trees are only a cache, kept until `pats clean --all` removes them (a tree in use
  is refused, as a rootfs is), and the next init fetches again. init prunes neither: both are shared by every
  project on the machine.

### the published image

`ghcr.io/lczyk/pats/sandbox:v<version>` becomes ubuntu and the tools (`ca-certificates`, `curl`, `git`, `nodejs`,
`npm`, `python3`, `ripgrep`), with no harness. `images/provision-sandbox.sh` loses its npm step and its reading of
the pins, and `Dockerfile.sandbox-26.04` its four build args. node stays for the agents that reach for it; no
builtin harness needs it.

this breaks, and the release notes say so:

- a `build:` dockerfile on `FROM ${PATS_SANDBOX}` no longer finds the clis. it moves to `provision:`, or installs
  its own.
- `sandbox:latest` and `sandbox:26.04` move to a harness-free image when the first stable 1.x is published. a
  config that names either as its `image:` brings its own clis from then on. versioned tags already published keep
  what they hold.
- the clis are found at `/usr/local/bin/<cli>`, and no longer as npm's global packages.

## limits

- **glibc.** the artefacts are the vendors' glibc builds (codex's is static). a `base:` on musl gets trees whose
  executables do not start: not supported, and init's stamp fails on the first of them, naming the cli.
- **two arches.** `amd64` and `arm64`, as the image.
- **one version of a cli per sandbox.**
- **copilot unpacks itself.** its per-arch file is a self-extracting executable: on its first run in a home it
  unpacks 233 MB into `~/.cache/copilot`, which takes a few seconds, and when that is slow it prints a line about it
  before its version. a pair's home is fresh, so that is once a pair. measured on 1.0.107; the other three run as
  they are. placement has to settle it: the stamp must not take that first line for the version.
- **the source can go away.** a vendor that removes a version makes it unfetchable; a machine that has it cached
  keeps working. one that moved it is a url to change in the machine config, with no pats release.

## a worked case: mason

`tests/skills/pats.yaml` has one sandbox, `docker`, with `provision: provision`, and agents on
`claude-cli-keyless`, `codex-cli-keyless` and `opencode-openrouter`. nothing in it changes.

- its set is claude, codex and opencode. copilot is not placed: 206 MB less.
- `pats init` pulls the base, fetches three artefacts into the cache (once per machine and pats release), and builds
  base, three `COPY` layers, then mason's `provision` layer.
- an edit to `bin/provision` rebuilds that layer alone. an agent added on `copilot-cli-keyless` makes the next
  `pats run` stop with `pats init --sandbox docker`, and that init rebuilds from the copilot layer up, the
  provision layer included.
- the lxd sandbox it keeps commented out gets its own set the same way, from the agents pointed at it.

## tests

- the defaults' `clis`: `machine-config.md`.
- fetch, against a local http server: a match is cached and renamed into place; a mismatch names both shas and
  leaves no file; a cached file that was changed is refetched once; a redirect is followed; an oversized response is
  refused; a full cache needs no server.
- the tree, on crafted artefacts: the executable is linked; an entry leaving the tree is refused; setuid is dropped.
- the set: agents of two harnesses on one sandbox, a judge's harness, a step sandbox's empty set, a harness with no
  cli; a `-a` selection does not change it.
- a project's pin: `version` without `sha256` and the reverse; the url is the machine's artefact's with the item's
  version; a sha that is not 64 hex; an arch that is not one of the two;
  two pins of one cli in one sandbox.
- placement: the generated dockerfile for an empty set, a set of two, and a set under `base:` and `provision:`; the lxd
  layer's key moves with a pin and with the set; bwrap stacks one lower dir per cli; a changed `bin` moves the key and
  the tree's id, and not the cache's file.
- the run: `placed` in a pair's metadata is the pin entry's `harnesses`, every cli of the set, under bwrap too; a cli
  missing from `components` stops it with the init command, under a composed and an `image:` sandbox; a moved pin warns
  once and goes on; an item's own pin is not reported as drift from the machine's; an `image:` sandbox without the cli,
  and a `provision` that removes one, fail init; a bwrap session mounts the recorded trees after the config's pin moved,
  and a recorded tree that is gone stops the run.
- arch: an amd64 base on an arm64 daemon gets amd64 trees.
- e2e, per driver: a one-harness sandbox has that cli on `PATH` at the pinned version and none of the others.

## decided, open to change

- **pats fetches, not the image.** a script in the build (`curl`, `sha256sum`, `tar`) is less go, but it needs those
  tools in every base, a network in every build, a build on bwrap, and it downloads per image rather than per
  machine. one checked fetch in go serves all four drivers.
- **the vendor's per-arch package, not `npm install`.** one file and one sha per arch, no dependency resolution and
  no install scripts at build time.
- **harnesses under `provision`, not over it.** a changed set reruns provision, which a pats upgrade does anyway.
  over it would be cheaper to rebuild, but provision would no longer see the clis, and lxd would need a third image.
- **no harness key on the sandbox.** the agents already say it.

## open questions

- whether `pats harness pin` is a command of the released cli or a tool of this repo. the bump needs it either way;
  a user with a pin of their own needs it too, which argues for the cli.
- whether `nodejs` and `npm` stay in the base once no harness needs them.
- whether an `image:` or `build:` sandbox may later ask for pats's harnesses on top of what it brings.
