# the host driver

status: implemented. chained egress landed first, so a host sandbox behind the machine's proxy chains through it (below).

## purpose and limits

every pats driver puts the agent in an isolated place: a container, an lxd instance, later bwrap. some runs have their
isolation outside pats, or need what isolation takes away:

- **the boundary is already there.** a throwaway ci runner, an lxd vm made for one run.
- **the task needs the host.** the runner's own lxd or docker, `/dev/kvm`, a gpu, attached hardware.
- **debugging a harness** with nothing between it and the machine.

the host driver runs the agent as a plain process on the machine pats runs on, macos or linux, as the invoking user,
with that user's access to everything. harnesses run with their permission prompts off, as in every sandbox, which
here means a tool-using agent with no prompts on the user's machine. it is dangerous by design, and the holes below
are accepted, not bugs to close: the key that turns it on says so. the driver promises ordinary host execution,
normal cancellation and an honest record of what ran. it does not promise:

- **isolation of any kind.** the agent can read and change whatever the user can.
- **crash cleanup.** processes are ended while pats runs (below). after pats is killed, the agent and anything it
  started may survive, and `pats clean` does not end host processes. whoever owns the machine owns its final
  teardown: the disposable runner or vm this driver is meant for.
- **a hermetic eval.** a fresh harness home keeps the user's own harness config out, but machine-wide settings,
  managed configuration, and instruction files in the workdir's ancestors (a `~/CLAUDE.md`) can still reach the
  agent's prompt, hooks and tools. a clean machine is the caller's to provide.
- **the user's logins untouched.** pats hands a harness the same credentials as under every driver (below), but a
  host harness can also reach the os's credential store, the macos keychain included, and may use or refresh the
  user's own login there.
- **pinned harness versions.** host tools are the caller's: pats records the versions it saw, and does not refuse
  others.
- **enforced egress.** egress is advisory (below).

## configuration

```yaml
sandboxes:
  kind: sandbox
  items:
    runner:
      driver: host
      sand-not-boxed: true   # required: the agent runs on this machine, as you, with no isolation
      egress:                # advisory under host: honoured through the proxy env, not enforced
        allow:
          - .ubuntu.com
```

- `driver: host` without `sand-not-boxed: true` is a load error that says why: "sandbox "runner": the host driver
  runs the agent on this machine, as you, with no isolation; set sand-not-boxed: true to accept that". the key is
  the acknowledgement, kept in the config so it shows in review. `sand-not-boxed` on any other driver is a load
  error.
- keys a host cannot honour are load errors, each naming the key, rather than ignored: `image`, `build`, `base`,
  `provision`, `limits`, `sudo` (sandbox or task), `vm`, `remote`, `profiles`, `config`, `egress: none`,
  `egress.image`.
- `env`, `max-parallel` and `egress` (as a mapping) are allowed.
- when provider pinning lands (`provider-pinning.md`), a pinned agent is a load error under host: the pin is the proxy
  rewriting inference requests, which a host agent can go around.

## execution

the host driver is a `Driver` (`sandbox-seams.md`) with no images, no daemon and no networks; its image methods say
there are none, and its `Runs` lists nothing. a session is a set of host dirs and the processes run in them:

- **start** holds the process's lease as every driver does (one per pats process, kept until `sandbox.Shutdown`)
  and, with `egress`, starts the proxy (below). the workdir and the harness home are the temp dirs pats already makes
  under its cache for every pair; nothing is bound.
- **exec** runs argv as a host process in its workdir, in a process group of its own, through the runner host steps
  use (`runGroup`, `steps.md`, extracted where both can share it): the group is killed when the context ends and when
  the command returns, and output draining is bounded. what leaves the group (`setsid`, a daemon, a nested tool's
  own groups) is out of reach, as for a host step. a context cancelled before the process started is a launch error
  (`ErrLaunch`); one that killed a started process returns the killed result (exit -1, no error), as the container
  drivers do, so the pair ends as it would under them: a deadline is a timeout with collect run, a ctrl-c is
  `interrupted` with no sync or collect. `Root` is not available: a `prepare` runs as the
  invoking user, like a `stage`.
