# repeats

status: implemented, overlapping repeats and the dispatch order included.

## problem

one run of a selection is one sample per pair. an agent's score on a task varies run to run, so a baseline needs
several samples: n >= 5 is the usual floor, and an earlier evaluation took 14 separate `pats run`s to get there. today
that is a shell loop, and nothing in pats reads several runs together.

## proposal

- **`pats run -N/--repeat <n>`** runs the selection n times in one invocation. each repeat is an ordinary run: its
  own run dir, number and name. like any run it is unscored until `pats score` (or `pats score -N n` for the series).
  the repeats overlap: a pair of the next repeat may start while the last pairs of the one before are still running.
- **`pats score -N/--runs <n>`** scores the latest n runs, each on its own, as `-r` does for one.
- **`pats report -N/--runs <n>`** reports the latest n runs together.

n is a whole number of at least 1, checked before anything else happens. `-N` does not combine with `-r`.

## run: the series

`Run` splits its work in two:

- **once per invocation**: the signal context, the config lock, `defer sandbox.Shutdown()`, loading and selecting,
  `usePins`, the model check (`model-preflight.md`), the credential preflight, the upstream checks, the budget's ledger
  and the worker count (`-j`, auto included, resolved once).
- **once per repeat**: its run dir, its pairs, and its totals line.

### one queue

the invocation runs every pair of every repeat through one queue and one worker pool of `-j` slots. a repeat no longer
waits for the one before it to end, so the slots a slow pair leaves idle at the tail of one repeat go to the next
repeat's pairs.

- **the next pair to start** is the first queued pair, in dispatch order (below), that can start now: a `-j` slot is
  free and its sandbox's `max_parallel` has room. a pair held back by its sandbox's cap does not hold back a later
  pair of another sandbox.
- **`-j`** caps the invocation's running pairs, as it caps a run's today, and `max_parallel` caps a sandbox's across
  every repeat.
- **a single run** (`-N 1`, or no `-N`) is the same dispatcher, the same code, with one repeat queued: it dispatches
  every pair and is done. there is no second path for it. where today its pairs start in whatever order their
  goroutines win the slots, they now start in dispatch order.
- **one dispatcher** owns the queue: it picks the next pair, admits it, makes its run dir and hands it to a worker, one
  pair at a time. sharing today's blocking slot take between more goroutines would not keep any order.

### dispatch order: longest first

the ordering lives in `internal/sched`, a scheduler that knows nothing of pats and keeps no clock (its README says
how to use it); the dispatcher times the pairs and hands it what they took. the dispatcher starts the pairs it expects
to run longest first, so a long pair is not left to start last and run
alone at the tail while the other slots sit idle. it keeps an estimate per pair, from past runs and from what this
invocation has seen, and updates it as pairs start and end.

- **the key** of an estimate is the dry run's history key (`run-dry-run.md`): the pair's agent and task with its
  harness, model and effort, and its provider block once provider pinning (`provider-pinning.md`, proposed) adds it
  there. every repeat of a pair shares one estimate, so the estimate never
  depends on the repeat.
- **what is measured** is a pair's `duration_s`: the agent's run, as `metadata.json` records it. a pair with status
  `error` is no sample (an error is often immediate, and its duration is not the agent's), whatever else; `ok`,
  `nonzero`, `timeout` and `budget` are, with a cost or without (the cost history's `has_cost` filter is the dry
  run's, not this).
- **from history**: the median `duration_s` of matching past pairs, read once per invocation from the numbered run dirs
  under `.pats/runs/`, before the first pair starts. the reading is the dry run's, shared (one walk can gather cost
  and duration). a run dir or metadata file that cannot be read or does not parse is skipped without a word, since the
  dry run is where history is checked; no `.pats/runs/` is no history. a run never fails over its history.
- **from this invocation**: once a pair of the key has ended here as a sample, the median of this invocation's
  samples replaces the history's.
- **a running pair** raises its key's estimate to at least the time since it started (setup included, a slight
  over-estimate that only starts its siblings earlier): a pair still running at 15m is a pair of at least 15m, whatever
  its history said.
- **capped by the timeout**: no estimate is above the pair's effective agent timeout (the task's `timeout`, shortened
  by the agent's `time` budget, `budget-kinds.md`), so history from a longer timeout does not overrank it. a task
  `timeout: 0` with no agent `time` is no deadline and no cap; with an agent `time` that is the cap.
- **unknown** is a key with no history sample, no sample here and no pair running. unknown ranks above every
  estimate: it may be the longest, and starting it early learns its length soonest. once one of its pairs runs, the
  key has the running pair's floor above, so the key's other repeats rank by that.

the order is: unknown first, then the longest estimate, then the earlier repeat, then the selection's order. with no
history at all, every key starts unknown and ties, so the first pairs started are repeat 1's in the selection's order,
and from then on the running pairs' floors and the first samples rank the rest.

