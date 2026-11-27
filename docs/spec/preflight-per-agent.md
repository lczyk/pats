# a failed preflight drops its agent, not the run

status: proposed. `model-preflight.md` is the model check before it, `step-sandbox.md` the step sandboxes' own
preflight, `budget.md` the "not started" this reuses, `preflight-overlap.md` the gate this changes, and
`lxd-retry.md` the retries under a preflight's start.

## problem

`pats run` checks each distinct agent's credentials before any pair starts (`Preflight`,
`internal/eval/preflight.go`). the first agent that fails ends the run: the loop returns at once, no later agent
is checked, and no pair of any agent runs.

- **one bad agent costs every other.** a config of eight agents, one of them with an expired login, runs nothing.
  the user fixes that login, starts again, and finds the next one only then. two rounds were lost this way on a
  real batch.
- **the failure says nothing of the rest.** the agents after the failing one were never checked.
- **what it is right about**: no token is spent on a run whose agents cannot all answer. a run that is meant to
  compare agents and misses one is not the run the user asked for. that stays possible, behind a flag.

## proposal

`pats run` checks every agent, whatever an earlier one found. then:

- **all passed**: the run goes on as today.
- **some failed, each in a way that is the agent's or the place's** (below): each failure is logged as today's error is, with the
  agent's name, as it is found. the pairs of the failed agents are not started. the others run as they would have.
  the run ends with `ErrPreflight` and a closing line.
- **any failure that is the invocation's** (below): the run fails as today, before any pair starts.
- **all failed**: the run fails as today, with every agent's error, and makes no run dir.
- **`--strict-preflight`** keeps today's outcome: any failure fails the run before a pair starts. it still checks
  every agent first, so its error names all the failures, not the first.

### whose failure

`preflightAgent` can fail in three kinds of place. only the first two drop the agent.

- **the agent's**: the harness exits non-zero, times out, or reports an error in its stream. the key proxy saw no
  key go out, or saw a placeholder misused. a harness cli missing from the machine's PATH under a driver that uses
  the host's clis. this is what preflight is for.
- **the place's**: the agent's sandbox did not start or did not take the handover (`start: ...`). under
  `lxd-retry.md` a start is retried first, and a failure is logged as the place's. it drops only that agent b/c
  agents can use different sandboxes, and a place that failed one start may well start the next. the line says it
  was the place, so the user knows to look at the host, not the login.
- **the invocation's**: a temp dir not made, a harness that cannot be built from the config, a prompt file not
  written, the audit file missing or not readable. these are this machine's or this config's, every agent would
  meet them, and dropping agents over them would hide the cause. they fail the run as today.

the split is by where `preflightAgent` returns, not by reading error text. one site returns more than one kind
today: `keyProxyChecked`. it is split so each outcome has its own error: the session's close is the place's, the
audit missing or not readable the invocation's, a misused placeholder or no key substituted the agent's.
`readEgressAudit` returns its read error for this, where today it gives an empty audit.

one preflight can fail in more than one way: a harness that exits 1, then a close that fails. so once the session
has started, `preflightAgent` closes it and, under a key proxy, reads the audit on every path, not only after a
harness that passed. the close's error is kept, not dropped as the deferred `Close` drops it today. of the
failures one preflight met, the invocation's wins, then the place's, then the agent's: a failure that would fail
the run is never hidden by one that only drops the agent.

### what "not started" means here

the same as a budget stop's (`budget.md`): a pair not started has no pair dir, and `pats score` and `pats report`
see it as never run. so a mean over the agents that ran is not dragged down by pairs that never had a chance. the
run stays incomplete: a later run is a new run dir, and its pairs are another sample, not a repair of this one.

- **the closing line**, after the pairs: `WARN preflight failed for claude-opus (harness), codex-high (place):
  24 pair(s) not started`. the count is of the selection as it was asked: every pair of a failed agent, in every
  repeat under `-N`. a repeat that the budget then kept from starting counts its dropped pairs here, not in the
  budget's line, so no pair is in both. the budget's line counts what it stopped of the pairs that were left.