- **push** and **pull** are host copies. **handover** and **sync** are no-ops. **close** ends the session's groups
  and its proxy, and is safe to call twice.
- `prepare`'s material goes to a dir in the harness home (`<home>/.pats-prepare`), which prepare removes before the
  agent runs, as it removes `/pats-prepare` elsewhere.

### the paths an agent is told

pats names in-sandbox paths in what it hands the agent: `PATS_WORKDIR` and the cwd (`/workspace`), `HOME`
(`/pats-home`), `PATS_PROMPT_FILE`, `prepare`'s material (`/pats-prepare`), the trust bundle (`/sbx-ca/bundle.pem`).
they are constants today, since every driver binds or pushes to them. under host each is the host path of the dir
behind it. the driver supplies these paths, every driver but host keeping today's constants, and one place resolves
them (`newHarness` and `prepareEnv` read them from the sandbox), so no other code learns which driver it is on.

a host path can hold anything a dir name may, so no path is spliced into shell source: `inPath` (`prepare.go`) takes
the prepare material's path as a positional argument and expands it quoted, instead of writing it into its `sh -c`
wrapper.

## harnesses

- **the env**: the agent gets the env pats builds and nothing inherited: the harness's own (`agent.Env`, the
  credentials `agent.CredEnv` forwards), `HOME` the temp harness home, the sandbox's `env`, the egress env when set.
  from the host it takes `PATH` (with relative and empty entries dropped, so the cli preflight finds is the one the
  pairs run), `LANG`, `LC_*`, `TERM` and `TMPDIR`. a host ssh agent socket or cloud credential in the env never
  reaches the agent by inheritance. this is hygiene, not isolation.
- **credentials**: as under every driver: the forwarded token env, and the credential file copied into the temp home
  (`agent.HostCredsFile`), with the harness pointed at that home (`HOME`, `CODEX_HOME`). nothing more; what a host
  harness finds in the os's credential store is the stated limit above.
- **the clis**: they run from that `PATH`. a harness the host lacks fails preflight with its name and an install
  hint. there is no image to stamp, so preflight asks each cli its version and the run records that in
  `metadata.json`'s `components`, with a warning when it differs from the pinned version (`harness-pins.md`): what
  preflight saw, not a guarantee of what every pair ran.

## advisory egress

with an `egress` mapping, a host session runs the existing proxy, `pats egress run`, as a child process on
`127.0.0.1:0`, waits for its `listening <addr>` line (bounded), and gives the agent the usual contract:
`HTTP(S)_PROXY` and the lowercase forms, the policy merged with the harness's hosts as for every driver. a start that
fails part way stops and reaps the child; close stops and reaps it; a proxy that dies mid-run fails the pair, saying
so. shutdown may lose a tunnel's last audit event, a limit the docs name. the policy, the audit, url and method
rules, `deny-response` and the dial guard work as under any driver for what goes through the proxy.

- **tls inspection** (`Policy.NeedsCA()`: a url entry, or `methods`) needs a trust bundle on the host, and pats
  does not build one from the system in v1: it needs `SSL_CERT_FILE` set, naming a bundle the user vouches for, and
  refuses such a policy with that instruction otherwise. that bundle, with the run's CA appended, is what the trust
  env (`SSL_CERT_FILE`, `CURL_CA_BUNDLE`, `GIT_SSL_CAINFO`, `REQUESTS_CA_BUNDLE`, `CARGO_HTTP_CAINFO`) points the
  agent's tools at, `NODE_EXTRA_CA_CERTS` the run CA alone, and the bundle alone is the proxy's upstream roots. a
  policy of hosts only needs nothing. the system trust store is never touched.
