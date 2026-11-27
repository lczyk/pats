# preflight beside the first pairs' setup

status: implemented, as `preflight-per-agent.md` amends it (below). `init.md` is what a run starts from, `step-sandbox.md` the step sandboxes' own preflight, and
`budget.md` the run's budget, whose clock this moves.

## problem

`pats run` checks every agent's credentials before it starts a pair (`Preflight`, `internal/eval/preflight.go`):
each distinct agent is started in its real sandbox, behind its real egress, and asked for one word. only when all
have answered does the first pair begin, and the first thing a pair does is what the preflight just did: bring a
sandbox up. so the start of every run is two waits in a row that do not depend on each other.

- **on lxd the two are long.** a ci run with an lxd sandbox started its first agent 80 to 120 seconds after
  `pats run` began. a docker one took 11. the lxd time is a sandbox brought up for the preflight, then one for each
  pair of the first wave, each with its stage, its start and its in-sandbox prepare.
- **the preflights wait for each other too.** one agent after another, though no two share anything. a config with
  five agents on lxd brings up five sandboxes in a row before anything runs.
- **what it is not**: a cost per pair. it is paid once a run, so on a forty-minute job it is a minute or two.

not measured: how those 80 to 120 seconds split. a pair's `timings` and the preflight's lines under `-v` say it
from the next release on (`report.md`). the saving below is bounded by the preflight's own time, and is unknown
until that is read off a real run.

## proposal

the credential preflights run at the same time as each other, and as the setup of the first pairs. no agent of a
pair starts until every preflight has passed.

- **what does not move**: the pins, the model check, the dry run, and the step sandboxes' own preflight, all before
  anything else, as today. a step sandbox's check takes seconds, and a pair's stage runs in one.
- **the credential preflights start together**, one for each distinct agent, and the dispatcher starts with them.
- **a pair runs its setup as it does today**: stage and `prompt-cmd`, the sandbox's start, the in-sandbox prepare,
  the handover. then it waits at the gate.
- **the gate** is one for the run. it opens when every preflight has settled, passed or failed, and every pair
  waiting goes on to its agent, unless its agent failed. a pair that reaches it later walks through.
- **an agent whose preflight fails** is dropped (`preflight-per-agent.md`): each of its pairs is stopped wherever it
  is (queued, in setup, at the gate), its sandbox closed and its pair dir removed, failed setup or not. the others
  run. a failure that is the invocation's, every agent failing, or `--strict-preflight` fails the run instead: the
  gate never opens, every pair is stopped where it is, its sandbox closed, and no agent has run.

### one gate, not one an agent

a pair could wait for its own agent's preflight alone. it does not: with five agents, four that pass and one that
fails, the four would have spent tokens in a run that then fails. today not a token is spent before every agent
has answered, and that stays so.

### what a failed preflight leaves

today a run that fails its preflight has made no run dir. with pairs set up beside it, one has been made: stage
writes its log there, and its files.

- **a run dir in which no agent started is removed** when the run ends on a failed preflight, or on an interrupt
  before the gate opened. the next run takes its number. so a failed preflight still leaves nothing in the
  project, and nothing a history reads (`repeats.md`).
- under `-N` that is every repeat's dir that had begun.
- **what is not undone**: a stage that ran. it is the project's script, and what it did outside the run dir and
  the workdir is done. today no stage runs in a run whose preflight fails. this is the one thing a user can see
  change, beside the time.

### what waits, and for how long

- the setup's own timeout (`setupTimeout`) covers the start and the prepare, as today, and ends before the gate.
  the task's timeout and an agent's time budget cover the agent, and start after it. the wait at the gate is in
  neither.
- the wait ends with the preflights, which are bounded as today: a sandbox's start, and 90 seconds for the reply.
- **a pair's `timings` gain `gate_s`**, the time it waited there, so the wait shows where it happened and
  `total_s` still adds up (`report.md`).
- **the run's budget** is made before the dispatcher starts, since a pair is admitted against it, and its clock
  starts when the gate opens: where it starts today, at the end of preflight.

### slots

- a preflight takes no slot, of `-j` or of a sandbox's `max-parallel`, as today. it is one sandbox and one short
  call.
