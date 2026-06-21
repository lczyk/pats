# pats

score-based testing framework for testing and scoring agent tasks.

to use pats, put `pats.yaml` in your project root. see [`pats.example.yaml`](./pats.example.yaml) for an example of such a configuration. 

`pats.yaml` sets up three "vectors": `agents`, `tasks`, and `scorers`. a task is a single scenario given to the model under test. a scorer is a task to run on the output of the agent run which scores how well did the particular agent perform at a certain specific aspect of a task. each scorer outputs a value between 0.0 - 1.0. the primary way to refer to each of these is by their `id` which doubles as a human-readable name.

an `agent` comes in three kinds. a `harness` is an agent cli that runs the agentic loop in a sandbox -- pats ships an adapter per provider (`claude-cli`, `codex-cli`, `opencode`). an `adhoc` agent is the same idea with no preset: pats just runs a `command` you give it. an `api` agent is a raw model endpoint with no agentic loop. the key rule: only `harness` and `adhoc` agents can run tasks; an `api` agent has no loop to do work, so it's scorer-only -- bring your own harness if you want to test a model behind a key. scorers themselves are just `bash` (run a script -- trampoline to python/spacy/whatever inside it) or `agent` (ask an agent to judge).

finally, `pats.yaml` sets up the test and scorer matrices. `test-matrix` defines which `agent` x `task` combinations should run. `scorer-matrix` defines which `task` x `scorer` combinations should run. both matrices take an optional `weight` per row (omitted -> 1.0); the score is a weighted mean, so weight tunes how much each pair counts. a matrix row is a cross-product -- `agent`/`task`/`scorer` each take a scalar, a list, or `"*"` (all) -- so you rarely write one row per pair.

there are two (and a half) stages of a pats workflow. `pats run` runs your test-matrix your agents across all your tasks, and saves the result to `.pats/runs/<date_slug>-<run_number>/` -- that usually takes a hot moment. then `pats score` scores the most recent run, or you can ofc instruct it to test an older run too. note that this is not intended as a way to keep old historic runs and therefore, if you change the scorers and rerun on the old run, you *are* going to just run the new scorers. finally, if you have any agentic scorers they are not run by default, but only with `--agentic` flag.

task-running agents are always run in a `bwrap` or `container` sandbox. (api scorer agents don't run tasks, so they don't get a sandbox.)

### example

the agent under test is `claude-haiku-4-5`, run through the `claude-cli` harness. (it's a harness, not an `api` agent, precisely because only harnesses can run tasks.)

it has 3 tasks: `write-readme-simple`, `write-readme-hard`, `write-readme-monorepo`. judging by their ids, these tasks seem to be testing some behaviour related to readme authorship. they may be about testing the model itself, but also could be testing some skill or instruction given to the model.

there are 4 scorers: `only-ascii`, `title-correct`, `no-finite-verbs` and `no-colloquialisms`. former 2 scorers are implemented in a purely imperative manner. `only-ascii` deducts 0.1 form the score for each occurrence of excessive whitespace. `title-correct` is much closer to a binary yes/no question so it could simply output 1.0/0.0 respectively. latter 2 scorers are more _"fuzzy"_. `no-finite-verbs` scorer uses [`spacy`](https://pypi.org/project/spacy/) -- a python NLP library. `no-colloquialisms` is agentic, and therefore nondeterministic: it uses an agent to score the results. there is, in fact, one more agent defined for that purpose -- `claude-sonnet`, an `api` agent (scorer-only), such that we score a weaker model (`haiku`) with a stronger one (`sonnet`).
