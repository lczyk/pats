# steps in a sandbox

status: proposed. it depends on the build-tag seams (a spec of its own, to come) for `nohost`, and `docs/spec/snap.md`
moves from classic to strict confinement with it.

## problem

the host steps (`stage`, `prompt-cmd`, `collect` on a task or an agent scorer, `score` on a script scorer) run on the
machine pats runs on, as the user, with the host's shell, the host's tools and the host's whole environment
(`internal/eval/step.go`). so what a step can do depends on the machine. mason's `stage` needs `git`, and its scorers
need `python3` and pyyaml. it works on a laptop and a ci runner that have those, and fails anywhere else with `git:
not found`.

two things make that worse:

- **a strict snap**, which `docs/spec/snap.md` is being reworked into, has no host to run a step on. pats sees
  core24's root and what the snap ships, not the host's `/usr`, so a step there is a step on a machine nobody chose.
  guessing a toolset to ship would grow forever and still miss the first project that scores with `node` or `uv`.
- **a score is not reproducible** when it depends on whatever python the runner had.

## the rule

a task or a scorer can name a sandbox its host steps run in:

```yaml
sandbox:
  kind: sandbox
  items:
    tools:
      driver: docker
      base: ubuntu:26.04
      provision: provision-tools   # git, python3, python3-yaml
    agent-box:
      driver: docker
      egress:
        deny: [github.com/canonical/chisel-releases*]

tasks:
  kind: task
  defaults:
    step-sandbox: tools
  items: ...

scorers:
  kind: scorer
  defaults:
    step-sandbox: tools
  items: ...
```

- **`step-sandbox`** is a field on a task and on a scorer. it names an item of the `sandbox` vector, as an agent's
  `sandbox` does, and it takes `defaults` like any other field. a task in a `dir` vector, which takes no
  `defaults`, sets it in its own `task.yaml`, so that file stays self-contained. on a script scorer it places
  `score`. on an agent scorer it places the scorer's `stage`, `prompt-cmd` and `collect`, which reach `runPair`
  through `Scorer.Task()` (`internal/config/config.go`, `internal/eval/judge.go`), so the judge's task carries it.
  the judge agent itself runs in its agent's `sandbox`, as today. a name that resolves to no sandbox is a load
  error.
- **unset, a step runs on the host, as today.** the native build keeps the behaviour it has. no host-driver sandbox
  is involved and no `sand-not-boxed` is asked for.
- **a `step-sandbox` that names a `host`-driver sandbox is a load error**, saying to leave it unset. one way to run a
  step on the host, not two.
- **a `nohost` build** (the strict snap) has no host execution at all: the build tag leaves out the host driver and,
  with it, host steps. pats runs no user script in its own environment there. a task or scorer that has a host step
  and no `step-sandbox` is a load error there, one line per item and step, with no example or advice: `task
  "knockout/python3-minimal": stage needs a step-sandbox: this build runs no host steps`. there is no default step
  sandbox. pats's own work (the proxy child, pins, the report) is not a step and is unaffected.
- **the step sandbox is an ordinary sandbox.**
  - `pats init` pins it as it pins an agent's, and a run or a score refuses an unpinned one.
  - `pats clean` sweeps it under the lease of the run or score that started it.
  - its `egress:`, `env:`, `limits:` and `sudo:` apply to the steps that run in it.
  - it can be the same item as an agent's sandbox, which is then also its egress policy (below).
- **`max-parallel` bounds a sandbox's agents only.** a step box takes no slot (below). so `max-parallel` on a sandbox
  that only steps name is a load error: it would bound nothing.

## network

a step box uses its own sandbox's `egress:`, never the agent's, and never a task's `egress:` additions, which refine
the agent's policy only. the steps are the eval's trusted code: mason's `stage` clones the very repo its agent's policy
denies as the answer. a step sandbox with no `egress:` is open. one with a policy runs its own proxy and audit, one
per box, never reused across boxes (the proxies create their audit file fresh):

- a pair's before box writes `stage-egress.log` and its after box `collect-egress.log`. each is written in the box's
  staging dir (below) while the box runs, and moved into the pair's output dir after the box closes and its proxy
  drains.
- a scorer box writes its audit in its own staging dir, outside the run dir, since another box may be pushing that
  run dir meanwhile. once every scorer box of the run has closed and its proxy drained, the audits of each step
  sandbox are concatenated, box by box, into `.steps/score-egress-<sandbox>.log` in the run dir, replacing the file a
  previous scoring of that run left. no item name can begin with a dot, so no pair's output dir can take that path.