- so for a while a server holds the preflights' sandboxes beside the first wave's: with `-j 4` and three agents,
  seven where it held four. the pairs among them are idle, set up and waiting.
- a pair that waits at the gate holds its slot. nothing else could use it: no pair gets further.

### the proof of the proxy

an agent's preflight is also where a proxied sandbox is shown to have no way out but the proxy
(`Egress.SelfTest`), and the run refuses to start when it has one. a pair's sandbox now comes up before that is
known, and its prepare runs in it as root.

- the agent does not: it is behind the gate, and the gate needs the proof.
- stage and prepare are the project's own scripts, trusted as `pats.yaml` is. what the proof is for is the agent
  and what it reads.
- a proxy that holds keys (`key-proxy.md`) holds a pair's before the preflight has shown a key goes out through
  it. the sandbox has the placeholder either way.

### what is seen

- the lines of today, in a new order: `preflight: <agent> ok` may come after a pair's stage has logged.
- on a terminal the bar shows a pair at the gate as waiting for the preflight, not as running.
- a failed preflight prints its error once, after the pairs have been stopped, and nothing of theirs: no verdict
  line, no totals.

## what it saves

- **one preflight's time**, a run: the longest, since they overlap. more exactly the smaller of that and the
  first wave's setup.
- **the others' time**, with more than one agent: four of five.
- **nothing** where a sandbox starts in a second and a task has no stage: the two waits were never long.

## limits

- **a failed preflight costs a wave of setup**: up to `-j` sandboxes started and stages run for nothing, where it
  cost one sandbox. no tokens, as today.
- **a server sees more sandboxes at once** at the start (above). one sized for exactly `max-parallel` of a heavy
  sandbox has the preflight's on top for its length, as it had it alone before.
- **the order of the log changes**, and a script that read "preflight ok" as "nothing has started" is wrong.

## not doing

- **skipping the preflight** (a flag for a ci that trusts its keys). it is also the proof of the proxy, and a run
  that finds a dead key forty minutes in is what it exists against.
- **the preflight's sandbox reused for the first pair.** an agent has run in it.
- **a gate an agent** (above).
- **`pats judge`**, whose preflight is the same call before its own agent jobs: the same change, later, once this
  one has run.

## changes

- `internal/eval/run.go`: the dispatcher started beside the preflights, the gate, the run's budget made first and
  its clock set at the gate, the removal of a run dir no agent started in.
- `internal/eval/preflight.go`: the step sandboxes' check apart from the credential checks, and those at once.
- `internal/eval/pair.go`: the wait at the gate between the setup and the agent, `gate_s`.
- `internal/eval/progress.go`: a pair that waits.
- docs: `init.md` (run), `report.md` (`gate_s`), the readme's line on what preflight proves and when.

## tests

- a pair's stage, start and prepare happen while its agent's preflight is still running, and its agent starts
  only after: with a preflight held open, the pair is at the gate and no agent call has been made.
- two agents: their preflights overlap, and no pair of either starts its agent until both have passed.
- a preflight that fails: the run's error is today's, no agent ran, every started sandbox is closed, no run dir is
  left and the next run takes the number. with `-N 3`, none of three.
- an interrupt before the gate: the same, and the error is the interrupt's.
- an interrupt after the gate: the run dir stays, as today.
- a setup that fails while the preflight runs is that pair's error, as today, once the gate has opened: the run
  goes on.
- the setup's timeout does not run while a pair waits, and nor does the task's: a preflight slower than both
  fails neither.
- `gate_s` is in a waiting pair's timings and absent from one that came after the gate. `total_s` covers it.
- the run's time budget counts from the gate: a preflight longer than the budget stops nothing.
- a step sandbox that fails its own check stops the run before any pair's stage, as today.
- with one agent, `-j 1` and a task with no stage, the calls are those of today in another order.

## open questions

- whether it is worth it. the saving is one preflight a run, not yet measured, against a run dir that is made and
  removed, a gate, and a stage that can now run in a run that fails. the preflights at once with each other, and
  nothing else, has none of that cost and all of the saving for a config with several agents, and none of it for
  a config with one.
- a pair at the gate under `-vv`: whether it says so, once, or stays quiet.
- whether a preflight should take a slot of its sandbox's `max-parallel` after all, on a server that is exactly
  that big.
