# report

status: implemented.

## problem

the report is a convenience, not what pats is for. pats runs agents on tasks and scores what they leave; the report is
a human-readable look at one run, and anyone who wants more parses the results themselves. today it is neither quite
small nor quite parsable:

- `pats score` and `pats report` print three sections to stdout, mixed with log lines: a tasks x agents pivot
  (`report()`, `score.go`), a per-scorer breakdown of every pair, and a 13-column run stats table (`statsReport`,
  `stats.go`). on 10 agents x 30 tasks the breakdown alone is 300 lines, and the stats table is wider than 120
  columns.
- the only machine-readable result is `scores.json`, which is pats's own state: the base a narrowed score merges into
  (`cli-filters.md`), with derived maps keyed by `"agent/task"` strings and nothing of a pair's status or cost. the
  rest is in each pair's `metadata.json`. a script joins a dozen files, and every change to either is a break.

## proposal

- **`pats report --json`** prints one json document to stdout and nothing else: the machine interface of a run's
  results. it is on `report` only. `pats score` keeps printing the human report, and a script runs
  `pats score && pats report --json`.
- **the document is the contract.** `scores.json` and `metadata.json` stay pats's internal files: readable, but free to
  change between versions. the readme points scripts at `report --json`.
- **the human report shrinks** (below).
- **`cell` goes.** what the code and the docs call a cell, one scorer's result for one pair, is a score.

## the document

```json
{
  "version": 1,
  "run": {"name": "003-20260621-fluffy-bunny", "dir": "/abs/path/.pats/runs/003-20260621-fluffy-bunny"},
  "overall": 0.82,
  "agents": [
    {"name": "claude-opus", "mean": 0.9}
  ],
  "pairs": [
    {"agent": "claude-opus", "task": "slicer-libc6", "mean": 0.75,
     "status": "ok", "error": null, "duration_s": 312.4,
     "turns": 41, "input_tokens": 1203, "cache_read_tokens": 118520, "cache_creation_tokens": 9310,
     "output_tokens": 8812, "reasoning_tokens": null, "cost_usd": 0.41, "premium_requests": null,
     "denied_egress": [], "blocked_egress": []}
  ],
  "scores": [
    {"agent": "claude-opus", "task": "slicer-libc6", "scorer": "builds", "score": 1}
  ]
}
```

- **`scores`** -- every score the means are over, one per (agent, task, scorer), as `scores.json` holds them after any
  merges. a scorer that said `na` or failed has no entry, as it has no score; the human report and the means treat it
  the same way.
- **`pairs`** -- one per pair that has a `metadata.json` or a score: the union, so a pair that ran and scored nothing
  is listed with `mean: null`, and a scored pair whose metadata is gone (a run that died mid-pair) with `status:
  null`. the run fields come from its `metadata.json` as it is at report time. a `metadata.json` that cannot be read
  or parsed counts as missing: the pair is still listed, its run fields `null`, and a warning naming the file goes to
  stderr, so a report never fails on one bad pair and never drops one silently. the human stats table lists it, and
  a scored pair with no `metadata.json`, with status `?`. the pair dirs are listed, not globbed, since a run dir's
  path may hold glob metacharacters, and a dir that cannot be listed is warned about too. `writeJSON` writes through a temp file in the same dir and a rename, so a write cut short leaves the
  old file or none.
- **unknown is `null`, never zero.** the summary (`runSummary`, `progress.go`) keeps a count its log did not carry
  at 0: codex never reports turns, copilot never input tokens, and a run cut short may stop before the event that
  carries them. so the report reads a 0 turn, token or premium-request count as `null`. a real 0 is lost with it,
  which a finished agent run never has. `cost_usd` follows `has_cost`, and every count is `null` when the pair has no
  summary. a summary parser whose scan stopped early (`sc.Err()`: a line over the 8MiB cap, a read error) returns no
  summary, since a count over part of the stream is an undercount that looks whole.
