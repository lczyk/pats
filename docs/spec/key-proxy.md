# key proxy

status: implemented. it reverses "keeping a credential out of the sandbox" under "not doing" in
`harness-registry.md`, and "left out on purpose" in `docs/proposals/harnesses-1x.md`.

## problem

the agent's shell can read the key its harness holds. that is fine while the person who holds the key also wrote what
the agent reads. it stops being fine in ci on pull requests from forks: the fork controls `SKILL.md` and the files the
agent works on, so a hostile skill only has to tell the agent to `echo $OPENROUTER_API_KEY`, and the reply lands in the
transcript, the report and its artifact. the egress allowlist does not help, since nothing has to leave through the
proxy.

## threat model

- **hostile**: everything inside the agent's sandbox. the agent, its harness cli, the skills and files it reads, any
  process it starts, and whatever it sends through the proxy.
- **trusted**: pats and its config (`pats.yaml`, task files, the machine file), the host steps and the scripts they
  run (`stage`, `prompt-cmd`, `collect`, a host `score`), the proxy image, the user pats runs as and the engines it
  drives. a host step runs as that user, beside pats, the proxy and the engine, so pats cannot keep a key from it; a ci
  that runs a fork's code runs these from the base branch, or puts the steps in step sandboxes.
- **trusted to behave**: the provider (`openrouter.ai`), and a chained upstream proxy (`chained-egress.md`) if one
  terminates tls itself. both see the real key by design.

what this buys: code in the sandbox can use the key only through the proxy, only on the bound endpoints, only while
the pair runs. it cannot read it, copy it out, or spend it elsewhere.

what it does not: code in the sandbox can still spend through the bound endpoints (any process there can, not only the
harness: the placeholder works for whoever holds it). budgets, and a capped key on the provider's side, are for that.

## proposal

a sandbox can make its egress proxy hold the keys. the agent gets a placeholder in place of each key its harness
reads, and the proxy puts the real key in on the way out, in one header, on the endpoints the harness names. anywhere
else the proxy can see, the placeholder is refused and audited. for now the harnesses are `opencode-openrouter` and
`pi-openrouter`, whose one key is `OPENROUTER_API_KEY`.

### the setting

```yaml
boxes:
  kind: sandbox
  items:
    ci:
      egress:
        key-proxy: true
        allow: [github.com]
```

- **`key-proxy: true`** on a sandbox's `egress:` mapping: the proxy also holds the keys. it is pats's own key, beside
  `image`, not part of the proxy's policy. `false`, the default, is today's behaviour.
- it applies to every agent that runs in the sandbox: task pairs, agent scorers (judges) and the credential preflight.
  other sandboxes in the same config keep their own setting: a config mixing the two gives the real key to the pairs
  of its other sandboxes, and a judge in a key-proxy sandbox grading such a pair reads whatever that pair left.
- **load errors**, each naming the sandbox and, where there is one, the agent:
  - `key-proxy: true` with `egress` absent or `egress: none`: there is no proxy to hold the key. never a
    silent fallback to real keys.
  - on the `host` driver: the agent runs as the user pats runs as and can bypass the proxy.
  - an agent in the sandbox whose harness has an `auth-env` name with no binding (below), e.g. `claude-cli-keyless`:
    that key would go in real. a harness with no `auth-env` (`codex-cli-keyless`, which logs in by file) is not an
    error: it holds no env key, and the readme says a login file is not covered.
  - a harness of an agent in the sandbox that lists a bound key's name in `pass-env`: it would pass the real value.
  - a step sandbox's `step-env`, on a task paired with an agent in the sandbox or on a scorer grading such a pair,
    naming a bound key: it would hand the real value to the step.
- `key-proxy: false` with bindings in the harness table is no error: bindings only matter under `key-proxy`.

### bindings

a harness entry says where each of its keys may be spent, with `auth-bind`. the built-in table is the only place that
sets it: a machine file or a `pats.yaml` harness item that sets `auth-bind` is a load error. no gateways, no other
providers.

