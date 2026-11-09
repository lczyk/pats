# steps are shell scripts, and pats.yaml has no templating

status: implemented.

## problem

every step pats runs comes in two forms: an inline script (`stage`, `prepare`, `collect`, `score`, `provision`), run
by `sh`, and a `-cmd` twin (`stage-cmd`, `prepare-cmd`, `collect-cmd`, `score-cmd`, `provision-cmd`), split into
words and exec'd with no shell. on top, pats templates `${task}` / `${agent}` / `${scorer}` / `${sandbox}` /
`${vector}` into every field that is not content, at load.

the two forms do the same job. a `-cmd` value looks like a shell line and is not one: `stage-cmd: cp
cases/$PATS_SCORED_TASK_NAME/case.yaml .` hands `cp` the literal `$PATS_SCORED_TASK_NAME`. templating exists to put a
name into a `-cmd` (`score-cmd: scorers/${scorer}.sh`), and every name is already in the step's environment
(`PATS_TASK_NAME`, `PATS_AGENT_NAME`, `PATS_SCORER_NAME`). the one place templating is used on a field pats itself reads is
a prompt path, `prompt-file: prompts/${vector}-${task}.txt`, which the `dir` layout makes moot (each task holds its
own prompt) and `prompt-cmd` covers otherwise. a search of every pats config at hand (the example, mason, oci-factory)
found no other: no templated `image`, `build`, `base`, `model`, agent `sandbox`, egress entry, profile, config or
timeout.

so each step is one field, a shell script, and nothing in `pats.yaml` or a `task.yaml` is templated.

## the rule

- **one field per step.** `stage`, `prepare`, `collect` (task), `score` (scorer), `provision` (sandbox), and
  `prompt-cmd` (task, agent scorer) are scripts. a script with a `#!` line runs under that interpreter; any other runs
  under `/bin/sh -e`. a one-liner is a script too: `stage: ./stage.sh --fast` runs `./stage.sh` from the step's
  directory with `--fast`, and `score: scorers/scorer_$PATS_SCORER_NAME.py` expands the name, both by the shell.
- **the `-cmd` forms go:** `stage-cmd`, `prepare-cmd`, `collect-cmd`, `score-cmd`, `provision-cmd`. a config that
  sets one is a load error naming the field to use instead. 1.x is unreleased, so there is no alias.
- **the prompt keeps three forms**, since they differ in kind, not in how they run: `prompt` (the text),
  `prompt-file` (a file pats reads, relative to the task's directory), `prompt-cmd` (a script whose stdout is the
  prompt, run on the host after `stage`). `prompt-cmd` is a script under the rule above, where today it is split into
  words. it keeps its name, so it is the one field whose meaning changes without a load error: a value that relied on
  there being no shell (a `$`, a glob, a `;` or `|` passed to the command as text) now gets the shell's meaning. no
  config at hand sets `prompt-cmd`, and before this a `$` in one was never expanded, so one there was a bug more
  likely than a wish. accepted; the readme says it.
- **no templating.** `${...}` in a field is the field's text. so that a config written for templating fails loudly
  rather than reading a file named `prompts/${task}.txt`, a field holding `${task}`, `${agent}`, `${scorer}`,
  `${sandbox}` or `${vector}` is a load error naming the env var that carries it (`$PATS_TASK_NAME`, ...) or saying there
  is none (`${sandbox}`, and `${vector}` on an agent or a sandbox). the check covers every string field, scripts and
  `env` values included, but not `prompt`, whose text is the agent's: a literal `${task}` in a prompt reaches the agent
  as written. a script that means a shell variable of one of those five names renames it.

- **the vector joins the step env**, as the item's name already is: a task's steps (`stage`, `prompt-cmd`, `prepare`,
  `collect`) get `PATS_TASK_VECTOR`, a script scorer gets `PATS_SCORER_VECTOR`. so defaults shared across vectors by
  an anchor keep picking per-vector files: `prompt-cmd: cat "prompts/$PATS_TASK_VECTOR-$PATS_TASK_NAME.txt"` is what
  `prompt-file: prompts/${vector}-${task}.txt` was.

## where a step runs, and what it can reach

unchanged by this, written down because the `-cmd` forms used to blur it:

- **host steps** (`stage`, `collect`, `score`, `prompt-cmd`) run in the directory the item was defined in (the pats
  root, or a `dir` task's own directory), with the pats root's `bin/` first on `PATH`. so a helper is called by name
  (`stage: prepare-knockout`), a file beside the item by path (`collect: ./collect.sh`).
- **in-sandbox steps** (`prepare` in the pair's sandbox, `provision` in the image builder) get the script and the
  pats root's `bin/`, at a mount with `bin/` first on `PATH`, and nothing else from the host. what `prepare-cmd` and
  `provision-cmd` did besides, shipping the one host file their first word named, goes with them: a host file an
  in-sandbox step needs lives in `bin/` (`provision: provision-mason`), or its content is the inline script (`provision:
  |`). `stage` can also put a file in the workdir for `prepare` to run, though the agent then sees it.
- **the image key** of a provisioned image covers the script and `bin/`, as today: editing a `bin/` helper remakes
  the image.

a multi-line inline script runs from a temp file, so a python script inline cannot import a module beside the item;
`score: ./scorer.py` runs the file itself, which can.

## what changes in code

- `internal/config`: the `-cmd` fields leave `Sandbox`, `Task` and `Scorer`; their keys become load errors with the
  replacement named; the slots holding a step and its `-cmd` go (the prompt's slot stays); `interpolate` becomes a
  check for the five names; validation drops the `-cmd` alternatives.
- `internal/eval`: the task and scorer step envs gain `PATS_TASK_VECTOR` / `PATS_SCORER_VECTOR`; a host `step` is a
  script only (`step.cmd`, `resolveCommand`, `checkExecutable` go); `stageMaterial` takes a script only; `prompt-cmd`
  runs through the same host step as `stage`; `shellwords.go` (`splitArgs`) goes, having no other user.
- `src/sandbox`: `ProvisionMount`'s doc stops naming a -cmd's file.
- docs: the readme, `pats.example.yaml`, `lxd.md` (its `provision-cmd: ./provision.sh` example and the mason plan),
  `agent-scorers.md` (its `stage-cmd` / `prepare-cmd` / `collect-cmd` fields and examples).
- tests: the config tests of templating and slots become tests of the load errors; the step tests of `-cmd`
  resolution go; the run tests using `prepare-cmd` / `provision-cmd` use `prepare` / `provision`.

## not in this

- the agent-scorer env (`PATS_SCORED_*`, `agent-scorers.md`) is its own change.
- shipping a `dir` task's own directory into `prepare`: a task that wants its fixtures inside the sandbox stages them
  into the workdir, as today.
