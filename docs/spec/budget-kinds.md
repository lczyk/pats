# budget kinds

status: proposed, for 1.1.0. extends `budget.md`, which stays the reference for the ledger, stopping a pair and
repeats.

## problem

a budget is dollars only, on both levels: `budget: <usd>` on an agent caps each of its pairs, and `--budget <usd>`
caps the invocation. that leaves gaps:

- **codex and copilot report no cost.** a cap on them has nothing to check, and pats can only warn so. a model
  looping on either runs until the task's `timeout`.
- **a looping model shows in its counts first.** a run of 300 tool calls on a task that takes 30 is wrong whatever
  it cost, and the tool count is already kept live for every harness with a `parse`.
- **an unattended run has no clock.** `pats run` overnight can be bounded in dollars, not in hours.

## proposal

`budget` takes a mapping of kinds, as well as the number it takes now. a number is the `cost` kind, so every config
that works today keeps its meaning.

on an agent, per pair:

```yaml
haiku:
  kind: agent
  defaults:
    budget:
      cost: 2            # $ per pair
      tools: 300         # tool calls per pair
  items:
    haiku-claude-cli:
      harness: claude-cli-keyless
      model: claude-haiku-4-5-20251001
    haiku-opencode:
      harness: opencode-openrouter
      model: anthropic/claude-haiku-4.5
      budget:
        cost: 0.5        # overrides the default's cost, keeps tools: 300
        tokens: 2M       # reported tokens per pair
        time: 5m         # wall-clock per pair; only ever shortens the task's timeout
    old-style:
      harness: opencode-openrouter
      model: anthropic/claude-haiku-4.5
      budget: 1          # same as {cost: 1}, merged over the default's tools: 300
```

on the command line, for the invocation:

```sh
pats run --budget 20                      # $20, as now
pats run --budget cost=20,time=2h         # $20, or 2h after preflight, whichever first
pats run --budget tokens=50M              # 50m reported tokens across every pair
pats judge --budget cost=5,tools=2000
```

## kinds

| kind | unit | per pair | per invocation | read from |
|---|---|---|---|---|
| `cost` | us dollars | yes | yes | the spend as `budget.md` has it, list-price estimates included |
| `tokens` | reported tokens | yes | yes | the parse script's live usage (below) |
| `tools` | reported tool calls | yes | yes | the parse script's live tool count |
| `time` | wall-clock | yes | yes | pats's own clock |

- **`tokens`** is the sum of the input, output, cache read and cache write tokens a harness reports. harnesses and
  providers split a prompt between cached and uncached differently, and claude's input outside the cache is a few
  hundred tokens on a long run, so the sum is the one count that means the same across harnesses that report all
  four. it is a cap on what is reported: copilot reports output tokens only (below), so its sum is a fraction of
  what the model handled. it is not the `tokens` of the run's totals line, which is output tokens.
- **`time` on an agent** is a deadline for its pair's agent step, beside the task's effective timeout: the task's
  `timeout`, 10m when it sets none, none at `0`. a judge's is its scorer's, 5m when it sets none. the earlier
  deadline binds. a `time` at or above the effective timeout does nothing, and the readme says so: it shortens,
  never extends. under a timeout of `0` the agent's `time` is the only deadline.
- **`time` on the command line** is wall-clock from the end of preflight, once per invocation, across every repeat
  and, for `pats judge`, every run. it is an admission deadline, not a kill: once passed, no new pair or judge job
  starts, and the running ones finish under their own bounds. a pair with no timeout and no `time` of its own can
  therefore run long after it.
- **`tokens` and `tools` on the command line** are sums over every pair, finished and live, as cost is.

## what each harness reports

when a count arrives decides when a cap on it can act:

| harness | tools counted | tokens reported | tokens arrive |
|---|---|---|---|
| claude-cli-keyless | on the assistant message that calls one | all four | on the final `result` |
| codex-cli-keyless | when the call completes | input, output, cache reads | on `turn.completed` |
| copilot-cli-keyless | when the call starts | output only | per assistant message |
| opencode-openrouter | on its `tool_use` event | all four | per step |
| pi-openrouter | when the call starts | input, output, cache reads and writes | per assistant message |