two things follow from every repeat of a pair sharing one estimate, ties going to the earlier repeat:

- a pair never starts before its copy in an earlier repeat. so a repeat's first started pair comes after the one
  before's, and run dirs are made in repeat order.
- at a stop, a repeat has at least as many pairs started as the one after it, and the repeats with pairs not started
  are the last ones begun, consecutive.

with `-v` the dispatcher says once, before the first pair, how many pairs it has an estimate for: `dispatch: longest
first, history for 12 of 15 pair(s)`.

### starting a pair

the dispatcher starts a pair in three steps, in this order, and nothing else starts a pair:

1. check the context. once it is cancelled, nothing further starts.
2. ask the ledger to admit it (`budget.md`): this is where every run-wide cap, `time` included, is checked. a pair
   refused is not started, and no other pair is after it.
3. make its repeat's run dir if this is the repeat's first started pair.

a pair admitted is started. a ctrl-c or a cap reached after step 2 does not take it back: it runs into the cancelled
context, as a pair that started just before an interrupt does today, or runs to its end under a cap reached by
others. if step 3 fails the admitted pair does not run: it leaves the ledger with nothing used, counts as not
started, and the queue stops (below).

### the same pair twice at once

two repeats may now run the same agent and task at the same time, which no single run does. each has its own run
dir, workdir, home and sandbox, so pats' own state does not meet. the task's own scripts are another matter: stage,
prompt-cmd and collect run in the task's dir, shared by every pair of the task as they are today across agents, and
a script that names a host resource (a file in the task dir, a port, a remote object) by agent and task alone can meet
itself. such a script names it by `PATS_RUN_NAME` too, or `PATS_OUTPUT_DIR`. the readme's task-script notes say so.

### run dirs

a repeat's run dir is made when its first pair is about to start, after the budget admitted that pair (`budget.md`),
and not before. so run dirs are made in repeat order, their numbers follow the repeats, and a repeat none of whose
pairs started has no run dir. the log line `INFO run 2/5: 004-20261109-fluffy-bunny` comes when the dir is made, and
`latest` points at the newest dir made, which may be a repeat still running beside earlier ones.

a run dir that cannot be made stops the queue: no further pair starts, the running ones end as they would, and the
invocation fails with that error. the error wins over a reached budget's `ErrBudget`, though the budget's warning is
still logged when a cap was reached.

### logs and the bar

- **the label** of a pair carries its repeat when n > 1: `haiku/curl#2`, in all its log lines (status, warnings,
  setup errors, `-v`'s detail, `-vv`'s stream) and on the bar. with n = 1 it stays `haiku/curl`. the sandbox and the pair's env
  (`PATS_RUN_NAME`, `PATS_RUN_DIR`) are told the pair's own run, as today.
- **the bar** is one for the invocation: its total is n times the pairs, and its live lines show the running pairs of
  every repeat. a pair that will not start (refused by the budget, left in the queue at a stop) comes off the total,
  so the bar reads what ended of what will run, and it reaches its total when the last running pair ends.
- **a repeat's totals line** (`-v`) comes once, when every pair of the repeat has settled: ended, refused, or left in
  the queue at a stop. so it comes after its last running pair's output is out and that pair has left the ledger, even
  when siblings queued after it were refused earlier. it is prefixed with the repeat when n > 1:
  `INFO run 2/5: 3 ok, 1 budget  9.2k tok  $0.41  21m3s`, and its wall time is from its first pair's start to its last
  pair's end. repeats may settle out of order (repeat 2's line before repeat 1's), and a repeat never begun has no
  totals line.

### stopping

- **ctrl-c** starts no further pair in any repeat. running pairs end as an interrupt ends them today, every repeat
  begun keeps what it has, no further run dir is made, and the invocation exits non-zero (130).
- **a reached budget** starts no further pair in any repeat and makes no further run dir (`budget.md`).
- **failing pairs** do not stop the queue, in their repeat or any other.
- **`-n/--dry-run`** with `-N` makes no runs and multiplies the estimates (`run-dry-run.md`).

the invocation returns once every started pair has ended, its cleanup attempted, as a run returns today. a
cleanup that fails warns and leaves what it could not remove to `pats clean`, as now.

the readme's `-N` paragraph says the repeats overlap, where it says one run after another today.

## which runs "the latest n" are

one ordering everywhere: `sortedRunNames` (date, then number), which `-r`, `latest` and `pats list runs` already use.
the latest n are its last n entries. there is no series id: a repeat writes plain runs, and `-N` reads plain runs, so
a run made by hand in between is one of the latest n. `pats list runs` shows what `-N` will pick.

- **score**: the run list is taken once, up front. fewer runs than n is an error naming how many there are. each run
  is scored in turn, as `-r all` does: a run with nothing to score under the filters is skipped with a note, any other
  error stops.
