# pats

score-based testing framework for testing and scoring agent tasks.

to use pats, put `pats.yaml` in your project root. see [`pats.example.yaml`](./pats.example.yaml) for an example of such a configuration. 

`pats.yaml` sets up three "vectors": `agents`, `tasks`, and `scorers`. an `agent` definition is the model and it's provider -- all pats needs to run it. a task is a single scenario given to the model under test. a scorer is a task to run on the output of the agent run which scores how well did the particular agent perform at a certain specific aspect of a task. each scorer outputs a value between 0.0 - 1.0. the primary way to refer to each of these is by their `id` which doubles as a human-readable name.

finally, `pats.yaml` sets up the test and score matrices. `test-matrix` defines which `agent` x `task` combinations should run. `score-matrix` defines which `task` x `scorer` combinations should run and, optionally, how important each one is. 

there are two (and a half) stages of a pats workflow. `pats run` runs your test-matrix your agents across all your tasks, and saves the result to `.pats/runs/<date_slug>-<run_number>/` -- that usually takes a hot moment. then `pats score` scores the most recent run, or you can ofc instruct it to test an older run too. note that this is not intended as a way to keep old historic runs and therefore, if you change the scorers and rerun on the old run, you *are* going to just run the new scorers. finally, if you have any agentic scorers they are not run by default, but only with `--agentic` flag.

### example

an agent is `claude-haiku-4-5` provided by openrouters through an api key.

it has 3 tasks: `write-readme-simple`, `write-readme-hard`, `write-readme-monorepo`. judging by their ids, these tasks seem to be testing some behaviour related to readme authorship. they may be about testing the model itself, but also could be testing some skill or instruction given to the model.

there are 4 scorers: `only-ascii`, `title-correct`, `no-finite-verbs` and `no-colloquialisms`. former 2 scorers are implemented in a purely imperative manner. `only-ascii` deducts 0.1 form the score for each occurrence of excessive whitespace. `title-correct` is much closer to a binary yes/no question so it could simply output 1.0/0.0 respectively. latter 2 scorers are more _"fuzzy"_. `no-finite-verbs` scorer uses [`spacy`](https://pypi.org/project/spacy/) -- a python NLP library. `no-colloquialisms` is agentic, and therefore nondeterministic: it uses an agent to score the results. there is, in fact, one more agent defined for that purpose -- `claude-sonnet`, such that we score a weaker model (`haiku`) with a stronger one (`sonnet`).
