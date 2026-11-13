# proposal: task directories

status: implemented on the 1.x line.
scope: `pats.yaml` task vectors, how run fields (prompt, prepare, collect, score) are written and run, `internal/config`
and `internal/eval`.

## problem

a task's parts live wherever the config points: a prompt file, a prepare script, a collect script, ground-truth files.
in a real config (`canonical/mason`, `tests/skills/`) that is three parallel trees keyed by task id -- `prompts/<id>.txt`,
`tasks/prepare_<id>.sh`, `cases/<id>/` -- plus a `pats.yaml` entry per task. adding a case touches four places, and
reading one means opening all of them. the per-task prepare scripts are three-line wrappers that only set parameters.

the goal is to encourage (not enforce) a task being one self-contained directory, the way spread lays out its tasks, and
to make shared code reusable without each task encoding where it lives.

## task directories

a task vector takes `dir:` instead of `items:`.

```yaml
knockout:
  kind: task
  dir: cases/knockout
```

- every directory under `dir` that holds a `task.yaml` is one task, at any depth. other directories (and anything whose
  name starts with `.`) are just layout.
- a `task.yaml` below another task's directory is an error naming both.
- the task's id is its directory's own name. ids stay unique per kind, so two task directories with the same name are a
  duplicate-id error.
- `task.yaml` holds what an inline item holds, with the same strictness. errors name the file.
- a `dir` vector takes only `kind` and `dir`. `items`, `defaults`, or both `dir` and `items` are errors: a task in a
  directory gets all of its configuration from its own `task.yaml`.
- only task vectors take `dir`.

## run fields

each field that runs or provides something comes in two or three forms. a task or scorer sets at most one key of each
slot (exactly one for a task's prompt).

| slot    | inline        | file          | command       |
|---------|---------------|---------------|---------------|
| prompt  | `prompt`      | `prompt-file` | `prompt-cmd`  |
| prepare | `prepare`     |               | `prepare-cmd` |
| collect | `collect`     |               | `collect-cmd` |
| score   | `score`       |               | `score-cmd`   |

- **inline** is the content itself: the prompt text, or a script body. a script body starting with `#!` is run as an
  executable file (its shebang picks the interpreter); one without runs under `/bin/sh -e`. templates never expand in
  inline content, so `${VAR}` reaches the shell as written.
- **`-cmd`** is a command line, split into words posix-style and exec'd directly -- no shell. a first word without a `/`
  is looked up on `PATH`; one with a `/` is a file path. `prompt-cmd`'s stdout is the prompt; it runs after prepare.
- **`prompt-file`** is a file whose contents are the prompt.
- in a vector's `defaults` the slots merge as units: an item setting any key of a slot replaces the whole slot, and an
  explicit null on any key of it clears the slot.

this replaces 0.x's single field per slot, which guessed between file, executable and literal text by looking at the
value.

## where things run and resolve

- a path in a value (a `-file`, or a `-cmd` first word containing `/`) resolves from the directory of the file it is
  written in: the pats root for `pats.yaml`, the task's directory for `task.yaml`.
- host scripts run in that same directory. a task from a `task.yaml` runs its prepare, collect and prompt command in its
  own directory; items from `pats.yaml` run in the pats root.
- `<pats root>/bin/` is prepended to `PATH` for every host script, inline or `-cmd`. shared helpers live there and are
  called by name, so a task never encodes where they live.
- `PATS_TASK_DIR` (absolute) is exported to a task's scripts and to the scorers that score it.

## task env

`env:` on a task is a mapping of variable names to values, exported to that task's host scripts (prepare, collect,
prompt command). names follow shell rules and may not start with `PATS_`. templates do not expand in it. in a vector's
`defaults` it merges key by key; `KEY: ~` drops one inherited key.

## renames

- a scorer's `agent-id` is `agent`, matching how every other reference is named after what it points at.
- `${dir}`, generated prompts guessed from an executable `prompt:` value, and the 0.x meaning of `prepare` / `collect` /
  `score` as a file path are gone.

## not in scope

- `dir` on scorer vectors. mason's scorers are one script each and share a sibling python module, so a directory per
  scorer only multiplies files.
- multi-output scorers and a shared per-pair scoring step.
- variants.
