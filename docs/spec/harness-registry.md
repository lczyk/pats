# harnesses: entries in a table, and a kind in the config

status: implemented. `machine-file.md` is where the table lives, what an entry's keys are and the rule for laying
one table over another. `harness-parse.md` is the `parse` key. `harness-images.md` is how a harness's cli gets into
an image, and its pin.

## problem

- **a harness is a string compared in seven files.** `claude-cli-keyless` and its three siblings are 49 literals:
  the command, the hosts, the cli name, the credential variables and files (`internal/agent/agent.go`), the set of
  names and which take `effort` or report cost live (`internal/config`), the live stat scanner, the summary and the
  judge's reply (`internal/eval/progress.go`, `verdict.go`), which report no cost (`dryrun.go`), which get the model
  check (`openrouter.go`). `config.Harnesses` repeats `agent.HarnessCmds` because of an import cycle, and a test
  keeps the two in step. a fifth harness is an edit to each.
- **a harness cannot come from a config.** someone with a harness of their own, a builtin run with another flag or
  provider, or another version of a builtin's cli has no way in but a patch to pats.
- **an agent has to be a model.** `model` is required, and every harness is an llm cli. a thing that does the task
  and is not one (a script, a reference solution, a tool under test) cannot be an agent, though it is what a scorer
  is calibrated against: a no-op is the floor of a score, the reference solution its ceiling.

## proposal

### one table, read from a file

everything pats knows about a harness is one entry of the `harnesses` table of the machine file
(`machine-file.md`), with its cli's entry in `clis`, and the builtins are the entries of the defaults pats
embeds. no go code names a harness: every switch and map above reads the entry.

| key | replaces |
|---|---|
| `cli` | `HarnessCLIs`: the component it runs, with its artefact, pin and stamp (`harness-images.md`) |
| `run` | `HarnessCmds` |
| `egress` | `RequiredHosts` |
| `auth-env`, `pass-env` | `credKeys`, and which of them count as a credential |
| `login`, `auth-hint`, and the cli's `login-file` | `HostCredsFile`, `AuthPathVar`, `CredentialHint` |
| the cli's `home-env` | `HomeEnv` |
| `parse` | `statScanner`, `summarize`, `finalTexts`, and with them `LiveCostHarnesses` and `noCostHarnesses` |
| `models` | the openrouter check, chosen by harness name today |

the table is loaded where both `internal/config` and `internal/eval` can import it, with no second copy.
`effortHarnesses` goes: it holds all four. the catalogues stay go, keyed by name. with no machine file this
changes no behaviour.

### `kind: harness`

a `pats.yaml` may have harnesses of its own, in a vector like any other (`vectors.md`): `defaults` and `items`,
each item with the keys of a `harnesses` entry and the pin of its cli, `version` and `hash`
(`harness-images.md`). an agent's `harness:` names a harness of the machine or an item, never a vector.

```yaml
harnesses:
  kind: harness
  items:
    claude-cli-keyless:             # a default, at another version of its cli
      version: 2.2.0
      hash:
        amd64: sha256:<64 hex>
        arm64: sha256:<64 hex>
    noop:                           # no llm at all
      run: "true"

opencode:                           # two more ways of running one cli
  kind: harness
  defaults:
    cli: opencode
    parse: |
      ev = json.decode(line, default = None)
      ...
  items:
    opencode-anthropic:
      run: >-
        opencode run --model "anthropic/$PATS_MODEL" --dangerously-skip-permissions --format json --thinking
        "$(cat "$PATS_PROMPT_FILE")"
      egress:
        allow: [.anthropic.com, models.dev]
      auth-env: [ANTHROPIC_API_KEY]
    opencode-groq:
      run: >-
        opencode run --model "groq/$PATS_MODEL" --dangerously-skip-permissions --format json --thinking
        "$(cat "$PATS_PROMPT_FILE")"
      egress:
        allow: [api.groq.com, models.dev]
      auth-env: [GROQ_API_KEY]
```

the rules:

- an item is first filled from its vector's `defaults`, as any item is. the result is then laid over the machine's
  harnesses by the one overlay rule (`machine-file.md`): **named as a harness the machine has**, it changes that
  one key by key; **with a new name**, it is a harness of its own, written whole, and needs a `run`.
