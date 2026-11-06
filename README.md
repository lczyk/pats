# pats

![GitHub go.mod Go version](https://img.shields.io/github/go-mod/go-version/lczyk/pats)
![GitHub Tag](https://img.shields.io/github/v/tag/lczyk/pats?label=release)
[![lint_and_test](https://github.com/lczyk/pats/actions/workflows/lint_and_test.yml/badge.svg)](https://github.com/lczyk/pats/actions/workflows/lint_and_test.yml)
![GitHub License](https://img.shields.io/github/license/lczyk/pats)

<img src="assets/robot_cat.png" alt="pats logo" width="30">

score-based testing framework for testing and scoring agent tasks.

to use pats, put `pats.yaml` in your project root. see [`pats.example.yaml`](./pats.example.yaml) for an example of such a configuration. 

`pats.yaml` is a mapping of vectors. each top-level key is a vector's id, and a vector has a `kind` (`sandbox`, `agent`, `task`, `scorer` or `suite`), optional `defaults`, and non-empty `items` keyed by id. an item inherits every default it doesn't set and overrides whole fields; an explicit null (`collect: ~`) unsets one, and an empty item (`curl:`) inherits everything. a task is a single scenario given to the model under test. a scorer runs on the output of an agent run and scores one aspect of it, 0.0 - 1.0. ids are the primary way to refer to things, and they double as human-readable names: one namespace per kind, holding that kind's vectors and items, where an id names a set of items (a vector its items, an item itself) and must name exactly one set -- so a vector may share its id with its sole item, a task and a scorer may share an id, but a task vector `smoke` next to an unrelated task `smoke` is an error. no `/`, `$` or whitespace in ids. defaults are per-item templates: `${task}` / `${agent}` / `${scorer}` / `${sandbox}` expand to the item's id in that kind's fields and `${vector}` to its vector's id, everywhere except inline content -- a literal prompt, an inline script, `env` (any other `${x}` is a load error; inside a flow mapping `{...}` quote the value, else the `}` closes the mapping, and spell an empty item `a: ~`). to share defaults between vectors use a yaml anchor: `defaults: &x {...}` in one, `defaults: *x` or `defaults: {<<: *x, timeout: 5m}` in another.

a task's prompt is one of `prompt` (the literal text), `prompt-file` (a file to read) or `prompt-cmd` (a command whose stdout is the prompt; it runs after `prepare`, so it can look at the prepared workdir). the fields that run something -- a task's `prepare` and `collect`, a scorer's `score` -- each take one of two forms. the plain key is an inline script: a body starting with `#!` runs as an executable (its shebang picks the interpreter), one without runs under `/bin/sh -e`, and `${...}` in it is the shell's, not a pats template. the `-cmd` key (`prepare-cmd`, `collect-cmd`, `score-cmd`) is a command line, exec'd directly with no shell: the value is tokenised into argv (posix-sh quoting, but no `$`-expansion, globbing, or pipes), a first word containing a `/` is a file resolved from the directory of the file it's written in, and a bare first word is looked up on `PATH`. every host script gets `<pats root>/bin/` prepended to its `PATH`, so shared helpers go there and are called by name. an item sets at most one key of each of these slots, and in `defaults` a slot merges as a unit -- setting any key of it replaces the whole slot. host scripts run in the directory of the file their item was defined in: the pats root for `pats.yaml`, the task's own directory for a `task.yaml` (below). they get these env vars: `PATS_ROOT` (the config dir, absolute), `PATS_*_ID` (`PATS_TASK_ID`, `PATS_AGENT_ID`, and for scorers `PATS_SCORER_ID`), `PATS_MODEL`, `PATS_AGENT_HARNESS`, `PATS_TASK_DIR` (the directory the task was defined in, absolute), `PATS_OUTPUT_DIR` (this pair's `<run>/<agent>/<task>`), `PATS_RUN_DIR` (the run dir holding every pair's output) and `PATS_RUN_ID` (its basename, e.g. `003-20260621-fluffy-bunny` -- the agent in the sandbox gets this one too); a task's scripts additionally get `PATS_WORKDIR`, `PATS_AGENT_HOME` and the task's own `env:` (a mapping of variable names to values; it merges key by key over defaults, and names may not start with `PATS_` or be `PATH`). scorers don't get a workdir -- they read the collected output dir. `PATS_AGENT_HOME` is the host side of the agent's sandbox `HOME`, created (with the harness creds already copied in) before `prepare` runs, so `prepare` can seed harness config there -- e.g. a `~/.claude/settings.json` with hooks, or a codex `config.toml`. copy real files, not symlinks into the host tree, which don't resolve in the sandbox. it lives until the pair is done, so `collect` can read back what the agent left in it. both are host paths: inside the sandbox the workdir is mounted at `/workspace` (the agent's cwd) and the agent home at `/pats-home` (its `$HOME`), so a seeded file that must hold an absolute path -- a git remote url, say -- uses those.

a task vector can take `dir: <path>` instead of `items`, to keep each task in a directory of its own. every directory under that path holding a `task.yaml` is one task, at any depth; its id is the directory's name, and `task.yaml` holds the same fields as an inline task. directories starting with `.` are skipped, and a `task.yaml` below another task's directory is an error. a `dir` vector takes only `kind` and `dir` -- each task is configured by its own `task.yaml`, nothing is inherited from `pats.yaml`. its scripts run in its directory, so they reach their own fixtures by plain relative paths:

```yaml
# pats.yaml
knockout:
  kind: task
  dir: cases/knockout
```

```yaml
# cases/knockout/curl/task.yaml
env:
  BRANCH: ubuntu-26.04
prompt: |
  author a slice for the package `curl`.
prepare: prepare-knockout       # bin/prepare-knockout, found on PATH
collect-cmd: ./collect.sh       # next to this task.yaml
```

an `agent` runs a harness -- an agent cli that runs the agentic loop in a sandbox. `harness:` picks one of four, each baking in the cli and how it authenticates. `opencode-openrouter` is the `opencode` cli with models served via openrouter (reads `OPENROUTER_API_KEY` from the env; write the model without the `openrouter/` prefix, it's added for you). `claude-cli-keyless` is the `claude` cli, keyless: your oauth creds (`~/.claude/.credentials.json`) are copied into the sandbox, and the model is a raw anthropic id. `codex-cli-keyless` is the `codex` cli, keyless: your chatgpt login (`~/.codex/auth.json`) is copied into an isolated `CODEX_HOME`, and the model is a raw openai id -- a codex login kept only in the os keyring has no file to copy, so switch codex to file credential storage first. `copilot-cli-keyless` is the `copilot` cli, keyless: a github token read from `COPILOT_GITHUB_TOKEN` in the env (an ambient `GH_TOKEN`/`GITHUB_TOKEN` is deliberately not forwarded), and the model is a copilot model id (e.g. `claude-sonnet-4.5`, `gpt-5`) -- copilot's default login lives in the os keychain, so mint the token first: macos `security find-generic-password -s copilot-cli -w`, or `gh auth token`. an agent can set `effort:` -- reasoning effort, mapped to claude's `--effort` (low|medium|high), codex's `model_reasoning_effort`, copilot's `--effort` (none|minimal|low|medium|high|xhigh|max), and opencode's `--variant` (provider-specific, e.g. high|max|minimal) respectively. all four harnesses log a machine-readable event stream to `stdout.log` (claude: stream-json; codex: `--json`; copilot: `--output-format json`; opencode: `--format json --thinking`), which feeds the live tool counter and the cost/token summary in each pair's `metadata.json` -- keyless codex and copilot are billed against the subscription, so they report tokens but no per-run dollar cost (copilot additionally records its premium-request count). every agent can run tasks; an agent you don't list in any suite simply never runs as one -- which is how you keep a stronger model around purely to score a weaker one (it must then be some scorer's `agent`, else it's flagged as an orphan). scorers come in two flavours, told apart by their fields. a script scorer sets `score` (inline) or `score-cmd`; a script's shebang picks the interpreter and deps (e.g. `#!/usr/bin/env -S uv run --script` with inline pep-723 deps -- trampoline to python/spacy/whatever you like). it exits 0 and prints a float in `[0,1]` on its first stdout line, or `na` to skip (a non-zero exit is a failure). a uniform layout is one vector default, `score-cmd: scorers/${scorer}.sh`. an inline `score` runs from a temp file, so a python scorer importing a module next to it should use `score-cmd` instead. the other flavour sets `agent:` and one of `prompt` / `prompt-file` / `prompt-cmd` (ask that agent to judge) -- **not implemented yet**; configuring one is rejected at config validation until it lands.

finally, suites select what runs. a suite is a named (`agents`, `tasks`, `scorers`) triple, and both cross-products are implied within it: `pats run` runs its `agents` x `tasks`, `pats score` scores its `tasks` x `scorers`. each axis takes a scalar or a list of ids of that kind, vector or item, and a vector id expands to its items -- `tasks: [slicer, frobnicator]` is every task in the `slicer` vector plus one stray. a suite vector takes `defaults` like any other (`defaults: {agents: all}` once, instead of on every suite). `scorers` may be empty (a run-only suite). every agent, task, and scorer must land in at least one suite (agents referenced by a scorer's `agent` are exempt), so forgetting to wire a new item in is a load-time error rather than a silent coverage hole. suites may overlap (a `smoke` suite inside a `full` one) -- duplicate pairs are deduped, not errors; an item named twice within one axis is. scores aggregate as plain means (per pair over its scorers, then per agent over its tasks). narrow a run or score with `-s <id>` (a suite or a suite vector), `-a` and `-t` (agents / tasks, or their vectors).

there are two (and a half) stages of a pats workflow. `pats run` runs your suites -- your agents across their tasks -- and saves the result to `.pats/runs/<run_number>-<date_slug>-<friendly-name>/` (e.g. `003-20260701-jacquard-runner`; the two words are generated from the numeric prefix) -- that usually takes a hot moment. then `pats score` scores the most recent run, or you can ofc instruct it to test an older run too. note that this is not intended as a way to keep old historic runs and therefore, if you change the scorers and rerun on the old run, you *are* going to just run the new scorers. finally, if you have any agentic scorers they are not run by default, but only with `--agentic` flag (note: agent scorers aren't implemented yet, so `--agentic` currently has nothing to run).

agents are always run in a sandbox -- we never run without one. each agent names the sandbox it wants via `sandbox: <id>` (if only one sandbox is defined, it's the default). a sandbox has a `driver` (`docker`, the default; `podman`; or `bwrap`) and, under a container driver, at most one of `image` or `build`. with neither, it runs the published pats sandbox -- an ubuntu-based fat image carrying all the harness clis -- pinned to your pats version (`ghcr.io/lczyk/pats/sandbox:v<version>`); `image: <ref>` runs any other image instead (`ghcr.io/lczyk/pats/sandbox:latest` follows the newest stable release). alternatively `build: <path>` names a docker build context (a dir with a Dockerfile, or a Dockerfile path, relative to `pats.yaml`) that pats builds at the start of every run -- the layer cache makes the no-change case a no-op, and the built image id is recorded in each pair's `metadata.json`. the build gets the pinned pats sandbox as a build arg, so a dockerfile that extends it writes `ARG PATS_SANDBOX` / `FROM ${PATS_SANDBOX}` and tracks the pats it runs under. one guard: if the build context contains `.pats/` (run artifacts) and the dockerfile reads the context (a `COPY`/`ADD` not sourced `--from` another stage), the effective `.dockerignore` must exclude it (a literal `.pats/` line) or the run refuses to start -- pats won't bake your run history into the image, and won't edit ignore files for you. a context-less dockerfile (no `COPY`/`ADD`) needs no ignore file. `bwrap` (linux-only) is not implemented yet and is rejected at validation; `container` works anywhere docker does (incl. macos, where docker is itself a linux vm).

a sandbox can also declare an `egress` policy: `mode: open` (the default -- unrestricted network), `mode: none` (`--network none`), or `mode: proxy` -- a filtering sidecar that allows/denies by host (`default: deny` + `allow: [...]`, or `default: allow` + `deny: [...]`) and writes a per-pair audit log (`egress.log`; denied hosts also land in the run metadata, a built-in cheat detector). the pairing is enforced at load: a list that the mode or the default would ignore (an `allow:` under `default: allow`, a `deny:` under `default: deny`, either under `open`/`none`) is a config error rather than a line that quietly does nothing. under an allowlisting proxy, the hosts the harness itself needs -- the inference api, token refresh, opencode's startup fetches -- are merged in automatically per harness, so the config only lists what the *task* needs (e.g. apt mirrors). when host granularity isn't enough (allow github, but not one specific repo), `mode: mitm-proxy` is a superset of `proxy` adding url-level rules: hosts named in `deny-urls` (host-anchored patterns, e.g. `"github.com/*/chisel-releases*"`) get their tls terminated with a per-run CA the sandbox is told to trust, so each request is filtered by full url -- query string included, and a `..` in the path doesn't dodge a rule; all other hosts stay undecrypted tunnels. see `docs/proposals/network-egress.md` for the design.

## installation

pats is a single go binary. you need go 1.26+ to build it, and docker to run the sandboxes.

this is the 1.x line, released as `1.0.0-beta.N` pre-releases from the `1.x` branch until 1.0. `main` carries 0.x, and `@latest` resolves to it until 1.0 ships, so name the beta explicitly. 1.x reads a different `pats.yaml` shape from 0.x (see [`pats.example.yaml`](./pats.example.yaml)).

put it on your PATH with `go install`:

```sh
go install github.com/lczyk/pats/cmd/pats@v1.0.0-beta.1    # installs into $(go env GOBIN)
```

or run it without installing at all:

```sh
go run github.com/lczyk/pats/cmd/pats@v1.0.0-beta.1 run
go run github.com/lczyk/pats/cmd/pats@v1.0.0-beta.1 score
```

from a clone, `make install` builds the binary (upx-compressed if `upx` is on PATH) and symlinks it into `~/.local/bin`:

```sh
git clone -b 1.x https://github.com/lczyk/pats && cd pats
make install
```

### published images

pats publishes two images per release, both tagged `v<version>` and pinned by the binary of that version: the default sandbox (`ghcr.io/lczyk/pats/sandbox`, used by a sandbox that names no `image` or `build`) and the egress proxy (`ghcr.io/lczyk/pats/egress-proxy`, the sidecar behind `mode: proxy` / `mitm-proxy`). `:latest` on both follows the newest stable release. a released install pulls them from ghcr automatically on first use. the version is the module tag for a module install, else the `VERSION` file of the checkout; `pats --version` shows it. so a clone build pins to the images of the release its tree is based on, whether or not the tree has moved on -- build the images locally and pats finds those instead of pulling:

```sh
make images          # the sandbox, tagged :v<version>, :latest and :26.04 for the host arch
make egress-image    # the proxy, tagged :latest and :v<version>
```

## examples

typical loop -- run the suites, then score them:

```sh
pats run                 # run all agent x task pairs, save to .pats/runs/<n>-<date>-<adj>-<noun>/
pats score               # score the latest run (tasks x scorers)
```

narrowing and parallelism:

```sh
pats run -a haiku-claude-cli -t simple                  # just one pair
pats run -t write-readme                                # every task in that vector
pats run -s readme                                      # just one suite
pats run -j-1                                           # parallel pairs, auto job count
pats score -r jacquard-runner                           # score an older run by its friendly name
pats score -r .pats/runs/003-20260701-jacquard-runner   # ... or by path
```

all commands take `-c <path>` to point at a `pats.yaml` other than the one in the cwd.

### example config

the model under test is `claude-haiku-4-5`, run two ways: through the `claude` cli (`claude-cli-keyless`) and through `opencode` over openrouter (`opencode-openrouter`). running the same model under two harnesses isolates the harness's effect from the model's.

it has one task vector, `write-readme`, with 3 tasks: `simple`, `hard`, `monorepo`. the vector's defaults name the prompt file and scripts once (`prompt-file: prompts/${vector}-${task}.txt`, `prepare-cmd: scripts/prepare.sh --${task}`) and each task is just its id. judging by those, these tasks seem to be testing some behaviour related to readme authorship. they may be about testing the model itself, but also could be testing some skill or instruction given to the model.

there are 4 scorers in the `style` vector: `only-ascii`, `title-correct`, `no-finite-verbs` and `no-colloquialisms`. former 2 scorers are implemented in a purely imperative manner. `only-ascii` deducts 0.1 form the score for each occurrence of excessive whitespace. `title-correct` is much closer to a binary yes/no question so it could simply output 1.0/0.0 respectively. latter 2 scorers are more _"fuzzy"_. `no-finite-verbs` scorer uses [`spacy`](https://pypi.org/project/spacy/) -- a python NLP library. `no-colloquialisms` is agentic, and therefore nondeterministic: it uses an agent to score the results (commented out in the example until agent scorers are implemented). there is, in fact, one more agent defined for that purpose -- `judge`, a `claude-cli-keyless` agent running `sonnet`. it's left out of every suite, so it never runs as a task -- it only scores, letting us judge a weaker model (`haiku`) with a stronger one (`sonnet`).
