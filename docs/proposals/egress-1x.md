# proposal: egress on 1.x

status: scoped, not started. each tier below is its own PR or two.
scope: `src/sandbox/proxy`, `src/sandbox/egress.go`, `cmd/egress-proxy`, `internal/config`, `internal/eval`, the
`pats egress` cli, `images/Dockerfile.egress-proxy`, `.github/workflows/publish_images.yml`, `docs/`. 1.x owes 0.x no
backwards compatibility.

## where this starts from

today a sandbox declares `egress: {mode: open | none | proxy | mitm-proxy, default, allow, deny, deny-urls, allow-urls,
proxy-image}`. the engine in `src/sandbox/proxy` filters by host, terminates tls for hosts named by a url rule, and
writes one json line per request. `src/sandbox/egress.go` wires the docker sidecar (internal network, proxy container,
`docker logs -f` follower). `cmd/egress-proxy` is the sidecar binary, configured over `PROXY_*` env. `internal/eval`
folds denied hosts into `denied_egress` in each pair's metadata. `docs/proposals/network-egress.md` is the design and
says so.

three things shape what follows. bwrap, lxd containers and lxd vms are coming as drivers, so the proxy has to work as a
host process and not only as a docker sidecar. macos and linux both matter, so nothing may depend on the host being
reachable from a container network. pats will be a snap later, so the proxy stays inside the pats binary.

sizes: S is under a day, M a few days, L a week or more.

## 1. schema

```yaml
sandbox:
  egress:                      # absent = open network. scalar `none` = no network.
    default: deny              # optional. deny is the default; `allow` makes the lists a denylist.
    methods: [GET, HEAD]       # optional, tier 5. absent = every method. rejected at load until tier 5 lands.
    allow-private: false       # optional. true lets entries name loopback, link-local and rfc1918 targets.
    allow:
      - .ubuntu.com            # a host, or a suffix when it starts with a dot
      - github.com:443         # host:port. without a port, 80 and 443.
      - github.com/some-org/*  # host and path glob. a url entry; its host is mitm'd.
      - .githubusercontent.com/canonical/*     # a suffix host in a url entry
      - 'api.github.com ~^/repos/[^/]+/x$'     # host entry, then a go regexp over path?query
      - POST api.github.com/graphql            # a method prefix on a url entry
      - 10.0.5.20:5432                         # an ip, only dialable with allow-private
    deny:
      - github.com/*/some-answer-repo*
    deny-response: 403         # 403 | 404 | reset | timeout
    image: ghcr.io/...         # the sidecar image, docker only. was proxy-image.
```

- `mode` goes. a mapping means the proxy; the scalar `none` means no network (a custom `UnmarshalYAML` on `Egress`,
  the way `StrList` does it in `load.go`); omitting the key means open. mitm is not a mode: any url entry terminates
  tls for its host, and the entry is the opt-in.
- one `allow` and one `deny`. an entry is `[METHODS ]<host-entry>[/<glob> | ~<regexp>]`. the host entry is a literal
  host, a suffix (`.x`), or an ip, with an optional port. a glob or a regexp makes it a url entry. a deny match always
  wins, then allow, then `default`. both lists apply under both postures.
- globs: `*` matches anything, `/` included, over `host/path` as today.
- regexps: `<host-entry> ~<regexp>`. the host part is parsed like any entry, so the mitm set stays finite. the regexp
  is a go `regexp` over `path?query` and is tried against the same candidates a glob gets (the bare path, the path
  with its query, and both again after `path.Clean`). nothing is anchored for you: `~/repos/` matches
  anywhere, `~^/repos/[^/]+/x$` is exact. a missing host, a bare `~`, or a regexp that does not compile is a load
  error. a `~` inside a glob path (`github.com/~user/*`) is a glob; the sigil is only the first character after the
  whitespace that follows the host entry. backslashes want single quotes in yaml. `~.*` is `host/*` spelled
  differently and is allowed.
- entries and request hosts are normalised the same way before comparison: lowercase, one trailing dot stripped,
  idna to ascii via `x/net/idna`. the dependency is not optional: with lowercase alone a unicode `deny` entry and the
  punycode form a client sends never match, which under `default: allow` is a bypass. `x/net` is not in `go.mod`
  today; it comes in with this.