- absent means keep and a null means unset, through both steps: a key an item nulls is filled neither by its
  vector's `defaults` nor by the machine's harness. the two are laid over as written, before anything is decoded,
  so a key that is absent, one that is null and `login: false` stay three things.
- there is no other inheritance. a variant under a new name is a copy: `pats config` prints every entry, and what
  several variants share goes in their vector's `defaults`.
- a copy is the project's from then on. a release that corrects the default it was copied from (a host, a line of
  `parse`) does not reach it.
- a project has no `clis` (`machine-file.md`): an item's `cli` names one the machine has, `login: true` asks for
  that cli's login file, and `version` and `hash` pin that cli for the sandboxes its agents run in. the pin needs
  a `cli`, and its two keys come together.
- `parse` is a promise about what `run` prints, and pats checks it only as it runs (`harness-parse.md`): a
  replaced `run`, and a pinned `version`, must still print what the item's `parse` reads, or bring their own.
- `models` goes with `run` in the same way: an item that points an inherited command at another provider says
  `models: ~`, and the error of a model the catalogue lacks names that key.
- two items of one name, in one vector or two, are a load error, as for any kind.

`metadata.json` says where the harness a pair ran is from and carries a digest of it (`machine-file.md`,
`harness_from`, `harness_hash`), so a default's name that means something else in one project shows in that
project's records.

what follows from an entry's keys, wherever the entry is from:

- **no `parse`.** no live tool count, no cost, and no summary: the columns show what they show for a harness with
  no scanner today. as a judge its whole stdout is its reply (`agent-scorers.md`). preflight passes it on exit 0
  within the preflight timeout.
- **`model`** is required for an agent whose harness's `run` names `PATS_MODEL`, and optional otherwise.
  `PATS_MODEL` is then empty, `metadata.json` leaves `model` out and the report shows `-`. a `run` that is a script
  file reading the variable inside is not seen, and guards itself. the dry run's cost history takes a pair with no
  model as any other: the guard that skips one today goes.
- **`effort`** reaches every `run` as `PATS_EFFORT`. **`budget`** is accepted on every agent and checked against
  what `parse` reports (`harness-parse.md`).
- **preflight** runs every harness on the preflight prompt and exports `PATS_PREFLIGHT=1`, so a harness that cannot
  answer a prompt cheaply exits 0 on it.
- **no `cli`.** the host driver looks for none, and nothing is placed: what `run` needs comes from the sandbox's
  `provision`, its `base:`, or an `image:`.
- **`egress`** is the harness's part of a pair's policy, beside the sandbox's and the task's (`egress-1x.md`), and
  works as `RequiredHosts` does now:
  - under a sandbox that allowlists, `allow` joins the sandbox's and the task's. under `default: allow` it has
    nothing to add. `deny` joins theirs under both.
  - a deny wins, whoever wrote it: a sandbox's `deny` can cut a harness off from a host it asks for, and a
    harness's can cut a task off. the audit's `rule` and `pats egress check --agent` name the entry that decided.
  - the hosts of `allow` are the policy's `harness` list: what the `methods` posture leaves uninspected. a url
    entry the sandbox or the task writes on one still inspects it, as now.
  - an agent in an `egress: none` sandbox is a load error when its harness has any `allow`, as now for every
    default, and allowed when it has none. in a sandbox with no `egress` at all the key does nothing.
  - `pats egress check` and `run` read the entry where they read `RequiredHosts`. `suggest` reads no config, as
    now: it keeps out of what it proposes the hosts in each pair's recorded policy (`harness`, in `metadata.json`),
    which are the ones that run had, not today's. a pair with no recorded policy has none kept out.

### a new default: `pi-openrouter`

pi (`github.com/earendil-works/pi`) is a small coding agent with a json event stream and no permission prompts. it
becomes the fifth harness of the defaults, and the first that is an entry and nothing else:

- a `clis` entry `pi`: `pi-linux-<arch>.tar.gz` of its github release, pinned as the others.
- `run`: print mode, `--mode json`, `--no-session`, `--approve`, `--provider openrouter`, the model, `--thinking`
  from the effort, startup fetches off (`PI_OFFLINE`), and `--` before the prompt.
