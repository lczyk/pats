# proposal: a pats.yaml of vectors

status: implemented on the 1.x line. run fields (`prompt-file`, `prepare-cmd`, ...), task `dir` vectors and the scorer
`agent` rename came after it -- see `task-dirs.md`. since then the step `-cmd` keys (`prepare-cmd`, ...) are gone and
`pats.yaml` is not templated (`${task}` and the rest are refused), so the examples here do not load as written. see
`docs/spec/steps.md`.
scope: `pats.yaml` shape (1.x, no backwards compatibility), `internal/config`,
the `internal/eval` sites that index the config.

## problem

the 0.x config held one flat `agents:`, one `tasks:`, one `scorers:`, and
suites picked from them by id. every item carried its full definition and
every suite re-listed every id. in a real config (`canonical/mason`,
`tests/skills/pats.yaml`, ~210 lines) that was:

- 12 tasks, 10 of them identical apart from the id, each repeating
  `prompt: prompts/${id}.txt`, `prepare: tasks/prepare_${id}.sh`,
  `collect: tasks/collect.sh`.
- 32 scorers, every one `score: scorers/scorer_${id}.py`.
- the `slicer` suite re-listing 10 task ids and 28 scorer ids that already
  sat next to each other, and `agents: *all-agents` on every suite.

the grouping existed in the author's head and in the comments (slicer tasks,
help tasks, sdf scorers, transcript scorers). the config could not express it.

## shape

the file is a mapping of vectors. the key is the vector's id. a vector has a
`kind`, optional `defaults`, and `items` keyed by id. an item is a mapping of
fields, or empty to inherit everything.

```yaml
sandbox:
  kind: sandbox
  items:
    docker-mason-test:
      build: .
      egress: {mode: mitm-proxy, default: deny, allow: [...], deny-urls: [...]}

all:
  kind: agent
  defaults: {harness: claude-cli-keyless, effort: low}
  items:
    haiku: {model: claude-haiku-4-5-20251001}
    sonnet: {model: claude-sonnet-5}
    codex-mini: {harness: codex-cli-keyless, model: gpt-5.6-luna}

slicer:
  kind: task
  defaults: &slicer-defaults
    prompt-file: prompts/${task}.txt
    prepare-cmd: tasks/prepare_${task}.sh
    collect-cmd: tasks/collect.sh
  items:
    base-passwd:
    ca-certificates:
    curl:
    # ... 7 more

misc:
  kind: task
  defaults: {<<: *slicer-defaults, timeout: 5m, collect-cmd: ~}
  items:
    mason-help:
    frobnicator: {collect-cmd: tasks/collect.sh}

sdf:
  kind: scorer
  defaults: &py-scorer {score-cmd: "scorers/scorer_${scorer}.py"}
  items:
    target-present:
    yaml-parses:
    # ... 26 more

transcript:
  kind: scorer
  defaults: *py-scorer
  items:
    usage-printed:
    mason-stops-clean:
    refuses-nonexistent:
    refusal-verified:

suites:
  kind: suite
  defaults: {agents: all}
  items:
    slicer: {tasks: slicer, scorers: sdf}
    slicer-refusal: {tasks: frobnicator, scorers: [refuses-nonexistent, refusal-verified, orientation-called]}
    mason: {tasks: mason-help, scorers: [usage-printed, mason-stops-clean]}
```

mason drops to ~95 lines before comments, 30 of them the egress lists.

### vector

- `kind` -- required, singular: `sandbox`, `agent`, `task`, `scorer`, `suite`, `harness`.
- `defaults` -- optional. a mapping of that kind's item fields. every item
  inherits each field it does not set.
- `items` -- required, non-empty. a mapping from item id to a mapping of
  fields, or to null for "inherit everything". in block style a bare
  `curl:` is null; in a flow mapping it must be `curl: ~` (yaml.v3 reads
  `{curl:, x:}` as a key named `curl:`).

those are the only vector keys. a vector of kind `suite` is a group of suites
and takes `defaults` like any other. there is no flat single-item form: a
singleton is a vector with one item, and its `defaults` block is where the
second item's shared fields go when it arrives.

### one meaning of `kind`

`kind` is the vector's and nothing else's. the item fields that used to be
called `kind` are gone or renamed, so a vector's `kind` and an item's fields
never share a word:

- agent `kind` -> `harness` (`claude-cli-keyless`, ...). the readme already
  called them harnesses and `agent.HarnessCmds` was keyed by it. the env var
  follows: `PATS_AGENT_HARNESS`.
- sandbox `kind` -> gone. `driver` determines it: `docker` (the default) and
  `podman` are container drivers, `bwrap` is bwrap.
- scorer `kind` -> gone. `score` / `score-cmd` makes a script scorer, `agent:` + a prompt
  an agent scorer, both or neither is an error.

### names

an id names a set of items. a vector id names its items. an item id names the
set of one. there is one namespace per kind, and within it every id must name
exactly one set.

- a task `curl` and a scorer `curl` never meet: every lookup in pats knows
  which kind it wants (suite axis by field, cli filter by flag, env var by
  name). legal.
