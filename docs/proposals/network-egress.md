# proposal: sandbox network egress control

status: draft / proposed
scope: pats sandboxes (container driver first; bwrap later)

## problem

agents under test run in a sandbox with **unrestricted network**. for many
evals that invalidates the result: the agent can fetch the answer instead of
producing it.

concrete case (mason chisel-releases eval): a "knockout" task deletes the real
`slices/<pkg>.yaml` and asks the agent to reproduce it. with open network the
agent just `curl`s the file from another branch of the upstream repo:

```
curl -fsSL https://raw.githubusercontent.com/canonical/chisel-releases/ubuntu-22.04/slices/ca-certificates.yaml
```

byte-identical output, score 1.0, measured nothing.

but a blanket network block is wrong too: the same eval **legitimately** needs
the network -- `apt-get` to pull the `.deb` it slices (the sandbox doesn't ship
every package), and the agent harness itself needs to reach the inference api.

so the requirement is **selective egress**: allow what the task legitimately
needs (package mirrors, inference api, docs), deny the answer source (the
upstream repo), and -- critically -- **fail closed** so a non-cooperating agent
can't route around it.

## requirements

1. **per-sandbox policy**, declared in `pats.yaml` (versioned, reproducible).
2. **default-deny (allowlist)** as the recommended posture; allow named hosts.
   also support default-allow (denylist) for the "just block the obvious
   answer" convenience case.
3. **fail closed**: enforcement does not depend on the agent honouring proxy
   env or playing nice. a direct-to-IP `curl` must still fail.
4. **no decryption / no MITM**: host-level (sni/connect) granularity is
   sufficient and avoids a CA in the sandbox. https hides the path anyway, so
   path-level filtering is explicitly out of scope.
5. **works with the non-root agent uid** pats already uses (file ownership).
6. **egress audit**: log allowed + denied requests, surface denied attempts in
   the run metadata -- a built-in cheat detector.

## why the obvious options fall short

