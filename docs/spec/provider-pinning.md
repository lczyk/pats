# provider pinning

status: proposed.

## problem

an openrouter model id is served by several providers, and openrouter picks one per request by price, latency and
availability. the same id can come from providers with different quantisation, context limits, caching and price.
so on an `opencode-openrouter` agent, consecutive pairs (and consecutive requests within one pair) can be answered by
different deployments of "the same" model. that is noise on the model axis of every comparison, and it makes cost
unpredictable.

## proposal

- **an agent field `provider:`**, for `opencode-openrouter` only, with openrouter's provider routing keys in pats's
  kebab-case:

  ```yaml
  agents:
    kind: agent
    items:
      or-sonnet:
        harness: opencode-openrouter
        model: anthropic/claude-sonnet-4.5
        provider:
          order:
            - anthropic
            - amazon-bedrock
          allow-fallbacks: false
  ```

  - `order`: provider slugs, tried in this order.
  - `only`: provider slugs allowed at all.
  - `allow-fallbacks`: whether openrouter may go beyond the named providers when they fail. omitted, it is `false`.
  - `quantizations`: allowed quantisations (`fp8`, `bf16`, ...).

  pats sends the block as openrouter's `provider` request object (`order`, `only`, `allow_fallbacks`,
  `quantizations`), unchanged in meaning, with `allow_fallbacks` always written out.
- **validation.**
  - `provider:` on any other harness is a load error, and so are unknown keys (strict decode, as elsewhere).
  - an empty block, or empty lists, are errors: they pin nothing and read as if they did.
  - the block must name providers: a non-empty `order` or `only`. `quantizations` alone narrows the deployments but
    pins no provider, and is an error with that message; it can sit beside `order` or `only`.
  - `allow-fallbacks: true` is allowed with `order` (prefer these, fall back if needed), and the readme says that is
    a preference, not a pin.
- **the guarantee**, with `allow-fallbacks` false: every request of the pair is answered by a named provider or
  fails. with it true, only preferred.
- **the model check** (`model-preflight.md`) checks each named provider serves the model, and that some tool-capable
  endpoint survives the block, before any sandbox starts.
- **metadata** records the agent's `provider` block, and the dry run's cost estimate matches on it
  (`run-dry-run.md`), so history from another routing is not used for an estimate.

## how the pin is enforced

not through opencode's config. opencode merges a global config, `OPENCODE_CONFIG`, the project's `opencode.json` and
`.opencode/`, an inline `OPENCODE_CONFIG_CONTENT`, `OPENCODE_CONFIG_DIR`, a managed `/etc/opencode/` config, and then
agent options and the selected variant on top of the model's options, key by key. a task's `stage`, its `prepare`,
the sandbox image or the agent's `effort` can each put routing keys in one of those, and a pin that any of them can
override or extend is not a pin.

so the egress proxy sets it on the wire:

- **a pinned agent needs the egress proxy.** an agent with `provider:` whose sandbox has no egress policy (open
  network) or `egress: none` is a load error naming the agent and the sandbox.
- **the proxy rewrites.** for a pinned agent, the pair's proxy is given a rewrite for `openrouter.ai`: it mitm's the
  host (as a url entry would), and every `POST` to it is inference-shaped or refused. an inference request is any
  `POST` whose body is a json object with a `model` key, whatever the path (`/api/v1/chat/completions`,
  `/api/v1/responses`, `/api/v1/completions`, or one openrouter adds later): the proxy sets its top-level `provider`
  to pats's block, replacing whatever the client sent. a `POST` that is not inference-shaped is refused with a 403
  under a pin, audited `reason: pin-unsupported`, since pats cannot tell it does not carry routing. so a task config
  that switches opencode's sdk package or endpoint on the same host is still pinned or refused, never unpinned. the rest of the body is
  left as it was. the request then goes upstream with its `Content-Length` recomputed. responses, streamed or not,
  are not touched.
- **how the proxy gets it.** `proxy.Policy` gains `Rewrites`, beside `Harness`: set by pats from the agent, never
  read from pats.yaml. it is set in the one policy builder every path shares: a pair's `egressFor`, and
  `EffectivePolicy`, which `pats egress check --agent` and `pats egress run --sandbox --agent` use, so the checker
  and the standalone proxy see the same rewrite and posture as a real pair. a sandbox's `egress:` naming it is an unknown-key error, as now. it travels with the rest of the
  policy, so the docker and podman sidecar (`PATS_EGRESS_POLICY`) and the lxd proxy get it the same way. the
  policy is built per pair (`egressFor` in `newHarness`), so two agents sharing a sandbox with different pins each
  get their own. `NeedsCA` is true when there are rewrites, and the rewrite's host is mitm'd whatever the allow
  entries say.