- **tokens** -- one meaning across harnesses, whatever each log calls them:
  - `input_tokens`: input not served from the prompt cache, cache writes excluded (claude's `input_tokens`; codex's
    input minus its cached part, as `codexSummary` computes).
  - `cache_read_tokens`, `cache_creation_tokens`: the cache's reads and writes, where the harness reports them.
  - `output_tokens`: everything the model generated, reasoning included. claude and codex already count it so;
    opencode reports reasoning apart, so its summary parser adds it in.
  - `reasoning_tokens`: the reasoning part of `output_tokens`, where the harness reports it.
  - `premium_requests`: copilot's billing unit; `null` on the other harnesses.
- **`agents`** -- one per agent with a pair in `pairs`, its `mean` the mean of its pairs' means (as today,
  `aggregate`), `null` when none of its pairs has one. an agent whose every pair failed is listed, so the `failed`
  count agent scorers add (below) has somewhere to go.
- **`overall`** -- the mean of the non-null agent means; `null` when there are none.
- **means are unrounded.** rounding is the human report's.
- **order** -- every array sorted by its names (agent, then task, then scorer), so two reports of one run diff cleanly.
  the human report keeps its worst-first order.
- **`run.dir`** is absolute, `run.name` its basename.
- **version** -- an integer. a new field is not a new version, and a consumer ignores fields it does not know. a field
  renamed, removed or changing meaning bumps it. pats prints only the current version; there is no flag to ask for
  an older one.
- **later fields** -- agent scorers (`agent-scorers.md`) add `failed` to each agent (its unjudged pairs) and a
  `judges` array (per judge agent: cost, tokens, duration, failures), as new fields of version 1.

## the command

- `pats report [-r <run>] [--json]`. `-r` resolves as today; `--json` takes one run, as `report` does.
- with `--json`, stdout holds the document and nothing else: the `report: <dir>` line `Report` prints today is not
  printed (the document names the run), and no colour, no table. errors go to stderr with a non-zero exit and an
  empty stdout, so `pats report --json | jq` never parses half a line.
- no `scores.json` is the same error as today (`no scores for <run> (run pats score first)`), on stderr.
- it reads only run artefacts, no config, as `report` does today: the pair and score names are what the run recorded.

## the human report

one tasks x agents pivot, worst task first, each cell a pair's mean with a mark when the pair did not end ok, and
per-agent `avg`, `cost` and `time` rows. it replaced the breakdown and run stats table this section first proposed;
`report()` in `report.go` and the readme describe it.

- log lines stay on stdout as today. `report --json` is the one place stdout has to be clean.

## dropping cell

- the type `ScoreCell` is renamed for what it holds, a score. `eval.Score` is the command's function, so the type
  is `ScoreResult`.
- in `scores.json`, `cells` becomes `scores`, and the file gains an internal `version` (2; today's has none). a renamed
  key alone would not do: an old file still decodes, with no scores but its old `overall`, so `pats list runs` would
  show a number nothing backs. every reader checks the version first and treats any other as not scored by this pats:
  `pats list runs` shows `-`, `pats report` (and `--json`) fails with `<run> was scored by an older pats: run pats
  score -r <run>`, and a narrowed score refuses to merge into it with the same message, since merging would drop
  every score the file held. an unnarrowed `pats score` replaces it, as it replaces any file. 1.x is unreleased, so
  there is no reader for the old format: `pats score -r all` rewrites every run.
- user-facing words: `-j`'s help (`max scores to compute in parallel`), `--judge`'s (`each judges its pairs in a
  sandbox`), and the two log lines (`computing <n> scores, up to <m> in parallel`, `<n> agent scores need --judge`).
- the docs: `cli-filters.md`, `agent-scorers.md` (an error cell is a failed score, a judged cell a judged score, an
  agent cell an agent score) and the readme.

## not in this

- `--json` on `run`, `score`, `list` or `clean`, and other formats (csv, markdown, html). one machine format in one
  place; anything else is a script over it.
- the per-pair breakdown in the document beyond `scores`: a consumer groups them itself.
- a stable `scores.json` or `metadata.json`.

## several runs

`pats report -N <n>` reports the latest n runs together, as document version 2: `runs`, and per pair `mean`, `sd`,
`n` and one `samples` entry per run it ran in. a single run keeps version 1. see `repeats.md`.