none of them counts toward the agent's `denied_egress` or `blocked_egress`. a step box's denials get a warning of
their own: the end-of-pair line says how many hosts the pair's steps were denied, and `pats score` says the same per
step sandbox at its end, each pointing at the audit.

## what a step sees in the box

the contract a step has on the host carries over: same script rule, same directory, same `PATS_*` names, and the same
paths. so a script that works on the host works in the box when the tools it calls are there, and it does not depend
on what the limits below exclude.

- **paths are bound at their host paths.** each bind's in-box path is its host path. so `PATS_WORKDIR`,
  `PATS_OUTPUT_DIR`, `PATS_ROOT` and the rest keep their values, and an absolute path a script built from them
  resolves.
- **the tree:** the work tree that holds the pats root, bound read-only. pats finds it by walking up from the pats
  root and stops at the first `.git` entry, a directory or a file. a `.git` file is a submodule's or a linked
  worktree's, so a pats root in a submodule gets the submodule, not its superproject. "home" here is the user's real
  home: `$SNAP_REAL_HOME` under the snap, where `$HOME` is `~/snap/pats/<rev>`, and `$HOME` otherwise. the walk never
  takes the home or anything above it, and falls back to the pats root alone when it finds no `.git` below the home.
  a dotfiles repo at `~` never binds the whole home. a pats root that is the home itself or above it (`pats -c
  ~/pats.yaml`), compared by resolved paths, cannot use a step sandbox: starting the box is an error naming it.
  mason's `stage` reads `$PATS_ROOT/../..`, its repo root, which this covers.
  - **masked:** `<pats root>/.pats`, which holds the runs (`internal/eval/run.go`, `runsSubdir`), the pats cache dir
    (`docs/spec/cache-dir.md`), which holds every workdir, harness home and staging dir, and the legacy cache dir an
    older pats may still use (`legacyCacheDir` in `src/sandbox/cachedir.go`). each mask applies at its path and, when
    either is a symlink or sits under one, also at its resolved path wherever that falls inside the tree's resolved
    path. a `.pats` linked to `/repo/state` masks `/repo/state` too. a masked path is an empty tmpfs under docker,
    podman and bwrap (podman's with `notmpcopyup`, since by default it copies what lies beneath into the tmpfs):
    writable to the engine, so it can make the mountpoints of the binds that go back under it (an empty read-only
    directory cannot take one). lxd skips it on push. so a box never sees or copies other pairs' live state or pats's
    own, and the binds below put back only what a step may see.
  - **mount order:** the tree, then its masks, then the binds under them, parents before children. a driver keeps
    that order.
  - **read-only means read-only on every driver.** under lxd a read-only bind is pushed owned by root, with write
    removed from group and other, read given to all, execute given to all on every directory and on every file its
    owner can execute (so a `0700` helper still runs), and setuid and setgid cleared, recursively. a root-owned copy
    of a setuid file would otherwise be setuid root in the box. it is never handed to the agent user, so a step
    cannot change it. a step sandbox with `sudo: true` can, and its writes still never reach the host. a copy sits in
    ordinary directories, so whoever owns a directory above it could rename it away and put a writable one in its
    place. so a read-only bind whose in-box path lies under a directory the agent user owns, or one it can write
    without the sticky bit, is refused at start, naming the path. a directory pats's push creates is root's.
  - **git on the tree under lxd:** a root-owned checkout, and every submodule in it, fails git's ownership check for
    the agent user. so the box's home (below) gets a git config whose only entry is `safe.directory = *`. the box is
    throwaway and holds no repository but pats's read-only copy and what the steps make. under docker, podman and
    bwrap the tree keeps the host owner, who is the agent user.
  - **parents under lxd:** the push creates a bind's missing parents as root with default modes, and the parents
    the image already has keep the image's modes. those can shut the step user out: ubuntu images have
    `/home/ubuntu` at `0750 ubuntu:ubuntu`, so a project under `/home/ubuntu` (a cloud vm's or ci runner's usual
    place) would be out of reach, as would one under `/root`. so before the push pats gives every existing directory
    above a bind's in-box path traverse for all (`a+x`), adding no read or write. the box is throwaway, so nothing
    else depends on those modes.
  - **visible and reachable, or refused:** a daemon that cannot reach a host path may mount an empty directory in its
    place with no error (the docker snap outside the home, below). so at box start pats checks every bind from
    inside, as the step user, before any step runs, by an entry that is already there or that only this box uses:
    - the tree: the config file pats loaded (`pats.yaml`, or what `-c` named).
    - a read-only bind other boxes may share (a scorer box's run dir, a judge's `PATS_SCORED_DIR`): an entry pats
      lists in the host dir just before the start, which must show in the box. an entry missing in the box fails the
      check only if the host still has it on a second look. one gone from both (a host scorer removed it) makes pats
      list again and retry. a dir the host sees empty is checked for being reachable only, since an empty mount in its
      place shows the same thing.
    - a pair box's read-write binds, which no other box holds: under docker, podman and bwrap, a sentinel file pats
      puts in each on the host before the start and removes before the first step, which the mount shows at once.
      under lxd they are checked for being reachable only: the push copied them in, and a sentinel would be copied
      with them and come back on `Sync`.

    a bind the step user cannot see or reach fails the start, naming the path. nothing is added to or removed from a
    dir another box may be pushing.
- **per step, read-write:**
  - `stage` and `prompt-cmd`: the pair's workdir, its output dir and the agent's home (`PATS_AGENT_HOME`), which
    `stage` may seed with harness config.
  - `collect`: the workdir and the output dir.
  - `score`: none. the run dir being scored is bound read-only. a scorer's writes stay in the box's own filesystem
    until the box closes and never reach the host. a scorer's result is its stdout, as today.
- **a judge's pair** (an agent scorer's `stage`, `prompt-cmd` and `collect`) gets a pair's boxes, binds as above for
  the judge's own sample, plus the scored pair's output dir (`PATS_SCORED_DIR`, `internal/eval/judge.go`) read-only,
  which the `.pats` mask would otherwise hide.