- **the methods posture.** under `methods: [GET, HEAD]` the harness hosts are exempt from posture-triggered
  inspection today, so an inference `POST` passes, while a url entry naming a harness host already makes the posture
  apply to it. the rewrite keeps exactly that: when it is what brings `openrouter.ai` under inspection, the
  rewritten request is decided as uninspected harness traffic is (allowed under the posture); when a url entry
  already inspects `openrouter.ai`, the posture applies as it does today. an explicit `deny` wins either way.
- **the guarantee covers `openrouter.ai`.** a config that points opencode's openrouter provider at another `baseURL`
  sends inference elsewhere, unrewritten. under a default-deny policy that host is refused unless the sandbox allows
  it; under `default: allow`, or with a gateway allowed, it goes through. the readme says the pin holds for requests
  to openrouter.ai, and a pinned pair whose audit shows inference traffic to openrouter.ai with no rewritten request
  is flagged in its metadata (`provider_pin: "not seen"`).
- **audit timing.** the proxy writes a `rewrite` audit event when it rewrites, before forwarding upstream, since a
  request's own `req` line comes only once its response ends and a cancelled stream may never write one. pats folds
  the audit into `metadata.json` only after the session is closed and the proxy's log drained, which a fix ahead of this
  spec makes true for every pair. whether the audit is complete is
  a fact pats records rather than assumes: a session's `Close` returns an error when its log follower had to be
  killed after the grace, or exited with an error of its own (a dropped `docker logs` connection leaves valid but
  truncated lines), docker and lxd alike, or the audit file could not be written. `readEgressAudit` reports a file it
  could not read (other than one never made, under no egress) and lines it cannot parse. any of those, or an
  interrupted pair, sets `egress.audit_complete: false` in
  `metadata.json`, and then `provider_pin` is `"unknown"`, never `"not seen"`.
- **audit.** a rewritten request's audit line carries `rewrite: provider`, and `metadata.json`'s egress record counts
  them, so a pair shows its requests went out pinned.
- **one path for preflight and pairs.** the credential preflight runs through the same proxy, so its request is
  pinned as the pairs' are.

to verify on the pinned opencode (`version` of `opencode` in `internal/machine/defaults.yaml`) before building: that its
openrouter requests are json `POST`s with a `model` key on `openrouter.ai`, and that it sends no other `POST`s there
that the refusal would break. the proxy's own tests cover the rewrite with a recording
upstream.

## what it does not fix

- the provider an answer came from is not in opencode's json events. with fallbacks off, a request is answered by a
  named provider or fails, so a pair either ran on a pinned provider or errored.
- a provider can change its deployment over time. that is outside what a request can pin.

## tests

- config: `provider:` decodes, `allow-fallbacks` defaults to false; on a claude agent, empty, empty lists,
  quantisations alone and unknown keys are errors.
- a pinned agent on a sandbox with no egress, or `egress: none`, is a load error; `rewrites` in a sandbox's `egress:`
  is an unknown key.
- two agents sharing a sandbox with different pins each get their own block upstream.
- `pats egress check --agent` and `pats egress run --agent` for a pinned agent show and apply the same rewrite as a
  pair's policy.
- a follower killed after the grace, or an unparseable audit line, sets `audit_complete: false` and
  `provider_pin: "unknown"`.
- under `methods: [GET, HEAD]` the rewritten inference post passes; with a url entry already inspecting
  `openrouter.ai` the posture applies as today; an explicit deny of it still refuses.
- a follower that exits with an error, and an audit file that cannot be read, set `audit_complete: false`.
- proxy: a chat-completions body with a different `provider` comes out upstream with pats's block and every other key
  unchanged; a body with none gets it; a `/api/v1/responses` body is rewritten the same; a pair mixing the two
  endpoints has every request rewritten; a json `POST` with no `model` is refused 403 `pin-unsupported`; a non-json body and an oversized one are refused 400 and audited; a `GET
  /api/v1/models` passes untouched; a streamed response passes through.
- the audit marks rewritten requests, and the pair's egress record counts them.
- a rewrite whose upstream stream is cancelled still leaves its `rewrite` event; an interrupted pair records
  `provider_pin: "unknown"`.
- metadata carries the block.
