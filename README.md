# pats

![GitHub go.mod Go version](https://img.shields.io/github/go-mod/go-version/lczyk/pats)
![GitHub Tag](https://img.shields.io/github/v/tag/lczyk/pats?label=release)
[![lint_and_test](https://github.com/lczyk/pats/actions/workflows/lint_and_test.yml/badge.svg)](https://github.com/lczyk/pats/actions/workflows/lint_and_test.yml)
[![pats](https://snapcraft.io/pats/badge.svg)](https://snapcraft.io/pats)
![GitHub License](https://img.shields.io/github/license/lczyk/pats)

<img src="assets/robot_cat.png" alt="pats logo" width="30">

score-based testing framework for testing and scoring agent tasks.

to use pats, put `pats.yaml` in your project root. see [`pats.example.yaml`](./pats.example.yaml) for an example of such a configuration. 

`pats.yaml` is a mapping of vectors. each top-level key is a vector's name, and a vector has a `kind` (`sandbox`, `harness`, `agent`, `task`, `scorer` or `suite`), optional `defaults`, and non-empty `items` keyed by name. an item inherits every default it doesn't set and overrides whole fields; an explicit null (`collect: ~`) unsets one, and an empty item (`curl:`) inherits everything. a task is a single scenario given to the model under test. a scorer runs on the output of an agent run and scores one aspect of it, 0.0 - 1.0. names are how you refer to things: one namespace per kind, holding that kind's vectors and items, where a name names a set of items (a vector its items, an item itself) and must name exactly one set -- so a vector may share its name with its sole item, a task and a scorer may share a name, but a task vector `smoke` next to an unrelated task `smoke` is an error. a name follows chisel's rule for a package name: lowercase letters, digits, dots, pluses and single hyphens, a letter or digit first, no hyphen last, at least 2 characters (`^[a-z0-9](?:-?[.a-z0-9+]){1,}$`), so any debian package name fits (`python3.14`, `libstdc++6`) and no `/`, `_` or leading `.` does. nothing in `pats.yaml` is templated: a default is inherited as written, and a step that needs the name it runs for reads it from its env (`$PATS_TASK_NAME`, `$PATS_TASK_VECTOR`, `$PATS_SCORER_NAME`, below). a field holding `${task}`, `${agent}`, `${scorer}`, `${sandbox}` or `${vector}` -- from when pats expanded them -- is a load error naming the env var to use, except in a `prompt`, whose text reaches the agent as written (inside a flow mapping `{...}` quote a value holding `}`, and spell an empty item `a: ~`). to share defaults between vectors use a yaml anchor: `defaults: &x {...}` in one, `defaults: *x` or `defaults: {<<: *x, timeout: 5m}` in another.

a task's prompt is one of `prompt` (the literal text), `prompt-file` (a file to read) or `prompt-cmd` (a script whose stdout is the prompt; it runs after `stage`, so it can look at the staged workdir). an item sets at most one of the three, and in `defaults` they merge as a unit -- setting any of them replaces the others. every field that runs something -- a task's `stage`, `prepare` and `collect`, `prompt-cmd`, a scorer's `score`, a sandbox's `provision` -- is a shell script: one starting with `#!` runs as an executable (its shebang picks the interpreter), any other under `/bin/sh -e`. a one-liner is a script too: `collect: ./collect.sh --fast` runs that file with that flag, `score: scorers/scorer_$PATS_SCORER_NAME.py` expands the name. (`prompt-cmd` used to be split into words and run without a shell; a value that relied on that, passing a `$` or `*` as text, now gets the shell's meaning. the `stage-cmd`, `prepare-cmd`, `collect-cmd`, `score-cmd` and `provision-cmd` forms are gone.) a multi-line script runs from a temp file, so a python script inline can't import a module beside it; `score: ./scorer.py` runs the file itself, which can. every host script gets `<pats root>/bin/` prepended to its `PATH`, so shared helpers go there and are called by name. host scripts run in the directory of the file their item was defined in: the pats root for `pats.yaml`, the task's own directory for a `task.yaml` (below). they get these env vars: `PATS_ROOT` (the config dir, absolute), `PATS_RUN_DIR` (the run dir holding every pair's output) and `PATS_RUN_NAME` (its basename, e.g. `003-20260621-fluffy-bunny` -- the agent in the sandbox gets this one too). a task's scripts get the run they belong to: `PATS_TASK_NAME`, `PATS_TASK_VECTOR`, `PATS_TASK_DIR` (the directory the task was defined in, absolute), `PATS_AGENT_NAME`, `PATS_MODEL`, `PATS_AGENT_HARNESS`, `PATS_OUTPUT_DIR` (this pair's `<run>/<agent>/<task>`), `PATS_WORKDIR`, `PATS_AGENT_HOME` and the task's own `env:` (a mapping of variable names to values; it merges key by key over defaults, and names may not start with `PATS_` or be `PATH`). a scorer gets `PATS_SCORER_NAME`, `PATS_SCORER_VECTOR` and the pair it grades, under one prefix: `PATS_SCORED_DIR` (that pair's output dir), `PATS_SCORED_AGENT_NAME`, `PATS_SCORED_TASK_NAME`, `PATS_SCORED_TASK_DIR`, `PATS_SCORED_MODEL` and `PATS_SCORED_HARNESS`. a script scorer gets no workdir and none of the unprefixed names: it reads the graded pair's dir. a step sees only the names pats sets for it, never ones inherited from the environment pats runs in, and everything it starts ends with it: ctrl-c kills the lot, and a process it left running in the background is killed when it returns. `PATS_AGENT_HOME` is the host side of the agent's sandbox `HOME`, created (with the harness creds already copied in) before `stage` runs, so `stage` can seed harness config there -- e.g. a `~/.claude/settings.json` with hooks, or a codex `config.toml`. copy real files, not symlinks into the host tree, which don't resolve in the sandbox. it lives until the pair is done, so `collect` can read back what the agent left in it. both are host paths: inside the sandbox the workdir is mounted at `/workspace` (the agent's cwd) and the agent home at `/pats-home` (its `$HOME`), so a seeded file that must hold an absolute path -- a git remote url, say -- uses those. a task can also set `prepare`: it runs inside the sandbox, as root, after `stage` and before the agent -- spread's prepare, for system state the agent needs (packages, services, users). its script and the pats root's `bin/` -- and nothing else from the host, so a helper it calls lives in `bin/` -- are pushed to `/pats-prepare`, run with `bin/` first on `PATH` and the workdir as cwd, then removed before the agent starts, since the agent can read everything else. it gets the task's `env:` and `PATS_TASK_NAME`, `PATS_TASK_VECTOR`, `PATS_AGENT_NAME`, `PATS_AGENT_HARNESS`, `PATS_MODEL`, `PATS_RUN_NAME`, `PATS_WORKDIR` (`/workspace`) and `PATS_AGENT_HOME` (`/pats-home`) -- no host paths -- and runs under the sandbox's egress policy, so anything secret belongs in `stage`. what it leaves in the workdir and home is handed to the agent user. the task `timeout` covers the agent only; bringing the sandbox up and preparing it has its own ten-minute budget. a failed `stage`, start or `prepare` is the pair's error, named in its `metadata.json`, with the step's output in `stage.log` / `prepare.log` in the pair dir.

a task or a scorer can run its host steps in a sandbox instead: `step-sandbox: <sandbox>` names an item of the `sandbox` vector, and its `stage`, `prompt-cmd` and `collect` (a task's or an agent scorer's) or `score` (a script scorer's) run there, at their host paths, so a script that works on the host works in the box when the tools it calls are there. unset, they run on the host as above. the box sees the work tree that holds the pats root read-only (its runs dir and pats's cache masked), and writable only what the step writes: `stage` and `prompt-cmd` the workdir, the pair's output dir and `PATS_AGENT_HOME`, `collect` the workdir and the output dir, `score` nothing (the run dir is read-only). the host's environment stays out: `step-env: [NAME, ...]` passes the named variables in, and the box has a `HOME` of its own. the step sandbox's own `egress:` applies, never the agent's, its audit in `stage-egress.log` / `collect-egress.log` (and `.steps/` in the run dir for scorers). a `dir` task sets `step-sandbox` in its own `task.yaml`. a build without the host driver (the strict snap) runs no step on the host, so there `step-sandbox` is required on every item with a host step. see [docs/spec/step-sandbox.md](./docs/spec/step-sandbox.md) for what a box binds and the limits.

a task vector can take `dir: <path>` instead of `items`, to keep each task in a directory of its own. every directory under that path holding a `task.yaml` is one task, at any depth; its name is the directory's name, and `task.yaml` holds the same fields as an inline task. directories starting with `.` are skipped, and a `task.yaml` below another task's directory is an error. a `dir` vector takes only `kind` and `dir` -- each task is configured by its own `task.yaml`, nothing is inherited from `pats.yaml`. its scripts run in its directory, so they reach their own fixtures by plain relative paths:

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
stage: prepare-knockout         # bin/prepare-knockout, found on PATH
collect: ./collect.sh           # next to this task.yaml
```

an `agent` runs a harness -- an agent cli that runs the agentic loop in a sandbox. `harness:` picks one of five, each baking in the cli and how it authenticates. `opencode-openrouter` is the `opencode` cli with models served via openrouter (reads `OPENROUTER_API_KEY` from the env; write the model without the `openrouter/` prefix, it's added for you). `pi-openrouter` is the `pi` cli over openrouter too, with the same key and the same model ids. `claude-cli-keyless` is the `claude` cli, keyless: your oauth creds (`~/.claude/.credentials.json`) are copied into the sandbox, and the model is a raw anthropic id. `codex-cli-keyless` is the `codex` cli, keyless: your chatgpt login (`~/.codex/auth.json`) is copied into an isolated `CODEX_HOME`, and the model is a raw openai id -- a codex login kept only in the os keyring has no file to copy, so switch codex to file credential storage first. `copilot-cli-keyless` is the `copilot` cli, keyless: a github token read from `COPILOT_GITHUB_TOKEN` in the env (an ambient `GH_TOKEN`/`GITHUB_TOKEN` is deliberately not forwarded), and the model is a copilot model id (e.g. `claude-sonnet-4.5`, `gpt-5`) -- copilot's default login lives in the os keychain, so mint the token first: macos `security find-generic-password -s copilot-cli -w`, or `gh auth token`. an agent can set `effort:` -- reasoning effort, mapped to claude's `--effort` (low|medium|high), codex's `model_reasoning_effort`, copilot's `--effort` (none|minimal|low|medium|high|xhigh|max), opencode's `--variant` (provider-specific, e.g. high|max|minimal) and pi's `--thinking` (off|minimal|low|medium|high|xhigh|max, and a level pi does not know is its default with a warning) respectively. all five harnesses log a machine-readable event stream to `stdout.log` (claude: stream-json; codex: `--json`; copilot: `--output-format json`; opencode: `--format json --thinking`; pi: `--mode json`), which feeds the live tool counter and the cost/token summary in each pair's `metadata.json` -- keyless codex and copilot are billed against the subscription, so they report tokens but no per-run dollar cost (copilot additionally records its premium-request count). a harness of your own that reports tokens and no cost, on a model of the openrouter catalogue (`models: openrouter`), is priced by pats at the catalogue's list prices: the summary marks it `cost_estimated`, the report shows it as `~$`, and a `budget` stops on it. it is an estimate (another provider on the route may charge another), and there is none under `--offline`. every agent can run tasks; an agent you don't list in any suite simply never runs as one -- which is how you keep a stronger model around purely to score a weaker one (it must then be some scorer's `agent`, else it's flagged as an orphan). scorers come in two flavours, told apart by their fields. a script scorer sets `score`, a script; its shebang picks the interpreter and deps (e.g. `#!/usr/bin/env -S uv run --script` with inline pep-723 deps -- trampoline to python/spacy/whatever you like). it exits 0 and prints a float in `[0,1]` on its first stdout line, or `na` to skip (a non-zero exit is a failure). a uniform layout is one vector default, `score: scorers/"$PATS_SCORER_NAME".sh`. a multi-line `score` runs from a temp file, so a python scorer importing a module next to it is a file of its own, run by path (`score: ./scorer.py`). the other flavour, an agent scorer, sets `agent:` (one agent, not a vector) and one of `prompt` / `prompt-file` / `prompt-cmd`: that agent judges the output, run by `pats judge` (below).

the five are the defaults pats ships, as data: `pats machine --defaults` prints them, each a command, the hosts it needs, how it logs in and a small script that reads its output. a machine can change one, or add a harness of its own, in `~/.config/pats/machine.yaml` (`$XDG_CONFIG_HOME/pats/machine.yaml` when that is set; in the snap, `~/snap/pats/common/config/pats/machine.yaml`), or in a file `--machine <path>` names for one invocation. an entry named as a default changes the keys it sets and keeps the rest, a null unsets one, and an entry with a new name is a harness an agent can name. the file is optional and holds only what differs: `pats machine` prints what it makes of the defaults, `pats list harnesses` says where each harness is from, `pats init` and `pats run` say so when it changes anything, and each pair's `metadata.json` records where its harness came from (`harness_from`) with a digest of its definition (`harness_hash`). a file that is wrong stops pats with the file, the line and the entry. `pats harness parse <harness> <stdout.log>` runs a harness's output script over a log, to write one or to see why a column reads zero. `pats harness pin <cli> <version>` fetches a cli at a version and prints its pin, the hash of its file for each arch (`hash: sha256:<hex>`, the one kind there is): what the source served just then, so trust on first use. what it prints is an entry of the same file's `clis` table, and with it there the next `pats init` places that version. the rules are in `docs/spec/machine-file.md` and `docs/spec/harness-parse.md`.

a project changes or adds harnesses the same way, in a `kind: harness` vector of its `pats.yaml`: an item named as a harness that exists (a default, or one the machine added) changes the keys it sets for this project, and an item with a new name is a harness of its own, written whole. what several items share goes in the vector's `defaults`. an agent need not be a model: a harness whose `run` never reads `$PATS_MODEL` takes agents with no `model:`, so a script that does nothing is the floor of a scorer and a reference solution its ceiling. a harness with no `parse` has no cost or tool count, and as a judge its whole stdout is its reply. preflight runs every harness once with `PATS_PREFLIGHT=1` set, so one that cannot answer a prompt cheaply exits 0 on it. an item may also pin its cli at another version than the machine's, with `version` and `hash` (a `sha256:<hex>` per arch, as `pats harness pin` prints them): `pats init` then places that version in the sandboxes its agents run in, from where the machine's `clis` entry says the cli is fetched. the two keys come together, since a pin is bytes, and two harnesses that pin one cli differently take a sandbox each. the one thing a project cannot define is a new cli for pats to fetch: what such a harness runs comes from the sandbox's `provision`. naming it as the item's `cli` all the same has `pats init` check the image has it and record its version (`<cli> --version`) in each pair's `metadata.json`, with nothing fetched or placed. `docs/spec/harness-registry.md` has the rules, `pats.example.yaml` an example.

finally, suites select what runs. a suite is a named (`agents`, `tasks`, `scorers`) triple, and both cross-products are implied within it: `pats run` runs its `agents` x `tasks`, `pats score` and `pats judge` score its `tasks` x `scorers` (script and agent scorers respectively). each axis takes a scalar or a list of names of that kind, vector or item, and a vector name expands to its items -- `tasks: [slicer, frobnicator]` is every task in the `slicer` vector plus one stray. a suite vector takes `defaults` like any other (`defaults: {agents: all}` once, instead of on every suite). `scorers` may be empty (a run-only suite). every agent, task, and scorer must land in at least one suite (agents referenced by a scorer's `agent` are exempt), so forgetting to wire a new item in is a load-time error rather than a silent coverage hole. suites may overlap (a `smoke` suite inside a `full` one) -- duplicate pairs are deduped, not errors; an item named twice within one axis is. a suite with `manual: true` stays out of a `pats run` that names no suite and runs only when `-S` names it or a suite vector holding it: the place for an ablation or a costly sweep. `pats list suites` marks it, and an `-a` / `-t` that only a manual suite holds says which `-S` reaches it. score, judge and init do not look at it: they act on what ran, and a bare `pats init` still makes every suite's sandboxes. scores aggregate as plain means (per pair over its scorers, then per agent over its tasks). narrow a run or score with `-S <name>` (a suite or a suite vector), `-a` and `-t` (agents / tasks, or their vectors), and a score also with `-s` (scorers, or their vectors). lowercase short flags name the item kinds, uppercase the suites that group them. the filters intersect, and one that matches nothing in the selected suites is an error. a narrowed score merges into the run's `scores.json`, replacing only the scores it recomputed, so rescoring one fixed scorer keeps everything else.

there are three (and a half) stages of a pats workflow. `pats init` makes every image your suites' sandboxes need -- pulls the published sandbox and egress proxy, builds `build:` and `base:` / `provision:` images, provisions lxd layers, fetches the harness clis a sandbox's agents run and places them in the images it composes -- and pins each by id in `.pats/images.json`. it is the slow step, run when you say so: after cloning, after changing a sandbox, after upgrading pats. it takes `run`'s selectors (`-S`, `-a`, `-t`) plus `--sandbox <name>` and `--driver <d>`, and `--force` rebuilds and re-pulls what exists. `pats run` never builds or pulls: it starts every sandbox from its pinned image, and stops before the first pair, naming the `pats init` that fixes it, when a sandbox is not pinned, its image is gone, or the pin was made against another docker daemon or lxd remote than the one answering now. a sandbox whose provision material, build context, base or image ref, or pats version changed since its init gets a warning naming the `pats init --sandbox <name>` that rebuilds it, and runs on its pinned image. init removes what it replaces: an image pats built or pulled that a pin moved off goes once no project on this machine pins it (container engines; `~/.cache/pats/projects` lists the projects), or once it has gone 30 days without a use (lxd, whose server other machines share); what it cannot remove yet stays listed in the pin file and is tried again at the next init. while preflight runs, `pats run` also looks upstream for newer versions of what its images came from -- an `image:`, the `FROM` images of a `build:` or `base:`, an lxd base -- within 5s for all of them, and warns about each that moved (`--offline` skips it). before that, an `opencode-openrouter` agent's model is looked up in openrouter's public catalogue: a model it does not list, or one that does not take tools, stops the run before any sandbox starts, naming the closest ids (`--offline` skips this too; a catalogue that cannot be read is only a warning). init also stamps the harness versions inside each image into the pin and every pair's `metadata.json` (`components`), and a run warns when they are not the versions this pats pins. the egress proxy is the exception: a proxy pinned by another pats version or build stops the run, since it speaks this pats's protocol. then `pats run` runs your suites -- your agents across their tasks -- and saves the result to `.pats/runs/<run_number>-<date_slug>-<friendly-name>/` (e.g. `003-20260701-jacquard-runner`; the two words are generated from the numeric prefix) -- that usually takes a hot moment. then `pats score` scores the most recent run with its script scorers, or you can ofc instruct it to test an older run too. note that this is not intended as a way to keep old historic runs and therefore, if you change the scorers and rerun on the old run, you *are* going to just run the new scorers. finally, if you have any agent scorers, `pats judge` runs them, before or after `pats score`: it costs money, so it is a command of its own.

```mermaid
flowchart TD
    yaml[pats.yaml] --> init[pats init]
    init -->|images pinned by id| pins[.pats/images.json]
    pins --> run[pats run]
    yaml --> run
    run -->|one dir per pair| rundir[.pats/runs/NNN-date-name/]
    rundir --> score[pats score]
    rundir --> judge[pats judge]
    score --> scores[scores.json]
    judge --> scores
    scores --> report[pats report]
    classDef pats stroke:#0072BD,stroke-width:2px
    classDef state stroke:#EDB120,stroke-width:2px
    class init,run,score,judge,report pats
    class yaml,pins,rundir,scores state
```

blue is a pats command, yellow is a file it reads or writes. one pair, start to end:

```mermaid
sequenceDiagram
    box rgba(0,114,189,0.12)
    participant H as host (pats)
    end
    box rgba(217,83,25,0.12)
    participant S as sandbox
    end
    box rgba(119,172,48,0.14)
    participant P as egress proxy
    end
    H->>H: stage (host script, makes the workdir)
    H->>S: start from the pinned image
    H->>P: start beside it, with the pair's policy
    H->>S: prepare (root, inside)
    H->>S: run the harness on the prompt
    S-->>P: every connection out
    P-->>H: audit line per request (egress.log)
    H->>S: sync the workdir back (lxd only)
    H->>H: collect (host script)
    H->>H: write metadata.json
```

what that leaves in the project (green is the egress proxy's audit):

```mermaid
flowchart LR
    proj[project dir] --> patsdir[.pats/]
    patsdir --> pinfile[images.json<br/>pins per target]
    patsdir --> initlogs[init/sandbox.log]
    patsdir --> runs[runs/NNN-date-adj-noun/]
    runs --> pair[agent/task/]
    pair --> meta[metadata.json]
    pair --> out[stdout.log, stage.log,<br/>prepare.log, collect.log]
    pair --> eg[egress.log]
    runs --> sc[scores.json]
    runs --> jd[.judge/scorer/agent/task/]
    classDef state stroke:#EDB120,stroke-width:2px
    classDef proxy stroke:#77AC30,stroke-width:2px
    class pinfile,meta,sc state
    class eg proxy
```

and per user, shared by every project on the machine:

```mermaid
flowchart LR
    cache[~/.cache/pats/] --> leases[run leases]
    cache --> tmp[temp dirs: ca, build, stamps]
    cache --> store[bwrap/rootfs/digest/]
    cache --> projects[projects: every project<br/>that ran init]
    classDef state stroke:#EDB120,stroke-width:2px
    class leases,projects state
```

`pats judge` makes each agent score a normal agent call on a task pats synthesises from the scorer: the judge agent runs in its own sandbox, through the same harness, egress, credentials, pins and preflight as a task's agent (so `pats init` makes the judge sandboxes too), and its spend shows as a pair's does. its workdir is a copy of what the judged agent produced -- the pair's `stdout.log` and whatever `collect` gathered, without the logs pats wrote about the run and without symlinks -- and the judge can never touch the run. an agent scorer takes a task's fields to shape what the judge sees: `stage` runs on the copy (to add ground truth, or take out an answer key the judge must not read), then `prepare`, `collect` (into the judge's own dir), `env`, `sudo`, `egress` (added to the judge sandbox's lists) and `timeout` (5m by default). its host steps get the judge's own run (`PATS_AGENT_NAME` is the judge, `PATS_WORKDIR` the copy, `PATS_OUTPUT_DIR` its own dir) and the graded pair as `PATS_SCORED_*`; nothing naming the graded agent or its model reaches the sandbox. pats appends a fixed footer to the prompt, and the last line of the judge's reply must be `SCORE: <a number between 0 and 1>` or `SCORE: na`. only a pair whose files came home is judged: one that never synced, or whose harness reported an error, is `na`, out of the mean, and counted per agent in the report's `failed` row; one with no `metadata.json` waits. `-N n` judges each score n times and scores it with the mean of the samples; `--runs n` judges the latest n runs in turn; `-j`, `--budget` (a cap over every judge call of the invocation) and `--offline` work as for `pats run`, and `-a` / `-t` / `-s` / `-S` narrow as for `pats score`. each judgement lands in `<run>/.judge/<scorer>/<agent>/<task>/`: a `verdict.json`, and a dir per sample holding the judge's logs, `metadata.json` and its own `verdict.json`. its number merges into `scores.json`, and a plain `pats score` keeps it. there is no cache: after editing a judge, `pats judge -s <scorer>` redoes its verdicts, which stand until then.

`pats run -N 5` (`--repeat`) runs the selection five times, one run after another, each its own run dir; the credential preflight and the upstream checks happen once. `pats score -N 5` scores the latest five runs, and `pats report -N 5` reports them together: each pair's mean over its scored runs with its sd and n, the worst status of its attempts with how many were not ok, and the cost of every attempt. its `--json` is document version 2 (`docs/spec/repeats.md`); one run keeps version 1. "the latest five" is the five newest run dirs, whoever made them. an interrupted `pats run` now exits 130.

`pats run --budget 20` stops starting pairs once the spend the harnesses reported reaches $20, across every repeat; running pairs finish, and the run warns how many did not start and exits non-zero. `budget: 2` on an agent stops one pair once its reported spend passes $2 (status `budget`, collected like a timeout). it acts when the harness reports cost: opencode does per step, so its pairs stop mid-run; claude does once at its end, so its pair is marked there with its work done; codex and copilot report none, and pats warns after preflight that the cap has nothing to check. both are limits on reported spend: a request in flight is paid before it is seen, and with `-j` the run can pass its cap by what the running pairs spend. the credential preflight's one prompt per agent is not counted.

`pats run -n` (`--dry-run`) does everything up to the first sandbox and lists the pairs instead, each with an estimate: the median cost of the same pair (agent, task, harness, model and effort) in past runs, or unknown. with `--json` the list is one json object a line on stdout and the log goes to stderr. it takes no lock, so it runs beside a live run.

output is quiet by default: `pats init` says what it does for each sandbox and what it pinned, `pats run` prints one verdict line per pair, and `pats score` prints the report. `-v` adds detail: build steps, each pair's stats and the run's totals, each score. `-vv` also streams the builds, the host scripts, the agent's stderr and the scorers' output. a pair's host scripts log to its pair dir either way (`stage.log`, `prepare.log`, `collect.log`), and a build to `.pats/init/<sandbox>.log`.

agents run in a sandbox -- the one exception is the `host` driver below, which you have to ask for by name. each agent names the sandbox it wants via `sandbox: <name>` (if only one sandbox is defined, it's the default). a sandbox has a `driver` (`docker`, the default; `podman`; `lxd`; `bwrap`; or `host`) and, under a container driver, at most one of `image` or `build`. with neither, it runs on the published pats sandbox -- ubuntu and the tools agents reach for (git, curl, python3, ripgrep, node), with no harness cli in it -- pinned to your pats version (`ghcr.io/lczyk/pats/sandbox:v<version>`); `image: <ref>` runs any other image instead, as it is (`ghcr.io/lczyk/pats/sandbox:latest` follows the newest stable release). alternatively `build: <path>` names a docker build context (a dir with a Dockerfile, or a Dockerfile path, relative to `pats.yaml`) that `pats init` builds -- an unchanged context reuses the image it built before, and the built image id is recorded in each pair's `metadata.json`. the cache key covers the context and the id of every `FROM` image: a base you refresh with `docker pull` triggers a rebuild, and an absent base is pulled first, but pats never pulls one that is present. the build gets the pinned pats sandbox as a build arg, so a dockerfile that extends it writes `ARG PATS_SANDBOX` / `FROM ${PATS_SANDBOX}` and tracks the pats it runs under. one guard: if the build context contains `.pats/` (run artifacts) and the dockerfile reads the context (a `COPY`/`ADD` not sourced `--from` another stage), the effective `.dockerignore` must exclude it (a literal `.pats/` line) or the run refuses to start -- pats won't bake your run history into the image, and won't edit ignore files for you. a context-less dockerfile (no `COPY`/`ADD`) needs no ignore file. `container` works anywhere docker does (incl. macos, where docker is itself a linux vm).

the harness clis are not part of an image pats publishes: `pats init` places them. a sandbox gets the clis its agents run and no others (every agent of the config pointed at it, whatever a `-a` selects), each at the version and sha256 the `clis` table pins for the image's arch. init fetches each from its vendor once per machine into `~/.cache/pats/harness`, checks its sha256 (a mismatch stops init, naming both), and puts it in the image under `/opt/pats/harness/<cli>` with `/usr/local/bin/<cli>` linking to it: a `COPY` layer each on docker and podman, under the `provision` layer, so a provision script finds them and has the last word; in pats's own layer on lxd; and under bwrap, which builds no image, as a directory beside the image that a session stacks over it. so the default sandbox is built too, once an agent points at it: a pull and a copy. pats places clis only in the images it composes, the default sandbox and `base:` / `provision:`. an `image:` or a `build:` is your whole statement of what is inside and brings its own, a `build:` on `FROM ${PATS_SANDBOX}` included, and init fails when one lacks a cli its agents run. an agent added later on a harness whose cli the image lacks stops `pats run` with the `pats init --sandbox <name>` that adds it, and a pin that moved since init is a warning. each pair's `metadata.json` records what was placed (`placed`: version, sha256 and tree id per cli). the vendors' files are glibc builds for `amd64` and `arm64`: a `base:` on musl is not supported. `docs/spec/harness-images.md` has the rules.

a docker, podman or lxd sandbox can make its image from `base` and `provision` instead: the provision script runs once per image, as root, on top of `base` (the pats sandbox when `base` is empty), with the pats root's `bin/` first on its `PATH` and nothing else from the host (`provision: provision-box` runs `bin/provision-box`); docker and podman compile the two into a generated dockerfile on the same cache as `build:`, and the next `pats init` rebuilds the image when the script, `bin/` or the base changes. `limits: {cpu, memory, disk}` caps a sandbox's resources (`disk` is lxd-only) and `max-parallel` caps how many of its pairs run at once, whatever `-j` says.

`driver: lxd` runs the agent in an lxd system container, or a virtual machine with `vm: true`, on the server an `lxc` remote names (`remote:`; the cli's default remote when omitted) -- a real system with systemd and snapd, a vm boundary for untrusted runs, no docker. pats drives it through the `lxc` cli in an lxd project `pats` it creates on first use (a restricted client certificate gets the admin command to run once). the image is provisioned on the server and cached there as `pats-img-<key>`: with no `base`, the pats sandbox's tools on `ubuntu:26.04` first (the same `images/provision-sandbox.sh` the docker image is built with), then the sandbox's own `provision` on top. each pair gets a fresh instance; the workdir and the agent home go in as tar over exec and come back after the agent, so the server needs no shared filesystem. the agent runs as a `pats` user, never root, and may `sudo` only with `sudo: true` on the sandbox (a task can override it). `profiles:` (in the `pats` project) and `config:` pass through, but an instance whose expanded config carries a nic pats did not give it, or any `raw.*` key, is refused. egress works as under docker: `none` is no nic at all; open is a bridge of the pair's own with nat and an acl dropping private, local and multicast ranges (so not the server's lan; the one host service it still reaches is lxd's own dns and dhcp on that bridge, which lxd lets through ahead of any acl); a proxy policy puts the agent alone with its proxy container on a bridge with no host address, the proxy reaching out through the shared `patsup` bridge. a server that also runs docker, or ufw, drops traffic on bridges it was not told about; preflight catches that and prints the fix an admin applies once (see [setting up the engines](#setting-up-the-engines)): for ufw its three rules per bridge, for docker either the two `iptables -I DOCKER-USER` lines for `patsbr+` and `patsup`, or, with docker 28 or later, `net.ipv4.ip_forward=1` set before docker starts (a `/etc/sysctl.d` file), which keeps docker's FORWARD policy at accept. the lxd acls still fence each bridge either way. `PATS_LXD_UPLINK` names an existing bridge (e.g. `lxdbr0`) for image builders to use meanwhile. `-j-1` (auto) sizes by the server's cpus. `pats clean` sweeps runs whose lease lapsed (each run holds one on the server while it lives); it reads a lease's time against its own clock, so the machine running it and the ones running pats should keep their clocks within a few minutes of each other (ntp is plenty). `docs/spec/lxd.md` is the design.

`driver: bwrap` (linux only) runs each sandbox with bubblewrap: unprivileged namespaces over a registry image's filesystem, with no daemon and no root at run time -- for a ci runner or an lxd container or vm with no docker. it needs `bwrap` 0.10 or later (ubuntu 26.04's works as shipped; 24.04's 0.9 has no `--overlay`), and inside an lxd container `security.nesting=true`. `image:` is a registry ref, the pats sandbox by default, and `pats init` fetches its layers over the registry's http api and unpacks them under the pats cache, pinned by digest (public images only, gzip or plain layers). each exec is one bwrap over the session's overlay; `prepare` runs as root in the namespace, the agent as your uid. egress is enforced as under docker: a policy gives the sandbox a network namespace with loopback alone, and a forwarder inside relays `127.0.0.1:8080` to the proxy, which runs on the host on a unix socket; `egress: none` is that namespace with nothing in it; open shares the host's network, its loopback included. what it does not do: more than one uid (`chown` to another id fails, so a package whose install makes a system user does not install), `sudo` or setuid, resource limits, `build:` or `provision:`, or containers inside. `docs/spec/bwrap.md` is the design.

`driver: host` runs the agent as a plain process on the machine pats runs on, as you, with your access to everything: for a throwaway ci runner or a vm made for one run, a task that needs the machine's own lxd, docker, kvm or gpu, or debugging a harness. it is dangerous by design, so it needs `sand-not-boxed: true` on the sandbox, and `image`, `build`, `base`, `provision`, `limits`, `sudo`, `vm`, `remote`, `profiles`, `config` and `egress: none` are refused. the harness clis are the ones on your `PATH`: `pats init` has nothing to make, and the run records their versions (warning when they are not the ones this pats pins). the agent gets the env pats builds and only `PATH` (absolute entries), `LANG`, `LC_*`, `TERM` and `TMPDIR` from yours; each command runs in a process group of its own that ends with it. an `egress` policy is advisory: the proxy runs as a child process and the agent is handed its env, but nothing stops a tool that ignores it, so the self-test does not run, `metadata.json` says `enforced: false`, and reports name those pairs. tls inspection under host needs `SSL_CERT_FILE` set to a bundle you vouch for. after pats is killed, what the agent started may survive: the machine is yours to tear down. `docs/spec/host-driver.md` is the design and lists what it does not promise.

a sandbox can also declare an `egress` policy. absent, the network is open, and open means unfiltered: the internet, but also the host itself, its lan and a cloud host's metadata endpoint. for the internet without those, write `egress: {default: allow}`, a proxy whose dial guard refuses private, loopback, link-local, multicast and carrier-grade nat targets -- though a host or lan on global addresses still passes. `egress: none` is no network at all (`--network none`; an agent cannot run there, since every harness reaches an inference api, and the loader says so). a mapping runs a filtering proxy sidecar on an internal network: the sandbox has no route out except through it. `default: deny` (the default) makes `allow` an allowlist; `default: allow` makes `deny` a denylist; both lists apply either way and a deny match always wins. an entry is a host (`github.com`), a suffix (`.ubuntu.com`), a host with a port (`github.com:22`; without one, 80 and 443), an ip, or a url entry: a host plus a path glob (`github.com/*/some-answer-repo**`: `*` matches within one path segment, `**` anything, `/` included) or a go regexp (`'api.github.com ~^/repos/[^/]+/x$'`, over the path and query). a url entry terminates tls for its host with a per-run CA the sandbox is told to trust, so each request is filtered by full url -- an `allow` url entry matches the path exactly, a `deny` one case-insensitively, each erring toward closed; a `..` in the path does not dodge an entry. every other host is a tunnel the proxy never decrypts, and the tunnel holds the client to its word: the tls hello must name the CONNECT host, so a shared front end cannot be reached under a name the policy never saw. targets on private, loopback, link-local, multicast or carrier-grade nat (`100.64.0.0/10`) addresses are refused unless `allow-private: true`. `deny-response` picks what a refused request sees: `403` (the default), `404`, `reset`, or `timeout` (held 60 seconds, then reset). the proxy audits every request as one json line into the pair's `egress.log` (an `open` and a `close` per tunnel with the bytes each way, a `req` per request it can see into, each naming the entry that decided it), and refusals land in the run metadata in three lists: `denied_egress` is what no rule anticipated -- the agent reached for something nobody planned for, a built-in cheat detector -- `blocked_egress` is what the policy names (an explicit `deny`, a path no `allow` url entry admits, a private target, a tls hello naming another host), and `failed_egress` is upstream trouble (a dial failure, a certificate whose stapled ocsp response says revoked). only `denied_egress` is warned about at the end of each pair, the other two are in `metadata.json` alone. `metadata.json` also records the effective policy, the sidecar image id and the bytes moved. under an allowlisting proxy, the hosts the harness itself needs -- the inference api, token refresh, opencode's startup fetches -- are merged in automatically per harness, so the config only lists what the *task* needs (e.g. apt mirrors). preflight proves the topology before the first pair: with the proxy env unset, a direct connection out of the sandbox has to fail, or the run refuses to start. `image` overrides the sidecar image (docker and podman). `methods: [GET, HEAD]` is a posture against writes: an unlisted method is refused on every host the policy allows, which makes the proxy inspect (mitm) every allowed host but the harness's own; an `allow` entry that names its method (`POST api.github.com/graphql`) overrides it, a `deny` one (`PUT,PATCH github.com/*`) scopes the deny. a task can set `egress: {allow, deny}` too: its entries are unioned over its sandbox's for that task's pairs, the sandbox keeps every other field, and pairing such a task with an agent whose sandbox runs no proxy is a load error. rules every task of a vector shares are sandbox posture and belong on the sandbox; the task level is for the per-task bit -- the one answer repo, the one extra package index. in `defaults` the block is replaced whole, like any field, and `egress: ~` drops it. `docs/egress.md` is the reference, including running the proxy on its own (`pats egress run`) and the `pats egress check | suggest | log` tools.

## installation

pats is a single go binary. you need go 1.26+ to build it, and docker to run the sandboxes.

releases are tags on `main`, and a `-beta.N` tag is a pre-release. 1.x reads a different `pats.yaml` shape from 0.x (see [`pats.example.yaml`](./pats.example.yaml)).

put it on your PATH with `go install`:

```sh
go install github.com/lczyk/pats/cmd/pats@v1.0.1    # installs into $(go env GOBIN)
```

or run it without installing at all:

```sh
go run github.com/lczyk/pats/cmd/pats@v1.0.1 run
go run github.com/lczyk/pats/cmd/pats@v1.0.1 score
```

on linux there is also a snap, which needs no go. it is strictly confined, so it runs agents and steps in docker or local lxd sandboxes only: no host driver, bwrap or podman. connect it to the engine you use:

```sh
sudo snap install pats --beta
sudo snap connect pats:docker                          # an apt docker
sudo snap connect pats:docker docker:docker-daemon     # the docker snap
sudo snap connect pats:lxd lxd:lxd                     # lxd
```

### setting up the engines

**docker.** you must be able to use the socket without sudo (`sudo usermod -aG docker $USER`, then log in again). with the snap, connect the `docker` plug as above, and keep projects under your home, not in a hidden directory there (`~/.work/...`) and not under `/tmp`: a strictly confined snap sees neither. a pats that is not the snap (`go install`, a clone) run with the docker snap keeps what the docker snap's cli opens itself -- a container's id file, a generated dockerfile, a step's script -- in `~/snap/docker/common/pats-tmp`, since that cli sees no file under `/tmp` or a hidden directory of the home; `pats clean` sweeps it, and `~/.cache/pats/tmp/docker-snap` links to it. a `build:` context has to be where that cli can read it too: under the home and not in a hidden directory at its top, which `pats init` says when it is not ([docs/spec/docker-snap-paths.md](./docs/spec/docker-snap-paths.md)).

**lxd.** you must be in the `lxd` group, and `lxd init` must have run (a storage pool and the default profile's root disk). with the snap, connect the `lxd` plug as above. pats makes a bridge per sandbox (`patsbr<hash>`) and one shared uplink (`patsup`), and a host firewall that drops traffic on bridges it was not told about stops them. an instance that never gets an address times out with `no default route`; one that gets an address but cannot get out fails the egress self-test. both errors print the fix. an admin applies it once, for whichever firewall the host runs:

```sh
# ufw, the ubuntu default when enabled: its INPUT drop stops dhcp and dns on the bridges
for br in 'patsbr+' patsup; do
  sudo ufw allow in on "$br"
  sudo ufw route allow in on "$br"
  sudo ufw route allow out on "$br"
done

# docker on the same host: its FORWARD policy drops the bridges' traffic
for br in 'patsbr+' patsup; do
  sudo iptables -I DOCKER-USER -i "$br" -j ACCEPT
  sudo iptables -I DOCKER-USER -o "$br" -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
done
```

the ufw rules persist; the iptables ones do not across a reboot (with docker 28 or later, `net.ipv4.ip_forward=1` in `/etc/sysctl.d` before docker starts keeps its FORWARD policy at accept instead). meanwhile `PATS_LXD_UPLINK=lxdbr0` has image builders and egress proxies use a bridge that already works; it does not help an open sandbox, which is always on a bridge of its own.

from a clone, `make install` builds the binary (upx-compressed if `upx` is on PATH) and symlinks it into `~/.local/bin`:

```sh
git clone https://github.com/lczyk/pats && cd pats
make install
```

### published images

pats publishes two images per release, both tagged `v<version>` and pinned by the binary of that version: the default sandbox (`ghcr.io/lczyk/pats/sandbox`, used by a sandbox that names no `image` or `build`) and the egress proxy (`ghcr.io/lczyk/pats/egress-proxy`, the sidecar behind an `egress:` block -- the pats binary itself, started as `pats egress run`). `:latest` on both follows the newest stable release. a released install's `pats init` pulls them from ghcr. the version is the module tag for a module install, else the `VERSION` file of the checkout; `pats --version` shows it. so a clone build pins to the images of the release its tree is based on, whether or not the tree has moved on -- build the images locally and pats finds those instead of pulling:

```sh
make images          # the sandbox, tagged :v<version>, :latest and :26.04 for the host arch
make egress-image    # the proxy, tagged :latest and :v<version>
```

## examples

typical loop -- make the images once, run the suites, then score them:

```sh
pats init                # pull / build every sandbox's images and pin them in .pats/images.json
pats run                 # run all agent x task pairs, save to .pats/runs/<n>-<date>-<adj>-<noun>/
pats score               # score the latest run (tasks x script scorers)
pats judge               # judge it (tasks x agent scorers); costs money
```

narrowing and parallelism:

```sh
pats run -a haiku-claude-cli -t simple                  # just one pair
pats run -t write-readme                                # every task in that vector
pats run -S readme                                      # just one suite
pats run -j-1                                           # parallel pairs, auto job count
pats score -s style -a haiku-claude-cli                 # one scorer vector on one agent
pats score -r jacquard-runner                           # score an older run by its friendly name
pats score -r .pats/runs/003-20260701-jacquard-runner   # ... or by path
pats judge -s no-colloquialisms -N 3                    # one agent scorer, three samples a score
```

results:

```sh
pats report                                             # reprint the latest run's report
pats report --json | jq '.pairs[] | select(.mean < 1)'  # the same as one json document, for scripts
```

`pats score` and `pats report` print a human-readable report: one tasks x agents table of each pair's mean score, worst task first, a pair that did not end ok marked in its cell (`!` nonzero, `T` timeout, `B` budget, `E` error, `?` no metadata), and per-agent rows for the mean, the summed dollar cost and the summed agent time, plus a `failed` row when some pairs could not be judged (they are out of the mean), and a line per judge agent summing its judgements, failures and spend. it is a convenience, kept short. for anything more, `pats report --json` prints the run's results as one json document and nothing else: `version`, `run`, `overall`, `agents` (each with its mean and `failed_pairs`), `pairs` (each with its mean and what its run recorded: status, duration, turns, token counts, cost, egress) `scores` (one per agent, task and scorer) and `judges` (one per judge agent: judgements, failed, na, samples, cost, tokens, time). a value pats does not know is `null`, never 0. new fields may appear; a field renamed or removed bumps `version`. that document is the interface: the files under `.pats/runs/` (`scores.json`, each pair's `metadata.json`) are pats's own and change between versions.

all commands take `-c <path>` to point at a `pats.yaml` other than the one in the cwd.

### example config

the model under test is `claude-haiku-4-5`, run two ways: through the `claude` cli (`claude-cli-keyless`) and through `opencode` over openrouter (`opencode-openrouter`). running the same model under two harnesses isolates the harness's effect from the model's.

it has one task vector, `write-readme`, with 3 tasks: `simple`, `hard`, `monorepo`. the vector's defaults name the prompt and scripts once (`prompt-cmd: cat "prompts/$PATS_TASK_VECTOR-$PATS_TASK_NAME.txt"`, `stage: scripts/stage.sh --"$PATS_TASK_NAME"`) and each task is just its name. judging by those, these tasks seem to be testing some behaviour related to readme authorship. they may be about testing the model itself, but also could be testing some skill or instruction given to the model.

there are 4 scorers in the `style` vector: `only-ascii`, `title-correct`, `no-finite-verbs` and `no-colloquialisms`. former 2 scorers are implemented in a purely imperative manner. `only-ascii` deducts 0.1 form the score for each occurrence of excessive whitespace. `title-correct` is much closer to a binary yes/no question so it could simply output 1.0/0.0 respectively. latter 2 scorers are more _"fuzzy"_. `no-finite-verbs` scorer uses [`spacy`](https://pypi.org/project/spacy/) -- a python NLP library. `no-colloquialisms` is agentic, and therefore nondeterministic: it uses an agent to score the results, run by `pats judge`. there is, in fact, one more agent defined for that purpose -- `judge`, a `claude-cli-keyless` agent running `sonnet`. it's left out of every suite, so it never runs as a task -- it only scores, letting us judge a weaker model (`haiku`) with a stronger one (`sonnet`).