- **`PATS_RUN_DIR`** keeps its value. a task step's box holds only the pair's own output dir under it, not the other
  pairs'. a script scorer's box holds the whole run dir it scores, read-only.
- **the workdir mount:** every box, pair or scorer, gets a fresh empty `Spec.Workdir` (a pats temp dir) at
  `/workspace`, as every sandbox start needs one. no step uses it. the pair's workdir goes in as a bind at its host
  path.
- **the script** is pushed into its own in-box directory, unique to that one execution and removed when it ends, so
  no step finds another's script. it runs as on the host: a `#!` line picks its interpreter, any other runs under
  `/bin/sh -e`. it runs in the item's directory (the task's, the scorer's or the pats root), whose in-box path is its
  host path and which is read-only.
- **`PATH`** is the image's own, with the pats root's `bin/` first. the prepending happens inside the box, so a
  provisioned tool in `/usr/local/bin` is found.
- **`HOME`** is a fresh directory made in the box at its start, by a root exec, owned by the step user. it is not a
  bind, so nothing syncs back from it. tools that write caches or config work on any image. what a step leaves there
  stays until the box closes, so a later step in the same box sees it (below).
- **the environment**, later entries winning:
  - the image's own.
  - the session's: in an open box behind the machine's proxy, the proxy variables every sandbox gets
    (`docs/spec/chained-egress.md`). they sit under everything an exec sets, as for an agent
    (`src/sandbox/sandbox.go`, `lxd_session.go`).
  - the exec's: the step sandbox's `env:` (an agent gets its sandbox's `env:` in its exec env too,
    `internal/eval/harness.go`), then `step-env`, then the item's own `env:` (a task's or an agent scorer's, as
    today), then what pats sets (`PATS_*`, `PATH`, `HOME`). a script scorer has no `env:`, as today.

  so a sandbox `env:`, an item `env:` or a `step-env` that names a proxy variable overrides the machine's. under a
  policy that only cuts the step's way out, since the box has no other route. the machine-proxy variables are the one
  host-derived exception. otherwise the host's environment does not enter the box: `HOME`, `PATH`, a shell's `LANG`
  and everything else stay outside.
