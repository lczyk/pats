# transient lxd errors

status: implemented.

## problem

the lxd driver reaches its server through the `lxc` cli, often over a network (a laptop to a lab box). when that link
drops a connection, the call fails and nothing tries it again, except two narrow cases: a push whose remote tar fails or
arrives short (`pushTries`, `src/sandbox/lxd_files.go`, which also retries a tar that failed for a full disk), and an
exec whose env script arrives short (`envTries`, `src/sandbox/envflags.go`). one day of mason's rock runs against
armstrong cost:

- **finished work thrown away.** two pairs failed at `sync: pull /workspace: tar exited 1: Error: read tcp ...:8443:
  read: connection reset by peer`. the agents had finished, and the pair scored `error`.
- **a whole run, twice.** a transient failure at credential preflight (`sandbox never launched: lxc init ...`) ends
  the run with exit 1 before any pair starts (10 pairs and 5 pairs lost). preflight has no retry and no log line per
  attempt, so a slow one looks like nothing happening (19 min from run start to the first sandbox, with no line saying
  why).
- **a leaked sandbox.** a pair's cleanup gave up on `pats-work-1066874698` after 5 min with `a server operation on it,
  or the query for them, outlived 5m0s`. `deleteInstance` (`src/sandbox/lxd.go`) takes a failed operations query as
  busy and, while busy, drops the delete's own error, so a server it cannot reach looks the same as an instance with an
  operation running. nothing tries again before the run ends, and the pair's bridge and acl then fail as "in use". a
  repro on armstrong showed lxd itself does not hold a vm against `delete --force` (it goes in about 1s with an exec
  running, and a client killed mid-exec leaves no operation), so the link is the likely cause, though the server's log
  could not be read to confirm it.
- **a pair scored as the agent's fault.** one pair ended `nonzero` although the agent's output said the work was
  done: the exec's stream broke, not the agent. this one is not solved here (see not in scope).

## the space

what a call does decides whether a try again is safe:

kind | examples | try again
---|---|---
read | `query`, `list`, `show`, `info`, `remote get-default` | yes
idempotent write | `delete --force`, `config set`, `network acl set`, a network or acl delete | yes, a "not found" after a retry is success
read over exec | `Pull` (`tar -c` into a fresh temp dir, `syncBind`) | yes, each try into a new dir: the instance's side only reads
write over exec | `push` (tar into the instance) | no: a lost stream does not mean the remote tar stopped, and a second one can overlap it
create | `lxc init`, a network or acl create | only after the half-made thing is found and removed, or adopted
publish | `lxc publish` | no: the image has no name to find it by until its alias is set
the agent's exec | the harness run | never: the agent may have acted, and may still be running

and what a failure is:

- **transient**: the client could not talk to the server, or lost it mid-call. the text `lxc` prints for it:
  `connection reset by peer`, `broken pipe`, `connection refused`, `i/o timeout`, `no route to host`, `TLS handshake
  timeout`, `unexpected EOF`, a websocket closed abnormally (`close 1006`). the exit code says nothing (it is 1 for
  everything).
- **not transient**: everything else, including "not found", "already exists", "in use", quota and permission errors.
  trying again changes none of them.

the text is only evidence when nothing but `lxc` can have printed it. an exec's client error does not come back as an
error: `exitResult` (`src/sandbox/lxd_session.go`) turns `lxc exec`'s exit into a code, and the client's diagnostic
lands in the same stderr as the remote command's. so the text counts on:

- a non-exec call (`query`, `init`, `delete`, a network or acl call): its output is `lxc`'s alone.
- an exec of pats's own `tar` (`Pull`, `push`), on one line only: the last line of its stderr, when it starts with
  `Error: `, the prefix `lxc` gives its own failures. tar's own diagnostics start `tar: ` and may carry names the
  instance chose (a file called `connection reset by peer`), so they are never read. a name cannot forge the `lxc`
  line: tar escapes a newline in a name (`\n`), so no name starts a line of its own.

and on nothing else. a prepare step, a hand-over script, a harness or an agent may print the same words about its own
upstream, so a match in their output means nothing, and their failures are never `ErrPlace`.

## proposal

### one classifier, one sentinel

`transientLXD(err) bool` in the lxd driver matches the texts above on the error of an `lxc` call pats made for itself,
case-folded. it is the only place the list lives. a text it misses is a failure as today, so the list errs short: a new
text is added when a run hits it.

an `lxc` call that fails transiently, by the rule above, returns an error wrapping a new sentinel,
`sandbox.ErrPlace`: "the place failed, not what ran in it", whether the call is retried (after its last try) or not (a
single `init`). the error records whether its call was already retried (`placeErr{tries}`), so an outer layer can
tell a failure it may retry from one an inner layer has already used up. only the classifier makes one. a non-transient failure, a full disk under `push` included, never does. `ErrLaunch` stays what it is (a start or an exec that
never ran, for any reason, a bad config included): it is neither retried nor scored as the place's on its own.

### retries by call kind

a helper, `retry(ctx, what, fn)`, tries `fn` up to `lxdTries` (3) times while its error is transient, with a backoff of
2s then 5s, on the process clock (tests fake it), and stops at once when `ctx` ends. after the last try, or when `ctx` ends
during a backoff, the error wraps `ErrPlace` and the last cause (joined with `ctx`'s error in the second case, so a
deadline met while retrying still reads as the place's), and says how many tries there were:

```
lxc query armstrong:/1.0/operations ...: connection reset by peer (3 tries, transient)
```

where it goes:

- **reads and idempotent writes**: `l.lxc` gains a sibling, `l.lxcIdem`, used at every call site that is a read or an
  idempotent write. `l.query` uses it. the choice is the caller's, by name: the wrapper cannot tell `init` from `list`
  by itself in a way a reviewer can check.
- **`Pull`**: `Pull` keeps both causes, the exec's and `readTar`'s, and returns the exec's when it is transient (a
  stream cut mid-entry makes `readTar` fail too, with a text that hides the reset). the retry is around `syncBind`'s
  pull, each try into a new temp dir, so a cut stream never leaves a mix of two tries. a `readTar` failure with a clean
  exec (a refused path, a full local disk) is not retried. `Sync` keeps `syncTimeout` for all tries together.
- **`push`**: unchanged. its retry in place (`pushTries`) is safe as it is: it fires only on exit 120 or 121, which
  the push script itself returns once its tar has ended, so the remote side is known to be done. a lost stream
  reaches pats as `lxc`'s own exit (not 120 or 121) and is not retried in place. a push that fails transiently fails
  what it was part of, with `ErrPlace`: at start that is the create's to retry (below), on a fresh instance, so no
  two tars ever meet in one.
- **server setup**: `prepareServer` caches `setupServer`'s result in a `sync.Once`, a failure included, so one
  transient failure of the marker create fails every later start in the process. a failed setup is not kept: the
  next caller tries it again, under the same lock, so concurrent starts make one try at a time and all see a success.
  its calls are reads, idempotent writes and creates by name, so it runs under the helper. a create by name that
  meets "already exists" on a retry is the earlier try's, landed with its answer lost: the marker's name carries the
  run's token, so one there is this run's, and the project is shared by every run, so one there is the one wanted. a
  setup whose create and the read after it both fail transiently ends that start with `ErrPlace`, and the next start
  begins again with the read. such a setup does not delete the marker on its way out, as `setupServer`'s cleanup does
  today for one it made: whether it made it is not known, and a marker there may be a live setup's under another remote
  name for the same server. a failed setup leaves the marker in place in every case, a marker it is known to have made included:
  another remote name for the same server may have adopted it since. the marker of a run that never got set up has
  no resources beside it and a lease that is never renewed, so `pats clean` takes it once the lease is stale, and a
  later setup of the same run renews it.
- **create**: `Start` retries as a whole, up to `lxdTries`, when what failed carries an `ErrPlace` of a single try
  (an `init`, a network or acl create, an exec of pats's own `tar`, or any other write `Start` makes on what it has
just made: `config device add`, `network acl rule add`, `lxc start`, the egress setup's calls). none of those is
retried in place, since their answer may be lost after they took effect. the start's retry removes what the failed
try made first, which takes all of them with it, so the next try begins from nothing: both instances (the sandbox and,
under an `egress:` policy, its proxy, `<name>-p`), each by `deleteInstance` and each confirmed gone. the bridge and
acl are not reused: a network or acl create whose answer was lost can still land after a delete, as an `init` can,
and lxd's operations do not name it the way they name an instance. so each try makes its bridge and acl under new
names: the name's derivation (`s.bridge`, from the instance name and the run token, `src/sandbox/lxd.go`) takes the
try's number too, and stays within the 15 characters of an interface name, and the failed try's are deleted once, best effort, and go on the deferred deletes (below) when that fails or
when they may still land. they carry the run's token, so `pats clean` takes any that land later. a removal of either
instance that gives up ends the start there, as above.

a start's retry covers what the pair owns: its instances, bridge and acl, and their config. an image build a start
sets off (the proxy image under `startProxy`, built and published when no cached one fits) is not the pair's: it ends
in a `publish`, which is never repeated, and its result is shared. a transient failure inside a build ends the start
with an `ErrPlace` of used-up tries (no start retry). the build's key is then marked doubtful in the process: a
`publish` whose answer was lost may still be running, and the cache can look empty until its alias lands. the next
build of that key in the process first waits out the builder's operations (`instanceBusy` on the builder's name, as
`deleteInstance` does, under `instanceGone`), then reconciles: the build publishes under a run-private alias
(`pats-img-<key>-<token>`) and claims the shared one only once `publish` has answered, so a lost answer leaves the
image, if it landed, under the private alias alone. the reconcile looks for that alias, and claims it as the build
would have when it is there; it builds again only when both lookups answered "not found" (a lookup
that fails, after its tries, is not an absent image: the start fails with `ErrPlace` and the key stays doubtful, so
the link going down here cannot hide a published image), and only after the old builder is removed by
`deleteInstance` and confirmed gone (a build's cleanup may have given up and left it idle). a removal that gives up
fails the start the same way. while the key is doubtful, the failed
build's own cleanup does not remove the private alias. a wait that gives up fails that start too, with `ErrPlace`,
and leaves the key doubtful.

the same "not found, or not known" rule holds for every cache lookup, not only a doubtful key's: the image lookup
(`src/sandbox/lxd_image.go`) today takes any failed `fingerprint` lookup as a miss and builds. it builds only on a
"not found"; a lookup that fails after its tries ends the start with `ErrPlace`, so a link that comes back does not
publish over an image that was there. the cache's import fallback (`importCached`, `src/sandbox/lxd_cache.go`) holds
to it too: its check of the shared alias after a failed import passes a lookup that failed on, rather than dropping
it, and `image` builds after a failed import only when that check answered "not found".

the reconcile covers the builds a start sets off, which never force. a forced build (`pats init --force`) replaces a
shared alias that is there on purpose, so "neither alias is there" cannot be its test: a forced build that fails is
`init`'s failure, as today, and the user runs it again. it sets no doubtful mark, and none survives the process.

another process does not see the mark: its builder and private alias carry its own token, so it may build and publish
the same key meanwhile. that is the cost of a duplicate image, not a conflict: the two builds share no instance or
alias, the shared alias ends on one of them, and the other is a run's private image, which `pats clean` already
takes once unused (`unusedFor`). accepted, as today, when two runs build one key at once. an `ErrPlace` whose call already had its tries is
  not retried by the start: the link has stayed down through three tries and their backoff, and a new instance would
  meet it too. before a retry it
  removes what the failed try made: the instance by `deleteInstance` under a bound of its own (`launchCleanup`, 1 min),
  its bridge and acl after it. the retry goes ahead only when that removal succeeded: `deleteInstance` saw no operation
  on the instance and the delete said it is gone. a removal that gave up (a server it cannot reach, an operation still
  running) ends the start there with `ErrPlace`, since an `init` cut short can still land after a delete, and a tar of
  the failed try may still run. a bridge or acl create that meets "already exists" adopts it when its `user.pats.run`
  is this run's, else fails as now. the tries run under the caller's context (a pair's `setupTimeout`, which also
  covers hand-over and prepare), with no deadline of their own, and each try logs a line (below).
- **setup steps that are not pats's**: a prepare step or a hand-over script runs the suite's command, whose output
  is not evidence (above). not retried, and not classified.
- **the agent's exec**: not retried, and not classified: its exit reaches pats as a code with the harness's own
  stderr, so a lost stream and an agent that failed look the same (see not in scope).

### delete: say why, and try again at the end

- `deleteInstance` takes its context from the caller (a pair's cleanup gives it `instanceGone`, shutdown and the
  create retry their own bounds), keeps the last delete error and the last query error, and the give-up message names
  them: `... outlived 5m0s (last delete: <err>; last operations query: <err>)`, or `(busy: <operation description>)`
  when a running operation named it. so the next leak says which of the two it was.
- an instance whose cleanup gave up goes on the process's list of deferred deletes together with its pair's bridge and
  acl, whatever their own deletes said ("in use" included: it clears once the instance is gone). `shutdown` tries each
  once more, in that order, the whole pass bounded by `cliCleanup`, before the marker goes. a link that is back by then
  leaves nothing for `pats clean`. what is still there stays for clean, as now, and the marker goes as usual: clean
  finds the run by its resources.

### preflight

preflight gets no retry of its own. its sandbox's `Start` retries as above, and the harness run after it is never tried
again, whatever its output says: a preflight that ran is the answer. a start that fails after its tries fails preflight
as now, its error naming the tries.

each try logs a line at info, so a slow preflight says what it waits on:

```
preflight: claude-haiku: starting sandbox
preflight: claude-haiku: start failed (lxc init ...: connection reset by peer), trying again in 5s (try 2/3)
preflight: claude-haiku: sandbox up (41.2s), running the harness
```

the same start lines go to a pair's log, at detail.

### the infra status

a pair whose failure is the place's ends with status `infra`, so reports and means skip it the way they skip `error`
today, and a rerun of what is missing (a later spec) can find it. `infra` is a pair whose error chain carries
`ErrPlace`, at any step: start (after its tries), sync (after its tries), or an `lxc` call pats makes for itself
between them (a push at hand-over, a query). a suite's own step and the agent's exec never give one.

the order against the agent's own outcomes: a `timeout` or a budget stop is the agent's result, and stays it. a sync
that then fails transiently is appended to its error, as a sync error is today, not made `infra`. an interrupt of the
run wins over everything, as now. `metadata.json` gets `status: "infra"` and its cause in `error`. the multi-run report
counts `infra` beside `error`. the run loop's own count of pairs that came back ok (`back` in
`internal/eval/run.go`, which counts every status but `error`) leaves `infra` out too, so a run of nothing but
`infra` does not read as one that did work. the dry run's history of pair durations (`internal/eval/dryrun.go`,
which drops `error` and empty statuses) drops `infra` too, so a short infrastructure failure does not lower an
estimate. every other place that lists statuses one by one gets `infra` beside `error`: the run's closing tally
(`internal/eval/run.go`), `pats list`'s (`internal/eval/list.go`), and the single-run report's marks
(`internal/eval/report.go`), with a mark of its own.

## not in scope

- other drivers: docker and podman talk to a local daemon, and none of the failures above were seen there. the
  classifier is lxd's alone until one is.
- retrying the agent's exec, or resuming one: a fresh pair is the only safe repeat, and that is the rerun spec's.
- telling a lost agent stream from an agent that failed. `exitResult` (`src/sandbox/lxd_session.go`) turns `lxc
  exec`'s exit into the agent's code, and stderr is the harness's. a record of the agent's real exit would need a
  supervisor as root in the instance (the harness runs as `pats`), a bounded wait for an agent that runs on after the
  stream is gone (a task may have no timeout), and an answer for the output and usage lost with the stream (budgets are
  enforced from it). a spec of its own, if the case comes back.
- a push retried in place: it would need the remote tar's end confirmed first (an operation gone, a pid file). a push
  that fails is a start's to retry, on a fresh instance.
- a health check of the link before a run, or a circuit breaker across pairs: the tries are bounded, and a link down
  for minutes fails the pair as `infra`, which is the honest answer.

### who retries what

each failure is retried at one layer, the innermost that can:

- a read or idempotent write: at the call, `lxdTries`.
- a push's remote tar that failed or arrived short (exit 120 or 121): `pushTries`, as now. the tar's failure is not
  `ErrPlace`, so a full disk fails the start once it is out of those, with no start retry.
- an env script that arrived short: `envTries`, as now. not `ErrPlace` either.
- a transient create, or a transient exec of pats's own `tar`, in one try: the start, `lxdTries`.
- a read or write whose tries ran out: no one. it ends the start, or the step it was part of, as `ErrPlace`.

so each failure is tried at most `lxdTries` times, by one layer, and the worst a start costs is three creates, all
under `setupTimeout`.

## decided

1. three tries with 2s and 5s backoff. the error says how many tries there were, so later runs show whether it needs
   widening.
2. idempotent calls are marked one by one with `l.lxcIdem` at each call site, not through a list of verbs in `l.lxc`.
   the diff is longer but each site is reviewed on its own.

## tests

- the classifier: each text above, and a "not found" and an "already exists", against a table. a transient text in a
  harness's output is not matched.
- the helper, on a fake clock: stops after three transient errors, at once on a non-transient one, and when `ctx` ends
  mid-backoff; the last error wraps `ErrPlace`.
- `Pull`: an exec reset with a `readTar` error returns the reset; a `readTar` error with a clean exec is not retried.
- `deleteInstance` on the fake runner: a query and a delete that fail transiently for the whole window give the
  message with both errors; a shutdown after the link is back deletes the instance, then its bridge and acl.
- server setup: a transient marker create fails one start and the next start sets the server up; concurrent starts
  make one try at a time.
- create: a transient `init` failure removes the instance and tries again; a read that ran out of tries is not
  retried by the start; a removal that gives up ends the start
  with no further try; a bridge that exists with this run's token is adopted, one with another's is an error; a full
  disk under `push` is not retried by the start; a deadline met in a backoff still wraps `ErrPlace`.
- preflight: a start that fails twice then works passes with its lines; a harness that fails is run once.
- the agent's exec: a reset text in its stderr leaves the pair `nonzero`, as the agent's. a prepare step's likewise
  stays `error`.
- the run loop: a run of `infra` pairs counts none as ok. the dry run's estimate leaves `infra` pairs out.
- the tar line: a tar diagnostic naming a file `connection reset by peer` is not transient; an `Error: ...: connection
  reset by peer` last line is.
- server setup: a marker create whose answer is lost and whose retry meets "already exists" goes on as set up; one
  whose create and read both fail leaves the marker in place.
- create: a transient `config device add` or `lxc start` retries the start from a removed instance, not in place. a
  transient failure at the proxy instance's start removes both instances before the retry; a proxy instance whose
  removal gives up ends the start.
- server setup: a setup that fails after its marker create answered leaves the marker.
- create: a retried start makes its bridge and acl under new names; the old ones go on the deferred deletes.
- create: a transient failure inside the proxy image build ends the start without a retry, and the next build of
  that key waits for the builder's operations and reads the cache before building.
- the bridge name: two tries of one pair give two names, each within 15 characters.
- the reconcile: a doubtful key whose private alias is there is claimed, not built again; one with neither alias is
  built after its old builder is removed; a lookup that fails keeps the key doubtful and builds nothing; a builder
  whose removal gives up stops the build.
- the cache lookup: a lookup that fails after its tries builds nothing and fails the start; a "not found" builds.
  a failed import whose alias check also fails builds nothing either.
- a forced build that fails leaves no doubtful mark.
- the tallies: an `infra` pair shows in the run's tally, `pats list` and the report with its own mark.
- the status: `ErrPlace` at start, at hand-over and at sync gives `infra`; a timeout then a failed sync stays `timeout`.
- e2e (manual, armstrong): point a remote at a local tcp proxy in front of armstrong's 8443 that resets its
  connections on demand, reset mid-pull and mid-init, and see the pull retried, the start retried, and the pair `ok`.

## phases

1. the classifier, `ErrPlace`, the helper, reads and idempotent writes, `Pull`. the delete message, the deferred
   deletes and the shutdown pass.
2. create retries under one deadline, and the start lines (preflight's included).
3. the `infra` status in `metadata.json` and the reports.