- `auth-env: [OPENROUTER_API_KEY]`, `egress: {allow: [openrouter.ai]}`, `models: openrouter`.
- `parse`: a script written against a recorded run (`harness-parse.md`).

checked against release 1.5.2:

- **the files.** `https://github.com/earendil-works/pi/releases/download/v${version}/pi-linux-{x64,arm64}.tar.gz`,
  each with a digest github publishes, which is the hash `pats harness pin` prints. the executable is `pi/pi`
  (`bin: pi/pi`). it runs from where pats places it, and in a bare ubuntu image with nothing else installed.
  `pi --version` prints the bare version.
- **the key.** it reads `OPENROUTER_API_KEY`, and needs no login file.
- **the stream** is one json object a line, documented in pi's `docs/json.md`. usage and cost come on each
  assistant message, as openrouter reports them.
- **failure.** json mode exits 0 on a reply that failed: a refused key and a model openrouter does not have both
  end with `stopReason: error` and exit 0. so preflight reads the script's error too (`harness-parse.md`).
- **effort** goes to `--thinking` as it is: `off`, `minimal`, `low`, `medium`, `high`, `xhigh`, `max`. a level pi
  does not know is a warning on stderr and its default, `medium`. no effort is `medium` too.
- **models.** an id outside pi's own catalogue is passed on with a warning, so pats's openrouter catalogue is the
  check, as for opencode.
- **trust.** pi loads a folder's own extensions and settings only from one it trusts, and asks. `--approve` trusts
  the workspace for the run: pats is the outer sandbox, as for the other harnesses' permission flags. context
  files (`AGENTS.md`) load either way.
- **hosts.** a run under `egress: {default: allow}` reaches `openrouter.ai` and nothing else, with `PI_OFFLINE`
  and without it, and `pats egress suggest` has nothing to add. the variable stays: it turns off what
  pi fetches of its own accord, its model catalogue among it, which an allowlist would deny.

the same entry in a machine's file is pi on a pats that does not ship it yet.

omp and hermes are not defaults. each is a harness item in `pats.example.yaml`, installed by the sandbox's
`provision`: a default is a pin to bump at every release and a script to keep in step with its output, and both
move fast. they are also the first users of the path a project's harness takes.

### names

the `<cli>-<auth>` shape stays (`opencode-openrouter`, `pi-openrouter`). a cli that speaks to many providers gets
one default, for the provider pats tests, and any other provider is a project's item. defaults do not multiply by
provider.

## later

- **`clis` in a project** (`machine-file.md`, later): until then a project's harness on a cli no machine has gets
  its files from `provision`.

## not doing

- **`like`, or any way for one harness to start from another by name.** pats has no inheritance but a vector's
  `defaults`. it would need its own rules (how deep, what may change under it), and what it saves is a paste.
- **keeping a credential out of the sandbox.** the agent's shell can read the key its harness holds, under every
  harness. a proxy that puts the real key in on the way out would close that, and pats has the proxy for it. left
  out: for an eval the key is the user's own and short-lived in a sandbox that is thrown away.

## tests

- the defaults: `machine-file.md`. with no machine file every existing harness test passes unchanged.
- load: a new-named item with no `run`, in its own fields or its vector's `defaults`; `harness:` naming a vector;
  two items of one name; a harness with no `allow` under `egress: none`, and one with an `allow` refused there; a
  path, a method and a regexp in a harness's `egress`; `version`
  without a `cli`, and without `hash`; `login` on a cli with no `login-file`; a `clis` key in a `pats.yaml`;
  `model` missing where `run` names `PATS_MODEL`, allowed where it does not.
- the overlay: an item named as a default changes one key and keeps the rest; a null unsets a key the machine's
  harness has, and one the vector's `defaults` have; `login: false` over a default's `true`; `defaults` fill an
  item before it is laid over; a model outside an inherited catalogue names `models` in its
  error.
- a harness with no `parse` and no cli end to end, on the fake engine: `run: "true"` passes preflight and a pair,
  with no summary and no model in `metadata.json`; its `allow` reaches the policy and its `harness` list, and its
  `deny` the policy's; an `auth-env` and a `pass-env` variable reach the sandbox and an unset one does not; as a
  judge its stdout is the reply.
- `pats list` shows a project's harnesses beside the machine's, each with where it is from.