- **the error**: `ErrPreflight`, wrapping each agent's error. when more than one stop happened, the run returns
  the first of: `ErrInterrupted`, an error writing a run dir, `ErrBudget`, `ErrPreflight`. an interrupt wins as
  it does today (`Run` returns `ErrInterrupted` whenever its context ended). every line is still logged. the budget comes before the preflight b/c a cap reached is the one a rerun must change, and the
  preflight's line already says the rest.
- **no new file.** what a run is missing is in its log and in the pair dirs it lacks, as with a budget stop. a
  report that wants to say why an agent is missing is a separate change.

### the judge

`pats judge` stays all-or-nothing, and `--strict-preflight` is not on it. a judge that is dropped would leave the
judgements it owns as they were: an earlier run's scores in `scores.json`, read as fresh. telling the two apart
needs a record of which judge was skipped where, for each of the runs a judge invocation covers, and nothing has
asked for it. a judge invocation that fails its preflight is cheap to rerun: no agent work is lost.

### step sandboxes

a step sandbox's preflight (the first loop in `Preflight`) stays fatal. it is the config's or the machine's, every
pair of a task that names it would fail, and the check takes seconds.

### the order of the checks

the preflights run one after another, as today. running them at once is `preflight-overlap.md`'s, and this spec
does not depend on it. what changes in the loop is that every failure is kept, not returned, and the loop goes on
to the next agent. once all are checked, an invocation failure among them fails the run with every failure named,
before any pair starts. otherwise the agent and place failures drop their agents as above.

an interrupt during preflight stops it, as today: the agents not yet checked are not run, and the run ends with
`ErrInterrupted`.

## preflight-overlap.md

that spec opens one gate for the run once every preflight has passed, and fails the run if any fails. with this
spec the run does not fail on one agent, so that spec, when it is implemented, changes to:

- the gate opens once every preflight has **settled**, passed or failed in a way that drops the agent. no agent
  starts before every agent has answered, as that spec wants.
- a preflight that fails in a way that is the invocation's never opens the gate: every pair setup is cancelled and
  its sandbox closed, and the run fails, as that spec says of any failed preflight today.
- a pair of a failed agent is stopped wherever it is: at the gate, or still in its setup, which is cancelled. its
  sandbox is closed and **its pair dir removed**, whatever its stage wrote there, so it is not started in the
  sense above. its stage's effects outside the pair dir are not undone, as that spec says of a stage today.
- a run dir in which no pair is left follows that spec's rule and is removed.
- under `--strict-preflight` that spec stands as written.

## out of scope

- dropping an agent mid-run when its login expires. that is a pair's failure, recorded as one.
- a retry of a failed preflight. a credential failure does not heal in seconds, and a start is already retried
  under it (`lxd-retry.md`).
- `pats run --missing`, which would run the pairs this leaves out. deferred.
- a record of the dropped agents in the run dir, and the judge's partial runs, as above.

## tests

- three agents, the middle one's harness exits 1: all three are checked, the pairs of the first and third run,
  the second's have no pair dir, the run ends with `ErrPreflight` and the line counts its pairs and says harness.
- a start that fails for one agent: the same, and the line says place.
- a prompt file that cannot be written for the first agent: the other agents are still checked, then the run
  fails before any pair, its error naming every failure.
- a key proxy whose audit file is missing fails the run, also when the harness exited 1 first. one that saw no key drops the agent as harness. a close
  that fails drops it as place.
- the same with `--strict-preflight`: no pair runs, no run dir. two failing agents: the error names both.
- every agent fails: no run dir, the error names each.
- `-N 3` with one agent failing: the count is three times its pairs.
- `-N 3`, one agent failing, a budget reached before repeat 3: no pair is in both lines, the two counts add up to
  what was not run, and the error is `ErrBudget`.
- a step sandbox that fails its preflight still fails the run before any agent is checked.
- `pats judge` with one of two judges failing: fails as today, no scores written.
- an interrupt during the second agent's preflight: `ErrInterrupted`, no pair started.
- an interrupt and a run dir write error in one run: `ErrInterrupted`.

## open questions

1. is the default right? the run that compares agents may want strict, the one that collects results wants the
   rest. dropping is the default here b/c the cost of the strict one was the one felt, and the closing line and
   the exit code still say the run is incomplete.
