# cli filters

status: implemented.

## problem

a suite has three axes: agents, tasks, scorers. `pats run` uses two of them and filters both (`-a`, `-t`); `pats
score` uses all three and filters none. the only way to score a subset (one scorer just fixed, one judge being
iterated on) is a suite made for it: an edit to `pats.yaml` for a one-off selection.

a filter on an axis the model already has is not a second way of doing something; it narrows what suites select, the
way `run`'s filters do. so `score` gets the same filters, plus one for scorers.

## proposal

| flag | long | on | selects |
|---|---|---|---|
| `-a` | `--agent` | `run`, `score` | agents or agent vectors |
| `-t` | `--task` | `run`, `score` | tasks or task vectors |
| `-s` | `--scorer` | `score` | scorers or scorer vectors |
| `-S` | `--suite` | `run`, `score` | suites or suite vectors |

- lowercase short flags name the three item kinds, uppercase the grouping: `--suite` moves from `-s` to `-S` on both
  commands, and `-s` becomes `--scorer`. 1.x is unreleased, so `-s` changes meaning with no alias.
- each is repeatable and takes item or vector names, as `run`'s `-a` / `-t` do today. an unknown name is an error.
- they intersect: the suites selected (all when `-S` is absent) expand to their pairs, then each filter keeps only
  what it names. for `score`, `-a` / `-t` narrow the pairs scored and `-s` the scorers run on them.
- a filter value that reaches nothing, and a selection that leaves nothing, are errors (a typo guard), as they are for
  `run` today (`Config.FilterPairs`).
- a narrowed score (any of `-S`, `-a`, `-t`, `-s`) merges into the run's `scores.json`: the scores it set out to
  compute are replaced by what it computed (one that now fails or says `na` is dropped, not kept stale), every
  other score is kept unless the config no longer has its (agent, task, scorer), and the means cover them all. a
  narrowed score that reaches no score is an error and leaves the file alone; `-r all` skips a run where that happens
  and fails only when every run does. an unnarrowed score replaces the file, as before.
- `pats judge` takes the same filters and merges the same way (`agent-scorers.md`): `pats judge -s my-judge` judges
  that scorer's pairs and nothing else. `pats score` lost `--judge`: an agent score in its selection is read from its
  stored verdict.

## the one trap

names are per kind, so a suite and a scorer may share one. an old `pats score -s x` meant the suite `x`; it now means
the scorer `x`, silently when both exist. everywhere else a habit carried over fails loudly as an unknown scorer.
nothing guards this; it lasts only until people's fingers learn `-S`.

## to change with it

- `cmd/pats/main.go` -- the `RunCommand` and `ScoreCommand` flags.
- `internal/eval` -- `ScoreOptions` gains agents, tasks and scorers; the score phase filters its expanded pairs as
  `Run` does.
- `README.md` -- the flag examples (`pats run -s readme` becomes `pats run -S readme`).