- a `tools` cap stops every harness mid-run, but on codex only once a call has completed: a call that hangs is not
  counted until it returns, and the task's timeout is what bounds it.
- a `tokens` cap stops opencode, pi and copilot mid-run. claude's and codex's totals come once, at the end of their
  run, so a cap marks such a pair `budget` there, with its work done, as a cost cap does for claude now.
- a harness of your own reports what its parse script says. a harness with no `parse` reports neither count.

## values

- **`cost`** as now: finite, above zero, at most 1e6.
- **`tokens` and `tools`** are whole numbers above zero, at most 1e12. a value may carry a suffix: `k` (1e3), `m`
  (1e6) or `g` (1e9), either case, after a decimal number. the product must be whole, and is computed exactly.
  `20k`, `1.5M` and `2g` are values; `1.2345k`, `20kb`, `20Ki`, `1e3`, `-5` and `0` are errors. the suffixes are
  decimal: a token count is not a size in bytes. in yaml a plain integer works too (`2000000`), and a suffixed one
  is a string to yaml either way, so `2M` and `"2M"` are the same.
- **`time`** is a go duration above zero: `90s`, `5m`, `2h30m`.
- an unknown kind, a value of the wrong type and a value out of range are errors, at load for `budget` and at flag
  parsing for `--budget`, each naming the kind.
- `budget: {}` is no cap, as is a mapping whose every kind was unset (below).

the flag takes a bare number, which is the cost, or a comma-separated list of `kind=value` with the values above.
a kind given twice, an empty list and an empty value are errors. it is a new grammar: a cost written in a form the
old float flag took and the new one does not (`1e2`, `0x10`) is an error now.

## defaults

`budget` merges over a vector's defaults key by key, as `env` does: a kind the item leaves out is the default's,
`tools: ~` unsets one kind, and `budget: ~` unsets them all. a number is read as `{cost: <number>}` before the
merge, on either side, so `budget: 1` on an item replaces the default's cost and keeps its other kinds.

## stopping a pair

every kind given is a limit, and the first one passed stops the pair. it stops as a cost cap does now
(`budget.md`): the agent is killed, its files are synced and collected, and the pair's status is `budget`. its
`metadata.json` names the kind in `error` and in a new field `budget_kind`:

- `cost $2.04 passed its budget $2.00` (unchanged)
- `tokens 2013456 passed its budget 2000000`
- `tools 301 passed its budget 300`
- `time 5m0s reached its budget 5m0s`

counts print in full, so the two numbers are never rounded to the same one.

- **one line can pass two caps.** the counts a line brings are checked in a fixed order, cost, tokens, tools, and
  the first passed is the pair's `budget_kind`.
- **the first cause wins**, as now: the agent's context is cancelled once, with the kind that stopped it, and
  output arriving while it stops changes nothing. an interrupt (ctrl-c) is `error: interrupted` whatever else
  happened.
- **two deadlines.** an agent's `time` below the effective timeout is a timer of its own, which gives `budget`. the
  task's deadline gives `timeout`. when they are equal the agent's does nothing and the pair is `timeout`.

## the run cap

the ledger keeps a sum per kind, finished plus live, as it does for cost. once any kind given reaches its cap, no
new pair or judge job starts, and the ones running finish.

- **one check can reach two caps.** the kinds are checked in the order cost, tokens, tools, time, and the first
  reached is latched. it is the one the warning names, and later checks do not change it.
- **the warning** names the kind, its cap and where it stood: for cost, tokens and tools the sum when the run
  ends, for time the clock when the cap latched.
  - `budget $20.00 reached at $20.13: 14 pair(s) not started` (unchanged)
  - `budget tokens 50000000 reached at 50213456: 14 pair(s) not started`
  - `budget time 2h0m0s reached at 2h0m0s: 14 pair(s) of run 3/5 and 2 more run(s) not started`
  - `budget tools 2000 reached at 2013: 3 judge job(s) not started`
- **judges** are admitted per sample: under `-N 3` one score is three judge jobs, and the count in the warning is
  of jobs. what a judgement with samples held back does is as under a cost cap now: one with none started keeps
  its earlier verdict.

## the final count