- paths and queries are case-sensitive on the wire but not on every host (github resolves `Canonical/Chisel-Releases`
  to `canonical/chisel-releases`), so the two lists match them differently, each erring toward closed. an `allow` url
  entry matches path and query case-sensitively: matching less is the safe mistake, and `~^/Public/` does not admit
  `/public/`. a `deny` url entry matches case-insensitively: matching more is the safe mistake, and a deny written
  lowercase still catches `/Canonical/chisel-releases`. a regexp can opt out either way with `(?i)` or by being exact;
  a glob cannot, which is what the rule is for. the audit's `rule` says which entry decided, so a surprise is visible.
- `allow-private` is a sandbox scalar, default false. false refuses any dial whose target address is loopback,
  link-local, rfc1918, `::1`, `fc00::/7`, `fe80::/10` or v4-mapped, whatever the lists say, with `reason: private`
  in the audit. true makes those ranges dialable; an entry still has to match under `default: deny`. refused either
  way: the unspecified addresses `0.0.0.0` and `::`, which the kernel routes to the local stack, and pats' own subnet
  pool (section 2), where nothing legitimate lives and which is the cross-pair escape.
- validation that stays: a url entry's host is a literal or a suffix, never a bare `*`. a bare host in `allow` under
  `default: allow` can never matter and is an error. a method on a bare host entry is an error that says to write
  `GET host/*`. `image` under a driver other than docker is an error. `none` under an agent whose harness has
  `RequiredHosts` is an error naming the agent, the harness and the sandbox.
- validation that goes: "needs mode mitm-proxy", "deny is ignored under default deny".
- validation that is temporary: until tier 5 lands, `methods` and a method prefix on an entry are rejected at load
  with "not implemented yet", the way an agent scorer is today. a field that is accepted and forwarded but not
  enforced would look like filtering that silently does nothing.
- a task may set `egress: {allow, deny}` and nothing else. tier 3.

## 2. tier 1: schema, seam, fixes

one PR, several commits, one category each: `feat:` for the schema, `refactor:` for the seam and the single binary,
a `fix:` per behaviour change below.

### refactor

- `config.Egress` becomes `{Default, Methods, AllowPrivate, Allow, Deny, DenyResponse, Image}` plus the scalar form.
  `sandbox.Egress` mirrors it and `egressFor` in `run.go` stops copying fields one by one.
- one entry parser in `src/sandbox/proxy` replaces `matchAny` and `ParseURLRules`. `Rule` holds parsed entries and
  derives the mitm set from them. `audit` and `auditURL` become one function.
- the driver seam. `startEgressProxy` splits into a driver-neutral part (policy to `Rule`, the per-run CA and the
  merged trust bundle, the agent-side contract of `HTTP_PROXY`, `/sbx-ca/bundle.pem`, `/sbx-ca/ca.pem` and the tls
  env vars) and a per-driver attach (how the sandbox reaches the proxy, what makes it fail closed, how files get in).
  docker keeps the sidecar. lxd and bwrap will run the proxy in-process on the host.
- one binary. `cmd/egress-proxy` is deleted. the engine exposes `Serve(policy, ca, roots, auditWriter, listen)` and
  a minimal `pats egress run` ships in this tier, since the sidecar needs it: `--policy`, `--listen`, `--audit`,
  `--ca-cert`, `--ca-key`, `--roots`, each with its env fallback for the sidecar (`PATS_EGRESS_POLICY` holding the
  yaml block verbatim, `PATS_EGRESS_CA_CERT`, `PATS_EGRESS_CA_KEY`, `PATS_EGRESS_ROOTS`, all inline; flags win). the
  `PROXY_*` protocol goes with the old binary. `images/Dockerfile.egress-proxy` builds `./cmd/pats` and sets the
  entrypoint to `["/pats", "egress", "run"]`; `publish_images.yml` follows. the image keeps its name. the rest of the
  cli (`--sandbox`, `--task`, `--agent`, and the other subcommands) is tier 4.
- one dialer. every upstream connection (tunnel, mitm, plain http) goes through a `net.Dialer` whose `ControlContext`
  sees each attempted `ip:port` before it connects and applies the private-range guard there. this is what makes the
  guard hold against dns rebinding: there is no second resolution the check can miss.
- roots on both legs. the proxy's upstream `tls.Config.RootCAs` is the sandbox image's bundle, which `setupMitm`
  already extracts, not the distroless image's snapshot. tunnelled and mitm'd hosts then verify against one store,
  the one the user updates by rebuilding their image.
- the tls env contract is the five variables in `setupMitm` plus `CARGO_HTTP_CAINFO`. it is a closed list, extended on
  report. a new `env:` field on the sandbox (environment into the agent container) is the escape hatch for a tool not
  on it.