```yaml
opencode-openrouter:
  auth-env: [OPENROUTER_API_KEY]
  auth-bind:
    OPENROUTER_API_KEY: [POST openrouter.ai/api/v1/chat/completions]
pi-openrouter:
  auth-env: [OPENROUTER_API_KEY]
  auth-bind:
    OPENROUTER_API_KEY:
      - POST openrouter.ai/api/v1/chat/completions
      - {match: POST openrouter.ai/api/v1/messages, header: X-Api-Key}
```

- a map from an `auth-env` name to url entries in `docs/egress.md`'s syntax, each with a method prefix and a path, on
  a host named in full. a binding is on https to port 443 only, with the request's own `Host` naming that port, and
  on a path spelled without percent escapes.
- an entry says which header takes the key there. a line is `Authorization: Bearer <key>`. a mapping of `match` and
  `header` is the key as that header's whole value, for an endpoint that takes it bare. the header is one of a closed
  set, for now `X-Api-Key` alone, and not `Authorization`, which a line already means: a header the proxy or go's
  transport sets, strips or rewrites itself (`Host`, `Accept-Encoding`, the hop-by-hop ones) would carry no key, and
  a closed set is what lets a bound endpoint look at every header a key could ride in. the policy and a pair's
  recorded metadata carry an entry the same way, a line or that mapping. a sidecar from before this refuses the
  mapping, so a pair fails at its proxy's start, with the hint that names `egress.image`.
- a key may be bound to one endpoint more than once, in a header each. every entry the request is on counts, so the
  order they are written in decides nothing.
- the defaults are what the pinned clis were seen to send the key with (recorded below). `pi-openrouter` binds two
  entries, since pi takes openrouter's anthropic route for the `anthropic/*` models of its catalogue. a harness
  upgrade re-records it.
- a binding matches as an `allow` url entry does (`admitsURL`): the host normalised, the path matched case-sensitively,
  every reading of an ambiguous path (dot segments, `%2e`, `%2f`, a backslash, doubled slashes, control bytes) held to
  the entry. a path that reads more than one way is on no binding, so a placeholder sent on it is a misuse (below)
  rather than substituted. the path pats decides on is the path it forwards.
- pats merges each binding's entries into the pair's `allow` list, as it merges the harness's hosts. so a bound host is
  mitm'd (a url entry names it), a bound method passes a `methods: [GET, HEAD]` posture (an allow entry's own method
  prefix overrides the posture, `rule.go`), and an explicit `deny` still wins (deny entries are decided first).

### the placeholder

- one per bound key and per pair, drawn fresh for each pair, judge sample and preflight, so a misuse names its pair.
  a pair's proxy knows only its own pair's placeholders.
- the shape of the real value: the real value up to and including its last `-` (none if it has no `-`), then random
  characters from the alphabet of the rest (hex when the rest is all hex, else letters and digits), to the real
  value's length. a scrubbed response keeps its length; neither pinned cli checks a key's shape (recorded below), and
  the prefix costs nothing for one that might. pats does not check the key with the provider.
- the real value must be printable ascii with no space, at most 512 bytes, and its random part at least 16
  characters; anything else is an error at the pair's start naming the variable. an unset or empty key is left out,
  as today: no placeholder, no binding for it.
- the agent's env gets the placeholder where it got the real value (`agent.CredEnv`).

### how the proxy gets the real value

the proxy's policy gains the bindings: for each bound key, its placeholder and its url entries. it is filled by pats,
never read from `pats.yaml` (as `harness` is): an egress mapping naming it is an unknown key. the real values go
beside the policy, in `PATS_EGRESS_KEYS` (a json map from placeholder to real value), by the channel each
driver already uses for the run CA's private key:

- **docker, podman** the sidecar's environment, named with `-e NAME` on the cli's argv and valued in the cli's own
  environment (`envFlags`). the value is then in the sidecar's container config, so `docker inspect` on the daemon
  shows it, as it shows the CA key and a chained upstream's password today. the sandbox has no engine socket.
