# the machine config: what pats knows about its tools, as a file

status: implemented for the `harnesses` table; a machine's `clis` open with placement (`harnesses-1x.md`, step 6).
`harness-registry.md` is what a harness is and how a `pats.yaml` names and changes one, `harness-parse.md` the `parse`
key, and `harness-images.md` how a cli of the `clis` table gets into an image.

## problem

- **what pats knows about a harness is compiled in.** the command, the hosts it needs, the variables and the file
  it logs in with, the format of its output, where its cli is downloaded from: each is a vendor's detail, and each
  moves. a vendor that adds a host, renames a flag or a field, or repacks its release breaks every project on a
  machine until a pats release carries the fix.
- **none of it belongs to a project.** a moved url is the same fact for every `pats.yaml` on the machine. a key for
  it in each project is a copy to keep in step, and the wrong place: a `pats.yaml` says what an experiment runs,
  not where this machine fetches from.
- **a new harness is a patch to pats.** a harness someone wants in all their projects, or one pats does not ship
  yet, has no place.

## proposal

pats reads what it knows about harnesses from a file with two tables, `clis` and `harnesses`. the copy it ships is
embedded in the binary: the defaults. a machine may have one more, laid over the defaults. a builtin harness is an
entry of the defaults and nothing else: no go code names one.

### the rule for what goes in

the machine config holds what pats knows about the tools it drives: what a pats release would otherwise carry, the
same for every project on the machine. the test for a key is that a pats release could have shipped the value.

it never holds:

- anything of one experiment: agents, models, tasks, sandboxes, scorers, an egress policy.
- a credential.
- a switch for pats's own behaviour: a default driver, parallelism, an output format.

a `pats.yaml` stays the whole statement of an experiment. the machine config is not a second place for a project's
keys, and nothing in it is a default for one.

### where

- `$XDG_CONFIG_HOME/pats/config.yaml` when `XDG_CONFIG_HOME` is set and absolute, else `~/.config/pats/config.yaml`,
  on every os, as the cache dir is resolved (`cache-dir.md`). not `os.UserConfigDir`, which is another directory on
  macos.
- the file is optional. with none, pats runs on the defaults alone, as a fresh install does.
- there is no flag and no variable of pats's own that names another file, and no project-local one.
  `XDG_CONFIG_HOME` is what a test or a ci job sets.
- **the snap** sets `XDG_CONFIG_HOME` to `$SNAP_USER_COMMON/config`, beside the cache (`snap.md`), so the file is
  `~/snap/pats/common/config/pats/config.yaml`, kept across refreshes and edited in place. the `home` plug cannot
  read `~/.config`, and no plug is added for it.

### the file

```yaml
clis:
  claude:
    version: 2.1.319
    login-file: .claude/.credentials.json
    artefacts:
      https://registry.npmjs.org/@anthropic-ai/claude-code-linux-x64/-/claude-code-linux-x64-${version}.tgz:
        arch: [amd64]
        sha256: <64 hex>
        bin: package/claude
      https://registry.npmjs.org/@anthropic-ai/claude-code-linux-arm64/-/claude-code-linux-arm64-${version}.tgz:
        arch: [arm64]
        sha256: <64 hex>
        bin: package/claude

harnesses:
  claude-cli-keyless:
    cli: claude
    run: >-
      claude --print --output-format stream-json --verbose --include-partial-messages
      ${PATS_EFFORT:+--effort "$PATS_EFFORT"} --model "$PATS_MODEL" --permission-mode bypassPermissions
      "$(cat "$PATS_PROMPT_FILE")"
    egress:
      allow: [.anthropic.com, .claude.com]
    auth-env: [ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN, CLAUDE_CODE_OAUTH_TOKEN]
    login: true
    pass-env: [ANTHROPIC_BASE_URL]
    parse: |
      ev = json.decode(line, default = None)
      ...
```

a `clis` entry is a cli: what pats can fetch, place and stamp (`harness-images.md`), and where the cli keeps what
is its own. its name is the executable's name on `PATH`, and the stamp is its `--version`: a cli has to answer that
with a line holding an `x.y.z` version, as the four do. the keys:

- **`version`**: the version every artefact below is of.
- **`artefacts`**: the files the cli is published as, each keyed by its url, laid out as a slice's contents are in
  chisel: the thing is the key, and what is true of it sits under it. `${version}` in a url is replaced by the
  version, and every url holds it: one without serves the same bytes for every pin. nothing else in this file is
  templated, and a `${...}` in a `run` is the shell's. `http` or `https`.
  under a url:
  - **`arch`**: the arches the file is for, a list of `amd64` and `arm64`. no two artefacts of a cli name one arch.
  - **`sha256`**: the file's. with `version` it is the pin.
  - **`bin`**: the path of the executable inside the file, which is then a gzipped tar. without `bin` the file is
    the executable itself.
- **`login-file`**: where the cli keeps its login, as a path under the home. the host's copy is that path under the
  user's home, or the file `PATS_<CLI>_AUTH_PATH` names (the cli's name in capitals, a `-` as `_`).
- **`home-env`**: variables set to a path under the harness home, for every harness on the cli
  (`CODEX_HOME: .codex`).

