# run dry run

status: implemented, but for the provider block in the match and in metadata, which waits on `provider-pinning.md`.

## problem

there is no way to see what `pats run` with a selection would do before it does it: which pairs, on which sandboxes,
and what it is likely to cost. the selectors (`-S`, `-a`, `-t`) compose, vectors expand, and a typo in a selector is
an error but an over-broad one is a long, expensive run.

## proposal

- **`pats run -n/--dry-run`** does what `pats run` does up to the first sandbox, then lists the pairs and stops:
  - loads the config and expands the selection, as now;
  - checks the pins (`usePins`): a missing image is the same error a real run gives;
  - runs the model check (`model-preflight.md`) unless `--offline`;
  - does not run the credential preflight or the upstream checks, starts no sandbox, and makes no run dir.
- **the text output** is one log line per pair, then a total:

  ```
  INFO haiku/curl  sandbox docker  est $0.04 (median of 6)
  INFO sonnet/curl  sandbox docker  est unknown (no history)
  INFO codex-mini/curl  sandbox docker  est unknown (no history)
  INFO 3 pair(s), est $0.04 + 2 unknown
  ```

a task with a step sandbox (`docs/spec/step-sandbox.md`) shows it after the sandbox: `sandbox docker  steps tools`, and
`steps` in the json line.

- **`--json`** only goes with `-n`: `pats run --json` alone is an error before anything runs, so a preview flag
  never starts a paid run. it prints the same content as json lines on stdout, and nothing else there: warnings and `-v` lines go
  to stderr in json mode. one object per pair, then one total:

  ```json
  {"agent": "haiku", "task": "curl", "sandbox": "docker", "estimate_usd": 0.04, "samples": 6, "unknown": null, "repeat": 1, "estimate_total_usd": 0.04}
  {"agent": "sonnet", "task": "curl", "sandbox": "docker", "estimate_usd": null, "samples": 0, "unknown": "no history", "repeat": 1, "estimate_total_usd": null}
  {"total": true, "pairs": 3, "executions": 3, "known_usd": 0.04, "unknown_pairs": 2, "max_cost_usd": null, "fits": null}
  ```

  `pairs` counts distinct pairs, `executions` pairs times repeats, `unknown_pairs` distinct pairs without an estimate.
  `fits` is whether `known_usd` is under `--max-cost`, null without one.
- **the estimate** is the median `summary.cost_usd` of past pairs that match, with `has_cost: true`, whatever their
  status. `samples` is how many there were. none: `unknown`, with the reason `no history`. a harness
  that reports no cost leaves no history, and reads the same.
- **matching.** a past pair matches when its `metadata.json` has the same agent name, task name, harness, model, effort
  and provider block (`provider-pinning.md`) as the agent and task now. a missing provider block matches only an
  agent with none. metadata from before `model` was recorded matches nothing.
- **metadata gains `model`, `effort` and `provider`**, for the matching, since the agent name alone can be pointed at
  another model or provider between runs.
- **history** is every numbered run dir under `.pats/runs/` (`sortedRunNames`, which skips the `latest` alias, so no
  run counts twice). no `.pats/runs/` yet (a project after `pats init`, before any run) is no history; another error
  reading it is an error. a pair dir with no `metadata.json` is skipped silently; one whose metadata does not parse is
  skipped with one warning naming the file; any other error reading a run dir, a pair dir or a metadata file stops
  the dry run with that path.
- **with `--repeat n`** (`repeats.md`) each pair's `repeat` is n and its total is the estimate times n; the text lines
  say `est $0.04 x5 = $0.20`.
- **with `--max-cost`** (`budget.md`) the total says whether the known part fits: `est $14.20 + 3 unknown, under
  --max-cost $20`.

## not doing

- **no price-list estimate.** turning tokens into dollars from openrouter's prices needs a token estimate per task,
  which pats does not have. past cost of the same pair is the honest number, and unknown is an answer.
- **no zero for harnesses without cost.** they are `unknown (no history)`, never `$0`.

## tests

- a config dir with past runs: the estimate is the median of the matching pairs only. another model, another provider
  block and a pair without `has_cost` are excluded; the `latest` alias does not double-count; a malformed metadata
  warns once and is skipped.
- no run dir is made and no sandbox is started (a fake sandbox constructor fails the test when called).
- a project with no `.pats/runs/` estimates every pair as unknown; an unreadable metadata file stops with its path.
- `pats run --json` without `-n` is an error and starts nothing.
- `--json` carries the same pairs and numbers as the text, and stdout holds only json when a pin is stale (its
  warning goes to stderr).