- `mitm_test.go` and `proxy_test.go` survive as behaviour tests; their setup changes with `Rule`. new tests: a
  resolver whose answers change between lookups (rebinding), an sni that differs from the CONNECT host, an inner
  `Host` that differs from the CONNECT host.

### fixes, all S

- the connect authority is bound to the tls authority. a tunnel today dials the CONNECT host and never reads the
  ClientHello, so `CONNECT objects.githubusercontent.com:443` followed by a hello naming `raw.githubusercontent.com`
  is served by the shared front end. the ClientHello is plaintext, so the tunnel buffers the whole handshake message
  (its own 3-byte length says how much, and it may span more than one tls record), reads the sni, and requires it to
  equal the CONNECT host before writing every buffered byte upstream unchanged. nothing is decrypted and no CA is
  involved. the cases: sni present and equal, pass. sni absent, deny unless the CONNECT host is an ip
  literal. ech present (the real name is encrypted), deny. not tls at all: on port 443 deny, on any other allowed port
  pass with `kind: raw` in the audit, since a `CONNECT host:22` carrying an ssh session has no hello. in mitm,
  `GetCertificate` only ever signs the CONNECT host and every inner request's `Host` must equal it. mismatches are
  refused with `reason: sni-mismatch`, `reason: ech` or `reason: host-mismatch`.
- private-range guard, as in section 1, enforced in the dialer's `ControlContext`.
- ports. parsed from entries; without one, 80 and 443.
- proxy by ip. pats owns the subnet (next item), places the proxy at a fixed `--ip`, and the agent gets
  `HTTP_PROXY=http://<ip>:8080`. no resolver in the path, on any driver.
- subnet pool. `network create --internal --subnet 10.213.x.y/29` from a pats-managed `/16`, the range overridable
  by env. docker's default pool is around 15 to 30 networks per daemon and `-j` reaches it. allocation is by attempt:
  try the next `/29`, and on "Pool overlaps" or a name clash advance and retry. a counter alone cannot work, since two
  pats processes on one daemon (two config dirs, or two runs) see different counters, and a network a crashed run
  left behind still holds its subnet. `pats clean` (tier 4) sweeps those.
- dns leak, docker only. with `--internal`, docker's embedded resolver still forwards external names to the host.
  the agent container gets `--dns 0.0.0.0` so there is no upstream. podman's aardvark forwards nothing on an internal
  network already (verified on podman 6.1.4 / netavark 2.1.0: container names resolve, external names fail, with or
  without `--dns`).
- default network per driver. `network connect bridge` is docker's name; podman's default network is `podman`
  (`10.88.0.0/16`, dns off), so proxy mode fails there today. resolve the name per driver.
- readiness. the proxy prints `listening` on stderr; pats waits for the line before starting the agent. the audit
  follower captures stdout only, so the line never lands in `egress.log`. host-side drivers have no race.
- server timeouts. `ReadHeaderTimeout` and an idle timeout on the `http.Server`. `deny-response: timeout` is a
  handler holding the connection, which neither of those interrupts, so it holds for a fixed 60 seconds and then
  resets.
- fail-closed smoke test. preflight already runs the harness through the real egress path; it adds a canary that,
  from inside the sandbox with the proxy env unset, tries tcp4 and tcp6 to a public address, a dns query over udp
  to `8.8.8.8:53`, and a name lookup, and requires all four to fail. for udp "fail" means no answer within a short
  deadline or a socket error; a successful `connect` or `write` proves nothing, since udp has neither. otherwise the
  run refuses to start and names the driver. it is a smoke test of the topology, not a proof of it; each new driver
  still gets reviewed.

## 3. tier 2: audit