- **`step-env`** passes host variables a step needs, by name, beside `step-sandbox`: `step-env: [GITHUB_TOKEN]` sets
  `GITHUB_TOKEN` in the box to its value in pats's environment. a name pats's environment lacks is not set by pats,
  so the image's or the sandbox's value, if any, stays. load errors:
  - `step-env` without `step-sandbox`, where the whole host environment is there anyway.
  - a name `env:` rejects (not a shell name, `PATS_*`, `PATH`, or one the shell will not set,
    `internal/config/validate.go`), and `HOME`, which pats sets in the box.
  - a name the item's own `env:` also sets: one way to give a step a variable.
- **`HOME` in the item's `env:`** reaches host steps today. with a `step-sandbox` set it is a load error, since the
  box's `HOME` is pats's (above) and would silently win.
- **the user:** the agent's user, as an agent runs. under docker, podman and bwrap that is the host uid
  (`src/sandbox/sandbox.go`, `bwrap.go`), so what a step writes through a bind is the user's, and a pats run as root
  runs its steps as root, as it runs its agents. under lxd it is the instance's non-root agent user, and the
  read-write binds come back through `Sync` as the host user's. `sudo:` on the step sandbox works as for an agent
  (lxd only).

## the box's life

- **per pair, up to two boxes**, one per phase that has a step. a task with no `stage` and no `prompt-cmd` gets no
  before box, and one with no `collect` no after box. pats writes nothing into a bind while a box holds it, since
  under lxd `Sync` replaces the host dir's contents with the box's copy (`src/sandbox/lxd_files.go`).
  - **staging:** each box has a staging dir, a pats temp dir (`sandbox.MkTemp`), for what pats writes while the box
    holds the binds: the step's log and the box's egress audit. it is outside every bind, since the cache dir is
    masked in the tree (above). its contents move into the output dir after the box closes.
  - **before the agent:** a box runs `stage` and then `prompt-cmd`. `stage`'s output streams to the terminal and to
    `stage.log` in the staging dir. `prompt-cmd`'s stdout is the prompt, held in memory, and its stderr is kept for
    the error as today. the box syncs and closes. then pats moves the log into the output dir, writes `prompt.txt`
    into the agent's home, and starts the agent's sandbox, so the agent sees what `stage` left.
  - **after the agent:** the agent's session closes before `collect`, whether `collect` runs on the host or in a box,
    so its proxy has drained `egress.log` into the output dir before anything reads or holds it. today that close
    comes after `collect` (`internal/eval/pair.go`). a host `collect` reads only the synced host copy, so it loses
    nothing by the earlier close. then a box runs `collect`, its log staged the same way, syncs and closes, and only
    then does pats move the staged files in or write anything else to the output dir.
  - **failures:** a failed start, step or sync of the before box is the pair's setup error, as a failed `stage` is.
    the same for the after box is a warning, as a failed `collect` is. under lxd `Sync` pulls each bind whole beside
    its host path and swaps it in only then (`src/sandbox/lxd_files.go`), so a pull that fails leaves that bind's
    host copy as it was. the binds are synced one after another, though, so a failure on the second leaves the first
    already swapped. a failed sync can leave the pair's host dirs part old and part new, and is not rolled back.
    under docker, podman and bwrap a step writes through to the host as it runs, with nothing to sync. a pair whose
    box failed is marked as above, and its output dir is not to be trusted beyond that.
  - **time:** a box's start, with its checks, is bounded as an agent sandbox's start is (`setupTimeout`,
    `internal/eval/prepare.go`), and its sync as an agent's sync is (`syncTimeout`). the steps themselves get no
    timeout, as host steps have none today. a start or sync that runs out fails as above.
- **nothing outside the binds outlives a box**, and the two boxes of a pair share nothing but the binds. a step that
  keeps state between `stage` and `collect` keeps it in the workdir or the output dir.
- **scoring:** per run scored, a pool of up to `-j` boxes per step sandbox, started as scorers need them and closed
  when that run is scored. `Score` is per run (`pats score -r all` calls it once a run), and a box's binds are fixed
  at its start, so each box's read-only bind is that run's dir. a box runs one scorer at a time. what a scorer leaves
  in the box's own filesystem (`/tmp`, `HOME`) is there for the next scorer the box runs (limits).
- **slots:** a step box takes no `max-parallel` slot. it belongs to a pair that already holds one, and a step sandbox
  that is also the agent's would otherwise deadlock at `max-parallel: 1`. `-j` bounds a pair's boxes, since each
  belongs to a running pair. scoring holds up to `-j` boxes per distinct step sandbox its scorers name.