a `harnesses` entry is one way of running an agent:

- **`cli`**: a `clis` entry. without it the harness has no cli: nothing is placed, stamped or looked for on a host.
- **`run`**: the one-shot command, a script under the rule of `steps.md`, with the agent's `PATS_*` variables. the
  one required key.
- **`egress`**: what the harness itself needs of the network, as a task says it: `allow` and `deny`, joined to the
  sandbox's and the task's lists for each pair, where a deny wins (`egress-1x.md`). its entries are hosts only (a
  name, a `.suffix`, a `host:port`): a path, a method or a regexp is an error. the hosts of `allow` are the ones
  the `methods` posture leaves uninspected: a harness's own traffic is not the task's to inspect.
- **`auth-env`**: host variables forwarded into the sandbox when set, each a credential.
- **`login`**: `true` copies the cli's `login-file` into the harness home. it needs a `cli` that has one. `false`,
  the default, copies nothing: a harness that runs on a key must not find a login there, and an entry that changes
  a keyless one into that says `login: false`.
- **`auth-hint`**: what the warning says when no credential is found. without it the warning lists `auth-env` and
  the login file.
- **`pass-env`**: host variables forwarded when set, which are not credentials (a base url).
- **`parse`**: a script that reads the command's stdout (`harness-parse.md`): the live tool count, the summary,
  cost and a judge's final reply. without it there are none of those, and the whole stdout is the reply.
- **`models`**: the catalogue an agent's `model` is checked against (`openrouter`). without it there is no check.
  the one thing an entry names that is go.

with no `auth-env` and no `login` a harness needs no login, and pats warns about none. every harness takes an
agent's `effort` as `PATS_EFFORT`; what it means is the command's.

### the defaults

the defaults are a file of this format in the source tree, embedded in the binary. they hold the four harnesses of
today and their clis (and `pi-openrouter`, `harness-registry.md`), with the values `internal/agent/agent.go`,
`images/harness-versions` and the readers of `internal/eval` hold now. the per-release bump of `harness-pins.md`
edits its `clis`.

a test holds, for the defaults: every harness has a `cli`; every cli has a version and, for each of the two arches,
an artefact; every `parse` compiles; every `models` names a catalogue pats has.

### the overlay

one rule, wherever one table is laid over another: the machine's file over the defaults here, and a project's
harnesses over the result (`harness-registry.md`).

- **an entry named as one below it** changes that one: each key the entry sets replaces that key's whole value, and
  the keys it leaves out keep theirs. `egress:` restates both its lists; `artefacts:` restates every file.
- **an entry with a new name** is a new cli or harness, and is written whole.
- absent means keep, and an explicit null means unset (`parse: ~`), as in a vector (`vectors.md`, merge). a null
  holds through every layer above the one it clears: nothing below fills the key again.
- an entry that sets `version` sets `artefacts`, and the reverse: a sha is of one version.
- nothing removes an entry, and there is no other inheritance: no entry names another to start from.

the result, checked as a whole, is what pats runs with: every harness has a `run`; its `cli` and `models`, where set,
name something that exists; its `parse` compiles; its `egress` entries are hosts the proxy's parser takes; `login` has a
cli with a `login-file`. every cli has a version and at least one artefact, each with an `arch` and a `sha256`. an arch
a cli has no artefact for is an error only at an init that needs it. names are lower-case letters, digits and `-`; a
variable in `auth-env`, `pass-env` or `home-env` is a variable's name, not `HOME` or a `PATS_` one (pats sets those),
and named once across the three; a `bin` stays inside its artefact; a `login-file` and a `home-env` path stay under the
home. an unknown key, in either table or at the top, is an error. each error names the file, the line and the entry, and
stops every command that reads the file: a machine config that is wrong is never half applied.

it is read by every command that reads a `pats.yaml`, before it, and by the three that need the tables with no
project: `pats config`, `pats harness pin` and `pats harness parse`. no other command reads it.

### what a project may not say

a `pats.yaml` lays its `kind: harness` items over the machine's harnesses by the rule above
(`harness-registry.md`). it has no `clis`: a cli, its `artefacts`, its `login-file` and its `home-env` are the
machine's. so a project can pin a cli (`version` and `sha256`, on a harness item) and can ask for a cli's login
(`login: true`), and cannot say where bytes come from or which file leaves the user's home.

an agent whose `harness` is found in neither file is a load error that says where it looked: not a default of this
pats, not in the machine config (its path), not an item of a harness vector. a project that names a harness from
one machine's file runs only on machines that have it.

### seeing it

a file outside the repository now changes what a run does, so it shows:

- **`pats config`** prints the effective tables as yaml, under a comment that names the file and says whether it
  is there. `pats config --defaults` prints the defaults: the text to copy an entry from.
- **`pats init` and `pats run`** print one line when the file changes or adds anything, naming each:
  `INFO machine config ~/.config/pats/config.yaml: changes clis.codex.artefacts,
  harnesses.claude-cli-keyless.egress; adds harnesses.pi-openrouter`.