- **lxd** the proxy instance's `exec` reads its env from stdin (`envScript`), never argv, as now.
- **bwrap** the proxy is a child process on the host, its env set by pats. the box has its own pid namespace and
  `/proc`, so the agent cannot see it.

the proxy refuses to start when a binding's placeholder has no value in `PATS_EGRESS_KEYS`, when that variable
does not parse, or when a value breaks the rules above. a proxy that predates the feature refuses the policy itself
(it decodes with known fields only). both fail the pair at its start, never forward a placeholder unbound. the new
policy field is `omitempty`, so a policy without bindings is unchanged on the wire.

the docker and podman sidecar is a published image (`ProxyImage`: `v<VERSION>`, or `latest` for a version that is not
release-shaped) unless the sandbox's `egress.image` names one, which `pats init` pins as it is. so a dev build's
default sidecar may predate the feature: it refuses the policy, the pair fails at its start, and the error points at
`egress.image` (a proxy built from this tree) or a release that carries the feature. `proxy-from-self.md` would make
that automatic. lxd and bwrap run this pats as the proxy where they can.

the real value is never written to disk, a log, `metadata.json`, the audit or an error message. the audit records
the inbound request's url, method, user agent and status, never headers.

### on the wire

bound hosts are mitm'd, since the merged bindings are url entries (`MitmHost`, `NeedsCA`), so the sandbox gets the run
CA's bundle as it does for any url entry. substitution happens only on a connection whose
upstream certificate verified, as every mitm'd upstream is checked now.