- **a step's processes end with it.** on the host a step runs in a process group of its own, killed whole when the
  step exits, and its output is drained for at most `sandbox.GroupWaitDelay` after (`src/sandbox/group.go`). that
  reaches what is still in the group, not a child that left it (`setsid`, limits). a box has no terminal, so a
  shell there cannot make a group (dash: "can't access tty"). a box runs one step at a time, so the step user's
  processes are the step's, and the kill goes by user:
  - **docker, podman, lxd:** the step runs under a small `sh` wrapper, in the same exec, that runs the script, then
    sends `kill -KILL -1` as the step's user and exits with the script's status. the kill reaches every process of
    that user but the wrapper itself, and finding none is not an error. it comes before the exec returns, so a
    background child holding the step's output open cannot keep the exec waiting, and nothing the step left runs into
    a sync or the next step. it needs only the shell's `kill` builtin. what the box needs runs as another user:
    - docker and podman: the keepalive (`sleep infinity` under `--init`) runs as root, set explicitly on the run,
      not left to the image's `USER`. the egress proxy is a sidecar container.
    - lxd: the instance's services and pats's plumbing run as root, and the proxy is an instance of its own. an
      image whose `pats` user is uid 0 (`src/sandbox/probes.go` reuses an existing `pats` account) cannot be a step
      sandbox, since the kill would reach root. starting such a box is an error naming the image.
    - when the step's user is root (docker or podman with pats run as root), the wrapper sends no kill. leftovers
      then live until the box closes (limits).
  - **bwrap:** no kill. every exec is its own `bwrap --unshare-pid`, and its pid namespace dies when the exec's main
    process does, taking the forwarder and any leftovers along (`src/sandbox/bwrap.go`). a kill by user would hit the
    forwarder, which runs as the step's user.
- **cancel:** a cancelled step closes its box. `Session.Exec` alone is not enough, since under lxd it kills only the
  agent uid's processes (a `sudo: true` step's root processes survive) and under bwrap it keeps the session. on the
  host a cancel kills the step's process group, as today.

## per driver

- **docker:** binds are `-v` mounts and each mask a tmpfs. a box costs a container start. under the docker snap
  every bound path must be under the home. a path outside it mounts as an empty directory with no error from the
  daemon, as for an agent sandbox today (`src/sandbox/tmp.go`), so the visibility check above refuses such a box.
- **podman:** as docker, with `notmpcopyup` on each mask's tmpfs.
- **rootless engines:** rootless podman and rootless docker, and docker with `userns-remap`, map the uid pats passes
  (`src/sandbox/sandbox.go`) to another uid on the host. a step there can neither read pats's `0700` temp dirs nor
  write through a bind as the user, the same failure rootless podman agents have today (`EACCES` on their home). so
  preflight and every box start ask the engine (`docker info`'s security options, `podman info`'s
  `host.security.rootless`) and refuse such an engine as a step sandbox, naming the cause, before any path error
  can stand in for it. the fix (a user-namespace mapping that keeps the host uid) belongs to the container driver
  and serves agents and steps alike.
- **lxd:** binds are pushed at start, the read-write ones pulled back by `Sync`, as for an agent session, read-only
  ones root-owned (above).
  - **emptied first.** a mount hides what the image has at its path, and a push into an existing directory does not
    (`src/sandbox/lxd_files.go` extracts over it). so pats empties each bind's in-box destination before pushing into
    it, and every masked path inside it, before the nested binds go in. a step sees the host's files at a bind's path
    and nothing the image had there, as under the other drivers.
  - **symlinked sources.** a bind whose host path is a symlink, or sits under one, is pushed from its resolved path
    and lands at its host path as written, since the push walks a directory without following a link at its root.
  - **a tree that changes while pushed.** the push lists a directory, then opens and copies each entry
    (`writeTar`), and a tar header fixes each file's size before its bytes go. today any change in between fails the
    push: an entry that vanishes, a file that grows or shrinks. the user editing or building in the same checkout
    during a long run does that, as does a host step, and every pair pushes the tree up to twice. so:
    - the push takes each file's size from its open handle, not from an earlier stat, and copies exactly that many
      bytes. a file that grows meanwhile (an append-only log in the checkout) goes in as a clean prefix, with no
      failure.
    - an entry that vanished before it was opened is skipped.
    - only a file that shrinks while its own bytes are copied fails the attempt, since its header promised bytes
      that are gone.
    - a failed attempt is retried, up to three attempts in all, then the start fails with `<path> changed while it
      was pushed`. every attempt starts from an emptied destination (above), so nothing an earlier attempt extracted
      survives into the box.
    - each attempt has a budget of its own (`setupTimeout`), so a tree that takes minutes to push is not cut short
      by its own retries. a start can then take up to three such budgets.
  - **cost.** a box pushes the whole tree, less the masks, each time it starts, and a scorer box the whole run dir. a
    large repository or run makes that the cost of every box, so a project that hits it keeps its pats root in a
    small tree or uses a container step sandbox.
