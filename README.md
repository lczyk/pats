# pats

![GitHub go.mod Go version](https://img.shields.io/github/go-mod/go-version/lczyk/pats)
![GitHub Tag](https://img.shields.io/github/v/tag/lczyk/pats?label=release)
[![lint_and_test](https://github.com/lczyk/pats/actions/workflows/lint_and_test.yml/badge.svg)](https://github.com/lczyk/pats/actions/workflows/lint_and_test.yml)
![GitHub License](https://img.shields.io/github/license/lczyk/pats)

<img src="assets/robot_cat.png" alt="pats logo" width="30">

score-based testing framework for testing and scoring agent tasks.

to use pats, put `pats.yaml` in your project root. see [`pats.example.yaml`](./pats.example.yaml) for an example of such a configuration. 

`pats.yaml` sets up its "vectors": `agents`, `tasks`, and `scorers` (plus `sandboxes`, covered below). a task is a single scenario given to the model under test. a scorer is a task to run on the output of the agent run which scores how well did the particular agent perform at a certain specific aspect of a task. each scorer outputs a value between 0.0 - 1.0. the primary way to refer to each of these is by their `id` which doubles as a human-readable name.

fields that run something -- a task's `prompt`/`prepare`/`collect` and a scorer's `score` -- are exec'd directly, not via a shell: the value is tokenised into argv (posix-sh quoting, but no `$`-expansion, globbing, or pipes -- those belong inside the script you call), and argv[0] (a `+x` file resolved against the config dir) runs with the rest as literal args. so these must name an executable file, **not** an inline shell snippet (`collect: rm -rf x && cp ...` won't work -- put it in a script). they get `${id}` expansion (-> that entry's own id) and these env vars: `PATS_*_ID` (`PATS_TASK_ID`, `PATS_AGENT_ID`, and for scorers `PATS_SCORER_ID`), `PATS_MODEL`, `PATS_AGENT_KIND`, and `PATS_OUTPUT_DIR`; task fields (prompt/prepare/collect) additionally get `PATS_WORKDIR` (scorers don't -- they read the collected output dir, not a workdir). a `prompt` has three modes by what argv[0] resolves to: a `+x` file is exec'd and its **stdout** becomes the prompt; a plain (non-`+x`) file has its **contents** used; anything that isn't a file (incl. free text with spaces or apostrophes) is the **literal** prompt.

an `agent` is a harness -- an agent cli that runs the agentic loop in a sandbox. it comes in two kinds, each baking in the cli and how it authenticates. `opencode-openrouter` is the `opencode` cli with models served via openrouter (reads `OPENROUTER_API_KEY` from the env; write the model without the `openrouter/` prefix, it's added for you). `claude-cli-keyless` is the `claude` cli, keyless: your oauth creds (`~/.claude/.credentials.json`) are mounted into the sandbox, and the model is a raw anthropic id. an agent can set `effort:` -- reasoning effort, mapped to claude's `--effort` (low|medium|high) and opencode's `--variant` (provider-specific, e.g. high|max|minimal) respectively. both harnesses log a machine-readable event stream to `stdout.log` (claude: stream-json; opencode: `--format json --thinking`), which feeds the live tool counter and the cost/token summary in each pair's `metadata.json`. every agent can run tasks; an agent you don't list in any suite simply never runs as one -- which is how you keep a stronger model around purely to score a weaker one (it must then be referenced by a `kind: agent` scorer, else it's flagged as an orphan). scorers come in two flavours. a file scorer (the default -- just omit `kind`) names an executable `score:` script; pats execs it directly and its shebang picks the interpreter and deps (e.g. `#!/usr/bin/env -S uv run --script` with inline pep-723 deps -- trampoline to python/spacy/whatever you like). it exits 0 and prints a float in `[0,1]` on its first stdout line, or `na` to skip (a non-zero exit is a failure). `${id}` in the `score:` path expands to the scorer's id, so a uniform layout reads `score: scorers/${id}.sh` (inside a flow mapping `{...}` quote it -- `"scorers/${id}.sh"` -- else the `}` closes the mapping). the other kind is `agent` (ask an agent to judge) -- **not implemented yet**; configuring one is rejected at config validation until it lands.

finally, `pats.yaml` groups everything into `suites`. a suite is a named (`agents`, `tasks`, `scorers`) triple, and both cross-products are implied within it: `pats run` runs its `agents` x `tasks`, `pats score` scores its `tasks` x `scorers`. every id is explicit -- there are no wildcards; when two suites share a list, reuse it with a yaml anchor (`agents: &all [...]` then `agents: *all`). each axis takes a scalar or a list; `scorers` may be empty (a run-only suite). the flip side of explicit lists is an orphan check: every agent, task, and scorer must appear in at least one suite (agents referenced by a `kind: agent` scorer are exempt), so forgetting to wire a new entry in is a load-time error rather than a silent coverage hole. suites may overlap (a `smoke` suite inside a `full` one) -- duplicate pairs are deduped, not errors. scores aggregate as plain means (per pair over its scorers, then per agent over its tasks). narrow a run or score to one suite with `-s <id>`.

there are two (and a half) stages of a pats workflow. `pats run` runs your suites -- your agents across their tasks -- and saves the result to `.pats/runs/<run_number>-<date_slug>-<friendly-name>/` (e.g. `003-20260701-jacquard-runner`; the two words are generated from the numeric prefix) -- that usually takes a hot moment. then `pats score` scores the most recent run, or you can ofc instruct it to test an older run too. note that this is not intended as a way to keep old historic runs and therefore, if you change the scorers and rerun on the old run, you *are* going to just run the new scorers. finally, if you have any agentic scorers they are not run by default, but only with `--agentic` flag (note: agent scorers aren't implemented yet, so `--agentic` currently has nothing to run).

agents are always run in a sandbox -- we never run without one. the `sandboxes` vector defines the available ones; each agent names the sandbox it wants via `sandbox: <id>` (if only one sandbox is defined, it's the default). a sandbox has a `kind` (`container` or `bwrap`) and, for container kinds, a `driver` (`docker` now, `podman` later) and exactly one of `image` or `build`. pats publishes a ready-made ubuntu-based fat image carrying all the harness clis at `ghcr.io/lczyk/pats/sandbox:<ubuntu-ver>` -- point `image` at that or your own. alternatively `build: <path>` names a docker build context (a dir with a Dockerfile, or a Dockerfile path, relative to `pats.yaml`) that pats builds at the start of every run -- the layer cache makes the no-change case a no-op, and the built image id is recorded in each pair's `metadata.json`. one guard: if the build context contains `.pats/` (run artifacts) and the dockerfile reads the context (a `COPY`/`ADD` not sourced `--from` another stage), the effective `.dockerignore` must exclude it (a literal `.pats/` line) or the run refuses to start -- pats won't bake your run history into the image, and won't edit ignore files for you. a context-less dockerfile (no `COPY`/`ADD`) needs no ignore file. `bwrap` (linux-only) needs no image at all -- the command runs directly on the host under bubblewrap, with the host fs as a read-only rootfs (`/home`, `/root` and `/run` masked) and the same egress modes (proxy modes run the filter in-process, no sidecar). `container` works anywhere docker does (incl. macos, where docker is itself a linux vm).

a sandbox can also declare an `egress` policy: `mode: open` (the default -- unrestricted network), `mode: none` (`--network none`), or `mode: proxy` -- a filtering sidecar that allows/denies by host (`default: deny` + `allow: [...]`, or `default: allow` + `deny: [...]`) and writes a per-pair audit log (`egress.log`; denied hosts also land in the run metadata, a built-in cheat detector). under an allowlisting proxy, the hosts the harness itself needs -- the inference api, token refresh, opencode's startup fetches -- are merged in automatically per agent kind, so the config only lists what the *task* needs (e.g. apt mirrors). when host granularity isn't enough (allow github, but not one specific repo), `mode: mitm-proxy` is a superset of `proxy` adding url-level rules: hosts named in `deny-urls` (host-anchored patterns, e.g. `"github.com/*/chisel-releases*"`) get their tls terminated with a per-run CA the sandbox is told to trust, so each request is filtered by full url; all other hosts stay undecrypted tunnels. see `docs/proposals/network-egress.md` for the design.

## installation

pats is a single go binary. you need go 1.26+ to build it, and docker to run the sandboxes.

put it on your PATH with `go install`:

```sh
go install github.com/lczyk/pats/cmd/pats@latest    # installs into $(go env GOBIN)
```

or run it without installing at all:

```sh
go run github.com/lczyk/pats/cmd/pats@latest run
go run github.com/lczyk/pats/cmd/pats@latest score
```

from a clone, `make install` builds the binary (upx-compressed if `upx` is on PATH) and symlinks it into `~/.local/bin`:

```sh
git clone https://github.com/lczyk/pats && cd pats
make install
```

### egress proxy image

a sandbox with `mode: proxy` or `mitm-proxy` runs a filtering sidecar from a published image, pinned to your pats version (`ghcr.io/lczyk/pats/egress-proxy:v<version>`). a released install pulls it from ghcr automatically on first use. if you installed from a clone whose version isn't published yet, build the image locally first so pats finds it instead of trying to pull a missing tag:

```sh
make egress-image    # builds from this checkout, tags :latest and :v<version>
```

## examples

typical loop -- run the suites, then score them:

```sh
pats run                 # run all agent x task pairs, save to .pats/runs/<n>-<date>-<adj>-<noun>/
pats score               # score the latest run (tasks x scorers)
```

narrowing and parallelism:

```sh
pats run -a claude-cli-keyless -t write-readme-simple   # just one pair
pats run -s readme                                      # just one suite
pats run -j-1                                           # parallel pairs, auto job count
pats score -r jacquard-runner                           # score an older run by its friendly name
pats score -r .pats/runs/003-20260701-jacquard-runner   # ... or by path
```

all commands take `-c <path>` to point at a `pats.yaml` other than the one in the cwd.

### example config

the model under test is `claude-haiku-4-5`, run two ways: through the `claude` cli (`claude-cli-keyless`) and through `opencode` over openrouter (`opencode-openrouter`). running the same model under two harnesses isolates the harness's effect from the model's.

it has 3 tasks: `write-readme-simple`, `write-readme-hard`, `write-readme-monorepo`. judging by their ids, these tasks seem to be testing some behaviour related to readme authorship. they may be about testing the model itself, but also could be testing some skill or instruction given to the model.

there are 4 scorers: `only-ascii`, `title-correct`, `no-finite-verbs` and `no-colloquialisms`. former 2 scorers are implemented in a purely imperative manner. `only-ascii` deducts 0.1 form the score for each occurrence of excessive whitespace. `title-correct` is much closer to a binary yes/no question so it could simply output 1.0/0.0 respectively. latter 2 scorers are more _"fuzzy"_. `no-finite-verbs` scorer uses [`spacy`](https://pypi.org/project/spacy/) -- a python NLP library. `no-colloquialisms` is agentic, and therefore nondeterministic: it uses an agent to score the results (commented out in the example until agent scorers are implemented). there is, in fact, one more agent defined for that purpose -- `judge`, a `claude-cli-keyless` agent running `sonnet`. it's left out of every suite, so it never runs as a task -- it only scores, letting us judge a weaker model (`haiku`) with a stronger one (`sonnet`).