- the event schema. every line carries `ts`, `ev` and `host`. `ev` is one of:
    - `open`: a tunnel was requested. `port`, `kind` (`tunnel`, `raw`), `allowed`, `rule`, the resolved `ip`,
      and on refusal a `reason`.
    - `close`: a tunnel ended. `in`, `out`, `ms`. never a denial.
    - `req`: a request the proxy saw in clear (mitm'd or plain http). `port`, `kind` (`mitm`, `http`), `allowed`,
      `rule`, `method`, `url`, `ua`, `status`, `in`, `out`, `ms`, and on refusal a `reason`.
  `rule` is the entry that decided, or `default`. `pipe` becomes a counting copy. the line counter counts `open` and
  `req`. the stats `NET` column gains bytes; `metadata.json` gains per-pair bytes, the effective policy, and the
  sidecar image digest.
- three lists, classified by `rule` and `reason`. every `open` or `req` with `allowed: false` lands in exactly one:
    - `denied_egress`: `rule: default`. the agent reached for something nobody anticipated. the cheat signal.
    - `blocked_egress`: a policy refusal the agent triggered. an explicit `deny` entry, an `allow` url entry that did
      not match (`rule` names the host's allow entry, `reason: url-not-allowed`, so it never reads as `default`), the
      `methods` posture, the private-range guard, and the sni, ech and `Host` mismatches.
    - `failed_egress`: upstream trouble, not the agent's doing. `reason: revoked` and dial failures.
  the first two go in the end-of-pair warning with different labels; the third does not. explicit deny is not news,
  and an upstream outage is not a cheat. no table of known-noise hosts: a user adds the host to `deny` once.
- upstream verify hook. `VerifyConnection` on the proxy's upstream `tls.Config` (it receives the `ConnectionState`,
  so it sees `OCSPResponse`, and it runs on resumed sessions, which `VerifyPeerCertificate` does not) checks a stapled
  ocsp response when the server sends one, via `x/crypto/ocsp`. `Revoked` is a 502 to the agent and an audit line
  with `reason: revoked`. no staple means pass, with `revocation: unchecked` in the audit. revocation infrastructure
  never fails a run. there is no interface for other checkers because none are planned.

## 4. tier 3: task-level override

a task may declare `egress: {allow, deny}`. the effective policy for a pair is the sandbox's lists union the task's
lists, with the sandbox's scalars. a task cannot set `none`, `default`, `methods`, `allow-private`, `deny-response`
or `image`; a separate `TaskEgress` struct makes those unknown-field errors. a task with `egress` that a suite pairs
with an agent whose sandbox has no egress, or `none`, is a load error naming all three.

two merges, kept apart in the docs. at config time, `defaults` to item replaces the whole value, as for every other
field. at run time, sandbox plus task unions.

in the `items` layout the vector's `defaults.egress` carries what the tasks share and an item overrides the whole
block. in the `dir` layout every `task.yaml` carries its own, as task directories have no defaults by design. a rule
shared by every task of a vector is sandbox posture and belongs on the sandbox; the task level is for the per-task
bit. size M: `config.go`, the slot handling in `load.go`, `validate.go`, `egressFor`, metadata, tests, the readme and
the example.

## 5. tier 4: the `pats egress` cli

```
pats egress check <sandbox> [--task <id>] [--agent <id>] [--method GET] <url>
pats egress suggest <run-dir>
pats egress log <pair-dir> [--denied] [--method POST,PUT,PATCH,DELETE]
pats egress run --policy <file> | --sandbox <id> [--task <id>] [--agent <id>]
               [--listen :8080] [--audit <file>] [--ca-cert <pem> --ca-key <pem>] [--roots <pem>]
pats egress ca --out <dir>
pats clean
```

- `check` evaluates the merged `Rule` and prints the verdict with the deciding entry, whether the host would be a
  tunnel or mitm'd, and, under `methods` with `default: allow`, that every host is inspected. harness hosts are merged
  per agent (`RequiredHosts`), so the same sandbox can answer differently for different agents: with `--agent` the
  verdict is the effective one, without it the output says "sandbox and task only, harness hosts not merged".
  `--method` defaults to `GET`, since under tier 5 the verdict depends on it.
- `suggest` folds a run's `egress.log` files into an `allow:` block. `req` lines are grouped by user agent; tunnels
  expose neither user agent nor method and are listed ungrouped. harness hosts are marked so they stay out of the
  user's list. `methods: [GET, HEAD]` is proposed only when the run had no blind tunnels and every `req` was a `GET`
  or `HEAD`; with tunnels present it says the methods are unknown instead.
- `log` is the human table over the audit json.
- `run` grows `--sandbox`, `--task` and `--agent` on top of the tier 1 minimum, so a policy can be served straight
  out of `pats.yaml`. a policy file is the `egress:` block verbatim; listen address, audit path, CA and
  roots are flags, since they are deployment rather than policy. the sidecar passes the same things as env.
- `ca` mints a CA pair for standalone mitm; a pats run makes its own.
- `clean` is not egress-specific: it removes `sbx-*` containers and networks older than the oldest live run and the
  leftovers under `~/.cache/pats/tmp/`.
- docs: `docs/proposals/network-egress.md` is promoted to `docs/egress.md` with a standalone section at the end.
  `pats.example.yaml` gains commented per-ecosystem host lists (apt, pypi, npm, crates, go) as examples to copy.

## 6. tier 5: the `methods` posture

`methods: [GET, HEAD]` denies every unlisted method. it is strong: setting it makes the proxy inspect every allowed
task host, harness hosts excepted, because a tunnel cannot see a method. the exception is only for the inspection
`methods` forces: a url entry the user writes on a harness host still mitm's it, as it would without `methods`, and
the posture applies there too. absent, nothing extra is inspected. per-entry
method prefixes override it in both directions (`POST api.github.com/graphql` in allow, `PUT,PATCH github.com/*` in
deny). under `default: allow` this inspects every host the agent reaches other than inference; it is opt-in and
spelled, and `check` and the readme say so. a list naming every method still inspects. a per-entry tunnel escape for
cert-pinned tools waits until someone needs it. depends on tier 2 for `method` in the audit. this tier lifts the
tier 1 load rejection of `methods` and method prefixes. size M.

## 7. order

1. tier 1: schema, seam, fixes.
2. tier 2: audit.
3. tier 3: task-level override.
4. tier 4: the cli, `pats clean`, the docs promotion, the example comments.
5. tier 5: `methods`.

## 8. shelf

named so they are not rediscovered, not scheduled.

- quotas (`max-bytes`, per host and total), once bytes are in the audit.
- cidr entries, once the private-range guard exists and someone has a local service.
- websocket passthrough on a mitm'd host. documented as a ceiling until then.
- a cache. the recipe is an apt-cacher-ng sidecar on the allow list, documented, no pats code.
- a transparent sni-sniffing listener, when lxd lands and the host owns the bridge. the sni parser from tier 1 is
  the piece it reuses.
- a per-entry tunnel escape under `methods`.
- `inspect: true`, if "audit every url" is ever asked for on its own.
- `NO_PROXY` for companion services on the internal network, when such services exist.
- holding plain http over CONNECT to the same host rule via its `Host` header, if it ever matters.

## 9. not doing

- presets such as `@ubuntu`. a table of facts about the internet that rots silently; `suggest` and example comments
  cover the need.
- `on-denied: fail | score`. a denial is a logged fact; what it means for a score is the scorer's business.
- an `audit` mode. `egress: {default: allow}` is the discovery posture.
- a gateway mode on docker. it needs `NET_ADMIN` in the agent container.
- socks5. record and replay.
- a known-noise-host table. replaced by `blocked_egress` versus `denied_egress`.
- upki or crlite revocation in the proxy. the verify hook does stapled ocsp and nothing else.

## 10. platform notes

- macos, under docker desktop or podman machine: the daemon lives in a vm, host paths are shared only under `$HOME`
  (`tmp.go` already builds on this), and the host is not reachable from an internal network, which is why docker
  keeps the sidecar. the address pool is the vm's.
- linux: direct. the docker snap has the same `$HOME`-only visibility.
- snap packaging, later: strict confinement means pats reaches docker and podman over their sockets, `~/.cache/pats`
  becomes the snap's user data, and the host-side proxy for lxd and bwrap runs inside the snap, so its listener must
  be reachable from the lxd network. the one-binary decision and the driver seam are drawn with this in mind.

## verification

- `go test ./src/sandbox/... ./internal/...`. `proxy_test.go` and `mitm_test.go` cover rule matching and the tunnel
  and mitm paths with in-process servers, plus the rebinding, sni-mismatch, host-mismatch and fragmented-hello tests
  from tier 1, and the case rules for `allow` and `deny` url entries.
- `make egress-image` and a run of the built image: it must start as `/pats egress run`, print `listening`, and
  serve a policy handed over env. `go test` alone does not exercise the dockerfile.
- the fail-closed smoke test in preflight is the end-to-end gate for each driver.
- a mason-style sandbox under the new schema with a url deny: `curl` the denied url and confirm the chosen
  `deny-response` and a `blocked_egress` entry; `curl` an unlisted host and confirm a `denied_egress` entry; `curl`
  an allowed host with `--resolve` pointing its name at another allowed host's address and confirm the sni refusal.
- `pats egress check --agent` on the same config agrees with the audit's `rule` field.
- podman: one pair under proxy mode on `podman machine`, covering the default network name, the subnet, the fixed ip
  and readiness.