- **`--add-host h:0.0.0.0` (dns blackhole).** trivial, no caps. but denylist
  only (can't enumerate "everything else"), and leaky: bypassed by direct-IP,
  mirrors, gitlab, web.archive. not fail-closed. fine as a stopgap, not the
  design.
- **iptables allowlist (anthropic `init-firewall.sh`).** robust, default-deny,
  the right model. but needs `NET_ADMIN`+`NET_RAW` and applies as **root** at
  container start -- which collides with pats running the agent as a non-root
  uid. you'd need an image entrypoint that firewalls as root then drops to the
  uid (gosu), making firewall logic a hidden image/pats contract. workable but
  couples enforcement into every sandbox image.

## proposed design: proxy sidecar on an internal network

enforcement lives **outside** the agent container, so the agent stays non-root
with no caps and cannot bypass it.

```
        internal docker network (no gateway -> no internet)
   +-----------------------------+        +--------------------------+
   |  agent container            |        |  egress-proxy container  |
   |  - non-root uid             |  --->  |  - on internal net AND   |
   |  - HTTP(S)_PROXY=proxy:8080 |  CONNECT|    bridge net (internet) |
   |  - no direct route out      |        |  - allow/deny by sni     |
   +-----------------------------+        |  - logs every request    |
                                          +--------------------------+
                                                      |
                                                  internet
```

key properties:

- **fail closed by topology.** the agent container is attached to a docker
  `--internal` network -- no nat, no route to the internet. the *only* path out
  is the proxy. ignore the proxy env and curl direct -> no route -> fails. this
  is the property `--add-host` and tool-deny-lists lack.
- **sni allow/deny, no mitm.** for https the proxy sees the `CONNECT host:443`
  (and the tls sni); it allows or denies the tunnel by hostname without
  decrypting. for plain http it sees the host header. host granularity, zero
  cert handling.
- **dns via the proxy.** with `HTTP(S)_PROXY` set, the agent sends the hostname
  to the proxy and the proxy resolves it. the agent needs no resolver -> closes
  dns-based exfil and direct-IP tricks in one move.
- **non-root, no caps.** all privilege stays in how pats wires the networks;
  neither container needs `NET_ADMIN`.
- **audit for free.** the proxy logs allowed + denied. pats copies the denied
  list into the run metadata -> every run says whether the agent *tried* to
  reach a blocked host (e.g. the upstream answer). a cheat that's now blocked
  *and* recorded.

candidate proxy: `tinyproxy` (tiny, allow/deny by host) or `squid` (richer
acls, better logging). a small purpose-built go proxy is also viable and keeps
the dependency in-tree. pats would publish `ghcr.io/lczyk/pats/egress-proxy`.

## config schema

```yaml
sandboxes:
  - id: docker
    kind: container
    driver: docker
    image: mason-test/sandbox:26.04
    egress:
      mode: proxy          # open (today) | none (--network none) | proxy
      default: deny        # proxy mode: deny (allowlist) | allow (denylist)
      allow:               # reachable hosts when default: deny
        - api.anthropic.com
        - archive.ubuntu.com
        - ports.ubuntu.com
        - security.ubuntu.com
      # deny: [github.com, ...]   # blocked hosts when default: allow
```

- `mode: open` -- current behaviour (open network). the default.
- `mode: none` -- `docker run --network none`. full lockdown, zero deps. **only
  valid for agents that need no network at all** -- a local model or an adhoc
  compute task. a harness agent that calls a remote inference api is dead under
  `none` (it can't reach the api), so this mode does not apply to remote-api
  harness evals.
- `mode: proxy` -- the sidecar above. `default`+`allow`/`deny` set the policy.
  **the only viable mode for remote-api harness agents**: the inference host
  goes on the allowlist (e.g. `api.anthropic.com`) so the agent runs, while the
  answer source stays blocked. mirrors why anthropic's `init-firewall` allowlist
  always includes the api.

per-sandbox; tasks select a sandbox, so different tasks can have different
egress by pointing at different sandbox ids.

## phasing

note: `mode: none` does **not** help remote-api harness evals (no inference
access), so it is not a stepping stone to the mason path -- it is a separate
niche (local-model / adhoc). the mason path needs `mode: proxy` directly.

1. **mode off|none.** trivial: thread `egress.mode`; `none` adds `--network
   none` to the run args. small, self-contained -- covers offline/local-model
   and adhoc-compute evals. (does not unblock mason; see note above.)
2. **mode proxy.** the real feature and the mason path: pats creates an internal
   network + bridge network, starts the proxy sidecar with the rendered
   allow/deny (inference host on the allowlist), runs the agent container with
   `HTTP(S)_PROXY` + only the internal net, tears both down after. publish the
   proxy image. wire the allowed+denied audit into run metadata.
3. **denylist convenience + bwrap.** `default: allow` with `deny:`; bwrap
   egress via `--unshare-net` (mode none) now, slirp4netns+filter (mode proxy)
   later.

## extension: url-level filtering (`mode: mitm-proxy`)

status: implemented.

host granularity can't express "allow github, but not the `chisel-releases`
repo" -- the repo name lives in the https path, which a CONNECT tunnel never
sees (requirement 4 rules out decryption... for `mode: proxy`). when a task
legitimately needs *some* of a host but not all of it, there's a separate
opt-in mode: `mitm-proxy`, a superset of `proxy`.

- **selective decryption.** only hosts named by a `deny-urls` rule get their
  tls terminated; every other allowed host stays a blind tunnel. the inference
  api is never decrypted. to make the mitm set computable, url patterns are
  **host-anchored**: the part before the first `/` must be a literal hostname
  (a wildcard host would mitm everything -- rejected at validation).
- **per-run ephemeral CA.** pats generates a CA per pair; the key is mounted
  into the proxy sidecar only. the agent container gets the cert + a merged
  trust bundle (image roots + run CA) bind-mounted over
  `/etc/ssl/certs/ca-certificates.crt`, plus `SSL_CERT_FILE` /
  `REQUESTS_CA_BUNDLE` / `NODE_EXTRA_CA_CERTS` for tools that don't read the
  system bundle. a tool that still distrusts the CA fails its connection --
  fail closed, not bypassed.
- **audit gains urls.** mitm'd requests log the full url (allowed and denied);
  denied ones land in the run metadata like denied hosts.

```yaml
egress:
  mode: mitm-proxy
  default: deny
  allow: [archive.ubuntu.com, github.com, raw.githubusercontent.com]
  deny-urls:
    - "github.com/*/chisel-releases*"
    - "raw.githubusercontent.com/*/chisel-releases*"
```

`*` in a pattern matches anything, `/` included. rules are deny-only for now
(`allow-urls` under an allowlisting host would be the inverse -- add when a
task needs it).

known ceilings: mitm'd connections are http/1.1 only (the proxy declines h2 in
alpn; clients downgrade). cert-pinning tools break on mitm'd hosts -- that's
inherent, keep pinned hosts out of `deny-urls`. mirror leak remains: a copy of
the denied content on an un-ruled host isn't caught -- `default: deny` mostly
moots this since such hosts aren't allowed at all.

## open questions

- **proxy choice**: off-the-shelf (tinyproxy/squid) vs a small in-tree go
  proxy. in-tree = no extra image dep + easy audit-log format, but more code to
  own. lean in-tree for the audit integration.
- **apt over the proxy**: apt honours `Acquire::http(s)::Proxy` / `http_proxy`.
  ubuntu mirrors are the allowlist's main entries; confirm apt works cleanly
  through CONNECT for the https mirror and plain for http.
- **wildcards in allow/deny** (`*.ubuntu.com`)? sni matching makes this easy;
  worth supporting to avoid enumerating mirror cnames.
- **default posture for mason**: `deny` + allow `[api.anthropic.com,
  *.ubuntu.com]`. that's debs + inference, no upstream repo. revisit once the
  scorers exist.
- **teardown on ctrl+C**: the proxy + networks must be cleaned on interrupt
  (ties into the signal-cancel handling already added to the run phase).

## impact on existing code

- `config.Sandbox` gains an `Egress` struct.
- `sandbox.New`/`container.Run` learn `mode none` (one run arg) now; `mode
  proxy` becomes a small orchestrator (create networks, start proxy, run agent,
  collect audit, teardown).
- run metadata gains a `denied_egress` list per pair.
- back-compat: absent `egress` == `mode: open`, today's behaviour.