- a task vector `smoke` of `{curl, base-passwd}` and a task item `smoke` in
  another vector: one name, two sets. error.
- a task vector `mason-help` whose only item is `mason-help`: one name, one
  set. legal, no special case. singletons cost nothing.
- two task items `curl` in two vectors: two objects, not one set twice. error.
  item ids are unique per kind, before the set rule applies.
- vector ids are top-level mapping keys, so yaml.v3 rejects a duplicate on
  parse.
- ids may not contain `/`, `$` or whitespace. they end up in run dirs
  (`<run>/<agent>/<task>`), env vars and templates.

### references

a suite axis (`agents`, `tasks`, `scorers`) is a scalar or list of ids of that
kind. each id expands to its set, in listed order, vector items in vector
order. a vector id and an item id mix freely:
`scorers: [sdf, orientation-called]`. an item landing twice within one axis
is an error (a typo guard, loud over silent).

`-a`, `-t`, `-s` on the cli take the same ids with the same expansion.

references that mean one thing -- agent `sandbox: x`, scorer `agent: x` --
must name a set of one (`Config.One`). a vector of two there is an error. the
single-sandbox default stays "exactly one sandbox item in the file".

### merge

- field granularity, whole value. an item's `prepare-cmd` replaces the default's.
  a list (`egress.allow`) replaces the default's list, it does not append.
- absent means inherit. an explicit null (`collect-cmd: ~`) means unset, for an
  item that must not run the vector's collect, or for a `defaults` block that
  drops a field it took from an anchor (`misc` above).
- a whole item that is null inherits everything. a null field inside an item
  unsets that field. different nodes, no clash.
- required-field checks (a prompt, `harness`, `score` / `score-cmd` / `agent`, suite
  `agents` / `tasks`) run on the merged item, so a vector can supply them.

### sharing defaults between vectors

yaml anchors and merge keys, by design. `defaults: &x {...}` in one vector,
`defaults: *x` or `defaults: {<<: *x, timeout: 5m}` in another. yaml.v3
resolves `<<` and aliases when a node is decoded, so the null-unset rule and
the unknown-field check both see the merged result. no pats syntax, no cycle
checks, no abstract vectors. a feature that shares *items* between vectors
would be a different thing with a different name.

### interpolation

`${id}` is gone. in a vector-level default "id" would be ambiguous. instead,
resolved at load after the merge, so a default is a template expanded per
item:

- `${task}`, `${agent}`, `${scorer}`, `${sandbox}` -- the item's own id, in
  that kind's fields only.
- `${vector}` -- the enclosing vector's id. always the vector the item sits
  in, whatever anchor its defaults came from.
- any other `${x}` is a load error, so a stale `${id}` cannot hide.
- suite fields are references, not templates. no interpolation there.
- inside a flow mapping `{...}` a template must be quoted, or the `}` closes
  the mapping.

## validation

all at load, all collected into one error. `Parse` reports structure, with
the line it came from:

- vector: missing or unknown `kind`, `items` missing / empty / not a mapping,
  unknown key on the vector itself.
- `defaults` and items: unknown field for the kind, item value that is
  neither a mapping nor null.
- templates: unknown `${x}`, kind variable in the wrong kind.

`Validate` reports meaning:

- names: duplicate item id per kind, one id naming two sets, illegal
  characters.
- per kind: the field checks (image xor build, known driver and harness,
  model, prompt, timeout parses, scorer flavour).
- references: unknown id, id of the wrong kind, set-of-one where one is
  required, duplicate within an axis.
- orphans: every item must land in some suite, via its vector or by name. an
  agent referenced only by a scorer's `agent` is exempt.

## go side

`Config` is the resolved config: flat `[]Sandbox` / `[]Agent` / `[]Task` /
`[]Scorer` / `[]Suite` in file order, each item carrying `Name` and `Vector`,
every field already merged and interpolated. eval indexes those slices and
nothing else; the lazy `Resolved*` helpers are gone.

`Parse` walks the document as `yaml.Node` (go maps lose order; run and list
order follow the file). per vector it reads `kind`, decodes `defaults` and
each item into `map[string]yaml.Node` -- which resolves aliases and merge
keys, keeps each value's tag (null detection) and line (error messages), and
exposes unknown keys -- overlays item on defaults at the key level, rebuilds
a mapping node from the surviving values and decodes that into the kind's
struct. the known-field set is the struct's yaml tags. interpolation walks
the struct's string fields by reflection.

two yaml.v3 facts shaped that: a sub-node cannot be re-marshalled for a strict
`KnownFields` decode (aliases lose their anchor), and a custom `UnmarshalYAML`
is never called for a null node, so a pointer or option field type cannot tell
`collect: ~` from absent. the node map does both.

name sets (`Config.sets`) are built from the resolved items: an item id maps
to itself, a vector id to its items. suite expansion, cli filters,
`ResolveSandbox` and the orphan check all go through them.

tests build configs from yaml strings through `Parse`, so every fixture
exercises the merge.

## not in scope

- suite-level overrides (a suite re-timing a task). a suite stays a
  selection, vectors own definitions.
- a suite level in the run dir, or per-suite namespaces.
- sharing items between vectors.
