# egress

how a pats sandbox reaches the network, and how to run the egress proxy on its own. the design history is in
`docs/proposals/egress-1x.md`.

## why

an agent under test with an open network can fetch the answer instead of producing it. a blanket block is wrong too:
the same eval may need a package mirror, and the harness always needs its inference api. so the sandbox gets
selective egress that fails closed: it is allowed what the task needs, refused the answer source, and it cannot route
around the filter.

## shape

```yaml
sandbox:
  kind: sandbox
  items:
    box:
      egress:                      # absent: open. `egress: none`: no network at all.
        default: deny              # deny (the default) or allow
        methods: [GET, HEAD]       # optional: refuse every other method (inspects every allowed host)
        allow:
          - .ubuntu.com            # a host, or a suffix when it starts with a dot
          - github.com:22          # a host on one port; without a port, 80 and 443
          - '.githubusercontent.com/canonical/*'   # a url entry: a host and a path glob
          - 'api.github.com ~^/repos/[^/]+/x$'     # a url entry: a host and a go regexp over path?query
          - 10.0.5.20:5432         # an ip, dialable only with allow-private
          - POST api.github.com/graphql            # a method prefix: admits POST here whatever methods says
        deny:
          - 'github.com/*/some-answer-repo*'
        deny-response: 403         # 403 | 404 | reset | timeout
        allow-private: false       # true lets entries name loopback, link-local and rfc1918 targets
        image: ghcr.io/...         # the sidecar image (container drivers)
      env:                         # extra environment for the agent, under what pats sets
        DENO_CERT: /sbx-ca/bundle.pem
```

- **open** no `egress` key means no filtering at all: the agent reaches the internet, and also the host, its lan and
  a cloud host's metadata endpoint (`169.254.169.254`). `egress: {default: allow}` passes every host but has the dial
  guard refuse what is never the public internet: loopback, rfc1918 and `fc00::/7`, link-local (the metadata endpoint
  among it), multicast and carrier-grade nat `100.64.0.0/10` (where tailscale and the like put a host's network). a
  host or lan numbered out of global addresses is not covered: it looks like the internet from here.
- **precedence** a deny match always wins, then an allow match, then `default`. both lists apply under both postures.
  a bare host in `allow` under `default: allow` can never matter and is a load error.
- **url entries** a glob's `*` matches anything, `/` included, over the whole path (and the query, when the glob names
  one). a regexp is unanchored unless you anchor it. a `..` in the request path does not dodge an entry: every entry
  is also tried against the `path.Clean`ed path. an `allow` url entry matches the path case-sensitively, a `deny` one
  case-insensitively, each erring toward closed. a host with `allow` url entries passes only paths one of them admits.
- **mitm** a url entry is the opt-in to tls termination for its host: the proxy presents a leaf from a per-run CA and
  filters each request. every other host is a tunnel the proxy never decrypts. the run CA's key reaches the proxy
  over env only and never touches disk.
- **hosts** are compared lowercase, without a trailing dot, in their idna ascii form, so `GitHub.com.` and
  a unicode name's punycode match the entries you would expect.
- **harness hosts** under `default: deny` the hosts the harness itself needs (inference, token refresh, startup
  fetches) are merged in per agent. pats.yaml lists only what the task needs.
- **tasks** a task may set `egress: {allow, deny}`. its entries are unioned over the sandbox's for that task's pairs;
  posture (`none`, `default`, `deny-response`, `allow-private`, `image`) stays the sandbox's. a task with `egress`
  paired with an agent whose sandbox runs no proxy is a load error.
- **methods** `methods: [GET, HEAD]` refuses every other method, with `rule: methods` and `reason: method` in the
  audit. a tunnel cannot show a method, so the posture makes the proxy inspect every allowed host -- under
  `default: allow`, every host reached -- except the harness's own (its inference api is never decrypted for it; an
  explicit url entry on such a host still mitm's it, and the posture applies to what it shows). an entry can carry a
  method prefix, which needs a url part: `POST api.github.com/graphql` in `allow` admits that method there whatever
  the posture, `PUT,PATCH github.com/*` in `deny` refuses only those methods. absent, every method passes and nothing
  extra is inspected.