- **advisory**: what does not go through the proxy is not seen. an agent that unsets the env, opens a raw socket,
  or uses a tool that ignores the proxy env goes straight out and leaves no audit line, and an agent that drives the
  host's lxd or docker starts instances on networks no proxy env reaches. so the fail-closed self-test does not run
  (preflight says "egress is advisory under the host driver" instead of refusing), and the records below say so.

### behind a proxy

a machine that only gets out through a proxy of its own (a corporate one, a ci network's) sets `HTTP(S)_PROXY` for
everything it runs. under "nothing inherited" a host agent would lose that, its only way out, so when the host
sandbox sets no `egress` the host driver passes the machine's proxy env, as pats read it (`chained-egress.md`), through
to the agent: `HTTP_PROXY`, `HTTPS_PROXY`, the lowercase forms, `NO_PROXY`, and with `SSL_CERT_FILE` set the trust env
pointing at it. a host sandbox that sets `egress` runs its proxy with pats's own env, so the proxy chains through the
machine's as under every driver. an upstream on the host's loopback is fine here: a host agent and its proxy reach it
as pats does.

running pats inside a pats sandbox (an agent under test that runs pats itself) is not supported, under host or any
driver.

## records and the cli

- `metadata.json` gains `driver` on every pair, under every driver, and for host `sand_not_boxed: true`. its `egress`
  record gains `enforced`: `false` under host, `true` under every other driver, absent (unknown) on a pair from
  before the field.
- reports carry it: the single-run json pair and the multi-run json attempt gain `egress_enforced` (`null` when
  unknown), the judges section counts samples run with advisory egress, and both human reports (one run, `-N`) add a
  line naming the advisory pairs under the table, so an empty `denied_egress` there is not read as clean.
- `pats run` and `pats judge` print one warning before the first pair when the selection uses a host sandbox:
  `sandbox "runner" runs agents on this machine, unsandboxed (sand-not-boxed); egress is advisory`.
- `pats egress check` notes, beside its verdict on a host sandbox, that the verdict holds only for traffic that goes
  through the proxy.
- `pats init` has nothing to make for a host sandbox and says so in one line; it writes no pin, and `usePins`
  skips it.
- `pats clean` is unchanged: it sweeps temp dirs and leases as today, and does not look for host processes.

## later

- **trust bundles from the system**: linux's from the files go's root discovery uses; macos's from the keychain
  with its trust settings applied (a plain export loses "never trust"). until then `SSL_CERT_FILE` is required for
  tls inspection.
- **crash cleanup of host processes**: a supervisor leading each group, with a way for `pats clean` to end a dead
  run's groups safely. worth it if host runs on long-lived machines become common.
- **proxy shutdown guarantees** (joining tunnel copies, cancelling pending dials and refusal waits): general proxy
  work, for every driver.

## not doing

- a network-namespaced host mode (host files, enforced egress): bwrap's job, linux only. host stays the same on
  macos and linux.
- checks that the host is clean (instruction files, managed settings, credential stores): a hermetic machine is the
  caller's.
- protecting the user's files or logins from the agent: there is nothing to protect them with; that is what the key
  says.

## tests

- validation: each refused key, the missing key, the key on another driver.
- paths: an agent under host is told host paths; a host prepare runs with `$`, a backtick and a quote in the temp
  path.
- env: a canary var in pats's env never reaches the agent; `PATH` does, without relative entries.
- execution: a background child of the agent dies with the pair and on ctrl-c; a cancel before start is an error
  with nothing collected; a deadline after start is a timeout with collect run.
- advisory egress: a host run through the proxy with a deny entry leaves the audit line, `enforced: false` in
  metadata, and the report lines; a curl with `--noproxy '*'` goes out with no audit line, the documented limit; a
  `NeedsCA()` policy without `SSL_CERT_FILE` is refused; the proxy child is reaped on a failed start and on close.
- behind a proxy: the machine's proxy env passes through when the sandbox sets no egress; a loopback upstream is not
  refused under host.
