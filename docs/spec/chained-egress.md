# chained egress

status: implemented, for docker, podman and lxd. the host driver and bwrap pick it up as they land.

## problem

some machines only reach the internet through a proxy of their own: a corporate one, a ci network's. they say so
the usual way, `HTTPS_PROXY` / `HTTP_PROXY` (and lowercase) in the environment. pats's egress proxy dials the
internet directly, so on such a machine every sandbox with an `egress` policy fails: its proxy cannot reach anything.
a sandbox with no `egress` fails too, since its agent has no proxy env and no direct route.

chained egress makes pats work behind a proxy: pats's own proxy reaches the internet through the machine's, and an
open sandbox's agent is handed the machine's proxy. pats's policy still applies first: behind a proxy a sandbox is
held to pats's rules, then to the machine's. chaining changes the route, not the decisions.

v1 covers the drivers that exist: docker, podman, lxd. the host driver (`host-driver.md`) and bwrap pick it up when
they land, reusing this.

## the contract

- the upstream comes from pats's own environment at start, read the way go's `net/http` reads it
  (`golang.org/x/net/http/httpproxy`): `HTTPS_PROXY` for https, `HTTP_PROXY` for plain http, the lowercase forms,
  `NO_PROXY` / `no_proxy`. there is no config key and no flag: the machine already says where its proxy is, every
  other tool reads it from the same place, and so does the standalone `pats egress run`.
- an upstream is `http://[user:pass@]host:port`. basic auth is the one proxy auth pats speaks; an ntlm or kerberos
  proxy is reached through a local helper (cntlm, px) listening where the sandboxes can reach it. any other scheme
  (`https://`, `socks5://`) is a start error naming it, never a silent direct dial. `ALL_PROXY` / `all_proxy` are
  ignored, as go's `net/http` ignores them: set for some other tool, they neither chain pats nor stop it.
- the credential in the url never reaches a command line, a log or a record: every message naming the upstream
  redacts it.
- the start says when chaining is on, one line before the first pair: `egress via upstream
  http://proxy.corp:3128` (and the http upstream when it differs), so a run behind a proxy is never a surprise.
  `pats egress check` adds to its verdict whether the url would go through the upstream or direct (`NO_PROXY`).

## routing

the proxy makes every upstream connection through `Proxy.Dial` or, for plain http, its transport. behind a proxy each
gets a second implementation, chosen per destination by the same `httpproxy` rules (`NO_PROXY` matches dial
directly, as today):

- **tunnels and tls inspection**: the dial connects to the upstream and sends `CONNECT host:port`, with
  `Proxy-Authorization` when the url has credentials. a 2xx hands the connection back as if dialled directly, so the
  tunnel splice, the sni binding check and the mitm transport's tls to the origin work unchanged. any other status is
  a failed upstream, audited with the reason `upstream: <status>`, and `readEgressAudit` (`run.go`), which counts only
  `dial` and `revoked` as failures today, counts `upstream:` reasons too, so they land in `failed_egress`, apart from
  pats's own denials. there is no fallback to a direct dial.
- **plain http**: forwarded to the upstream in absolute form (`GET http://host/path`), not by CONNECT, since proxies
  commonly refuse CONNECT to port 80.
- **the dial guard**: it still checks literal destination addresses, and every direct dial `NO_PROXY` selects. a
  proxied name is resolved by the upstream, so pats cannot know which address it reaches: for those, destination
  address checks are the upstream's, a limit the docs state. the configured upstream's own address is exempt from
  the guard: a proxy on the lan is the point.

## delivery

- **the proxy**: the docker and podman sidecar take the upstream as `PATS_EGRESS_UPSTREAM_HTTPS`,
  `PATS_EGRESS_UPSTREAM_HTTP` and `PATS_EGRESS_NO_PROXY`, pats's own keys, which ride in the cli's environment
  (`envFlags`) rather than as `KEY=value` arguments the way `HTTPS_PROXY` would (`cliReads`). lxd's proxy instance
  takes them through its exec env on stdin, as every exec does. these keys are pats's internal protocol, not a user
  setting.
- **open sandboxes**: a sandbox with no `egress` gets the machine's proxy env, `HTTP_PROXY`, `HTTPS_PROXY`, their
  lowercase forms, `NO_PROXY` and `no_proxy`, on every exec of the session, the agent's and `prepare`'s among them,
  by the env-on-stdin path (`envScript`). not on lxd's file-transfer execs, which carry their env as
  `--env KEY=value` since stdin holds the file stream (`execArgs`): those need no proxy, and a credential must not
  ride on their command line. under lxd the open bridge's acl rejects private ranges, so the upstream's addresses are
  cut out of its reject rule: a literal as is, a name resolved on pats's host at start (a remote server that resolves
  it elsewhere is a limit: the open self-test then fails, naming the upstream).