- **`metadata.json`** of a pair gains `harness_from` (`default`, `machine` or `project`: the last place that set a
  key of the harness or of its cli) and `harness_sha256`, a digest of the resolved harness entry and of its cli's
  version, `login-file`, `home-env`, and each artefact's `arch`, `sha256` and `bin`, but not its url. it is the
  definition the run asked for: two pairs with one digest were asked to run the same thing, whichever machine and
  pats release they ran on. what was in the image is `image`, `components` and `placed` (`harness-images.md`): a
  stale image, or a `provision` that swapped the cli, shows there and not in the digest.
- **`pats list harnesses`** shows each harness with where it is from.

`pats harness pin <cli> <version>` (`harness-images.md`) prints a `clis` entry's `version` and `artefacts`, for
this file or the defaults, and the `version` and `sha256` of a project's item.

## limits

- **an override outlives its reason.** a changed entry keeps its changed keys after a release fixes them, and
  hides what the release changed in those keys (a host the vendor added since). the line `pats init` and
  `pats run` print is the reminder; nothing expires.
- **the file is written for one pats.** a key a later pats renames is an unknown key, and an error, until the file
  is edited.
- **a machine pin moves every project** that does not pin for itself, as a pats release does. it shows in the line
  above, in the image's stale warning and in `components`.
- **a project that brings a new cli** cannot be whole on its own: a `clis` entry is the machine's. its files come
  from `provision`, or each machine adds the entry.
- **a `pats.yaml` is trusted as the scripts it names are.** its `auth-env` and `pass-env` hand a sandbox whichever
  host variables it lists, and its `egress` lets them out. that is so today for `step-env`.

## later

- **the trust variables.** the six variables that point a tls stack at the run's CA bundle
  (`src/sandbox/egress_material.go`, `trustEnv`) are the same kind of fact: which variable each tool reads. a
  third table, when a tool pats does not know needs one.
- **where the sandbox image comes from**: a registry mirror for `ghcr.io/lczyk/pats/sandbox`.
- **`clis` in a project**, as a vector of its own kind, if a project has to bring a cli no machine has.

## not doing

- **a seeded copy.** the docker snap ships a base `daemon.json` and keeps a writable copy in its data dir. a copy of
  the defaults would freeze the pins of the release that wrote it; this file holds only what differs.
- **`snap set`.** the docker snap also merges `snap set docker daemon.<key>` into its file. that is system-wide,
  root's, and flat keys for what are nested tables here: a second way to write one file.
- **layers below the machine** (a system-wide `/etc` file, a per-project machine file). one optional file.
- **inheritance by name** (an entry that says which other it is like). pats has none elsewhere. an entry changes
  the one of its name, or is written whole from a copy `pats config` prints.
- **environment variables for single keys.** `PATS_OPENROUTER_URL` stays what it is, a seam for tests.

## tests

- the defaults pass their test (above), and with no machine file the harnesses, their commands, hosts and
  variables are today's: the existing tests of each run unchanged.
- where: an absolute, a relative and no `XDG_CONFIG_HOME`; no file; an unreadable file is an error, not a skip.
- the overlay: a key replaced whole, a key left out kept, a key unset by a null, `login: false` over `true`; a new
  entry missing `run`; a `cli` or `models` that names nothing; a `parse` that does not compile; an `egress` entry
  with a path; two artefacts for one arch; `version` set without `artefacts`; `login` on a cli with no
  `login-file`; a harness with no `cli`, allowed; an unknown key; a bad name; a `bin` that leaves its artefact and
  a `login-file` that leaves the home. each error names file, line and entry.
- an agent on a harness found nowhere, with the three places in the message.
- seeing it: `pats config` with and without a file; the note names a changed key and an added entry, and is absent
  with no file and with an empty one; `harness_from` for each of the three; `harness_sha256` moves with `run` and
  with the cli's sha, and not with an artefact's url.
- a machine entry end to end, on the fake engine: a harness with a new cli is fetched from a local server, placed,
  stamped and run, and its `parse` fills the summary.
- the snap: `pats config` names `~/snap/pats/common/config/pats/config.yaml`, and the shipped `lxc` and docker
  clients find their own config as before with `XDG_CONFIG_HOME` set.

## decided, open to change

- **one overlay rule at every layer.** defaults, machine, project: an entry of the same name changes key by key, a
  new name is whole.
- **the login file and `home-env` are the cli's.** where a cli keeps its login is a fact about the cli, and with it
  on the cli a project never names a path under the home.
- **a pin on the cli in the machine's file, on the harness in a project.** the machine has the `clis` table; a
  project has none, and pins through the harness its agent names.
- **the name is `config.yaml`, printed by `pats config`.** the docs call it the machine config, and a `pats.yaml`
  the project's, so the two are not mistaken for each other.
- **`effort` and `cost` are not keys.** all four harnesses take an effort, and a harness that does not ignores a
  variable. cost is what `parse` reports.

## open questions

- whether a machine pin (`clis.<name>.version`) should be allowed, or the machine may only say where a version
  comes from and a pin stays the release's and the project's.
- whether the keys of an entry and the calls of `parse` are part of what 1.0 promises to keep, or the promise is
  only the file's place, that it is optional, and that a wrong one is an error.
