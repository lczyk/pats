# model preflight

status: implemented, but for the provider pins check, which waits on `provider-pinning.md`.

## problem

an `opencode-openrouter` agent names an openrouter model id (`model: openai/gpt-4o-mini`; the harness adds the
`openrouter/` prefix). a mistake in it surfaces late and expensively:

- a typo'd or retired id fails the credential preflight (`preflight.go`), which first starts a sandbox for the agent
  (minutes on a cold lxd server) to find out.
- a model without tool support passes that preflight, since the trivial prompt needs no tools, and then every pair
  stages, starts a sandbox, and gets an agent that can only answer in prose.

## proposal

- **a model check, before any sandbox and before the run dir.** for the distinct `opencode-openrouter` agents in the
  selection, `pats run` fetches the public catalogue `GET https://openrouter.ai/api/v1/models` once, on the host,
  and checks each agent's model:
  - **it exists.** the id matches a catalogue `data[].id` exactly. catalogue variants (`:free`, `:extended`,
    `:thinking`, ...) are catalogue ids and match only themselves. the routing shortcuts `:nitro`, `:floor`,
    `:online` and `:exacto` are not in the catalogue, and openrouter takes suffixes in any order. so the id is split
    on `:` after its base, every routing shortcut is dropped wherever it sits, and what is left, in its order, is
    matched (`x/y:nitro:free` is checked as `x/y:free`). the agent still runs with the id as written. an id whose
    resolved form is not in the catalogue fails as missing.
  - **it takes tools.** the catalogue entry's `supported_parameters` contains `tools`. opencode drives the agent
    through tool calls.
- **failing is a load error.** a missing id or a model without tools stops the run, naming the agent, the model and
  the reason. a missing id also lists up to three of the closest catalogue ids by edit distance (`agent "or-mini":
  model "openai/gpt-4o-mni" is not on openrouter; closest: openai/gpt-4o-mini`).
- **where it runs.** in `Run`, right after `usePins` and before `nextRunDir`, the upstream checks and `Preflight`, so
  a bad id makes no run dir and starts nothing. once per invocation: under `--repeat` (`repeats.md`) it runs before
  the first repeat only. `pats run --dry-run` (`run-dry-run.md`) runs it too.
- **the network.** plain unauthenticated requests from the host, outside any sandbox and its egress policy. the
  catalogue is public, and the check answers "does this model exist and take tools", not "may this key use it": the
  credential preflight still answers the latter. the whole check, catalogue and endpoint lists together, is bounded
  by one 10s timeout.
- **failure to check is a warning.** the check's own timeout, a non-200, a body that does not parse or lacks `data`:
  warn `model check skipped: <why>` and go on. but a cancelled parent context (ctrl-c) is not swallowed: the run
  stops as interrupted, with no run dir. `--offline` skips the check.
- **provider pins.** an agent that pins providers (`provider-pinning.md`) is also checked against its model's
  endpoint list, `GET /api/v1/models/{author}/{slug}/endpoints`, fetched once per distinct model, concurrently,
  within the same 10s:
  - **matching errs on passing.** openrouter's slug rules have tiers and aliases (`openai/priority` can be served by
    `openai/fast`) that pats does not copy. so the check matches by provider only: the part of a slug before any `/`
    against the part of an endpoint's `tag` before any `/`. an endpoint-specific slug (`deepinfra/fp8`) is checked
    as its provider (`deepinfra`). the check can pass a pin openrouter then refuses, never refuse one it accepts;
    the credential preflight, which runs pinned, catches the rest.
  - each slug in `order` and `only` must match some endpoint's provider, else a load error naming the provider.
  - at least one endpoint must survive the block, matched as above, and take tools, else a load error: every
    request would fail. an endpoint takes tools when its `supported_parameters` lists `tools` or any of its
    `supports_tool_choice` flags is true (azure's endpoints advertise the latter only). `only` and `quantizations` always restrict; `order` restricts only with
    `allow-fallbacks: false` (with fallbacks on it is a preference, and any endpoint `only` allows may serve).
  - quantisations match as openrouter matches them: an endpoint's `quantization` equal to a listed value, and the
    families openrouter folds together (`fp8` takes `mxfp8`; `fp4` takes `mxfp4` and `nvfp4`). a endpoint with no
    `quantization` matches only when the block lists none.
  - an endpoint list that cannot be fetched or parsed is a warning for that agent, never a missing provider.

## not doing

- **no check for other harnesses.** claude, codex and copilot take their own model names, with no public list to
  read. their credential preflight stays the check.
- **no caching across runs.** one request per run is cheap, and a cached list would hide a model openrouter just
  retired.
- **no per-key availability.** `/models/user` filters by the key's settings, but the credential preflight already
  fails on a model the key cannot use.
- **no context-length or price checks.** both are facts about a model, not mistakes in a config.

## seams

- the base url comes from `PATS_OPENROUTER_URL` (default `https://openrouter.ai/api/v1`), so tests point it at an
  `httptest` server.
- `checkOpenrouterModels(ctx, agents, client) (warnings []string, err error)`, called from `Run` and the dry run.

## tests

- an exact id passes; a typo fails with the closest ids; a model without `tools` fails; `:free` matches only itself;
  `:nitro` checks the base id; `x/y:nitro:free` and `x/y:free:floor:nitro` check `x/y:free`; an unknown suffix not in
  the catalogue fails.
- a 500, a timeout, `{}` and a non-json body each warn and pass, for the catalogue and for an endpoint list.
- a cancelled context errors, and no run dir is made.
- `--offline` makes no request.
- pins: `deepinfra/fp8` passes when any `deepinfra/...` endpoint exists, and `openai/priority` when any `openai`
  endpoint does; a provider with no endpoint fails; quantisations that
  exclude every tool-capable endpoint fail, with fallbacks on or off; `quantizations: [fp8]` keeps a tool-capable
  `mxfp8` endpoint; an endpoint with no `tools` parameter but `supports_tool_choice.auto: true` takes tools.
