# budget

status: implemented.

## problem

nothing stops a run from spending. a model that loops keeps calling until the task's `timeout`, every pair does the
same, and `-j` multiplies it. the openrouter key's own limit is a backstop, but a key that runs dry turns the rest of
the run into errors instead of a clean stop, and the errors look like model failures in the report.

## proposal

two caps on reported spend, in us dollars:

- **`pats run --max-cost <usd>`**, for the invocation. once the spend reaches the cap, no new pair starts. running
  pairs finish. the run logs `WARN budget $20.00 reached at $20.13: 14 pair(s) not started` and exits non-zero. pairs
  not started have no pair dir, and `pats score` and `pats report` see them as never run.
- **`max-cost: <usd>` on an agent**, per pair. a pair whose spend passes its agent's cap is stopped: the agent is
  killed, the sandbox's files are synced and collected as for a timeout, and the pair's status is `budget`, with
  `error: "cost $2.04 passed max-cost $2.00"` in its `metadata.json`.

both are limits on spend as the harness reports it, not hard ceilings: a request in flight is paid for before its
cost is seen, and one step can cost more than a cap. the task's `timeout` stays the independent bound.

## values

- a cap is a finite number above zero, at most 1e6. zero, a negative number, `inf` and `nan` are errors, at load for
  `max-cost` and at flag parsing for `--max-cost`. omitted means no cap.
- spend is kept as `float64` dollars, summed as reported, with no rounding. the log prints two decimals.

## where the cost comes from

cost is whatever a harness's parse script reports (`harness-parse.md`), as it reports it:

- **opencode-openrouter**: every `step_finish` event carries the step's `part.cost`, which joins the pair's live
  spend as it streams.
- **claude-cli-keyless**: the cost is in the final `result` event only. it joins the spend there, while the agent
  still runs: a per-agent cap stops a claude pair at its end, with its work done, and marks it `budget`. (a
  keyless, subscription-billed run's cost is notional anyway.)
- **codex-cli-keyless, copilot-cli-keyless**: no dollar cost (`has_cost: false`). they count as zero.
- **validation**: `max-cost` loads on every agent: pats cannot know what a script will report. an agent whose
  preflight reported no cost, and that has a `max-cost` or runs under `--max-cost`, is warned about once: `WARN
  agent X reports no cost: max-cost has nothing to check`. with preflight skipped nothing warns.
- **the credential preflight** runs each agent once on a trivial prompt (`preflight.go`), before any pair. its spend
  is not counted and not capped. it is one short prompt per agent, and the readme says the cap starts at the first
  pair.

## the ledger

the run keeps a ledger under one mutex: the finished spend, and the running pairs' live spend by pair.

- **admission.** a pair takes its `-j` slot, then asks the ledger to start. the ledger says no once finished plus
  live spend has reached the cap. a pair told no records nothing and counts as not started.
- **live spend.** the harness scanner adds each step's cost to the pair's entry, under the ledger's mutex (a step is
  a few per minute, so the lock is cheap).
- **completion.** when a pair ends, by any path (ok, error, timeout, budget, interrupt, a failure writing its
  metadata), the ledger moves it from live to finished in one locked step: its final spend is its summary's
  `cost_usd` when there is a summary with `has_cost`, else its live spend. so the step costs are never counted twice,
  and spend already seen is kept when the summary is missing.
- **overshoot.** with `-j 8`, up to 8 pairs are in flight when the cap is reached, and the run can pass its cap by
  what they spend before finishing. the per-agent cap bounds each of them, where the harness allows. the readme says
  so.

## stopping a pair

- the agent's exec gets its own context, a child of the pair's, cancelled with the cause `errBudget` when the pair's
  live spend passes its cap. sync and collect run on the pair's context, which the budget never cancels, so they run
  as after a timeout.
- the first cause wins: a pair whose deadline passed first is `timeout`, one whose cap passed first is `budget`, an
  agent that exited on its own before either is judged by its exit code. an interrupt (ctrl-c) of the run is
  `error: interrupted`, as now, whatever else happened.
- a sync that fails after a budget stop is recorded with it (`cost $2.04 passed max-cost $2.00; sync: ...`), as for
  timeouts.

## the run cap and repeats

under `--repeat` (`repeats.md`) the run cap is the budget of the whole invocation. once it is reached, no further
pair starts in the current repeat, and no further repeat starts: no new run dir is made. the warning counts both:
`budget $20.00 reached at $20.13: 14 pair(s) of run 3/5 and 2 more run(s) not started`.

## where it shows

- `budget` is a status: `runTotals` counts it (`3 ok, 1 budget`), the report's status mark and legend name it, and
  `report --json` carries it in a pair's `status`.
- `-v`'s totals line already shows the summed `$`. the bar's live line shows a pair's spend once non-zero.

## not doing

- **no estimate-based refusal.** the cap reacts to spend. `pats run --dry-run` (`run-dry-run.md`) is where an
  estimate answers "can i afford this" before starting.
- **no per-task or per-suite cap.** the agent cap covers the looping model.
- **no kill of running pairs at the run cap.** they are paid for up to that point, and killing them turns spent money
  into error rows.

## tests

- a fake harness emitting opencode `step_finish` lines: a per-agent cap stops it mid-stream, status `budget`, sync
  and collect ran, the error names both amounts.
- `-j 1`, three pairs of $1 each, a cap of $1.5: two run, one is not started, the run errors with the count.
- `-j 3` with a fake harness: concurrent completions and admissions never count a pair twice (race detector on).
- a pair whose metadata write fails keeps its live spend in the ledger.
- a deadline and a cap passing in either order give `timeout` and `budget` respectively.
- validation: `max-cost: 0` and `--max-cost -1` are errors. `max-cost` on a claude agent loads, and its pair is
  `budget` at its `result`; on a codex agent it loads and warns after preflight.
- `report` shows a budget-stopped pair with its own mark.