for each request the proxy sees in clear (on a mitm'd host, or plain http), in this order:

1. **find the placeholders** in the request's url (path and query) and in every header name and value.
2. **a placeholder anywhere but its binding's one header** refuses the request: 403, audited `rule: key`,
   `reason: misuse`, before the policy is asked. that is a placeholder to another host, path, method or port, over
   plain http even to a bound host, in the url, or in any header other than the one below. the header one endpoint
   names is no good on another: a bearer placeholder on an `X-Api-Key` endpoint is a misuse, and the reverse. the
   403's body names neither the placeholder nor the query.
3. **otherwise the policy decides**, as today. an explicit `deny` still refuses.
4. **substitution**, on an allowed request to a bound endpoint, in the header its entry names: an `Authorization`
   whose value is exactly `Bearer <placeholder>` becomes `Bearer <real value>`, or a named header whose value is
   exactly the placeholder becomes the real value. nothing else in the request changes. a bound request with no
   credential goes out as it is. a bound endpoint takes no other credential: each header a key can be spent in
   (`Authorization`, `X-Api-Key`) is absent there or is that one spend, so one in another shape, given twice, or
   holding a key of the agent's own, beside the placeholder or alone, is refused as misuse, and one key at most is
   spent on a request.
5. **the response**, on a bound endpoint, scrubbed as defence in depth (the provider is trusted not to echo the key,
   and a key it re-encodes is not caught):
   - the proxy drops the client's `Accept-Encoding` before forwarding, so go's transport asks for gzip itself and
     hands back plain text. a response that still carries a `Content-Encoding` other than `identity` (`br`, `zstd`, a
     gzip it sent unasked on a `HEAD` or `Range` request) is refused, 502, `reason: key-scrub`.
   - the real value becomes the placeholder in the response headers, in the body, and in the trailers once the body
     ends. any status, `206` and error bodies included.
   - the body is scrubbed streaming: each read is passed on at once, holding back only the longest tail that could
     begin the real value (usually nothing), so a streamed answer (sse) flows event by event. at the end of the body
     the held tail, a beginning of the real value, goes out as the placeholder's beginning of the same length; on an
     upstream error it is dropped and the error returned. same length, so `Content-Length` holds.
   - informational (1xx) responses are passed on unscrubbed, as `httputil.ReverseProxy` relays them before the
     response is in hand.

a request's trailers come after the decision on it, too late to look for a placeholder in, so where keys are held none
go on. request bodies are not scanned. the agent can read its own env, so the placeholder may sit in its conversation,
and the conversation is the body it sends to the bound endpoint: a body check there would fail honest runs. elsewhere
a body scan would need buffering for a signal the header and url checks mostly give.

the proxy follows no redirect: a `3xx` goes back to the client, whose next request is checked from step 1, so a
redirect to another host carries the placeholder there and is refused, never the real value.

what the proxy cannot see: a host it only tunnels shows nothing, so a placeholder sent there is neither refused nor
audited. it is worthless there; the misuse signal is partial, and the readme says so.

### the audit and metadata

- a misuse is an audit `req` line with `allowed: false`, `rule: key`, `reason: misuse` and the url (the
  placeholder in it is replaced by `<placeholder>`). `readEgressAudit` folds it into `metadata.json` as
  `key_misuse` (a list of urls, beside `denied_egress`), not into `blocked_egress`. a `key-scrub`
  refusal folds into `failed_egress`.
- a substituted request's audit line carries `key: substituted` once the origin answered it, and the pair's egress
  record counts them
  (`egress.keys_substituted`), so a pair shows its key went out through the proxy.
- a misuse does not change the pair's status. pats warns of it as the pair ends, `pats report` prints a line per pair
  with one, after the pivot, as it prints the host driver's advisory line, and its json carries `key_misuse` per pair.
  a judge sample's metadata carries it too.
- the credential preflight writes its audit to a temp file of its own. after the agent ends it closes the session,
  which drains the proxy's log, then reads the file and removes it. it fails when the file is missing or unreadable,
  when it holds a misuse (naming the url), or when it holds no substituted request: a harness that exits 0 without
  authenticating does not pass. the preflight's request to openrouter is the end-to-end check, so a substitution that
  does not happen is caught before any pair runs.

### host steps

host steps are trusted (threat model). still, with `key-proxy: true`, pats drops every name any harness
lists in `auth-env` from the environment a host step inherits (`hostEnviron`), for a `stage`, `prompt-cmd` and
`collect` of a pair whose agent runs in such a sandbox, and for a host `score` of a pair whose agent does (by the
sandbox the current `pats.yaml` gives it). a judge's own host steps follow its own agent's sandbox. this keeps the key
out of what a careless step prints; it does not stop a hostile one, which can read it from pats's own process, the
proxy or the engine. other secrets in pats's environment stay visible to host steps, as today.

the readme's "a step sees only the names pats sets for it" is about the `PATS_*` names: a host step inherits the
rest of pats's environment. the readme says so, and what a key proxy changes.

### commands

- `pats egress check --agent` shows a key-proxy sandbox's bindings by env name and url entry, never a value.
- `pats egress run --sandbox` on a key-proxy sandbox is refused, naming the setting: the standalone proxy has no
  source of real values.

### provider pinning

`provider-pinning.md` (proposed) also rewrites requests to `openrouter.ai` in the proxy. whichever lands second
reconciles the two: both act on the same mitm'd request, pinning on the body and this on one header, and the order
between them does not matter. the `methods` handling here (a binding's method prefix) and pinning's (an exemption for
harness traffic) must agree when both exist.

### what it does not fix

- **spend**, by any code in the sandbox, through the bound endpoints.
- **trusted parts turned hostile**: a fork's `pats.yaml` can switch the setting off, and a fork's host steps can read
  the key. the ci runs both from the base branch.
- **login files** (`claude`, `codex`) and keychain logins: the key is a file the cli reads and refreshes.
- **other harnesses**: `claude-cli-keyless`'s env keys likely fit once checked; `copilot-cli-keyless` trades its token
  for a short-lived one the agent then holds. not now.
- **an orphaned sidecar** holds the key until `pats clean` removes it, as it holds the CA key today.

## recorded

what the pinned clis send, run as their harnesses run them, against a recording mitm proxy that answered
`openrouter.ai` itself with a canned streamed completion and passed other hosts on, with a dummy key:

- **opencode 1.19.5**: `GET models.opencode.ai/api.json` and `GET registry.npmjs.org/@opencode-ai%2fplugin`, neither
  with the key; then two `POST openrouter.ai/api/v1/chat/completions` (a title, then the turn), streamed, each with
  `Authorization: Bearer <key>`. also `Http-Referer`, `X-Title`, and `Accept-Encoding: gzip, deflate, br, zstd`. no
  `GET /api/v1/models`, no generation-stats call, no retry, no websocket.
- **pi 1.5.2**: one `POST openrouter.ai/api/v1/chat/completions` per turn, streamed, with `Authorization: Bearer
  <key>` and the same `Accept-Encoding`; no other host, with or without `PI_OFFLINE`.
- **pi 1.5.2, an anthropic model**: its catalogue marks twenty openrouter models `anthropic-messages` (every
  `anthropic/claude-*` and `~anthropic/claude-*-latest` it lists) and the other 381 `openai-completions`. for those
  twenty it sends one `POST openrouter.ai/api/v1/messages?beta=true` per turn, streamed, with the key bare in
  `X-Api-Key` and no `Authorization`. a 401 is not retried, a 500 is, three times, the same way. an anthropic model
  id its catalogue does not list (`anthropic/claude-haiku-4-5-20251001`) goes to chat completions as a bearer token.
  the binding names the path alone, so the query passes (`docs/egress.md`: a query is matched only through a `?` the
  entry spells).
- **opencode 1.19.5, an anthropic model**: chat completions with a bearer token, as for any other model, on a 401
  too.
- neither put the key in a url or a body, or sent it to another host; both took a key not shaped like one
  (`pats-ph-` and hex) and sent it the same way; both asked for `br` and `zstd`, which the proxy's dropping of
  `Accept-Encoding` exists for.
- not covered: tool-call turns (the same endpoint), h2 (the recorder offered http/1.1, as the proxy does), and for
  the first two records the error and 401 paths.

## deployments

the feature adds one value on each driver's existing env channel, and turns on mitm for openrouter.ai, which mounts
the run CA's bundle into the sandbox as any url entry does. it works where egress with a url entry works now:

- **pats binary on linux** with apt docker, the docker snap (`docker-snap-paths.md`), rootless podman, or lxd (local
  or remote): as above.
- **pats strict snap** (`snap.md`): docker (apt or snap daemon) and local lxd. no podman, no bwrap, no host driver,
  and no host steps (`nohost`: steps run in step sandboxes), so the host-step stripping has nothing to strip there.
- **macos** with docker desktop, colima or a podman machine: the sidecar runs in the vm, its env from the cli; the
  bundle mount needs `~/.cache/pats` shared with the vm, as url entries already do.
- **a remote docker daemon** (`DOCKER_HOST`): the value goes over the api into the sidecar's config on that daemon.
  binds already need a shared filesystem there (`tmp.go`).
- **host driver**: refused.
- **dev builds** on docker or podman: the default sidecar may predate the feature and refuse the policy; an
  `egress.image` built from the same tree works (above).

## changes

- `internal/config`: the `key-proxy` key on `Egress`; the load errors above.
- `internal/machine`: `auth-bind` on a harness, set only by `defaults.yaml`, part of the harness's digest.
- `internal/agent`, `internal/eval/harness.go`: placeholders in the agent env, the bindings in the pair's policy,
  the real values to the driver.
- `src/sandbox/proxy`: the bindings in `Policy`, `PATS_EGRESS_KEYS`, the steps on the wire, the audit fields.
- `src/sandbox` (egress, lxd, bwrap): `PATS_EGRESS_KEYS` on each driver's env channel.
- `internal/eval`: preflight's audit, `key_misuse` in metadata and the report, host-step stripping.
- `cmd/pats`: `egress check` and `egress run`.
- docs: `docs/egress.md`, the readme (the setting, the step sentence), `harness-registry.md`'s "not doing".

## tests

- config: `key-proxy` decodes as a boolean, anything else is an error; `key-proxy: true` with egress absent,
  `egress: none`, or the host driver is a load error; an agent whose harness has an unbound `auth-env` name in a
  key-proxy sandbox is a load error, one with no `auth-env` is not; `auth-bind` in a machine file or a `pats.yaml`
  harness item, a bound name in `pass-env`, a bindings key in an egress mapping, and a `step-env` naming a bound key
  (on a paired task, on a scorer grading such a pair) are load errors; `key-proxy: false` with bindings present is not.
- placeholder: keeps the prefix, length and alphabet; a key with no `-` has no prefix; differs per pair, per judge
  sample and per preflight; a short random part, a space, a control byte or an over-long value is an error; an unset
  key gets none.
- agent env: the sandbox gets the placeholder, never the real value, for docker, podman, lxd and bwrap (fakes).
- delivery: the real value is in the sidecar's cli env and not its argv (docker, podman), on the lxd proxy's stdin and
  not its argv, in the bwrap proxy child's env; a proxy missing a binding's value, or given an unparseable map, does
  not start; an older proxy refuses a policy with bindings, and the pair's error names `egress.image`; an
  `egress.image` from the same build is pinned and substitutes.
- proxy, with a recording upstream: a bound `POST` with `Authorization: Bearer <placeholder>` goes out with the real
  value and nothing else changed; on an endpoint bound to `X-Api-Key`, the placeholder as that header's whole value
  goes out as the real value, query kept, and a bearer placeholder there, the placeholder in `X-Api-Key` on the
  bearer endpoint, in both headers, twice, in another shape or on a path below is refused as misuse; an endpoint
  another credential there, beside the placeholder or alone, is refused on either endpoint; a key bound twice on an
  endpoint is spent in either header, in either order of its entries, and not in both; an endpoint decodes from a
  line or a mapping of `match` and `header`, and an unknown key, a repeated one, a mapping without both, a json
  `null`, a header outside the set (`Host`, `Accept-Encoding`, `Cookie`) or `Authorization` are errors; the
  placeholder in another header, in the url, in `Authorization` of another shape,
  to another host, path, method or port, or over plain http is refused 403 `key`/`misuse`, audited, with a body that
  names neither; `/api/v1/x/%2e%2e/chat/completions` and `/api/v1//chat/completions` with the placeholder are refused
  as misuse; an explicit `deny` of the bound path still refuses; under `methods: [GET, HEAD]` the bound `POST` passes;
  the bound host is mitm'd with only a bare allow entry; a 302 from the bound endpoint to another host reaches the
  client, and the client's next request there is refused as misuse.
- scrub: a response echoing the real key in a header, a trailer and a body, gzip'd, chunked, split across reads, as a
  `206` and as a `401`, reaches the client with the placeholder; an sse response whose short event is followed by an
  idle upstream delivers the event at once; an upstream error mid-body drops the held tail; a `br` response, and a
  gzip one to a `Range` request, are refused `key-scrub`.
- metadata: a misuse lands in `key_misuse`, not `blocked_egress`, with the placeholder masked; a scrub refusal
  in `failed_egress`; substituted requests are counted; the report prints the misuse line and its json carries it.
- preflight: a misuse fails it, naming the url; no substituted request fails it; a missing audit fails it; the audit
  is read after the session closes.
- host steps: with `key-proxy: true`, `stage`, `prompt-cmd`, `collect` and a host `score` lack every `auth-env` name;
  without, they keep them.
- commands: `egress check --agent` shows bindings without values; `egress run --sandbox` on a key-proxy sandbox is
  refused.
- nothing of the real value in `metadata.json`, `egress.log`, `stdout.log`, `stderr.log`, the preflight's temp audit
  or pats's output, under `-vv` too; an interrupted pair leaves no value on disk.