a pair's live counts come from the live pass of its parse script, and its summary from the pass over
`stdout.log` after it ends (`harness-parse.md`). the two can differ: the live pass never sees a last line with no
newline, and a `usage(total = True)` may restate a total below the sum so far. so, per kind, as for cost now:

- **live**, each line's totals feed the pair's counts and the ledger as differences against the pass's previous
  totals. a difference can be negative, and the sums take it.
- **when the pair ends**, its counts in the ledger are its summary's when there is a summary: cost when the summary
  has one, tokens when the harness reported any, tools always. without a summary the live counts stand.
- **a cap is decided live.** a pair's status is set by the live counts only. a count that passes a cap only in the
  summary changes no status, and counts toward the run cap from then on.
- a run cap once reached stays reached, as now, whatever the final counts.

## warnings

after preflight, an agent with a cap of a kind its harness reports nothing of is warned about, once per kind. the
check is whether the harness reported the count at all, not whether it was above zero:

- `cost`, as now: `agent X reports no cost: its budget has nothing to check`.
- `tokens`, when preflight reported no token count, 0 included: `agent X reports no tokens: its tokens budget has
  nothing to check`.
- `tools`, when the harness has no parse script: `agent X reports no tool calls: its tools budget has nothing to
  check`. preflight asks the agent to call no tool, so zero calls there say nothing.

each counts the agent's own cap and the run's (`--budget`) alike. `time` always has something to check.

## where it shows

- the `budget` status, its report mark `B` and the legend stay as they are.
- `report --json` carries a pair's `budget_kind` beside its `status`: `null` for a pair not stopped by a budget,
  and for a pair from before this field. it is a new field, so the document's `version` stays.
- `pats run --dry-run --json` replaces the total's `budget_usd` with a `budget` object holding the caps given
  (`cost_usd`, `tokens`, `tools`, `time_s`). `fits` stays, about cost: `null` without a cost cap. the text form
  names the other caps, with no fit.

## compatibility

`budget: <number>` keeps its meaning, as does `--budget <number>` in its plain decimal form, and the cost messages
are unchanged. the dry run's json and the flag's grammar change, as above. a config using the mapping fails to load
on 1.0, which takes a number only. a minor release.

## not doing

- **no per-agent total across a run.** an agent's budget is per pair. a cap on all of one agent's pairs together
  would be a ledger bucket per agent, and wants its own spelling; it can come later without breaking this one.
- **no `turns` or `premium_requests` kinds.** both are in the parse output and would fit the same path, but nothing
  asks for them yet.
- **no binary suffixes.** `Ki` and `Mi` are for bytes.
- **no kill of running pairs at the run's `time`.** a deadline on finishing would need a cancellation policy for
  running work and its cleanup.

## tests

- load: `budget: 2` and `budget: {cost: 2}` give the same agent. key-by-key merge over defaults, a number over a
  mapping, `tools: ~` and `budget: ~`. an unknown kind, `tokens: 1.2345k`, `tools: 0`, `time: -5m` and `cost: inf`
  are errors naming the kind.
- suffixes: `20k`, `20K`, `1.5M`, `2g` parse to their values; `20kb`, `20Ki`, `k`, `1e3`, `1e3k` are errors.
- flag: `20`, `cost=20,time=2h`, `tokens=50M`; `cost=1,cost=2`, `nope=1`, `cost=`, `` are errors.
- a fake harness emitting opencode `step_finish` lines: a `tokens` cap stops it mid-stream and a `tools` cap stops
  it past its count, each with status `budget`, its `budget_kind`, sync and collect run.
- one line passing a cost and a tokens cap gives `budget_kind: cost`.
- a claude-style log with usage only on `result`: a `tokens` cap marks the pair `budget` at its end.
- `time` on an agent below the task's timeout gives `budget`; equal, `timeout`; with `timeout: 0`, `budget`.
- the ledger: tokens and tools sums, negative differences, the summary's counts replacing the live ones, the
  first kind latched, and a time cap on an injected clock.
- run-wide: `-j 1`, three pairs of one tool call each, `--budget tools=1`: one runs, two are not started.
- preflight warns for a tokens cap on a harness that reports no tokens, and for a tools cap on one with no parse.
- `report --json` has `budget_kind` on a budget-stopped pair and `null` elsewhere. the dry run's json has the
  `budget` object.
