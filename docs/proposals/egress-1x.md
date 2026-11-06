# proposal: egress on 1.x

status: scoped, not started. each tier below is its own PR or two.
scope: `src/sandbox/proxy`, `src/sandbox/egress.go`, `cmd/egress-proxy`, `internal/config`, `internal/eval`, the
`pats egress` cli, `docs/`. 1.x owes 0.x no backwards compatibility.

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
    methods: [GET, HEAD]       # optional, tier 5. absent = every method.
    allow:
      - .ubuntu.com            # a host, or a suffix when it starts with a dot
      - github.com:443         # host:port. without a port, 80 and 443.
      - github.com/some-org/*  # host and path. a url entry; its host is mitm'd.
      - .githubusercontent.com/canonical/*     # a suffix host in a url entry
      - "~^api\\.github\\.com/repos/[^/]+/x"    # a go regexp over the same host/path?query string
      - POST api.github.com/graphql            # a method prefix on a url entry
    deny:
      - github.com/*/some-answer-repo*
    deny-response: 403         # 403 | 404 | reset | timeout
    image: ghcr.io/...         # the sidecar image, docker only. was proxy-image.
```

- `mode` goes. a mapping means the proxy; the scalar `none` means no network (a custom `UnmarshalYAML` on `Egress`,
  the way `StrList` does it in `load.go`); omitting the key means open. mitm is not a mode: any url entry terminates
  tls for its host, and the entry is the opt-in.
- one `allow` and one `deny`. an entry is a host, a host with a port, an ip, a host plus a path glob (`*` matches
  anything, `/` included), a `~` regexp, or a method-prefixed url entry. a deny match always wins, then allow, then
  `default`. both lists apply under both postures.
- entries and request hosts are normalised the same way before comparison: lowercase, one trailing dot stripped,
  idna to ascii (`x/net/idna`, or lowercase and the dot alone if the dependency is unwanted).
- validation that stays: a url entry's host is a literal or a suffix, never a bare `*`. a bare host in `allow` under
  `default: allow` can never matter and is an error. a method on a bare host entry is an error that says to write
  `GET host/*`. `image` under a driver other than docker is an error. `none` under an agent whose harness has
  `RequiredHosts` is an error naming the agent, the harness and the sandbox.
- validation that goes: "needs mode mitm-proxy", "deny is ignored under default deny".
- a task may set `egress: {allow, deny}` and nothing else. tier 3.

## 2. tier 1: the cleanup PR

one `refactor:` with the fixes folded in, since they all touch the entry parser and `startEgressProxy`.

### refactor

- `config.Egress` becomes `{Default, Methods, Allow, Deny, DenyResponse, Image}` plus the scalar form. `sandbox.Egress`
  mirrors it and `egressFor` in `run.go` stops copying fields one by one.
- one entry parser in `src/sandbox/proxy` replaces `matchAny` and `ParseURLRules`. `Rule` holds parsed entries and
  derives the mitm set from them. `audit` and `auditURL` become one function.
- the driver seam. `startEgressProxy` splits into a driver-neutral part (policy to `Rule`, the per-run CA and the
  merged trust bundle, the agent-side contract of `HTTP_PROXY`, `/sbx-ca/bundle.pem`, `/sbx-ca/ca.pem` and the tls
  env vars) and a per-driver attach (how the sandbox reaches the proxy, what makes it fail closed, how files get in).
  docker keeps the sidecar. lxd and bwrap will run the proxy in-process on the host.
- one binary. `cmd/egress-proxy` is deleted. the engine exposes `Serve(policy, ca, roots, auditWriter, listen)`;
  `pats egress run` (tier 4) is both the sidecar's entrypoint (`["/pats", "egress", "run"]`) and the standalone cli.
  the `PROXY_*` protocol goes with the binary. the sidecar reads `PATS_EGRESS_POLICY` (the yaml block verbatim),
  `PATS_EGRESS_CA_CERT`, `PATS_EGRESS_CA_KEY` and `PATS_EGRESS_ROOTS`, all inline; flags win over env. the image keeps
  its name.
- roots on both legs. the proxy's upstream `tls.Config.RootCAs` is the sandbox image's bundle, which `setupMitm`
  already extracts, not the distroless image's snapshot. tunnelled and mitm'd hosts then verify against one store,
  the one the user updates by rebuilding their image.
- the tls env contract is the five variables in `setupMitm` plus `CARGO_HTTP_CAINFO`. it is a closed list, extended on
  report. a new `env:` field on the sandbox (environment into the agent container) is the escape hatch for a tool not
  on it.
- `mitm_test.go` and `proxy_test.go` survive as behaviour tests; their setup changes with `Rule`.

### fixes, all S

- private-range guard. resolve before dialling and refuse loopback, link-local, rfc1918, `::1`, `fc00::/7`,
  `fe80::/10`, v4-mapped addresses and the proxy's own bridge subnet. `allow-private: true` opts out. this closes
  reaching the host gateway, `host.docker.internal`, `169.254.169.254` and other runs' sidecars under `default: allow`,
  and dns rebinding of an allowed host under either posture.
- ports. parsed from entries; without one, 80 and 443.
- proxy by ip. pats owns the subnet (next item), places the proxy at a fixed `--ip`, and the agent gets
  `HTTP_PROXY=http://<ip>:8080`. no resolver in the path, on any driver.
- subnet pool. `network create --internal --subnet 10.213.x.y/29` from a pats-managed `/16`, a counter per run,
  the range overridable by env. docker's default pool is around 15 to 30 networks per daemon and `-j` reaches it.
- dns leak, docker only. with `--internal`, docker's embedded resolver still forwards external names to the host.
  the agent container gets `--dns 0.0.0.0` so there is no upstream. podman's aardvark forwards nothing on an internal
  network already (verified on podman 6.1.4 / netavark 2.1.0: container names resolve, external names fail, with or
  without `--dns`).
- default network per driver. `network connect bridge` is docker's name; podman's default network is `podman`
  (`10.88.0.0/16`, dns off), so proxy mode fails there today. resolve the name per driver.
- readiness. the proxy prints `listening`; pats waits for the line before starting the agent. host-side drivers have
  no race.
- server timeouts. `ReadHeaderTimeout` and an idle timeout on the `http.Server`. they also bound
  `deny-response: timeout`.
- fail-closed self-test. preflight already runs the harness through the real egress path; it adds a canary that, from
  inside the sandbox with the proxy env unset, opens a direct tcp connection to a public ip and requires it to fail.
  otherwise the run refuses to start and names the driver. this is how each new driver's topology gets proven.

## 3. tier 2: audit

- richer lines. `ev` is `open`, `close` or `req`. tunnels get an `open` (with `kind` and the resolved ip) and a
  `close` (with `in`, `out`, `ms`); mitm'd and plain-http requests get a `req` with `method`, `status`, `ua`, `url`
  and `rule`, the entry that decided it or `default`. `pipe` becomes a counting copy. `deniedEgress` is unchanged;
  the line counter skips `close`. the stats `NET` column gains bytes; `metadata.json` gains per-pair bytes, the
  effective policy, and the sidecar image digest.
- explicit deny is not news. `denied_egress` holds default denials only: the agent reached for something nobody
  anticipated. a request matching an explicit `deny` entry goes to `blocked_egress`. both are in the metadata and in
  the end-of-pair warning, with different labels. no table of known-noise hosts.
- upstream verify hook. `VerifyPeerCertificate` on the proxy's upstream `tls.Config` checks a stapled ocsp response
  when the server sends one (`ConnectionState.OCSPResponse`, `x/crypto/ocsp`). `Revoked` is a 502 to the agent and an
  audit line with `reason: revoked`. no staple means pass, with `revocation: unchecked` in the audit. revocation
  infrastructure never fails a run. there is no interface for other checkers because none are planned.

## 4. tier 3: task-level override

a task may declare `egress: {allow, deny}`. the effective policy for a pair is the sandbox's lists union the task's
lists, with the sandbox's scalars. a task cannot set `none`, `default`, `methods`, `deny-response` or `image`; a
separate `TaskEgress` struct makes those unknown-field errors. a task with `egress` that a suite pairs with an agent
whose sandbox has no egress, or `none`, is a load error naming all three.

two merges, kept apart in the docs. at config time, `defaults` to item replaces the whole value, as for every other
field. at run time, sandbox plus task unions.

in the `items` layout the vector's `defaults.egress` carries what the tasks share and an item overrides the whole
block. in the `dir` layout every `task.yaml` carries its own, as task directories have no defaults by design. a rule
shared by every task of a vector is sandbox posture and belongs on the sandbox; the task level is for the per-task
bit. size M: `config.go`, the slot handling in `load.go`, `validate.go`, `egressFor`, metadata, tests, the readme and
the example.

## 5. tier 4: the `pats egress` cli

```
pats egress check <sandbox> [--task <id>] <url>
pats egress suggest <run-dir>
pats egress log <pair-dir> [--denied] [--method POST,PUT,PATCH,DELETE]
pats egress run --policy <file> | --sandbox <id> [--task <id>]
               [--listen :8080] [--audit <file>] [--ca-cert <pem> --ca-key <pem>] [--roots <pem>]
pats egress ca --out <dir>
pats clean
```

- `check` evaluates the merged `Rule` and prints the verdict with the deciding entry, whether the host would be a
  tunnel or mitm'd, and, under `methods` with `default: allow`, that every host is inspected.
- `suggest` folds a run's `egress.log` files into an `allow:` block, grouped by user agent, with harness hosts marked
  so they stay out of the user's list. when a run shows only `GET` and `HEAD`, it proposes `methods: [GET, HEAD]`.
- `log` is the human table over the audit json.
- `run` is the engine's entrypoint. a policy file is the `egress:` block verbatim; listen address, audit path, CA and
  roots are flags, since they are deployment rather than policy. the sidecar passes the same things as env.
- `ca` mints a CA pair for standalone mitm; a pats run makes its own.
- `clean` is not egress-specific: it removes `sbx-*` containers and networks older than the oldest live run and the
  leftovers under `~/.cache/pats/tmp/`.
- docs: `docs/proposals/network-egress.md` is promoted to `docs/egress.md` with a standalone section at the end.
  `pats.example.yaml` gains commented per-ecosystem host lists (apt, pypi, npm, crates, go) as examples to copy.

## 6. tier 5: the `methods` posture

`methods: [GET, HEAD]` denies every unlisted method. it is strong: setting it makes the proxy inspect every allowed
task host, harness hosts excepted, because a tunnel cannot see a method. absent, nothing extra is inspected. per-entry
method prefixes override it in both directions (`POST api.github.com/graphql` in allow, `PUT,PATCH github.com/*` in
deny). under `default: allow` this inspects every host the agent reaches other than inference; it is opt-in and
spelled, and `check` and the readme say so. a list naming every method still inspects. a per-entry tunnel escape for
cert-pinned tools waits until someone needs it. depends on tier 2 for `method` in the audit. size M.

## 7. order

1. tier 1, the cleanup PR.
2. tier 2, audit.
3. tier 3, task-level override.
4. tier 4, the cli, `pats clean`, the docs promotion, the example comments.
5. tier 5, `methods`.

## 8. shelf

named so they are not rediscovered, not scheduled.

- quotas (`max-bytes`, per host and total), once bytes are in the audit.
- cidr entries, once the private-range guard exists and someone has a local service.
- websocket passthrough on a mitm'd host. documented as a ceiling until then.
- a cache. the recipe is an apt-cacher-ng sidecar on the allow list, documented, no pats code.
- a transparent sni-sniffing listener, when lxd lands and the host owns the bridge.
- a per-entry tunnel escape under `methods`.
- `inspect: true`, if "audit every url" is ever asked for on its own.
- `NO_PROXY` for companion services on the internal network, when such services exist.

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
  and mitm paths with in-process servers.
- the fail-closed self-test in preflight is the end-to-end gate for each driver.
- a mason-style sandbox under the new schema with a url deny: `curl` the denied url and confirm the chosen
  `deny-response` and a `blocked_egress` entry; `curl` an unlisted host and confirm a `denied_egress` entry.
- `pats egress check` on the same config agrees with the audit's `rule` field.
- podman: one pair under proxy mode on `podman machine`, covering the default network name, the subnet, the fixed ip
  and readiness.