- **open preflight**: the open-network probe (`reachesInternet`, `probes.go`) checks a direct route by `/dev/tcp`,
  which a proxy-only machine never has. behind a proxy the same probe dials the upstream's `host:port` instead,
  from inside the sandbox, and a failure is the start error below. a sandbox with a policy gets pats's proxy env as
  today, and the machine's `NO_PROXY` applies on pats's leg to the upstream, not the agent's.
- **podman's own copy**: podman copies the host's proxy variables into every container by default
  (`--http-proxy=true`), which would hand a policy sandbox's agent the machine's `NO_PROXY` beside pats's proxy. pats
  starts podman's agent and sidecar containers with `--http-proxy=false`. the docker cli does the same from a
  `proxies` block in `~/.docker/config.json`, for each variable the container does not set itself, so under docker
  pats sets every proxy variable on its containers explicitly, an empty `NO_PROXY` / `no_proxy` included where the
  machine's must not apply. the proxy env each gets is then the one described here, under either.
- **reachability**: the upstream must be reachable from the network the proxy dials from: the docker or podman
  bridge, the lxd uplink (a remote server's, under `remote`), and for an open sandbox the sandbox's own. an upstream
  on the host's loopback (`127.0.0.1`, `localhost`, `::1`) is not, and is a start error that says how out: "the
  upstream http://127.0.0.1:3128 is on this host's loopback, which a docker sandbox cannot reach; bind the proxy
  (cntlm, px) to an address the sandbox can reach, or run without it: env -u HTTPS_PROXY -u https_proxy -u
  HTTP_PROXY -u http_proxy pats ...". for anything else the check is a tcp dial, where the upstream is used: pats's
  proxy dials the upstream's `host:port` at start (bounded, a few seconds) and prints `listening` only once it
  connects, else exits naming the upstream and the network it could not reach it from, which pats's readiness wait
  reports as a start error. this proves a route, not the credential or the upstream's policy: a 407 or a refused
  CONNECT still shows at first use, as `upstream: <status>`.

## trust

a proxy that terminates tls presents certificates from its own CA, so the agent's tools and pats's proxy must trust
it. when pats runs behind a proxy with `SSL_CERT_FILE` set, that file's certificates are the machine's trust for the
run:

- they join the proxy's upstream roots.
- they reach every sandbox behind the proxy, by the material path the mitm bundle uses today (bound under docker and
  podman, pushed under lxd), with the trust env pointing at the merged bundle (the image's roots, the run CA under
  inspection, and these). that now includes open sandboxes and policies with no tls inspection, which get no bundle
  today: a host name alone still reaches the upstream's certificate, and the agent must trust it.

with no `SSL_CERT_FILE`, trust is today's, and an inspecting upstream fails verification until the user points
`SSL_CERT_FILE` at its CA.

## boundaries

- `pats init` pulling images through the proxy is the docker daemon's or the lxd server's own proxy setting, not
  pats's. a `build:` dockerfile or a `provision:` script gets neither the machine's proxy env nor its trust bundle
  from pats: one that fetches behind a proxy sets up its own proxy and ca.
- pats's own fetches (the lxd proxy binary, the openrouter model check, docker and podman's upstream image checks)
  use go's default transport, which already reads the same env. lxd's image checks go through `lxc`
  (`remoteFingerprint`), which follows its own proxy settings.
- the host driver's refusal of `egress` behind a proxy goes when it implements this.

## later

- the upstream in `metadata.json` (redacted), for provenance.
- checking the credential at start: it takes a CONNECT through the upstream to some host, which picks a destination
  on the user's behalf.

## not doing

- `https://` and socks upstreams, proxy auth beyond basic, pac files.
- a loopback upstream under the container drivers: reaching the host's loopback from a container differs by driver
  and platform; a helper listening on the bridge, or the host driver, is the answer.
- a config key or flag for the upstream: the env is the one place it is set.
- resolving proxied names locally to guard them: it would not establish which address the upstream connects to.

## tests

- the proxy against a fake upstream that records what it is sent: a tunnel by CONNECT with basic auth, a mitm'd
  request whose origin tls goes through CONNECT, plain http in absolute form, a `NO_PROXY` host dialled directly, a
  407 audited as a failed upstream with no direct retry, a policy denial never reaching the upstream.
- the guard: a literal private destination refused; the upstream's own private address reached.
- trust: an upstream terminating tls with a CA present only in `SSL_CERT_FILE` works for the proxy and for an agent
  in an open sandbox and in a hosts-only one.
- delivery: the sidecar's argv carries no credential; an lxd proxy instance gets the upstream; a loopback upstream
  under docker is refused with the `env -u` hint; an open sandbox's agent gets the proxy env; an unsupported scheme
  is a start error; `ALL_PROXY` alone changes nothing.
- reachability: an upstream that refuses connections fails the start, from the proxy for a policy sandbox and from
  the probe for an open one, naming the upstream; the start line names the upstream with its credential redacted;
  `pats egress check` says upstream or direct for a `NO_PROXY` host.