- **bwrap:** binds and the masks, as for an agent.
- **host:** not a step sandbox (above).

pats's fixed in-box paths (the workdir mount `/workspace`, the agent paths beside it in `src/sandbox`, the trust
bundle's) cannot double as host paths. a box whose tree, run dir, workdir or home would bind at or under one is
refused before it starts, naming the path.

## what is recorded

a pair's `metadata.json` gains `steps: {sandbox, image}`: the step sandbox and the pinned image its steps ran in,
absent when the steps ran on the host. a score report records the same per scorer. a step's tools are then written
down with the result.

## limits

- **a `nohost` build cannot run what needs the host.** every limit below that ends in "needs the host" names an eval
  that runs only on a native build.
- **a step cannot reach the host's daemons.** pats passes no engine socket in, and a docker or podman step sandbox
  has no key to add one. a `stage` that runs `docker` or `lxc` against the host needs the host (no `step-sandbox`).
  an lxd step sandbox's `profiles:` can add such a device, which is the user's own doing and outside what pats
  checks.
- **no host home.** `~/.ssh`, `~/.gitconfig`, `~/.netrc` and the ssh agent's socket stay outside. a `git clone` over
  ssh, or of a private repo over https, needs a token passed by `step-env` and used explicitly, or the host.
- **the tree and the step's own directory are read-only.** a step that writes beside itself (a scorer's cache file,
  a `stage` that edits the task dir) needs the host or a write to the workdir or the output dir instead.
- **the run dir is partial in a task step's box**, and read-only in a scorer's. a step that reads another pair's
  output, or a scorer that writes into the run dir, needs the host.
- **paths that point outside the binds break.** an absolute symlink to an unbound path, and a linked git worktree,
  whose `.git` file names a git dir in the main checkout. a step that runs `git` in a linked worktree needs the host,
  or a checkout of its own. a pats root in a submodule sees the submodule only, not its superproject.
- **the host's fixed in-box paths:** a project under `/workspace` or another pats in-box path cannot use a step
  sandbox.
- **rootless podman, rootless docker and docker's `userns-remap`** are refused as step sandboxes (above), until the
  container driver keeps the host uid.
- **a failed sync is not rolled back** across binds (above).
- **lxd carries regular files, directories and symlinks only.** a fifo or a unix socket a step leaves in a
  read-write bind is not pulled back, and the sync's swap of the host dir's contents removes the host's copy too
  (`src/sandbox/lxd_files.go`).
- **a tree that keeps changing during an lxd push** (a build writing into the checkout, a host step rewriting a
  file) can fail a box's start after the retries above. a run whose tree churns like that uses a container step
  sandbox, or keeps its build outputs outside the tree.
- **an lxd box sees its read-only binds as they were when it started.** it holds copies of the tree and, for a
  scorer box, the run dir, and a pooled box serves later scorers from those same copies. what a host step writes
  into either after that start is invisible to it, even with `-j 1`: a host scorer's lookup file under `PATS_ROOT`,
  or its output in the run dir. an lxd scorer that reads what a host step writes runs in a later `pats score`
  invocation than that step. under docker, podman and bwrap these binds are mounts and show host writes as they
  happen.
- **scorers sharing a pooled box share its filesystem.** what one leaves in `/tmp` or `HOME` the next one in that box
  sees. a scorer that needs a clean slate cleans up after itself, or the host.
- **leftovers outlive a step when its user is root:** a docker or podman step box with pats run as root. they live
  until the box closes, so they can run into the next step in the box, and one that holds the step's output open
  keeps the exec waiting until it exits.
- **root processes a step starts through `sudo`** (an lxd step sandbox with `sudo: true`) are beyond the step user's
  kill. they survive until the box closes, so they can run into the box's sync or the next step in it, and one that
  holds the step's output open keeps the exec waiting.
