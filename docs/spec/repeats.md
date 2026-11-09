# repeats

status: implemented.

## problem

one run of a selection is one sample per pair. an agent's score on a task varies run to run, so a baseline needs
several samples: n >= 5 is the usual floor, and an earlier evaluation took 14 separate `pats run`s to get there. today
that is a shell loop, and nothing in pats reads several runs together.

## proposal

- **`pats run -N/--repeat <n>`** is sugar for running the same `pats run` n times, one after another. each repeat is
  an ordinary run: its own run dir, number and name. like any run it is unscored until `pats score` (or `pats score
  -N n` for the series). spread has the same
  flag (`-repeat`).
- **`pats score -N/--runs <n>`** scores the latest n runs, each on its own, as `-r` does for one.
- **`pats report -N/--runs <n>`** reports the latest n runs together.

n is a whole number of at least 1, checked before anything else happens. `-N` does not combine with `-r`.

## run: the series

`Run` today does its setup and its pairs in one function, with the signal context, the config lock and
`sandbox.Shutdown` deferred inside it. a loop around it would shut the lease renewal down after the first repeat. so
`Run` splits:

- **once per invocation**: the signal context, the config lock, `defer sandbox.Shutdown()`, loading and selecting,
  `usePins`, the model check (`model-preflight.md`), the credential preflight and the upstream checks.
- **once per repeat**: check the context and the budget (`budget.md`), then make the run dir, run the pairs, and log
  the run's totals. the log says which repeat: `INFO run 2/5: 004-20261109-fluffy-bunny`.

stopping:

- ctrl-c ends the current repeat as an interrupt ends a run now, and no further repeat starts. the context is checked
  after every repeat, the last included, and an interrupted invocation exits non-zero (130). this changes a single
  `pats run` too, which today exits 0 after an interrupt.
- a reached budget starts no further pair and no further repeat, and makes no further run dir (`budget.md`).
- a repeat whose pairs fail does not stop the series. a repeat whose run dir cannot be made does, with that error.
- `-n/--dry-run` with `-N` makes no runs and multiplies the estimates (`run-dry-run.md`).

## which runs "the latest n" are

one ordering everywhere: `sortedRunNames` (date, then number), which `-r`, `latest` and `pats list runs` already use.
the latest n are its last n entries. there is no series id: a repeat writes plain runs, and `-N` reads plain runs, so
a run made by hand in between is one of the latest n. `pats list runs` shows what `-N` will pick.

- **score**: the run list is taken once, up front. fewer runs than n is an error naming how many there are. each run
  is scored in turn, as `-r all` does: a run with nothing to score under the filters is skipped with a note, any other
  error stops.
- **report**: every one of the n runs must be scored by this pats. a run without `scores.json` is an error (`run 004
  is not scored; pats score -N 3`), and so is one whose `scores.json` is of another version or does not parse, each
  named. `-N 1` is the latest run, exactly as with no flag, errors included.

## report: the numbers

- **an attempt** is one run's entry for a pair: it ran there. attempts are found from the run's pair dirs
  (`<run>/<agent>/<task>/`) as well as its `scores.json`, so a pair dir with no metadata and no successful score is
  still an attempt: unknown status, null mean, null duration. its mean may be null (every scorer `n/a` or failed). a
  single-run report keeps its current behaviour.
- **a pair's mean** is the mean of its attempts' non-null means, `n` their count, `sd` their sample standard
  deviation (n - 1), null when n < 2. a pair whose attempts all have null means has a null mean and n 0, as in a
  single-run report.
- **weighting stays as now.** an agent's mean is the mean of its pairs' means, and the overall mean is the mean of the
  agents' means, as `aggregate` does for one run. a pair with more samples does not weigh more.
- **the pivot** shows `mean +-sd` per cell, and `(n)` beside a pair whose n is below the most any pair has. a cell's
  status mark is the worst status over its attempts (`error` over `timeout` over `budget` over `nonzero` over
  unknown over `ok`), with the count of non-ok attempts beside it when there are any (`! 2/5`), so one failure in five
  is not hidden. an attempt without readable metadata is unknown (the single-run report's `?`): kept, counted as
  non-ok, never as a failure of a known kind.
- **cost and time** rows sum the cost of every attempt, scored or not, with `has_cost`, and average the duration over
  every attempt with a known duration (unknown when none has one): money spent on an unscored attempt is still spent. a cost with any attempt lacking `has_cost` is
  shown with a `+`, as a lower bound.
- runs with different selections are fine: the report is the union of pairs, each with its own n.

## report: the json

a single-run report keeps its current document, version 1, unchanged. a report over n > 1 runs is version 2:

```json
{
  "version": 2,
  "runs": [{"id": "004-20261109-fluffy-bunny", "dir": "/abs/.pats/runs/004-..."}],
  "overall": 0.81,
  "agents": [{"id": "haiku", "mean": 0.8}],
  "pairs": [
    {"agent": "haiku", "task": "curl", "mean": 0.82, "sd": 0.05, "n": 5,
     "samples": [
       {"run": "004-20261109-fluffy-bunny", "mean": 0.8, "status": "ok", "cost_usd": 0.04, "duration_s": 41.2,
        "scores": {"usage-printed": 1.0}}
     ]}
  ]
}
```

`samples` has one entry per attempt, null `mean` included, so a failed or unscored attempt keeps its status, error,
cost and duration. per-run facts of a pair live in its samples, each carrying its run id. a pair carries only what is
computed across runs. `report.md` gets the version 2 shape beside version 1.

## not doing

- **no series id, no series dir.** that would make a series a new kind of thing every reader has to know.
- **no parallel repeats.** `-j` already runs pairs in parallel within a run.
- **no confidence intervals or tests** in the report. mean, sd and n are enough to see whether two agents differ, and
  more belongs in the reader's own analysis of `report --json`.

## tests

- `run -N 3` with a fake sandbox makes three run dirs, runs the credential preflight once, keeps the lease renewal
  alive into the third, and stops after the current run on interrupt with no further run dir; an interrupt during
  the last repeat exits non-zero.
- a pair dir with no metadata whose scorers all failed is still an attempt in the multi-run report.
- a pair with a missing metadata in one run shows the unknown mark, is counted non-ok, and its duration is left out
  of the average.
- `-N 0` and `-N -1` are errors before any side effect.
- `score -N 4` with three runs is an error; with filters that leave one run empty, that run is skipped.
- `report -N 2` gives mean, sd and n per pair, null sd at n = 1, an all-`n/a` attempt kept in `samples` with a null
  mean but not in n, the worst status mark with its count on a mixed pair, cost summed over unscored attempts too,
  agent-equal weighting, and the version 2 json; `report -N 2` with an unscored run is an error; `report -N 1` matches `report`.