## topology

the agent container sits on its own `--internal` network: no gateway, no route out. the proxy sidecar sits on that
network at a fixed address and on the driver's default network, which is its way out. the agent is told
`HTTP_PROXY=http://<proxy ip>:8080` and needs no resolver: docker's is cut off from upstream with `--dns 0.0.0.0`,
podman's is disabled on the network. the host has no address on that network either, so a service listening on the
host is out of the agent's reach too. each pair gets its own `/29` out of `10.213.0.0/16` (override with
`PATS_EGRESS_POOL`, an ipv4 cidr of `/24` or wider); a subnet already in use is skipped.

under lxd the agent sits on a bridge of its pair's own with no host address: no dhcp, no dns, no routing, no host to
reach, and a rule-less acl rejecting both ways fences the bridge-host boundary besides. its only neighbour is the
proxy, a container of its own at a static address from the same pool, whose second nic on the shared `patsup` bridge is
the only way out; the proxy forwards nothing and listens on the bridge address only. the proxy binary is this pats
for the server's arch when it can be, else the published proxy image's, read from the registry without docker
(`PATS_LXD_PROXY_BIN` names a file instead). the canary runs from the agent before the proxy exists, preflight then
checks the agent reaches the proxy, and with `sudo: true` it also tries, as root, to route out through the bridge's
host side.

what the proxy enforces, beyond the lists:

- **the tunnel is bound to its CONNECT host.** the tls ClientHello is read (across records, replayed unchanged) and
  its sni must name the CONNECT host. no sni is refused unless the CONNECT host is an ip; an encrypted ClientHello
  (ech) is refused since the real name cannot be checked; anything that is not tls on port 443 is refused. a non-tls
  opening on another allowed port (an ssh session on 22) is piped as `kind: raw`, unless the host is mitm'd (a url
  entry names it, or `methods` is set): then it is refused with `reason: not-tls`. a mitm'd connection gets a leaf for
  the CONNECT host only, and every inner request's `Host` must name it too.
- **the dial is guarded.** the address actually connected to (after resolution, so a name that resolves or
  re-resolves to one is caught) must not be loopback, link-local, private or v4-mapped unless `allow-private`, and is
  never unspecified (`0.0.0.0`, `::`) or inside pats' own pool.
- **upstream tls** verifies against the sandbox image's own trust bundle, not the sidecar's, and a stapled ocsp
  response that says revoked fails the request.
- **the topology is checked.** preflight runs a canary on the internal network before the proxy exists: tcp4, tcp6, a
  dns query over udp and a name lookup must all fail, or the run refuses to start. once the proxy is up, its port must
  answer from that network, so a host firewall dropping internal traffic is named before any pair runs. the canary
  needs `bash`, `timeout` and `getent` in the image and says so when they are missing.

the sandbox trusts the run CA through `SSL_CERT_FILE`, `CURL_CA_BUNDLE`, `GIT_SSL_CAINFO`, `REQUESTS_CA_BUNDLE`,
`CARGO_HTTP_CAINFO` (all the merged bundle at `/sbx-ca/bundle.pem`) and `NODE_EXTRA_CA_CERTS` (the CA alone at
`/sbx-ca/ca.pem`). a tool that reads none of these fails its handshake on a mitm'd host: closed, not bypassed. the
sandbox's `env` is the place for one that needs its own variable.

## audit

the proxy writes one json line per event to the pair's `egress.log`:

- `open` a CONNECT: `kind` (`tunnel` or `raw`), `allowed`, `rule`, the `ip` dialled, and on refusal a `reason`.
- `close` a tunnel ended: `in` and `out` bytes, `ms`. always `allowed: true`.
- `req` a request the proxy saw in clear: `kind` (`mitm` or `http`), `method`, `url`, `ua`, `status`, `in`, `out`,
  `ms`, `revocation` (`good` or `unchecked` on a mitm'd host).

`rule` is the entry that decided, or `default`. refusals are folded into three lists in the pair's `metadata.json`:

- `denied_egress` -- `rule: default`: nobody anticipated it. the cheat signal.
- `blocked_egress` -- the policy refused it: an explicit `deny`, a path no `allow` url entry admits
  (`reason: url-not-allowed`), a private target, a tls hello naming another host.
- `failed_egress` -- upstream trouble: a dial failure, a revoked certificate.

only `denied_egress` is warned about at the end of each pair, the other two are in `metadata.json` alone. it also
records the effective policy, the sidecar image (its registry digest when pulled), and the requests and bytes that
crossed the proxy.

## commands

```
pats egress check <sandbox> [--task <id>] [--agent <id>] [--method GET] <url>
pats egress suggest [--run <run>]
pats egress log [--denied] [--method POST,PUT,PATCH,DELETE] <pair-dir>
pats egress run --policy <file> | --sandbox <id> [--task <id>] [--agent <id>] [flags]
pats egress ca --out <dir>
pats clean [--dry-run] [--older-than 24h] [--force] [--run <token>]
```

- `check` prints the verdict, the deciding entry, and whether the host would be a tunnel or mitm'd. it exits 1 on a
  refusal. without `--agent` the harness hosts are not merged, and it says so.
- `suggest` folds a run's audits into an `allow:` block for review. run once under `default: allow` to discover what
  a task reaches, then lock it down. requests are grouped by user agent; tunnels hide theirs and are listed apart.
- `log` is the audit as a table.
- `clean` removes what dead runs left: agent containers, sidecars, networks, lxd instances, bridges and acls (a
  killed run leaves its agent running), and temp dirs older than `--older-than`. a run is alive while its lease is
  fresh: a file in the pats cache dir for docker and podman, renewed every two minutes, the run's marker for lxd.
  pats labels everything it makes `pats.run=<token>` and `pats.owner=<host>/<uid>/<client>`; a run this host cannot
  judge (another user's or machine's on a shared daemon) is counted and left, and `--force` removes it. a fresh lease
  is never removed by `--force`, only by `--run <token>`.

## standalone

the proxy is `pats egress run`, so it runs without a pats.yaml, a sandbox or docker:

```sh
pats egress ca --out ./ca                      # only needed for url entries
cat > policy.yaml <<'POLICY'
allow:
  - .ubuntu.com
  - 'github.com/canonical/*'
deny-response: "404"
POLICY
pats egress run --policy policy.yaml --listen 127.0.0.1:8080 \
    --ca-cert ./ca/ca.pem --ca-key ./ca/ca.key --audit egress.log
HTTPS_PROXY=http://127.0.0.1:8080 curl --cacert ./ca/ca.pem https://github.com/canonical/chisel
```

the policy file is an `egress:` block verbatim; unknown keys are an error. every flag has an environment fallback,
which is how the sidecar is configured: `PATS_EGRESS_POLICY` (the yaml itself), `PATS_EGRESS_LISTEN`,
`PATS_EGRESS_AUDIT`, `PATS_EGRESS_CA_CERT` and `PATS_EGRESS_CA_KEY` (a pem file or the pem inline),
`PATS_EGRESS_ROOTS` (the upstream trust bundle; default the system roots) and `PATS_EGRESS_REFUSE` (cidrs never
dialled, comma-separated). a flag wins over its env. the proxy prints `listening <addr>` on stderr once the port is
open; the audit goes to stdout unless `--audit` names a file. the image `ghcr.io/lczyk/pats/egress-proxy` is this
binary with `pats egress run` as its entrypoint.

## ceilings

- mitm'd connections are http/1.1; clients downgrade from h2. websockets through a mitm'd host are not passed.
- a tool that pins certificates fails on a mitm'd host. keep pinned hosts out of url entries.
- a copy of denied content on a host the policy allows is not caught. `default: deny` mostly moots this.
- a tunnel to an allowed ip literal carries no sni and is not bound to a name.