- **`max-parallel` counts agents only.** on a sandbox that is both an agent's and a step sandbox, step boxes run on
  top of the agents it allows, so the place can hold more instances than `max-parallel` at once.
- **a host step's child that leaves its process group** (`setsid`, a daemon that double-forks) survives the step, as
  today. it can keep changing files during the next step, the agent's run or an lxd push. pats does not track it.
- **a host user named `pats` under lxd.** the project then lives under `/home/pats`, which in the box is the `pats`
  agent user's own home, owned by that user. every read-only bind under it is refused (above). such a host uses a
  container step sandbox, or keeps its projects elsewhere.
- **a step that kills its own wrapper** (`kill -9 $PPID`, or a `kill -KILL -1` of its own) ends the exec before the
  cleanup runs. its leftovers then live until the box closes, and the step counts as failed, since the exec reports
  the wrapper killed, not the script's status.
- **cost.** a box start per phase per pair, and up to `-j` per step sandbox per run scored, each with its own proxy
  under a policy. under lxd each box also pushes the tree, and a scorer box the run dir. the native default stays
  the host.
- **no host environment** beyond `step-env` and the machine-proxy variables every sandbox gets.

## what changes in code

- `internal/config`:
  - `StepSandbox` and `StepEnv` on `Task` and `Scorer`, carried by `Scorer.Task()`.
  - validation: the name resolves, the sandbox isn't host, the `step-env` rules, no `HOME` in `env:` beside a
    `step-sandbox`, `max-parallel` on a steps-only sandbox, and the `nohost` rule.
  - `usedSandboxes` and init's selection include step sandboxes.
- `internal/eval`:
  - a step gets two executors behind one contract (script, dir, env, stdout): the host process (today's `step`), and
    a `sandbox.Session` with the wrapper above.
  - `runPair` runs the before and after boxes when their phase has a step, with logs staged and moved, the prompt
    written after the sync, and the agent's session closed before `collect` in every case.
  - `Score` keeps a pool of up to `-j` boxes per step sandbox per run, one scorer at a time in each.
  - the tree walk and the masks, with symlinks resolved.
  - metadata and the score report gain `steps`, and the warnings gain the steps' denials.
  - preflight starts each step sandbox once, with the visibility check, and runs `/bin/sh -c true` in it.
- `src/sandbox`:
  - a mount gains a mask list: paths under it the box sees as empty (a tmpfs under docker, podman and bwrap, skipped
    by the lxd push). mounts keep their order, parents first.
  - lxd pushes read-only mounts root-owned, with the modes above (no group or other write, read for all, execute
    where the owner has it, no setuid or setgid), and pushes a symlinked source from its resolved path. `Start`
    chowns only the writable binds to the agent user, where it chowns every bind today (`chownAgent` over
    `paths(false)`, `src/sandbox/lxd.go`), and `HandOver` leaves read-only ones alone. agent sessions' read-only
    mounts get the same, which only tightens them.
  - the container keepalive runs as root explicitly.
  - the reserved in-box paths are one exported list, checked before a box starts.
  - lxd empties a bind's in-box destination, and the masked paths in it, before pushing, gives existing parents
    `a+x`, and pushes a file at the size of its open handle, skipping a vanished entry, with the whole push retried
    up to three times on a change.
  - podman masks with `notmpcopyup`. the container driver detects a rootless or remapped engine for step
    sandboxes.
  - the home a step sandbox guards is `$SNAP_REAL_HOME` when set, else `$HOME`.
  - the conformance tables gain rows: a mount at its host path written and synced back, an image file already at a
    bind's path and inside a mask that the step does not see, a read-only mount the agent user cannot write, a masked
    subdir that reads empty with a bind under it, and a background child killed when its step's exec ends.
- docs: the readme's steps section, `pats.example.yaml`, `steps.md` ("where a step runs"), `init.md` (step
  sandboxes pinned), `run-dry-run.md` (step sandboxes listed), and `snap.md` (the strict snap needs `step-sandbox`
  for host steps).

## not in this

- the build tags themselves, and the strict snap. this spec only says what `nohost` means for steps.
- running `prepare` or `provision` anywhere new. they already run in sandboxes.
- passing the host engine's socket into a step box.
- a step sandbox per step (one for `stage`, another for `collect` of the same task). a task's steps share one, and a
  scorer has its own.
- `env:` on a script scorer.