- **report**: every one of the n runs must be scored by this pats. a run without `scores.json` is an error (`run 004
  is not scored (pats score -N 3)`), and so is one whose `scores.json` is of another version or does not parse, each
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
  "runs": [{"name": "004-20261109-fluffy-bunny", "dir": "/abs/.pats/runs/004-..."}],
  "overall": 0.81,
  "agents": [{"name": "haiku", "mean": 0.8}],
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
cost and duration. per-run facts of a pair live in its samples, each carrying its run name. a pair carries only what is
computed across runs. `report.md` gets the version 2 shape beside version 1.

## not doing

- **no series id, no series dir.** that would make a series a new kind of thing every reader has to know.
- **no repeats as separate invocations side by side.** the config lock stays one run per project dir, and one
  invocation's queue already keeps the `-j` slots busy across repeats.
- **no dispatch order by cost, no weighting by slots, no switch.** longest first is the one order. `-j 1` still runs
  one pair at a time, in that order.
- **no duration estimate in the dry run.** the dispatcher's estimate is for ordering, and the dry run stays about
  cost.
- **no confidence intervals or tests** in the report. mean, sd and n are enough to see whether two agents differ, and
  more belongs in the reader's own analysis of `report --json`.

## tests

- `run -N 3` with a fake sandbox makes three run dirs, runs the credential preflight once, keeps the lease renewal
  alive into the third, and on interrupt starts no further pair and makes no further run dir; an interrupt during
  the last repeat exits non-zero.
- `run -N 2 -j 2` with one slow pair and one fast one: the fast pair of repeat 2 starts while the slow pair of repeat
  1 is still running, and the invocation returns only after both slow pairs end.
- the queue order, with a fake sandbox whose pairs end when the test says, a fake clock and injected history: with no
  history and `-j 1`, selection `x, y` (x 10m, y 1m) and `-N 3` start `x1, y1, x2, x3, y2, y3`: repeat 1 in the
  selection's order while both are unknown, then by what was learned; `a/x, a/y, b/z` with no history, `-j 2`
  and sandbox `a` at `max_parallel: 1` start `a/x, b/z, a/y`; with `-j 2` and two pairs ending at once, the next two
  started are the first two in dispatch order; with two sandboxes each at `max_parallel: 1`, a held-back pair lets a
  later pair of the other sandbox start, the caps hold across repeats, and run dirs are still made in repeat order.
- dispatch order: history `x` 10m, `y` 1m, `z` none starts `z, x, y`; a sample here replaces the history's median;
  a pair still running past its estimate raises its siblings above a longer history; an estimate over the current
  timeout ranks at the timeout, a task with `timeout: 0` is not capped, and one with `timeout: 0` and an agent `time`
  is capped at that; an `error` sample is ignored; a key whose only pair is running is no longer unknown.
  with `-N 3 -j 2`, pairs `long` (10m) and `short` (1m, 1m, ...): every `long` starts before the last `short`s, and the
  invocation ends sooner than repeat by repeat would on the fake clock.
- a history with an unreadable run dir and a metadata file that does not parse: the run starts, with no warning, and
  ranks by the rest.
- the same agent/task in two repeats at once gets two run dirs, two workdirs and `PATS_RUN_NAME`s of its own in
  stage and collect.
- a budget reached during repeat 2 of 3, with repeat 1's last pair still running: no third run dir, repeat 1's
  totals line after that pair ends, the warning `of runs 1-2/3 and 1 more run(s)`, the bar's total dropped by the
  refused pairs.
- a ctrl-c and a `time` cap each arriving between admission and the run dir: the admitted pair is started (and ends
  interrupted on ctrl-c), and nothing after it starts.
- with n > 1 the label carries the repeat (`haiku/curl#2`) in a pair's log lines, setup errors included, and each
  repeat's totals line comes once, repeat 2's before repeat 1's when it settles first.
- a run dir that cannot be made for repeat 2: its admitted pair does not run and is not in the ledger, the queue
  stops, repeat 1's running pairs end, and the invocation fails with that error.
- a pair dir with no metadata whose scorers all failed is still an attempt in the multi-run report.
- a pair with a missing metadata in one run shows the unknown mark, is counted non-ok, and its duration is left out
  of the average.
- `-N 0` and `-N -1` are errors before any side effect.
- `score -N 4` with three runs is an error; with filters that leave one run empty, that run is skipped.
- `report -N 2` gives mean, sd and n per pair, null sd at n = 1, an all-`n/a` attempt kept in `samples` with a null
  mean but not in n, the worst status mark with its count on a mixed pair, cost summed over unscored attempts too,
  agent-equal weighting, and the version 2 json; `report -N 2` with an unscored run is an error; `report -N 1` matches `report`.
