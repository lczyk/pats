# a run removes the sandboxes dead runs of this machine left

status: implemented. `lxd.md` has the lease and the marker acl, `lxd-retry.md` the deletes retried at shutdown.

## problem

a run that is killed outright (`kill -9`, an oom, a closed laptop lid, a lost ssh session with no `nohup`) runs
none of its cleanup. its sandboxes stay up until someone runs `pats clean`. one lxd vm stayed running for five
hours this way, holding its memory and its cpus on a shared host, and the next run on that host had less room.

- **`pats clean` knows already.** every run writes a lease file with its host and pid (`run-<token>.lease`,
  `src/sandbox/lease.go`), and the judge (`src/sandbox/clean.go`) calls a run dead when the lease names this host
  and the pid is gone, on every driver, lxd included since `90446a1`.
- **nobody calls it.** clean is a manual command. the user who just lost a run starts the next one, not a clean.
- **`lxd-retry.md` does not reach it.** its retry of deferred deletes runs at shutdown, and a killed run has none.

## proposal

`pats run` and `pats judge`, before their preflight, stop what costs the most: the sandboxes, networks and acls of
runs that are dead for certain, on the places this invocation is about to use. everything else stays for
`pats clean`.

- **which runs**: a run whose identity file (below) says it ran in this boot and pid namespace, and whose pid is
  gone.
  nothing else: not a stale lease, not another host's or user's run, not one with no lease file. only the pid
  check is certain, and a sweep that guesses wrong deletes a live run's sandbox.
- **which places**: the places this invocation's sandboxes use. a place it does not touch is not listed, so a
  remote that is down and not needed costs nothing.
- **what is removed**: the run's resources of kind `sandbox`, `network` and `acl` on those places, in clean's order
  (sandboxes first, one holds its network). not the lxd `marker` (the acl `pats-run-<token>`, its own kind in the
  listing): it holds the run's server lease, and the run's private aliases and images, which stay, are judged by
  it. `pats clean` removes it with them.
- **what is not**: the lxd marker, the lease and identity files, temp dirs, private aliases and images, and the image cache. the lease stays b/c
  a run may have used a place this invocation does not list: kept, it is still the evidence the next sweep or
  `pats clean` needs. the rest holds disk, not memory or cpus, and `pats clean` already has its own rules for it
  (an image kept until unused for 30 days, temps by age). a later `pats clean` takes the rest by its rules of
  today. on lxd the marker goes after the run's aliases, and a private image unused for
  less than 30 days stays past it, as it does today. the lease and identity files go by the two paths
  that remove a lease today: with a non-held run's last listed resource, when a place still lists some of it (a
  place the sweep did not reach), or else through the temp sweep, once every place listed, the run is not among
  them, its pid is not alive, its lease is stale and no temp dir names it. that may be several cleans later.
  the sweep does not hurry any of it.
- **one line per run swept**: `INFO stopped what run <token> left (its process is gone): 1 sandbox, 1 network,
  1 acl; pats clean removes the rest`. nothing when there is nothing to remove.
- **never a stop**: a place that cannot be listed, a remove that fails, a lock not taken within its bound. each is
  one warning, and the run goes on. the sweep is a courtesy, and the run's own work does not need it.
- **bounded**: 60s for the whole sweep, on its own context, and every list and remove takes that context. a sweep
  cut short says how far it got, as a warning.
- **`--dry-run`** sweeps nothing, and lists what it would remove under `-v`.
- **opting out**: `PATS_NO_SWEEP=1`, for a user who wants to keep a dead run's sandbox to look at. no flag, b/c it
  is rarely wanted and a flag on every run command is surface for little.

the sweep is a new function beside `process.clean`, not a call of it with options. clean removes a non-held run's
lease after each place, removes images and aliases, and reports in its own words. the sweep wants none of those,
and sharing the loop would put "unless sweeping" in each of them. it shares the listing, not the judge.

### the pid

`local()` compares the lease's host with `os.Hostname()` and checks the pid with `processAlive`. a hostname is not
an identity: two machines, or a container and its host, can share one and a cache dir, and then each sees the
other's pid as gone. `pats clean` has lived with that, as the user runs it by hand. a sweep that runs on every
start should not.

- **an identity file beside the lease**: `run-<token>.ident`, written once at the lease's first write, holding two
  words. the boot id (`/proc/sys/kernel/random/boot_id` on linux, `kern.bootsessionuuid` on macos), random for each
  boot of a kernel, and the pid namespace (the inode of `/proc/self/ns/pid` on linux, `-` on macos, which has
  none). a word pats cannot read makes no file. a machine id is not used: cloned vms and images share one.
- **a separate file, not a third field**, b/c an older pats reads the lease as `host pid` and parses the rest of
  the line as the pid: `host pid ns` would read as pid zero, and an older `pats clean` would lose its live-pid
  check. the lease file does not change.
- **the sweep's own verdict**, not the judge's. a run is swept only when its lease file and its identity file are
  both there, the boot id and the pid namespace are this process's, and the pid is gone. one boot id and one pid
  namespace are one kernel's pid space, so a gone pid there is gone. the lease's hostname must match too, as the
  judge's does. the one case this does not cover is a vm cloned from a live snapshot, running beside its original
  and sharing its cache dir: the clone has the same boot id and namespace. a cache dir is per user and per machine,
  and sharing one between live clones is not supported, here or by the machine lock. everything else is left to `pats clean`: an
  earlier boot, another namespace, a run with no identity file. the judge's stale and missing-lease verdicts are
  never the sweep's.
- **an earlier boot is not swept**, though its run is dead. telling "an earlier boot of this machine" from "another
  machine that shares the cache dir" needs a machine identity that cloned images do not share, and there is none
  to read. after a reboot, `pats clean` removes the leftovers, as today.
- **removed with the lease**: `endLease` and `pats clean` remove the identity file with the lease. an older pats
  that removes only the lease leaves it behind. an identity file with no lease is never read by the sweep. the temp
  sweep of `pats clean` (`cleanTempIn`, which scans only lease files today) gains a scan of identity files: one
  with no lease and older than `--older-than` is removed, and listed under `--dry-run`. every place that removes a
  lease file today (the run sweep of `src/sandbox/clean.go` for a non-held run, `--run` included, and the temp
  sweep) removes the identity file in the same step and names it in that lease's event. the orphan scan skips the
  identity of a lease in `planned`, so a dry run lists it once.

the judge itself does not change: `pats clean` judges as today.

a pid that is in use is no proof the run lives: it may have been reused. the judge treats a pid in use as alive,
whatever the lease's age, and so does the sweep. so a reused pid leaves the dead run's sandbox up until that
process ends, and `pats clean --run <token>` is the way to remove it sooner. that is the safe direction.

### lxd

a held lxd run (`Held`) has its lease on the server, renewed by its process. the judge checks the local lease file
first, so a run of this host whose pid is gone is dead at once, whatever the server's lease says. that is the only
held run the sweep touches.

the removes go through the driver's `Remove`, as `pats clean`'s do: on lxd a plain `lxc delete --force` of an
instance, then the bridge and the acl. this spec does not depend on `lxd-retry.md`. once that lands, a remove that
meets a reset is retried there, within the sweep's context.

### concurrency

the sweep takes the machine lock (`machineLock`, a file in the user's cache dir) for its own span, as `pats clean`
does, so a sweep and a manual clean of the same user on the same machine never work on one run at once.

the lock reaches no further: another user's or another machine's clean on the same lxd server takes another lock.
that is safe b/c every remove is of a resource of a run that is already dead, and a remove that meets "not found"
counts as done, as it does in `pats clean`. two cleaners at once do the work twice, no more. a live run is never
in the sweep's set, so its creating and its shutdown do not race the sweep.

### what it costs

the listing is clean's: on docker a list and an inspect of the containers and networks by label, on lxd a query of
instances, networks, acls and images by the `user.pats.*` keys. under a second on a reachable place. a place slow
to list is slow to start on, and the preflight starts sandboxes there a moment later. a slow image query delays
the sweep, within its 60s.

## out of scope

- a watchdog that removes a run's sandbox when its process dies, without waiting for the next run. it would need a
  process that outlives the run, which is the problem.
- sweeping other hosts' runs. a stale lease is a guess about another machine, and `pats clean` is where a user
  makes it.
- the identity file in the judge. `pats clean` could use it too. a separate change, once the file is written.

## tests

- a run of this machine, boot and namespace with a pid that is gone, and its sandbox, network and acl on a fake
  place: a run removes all three, keeps the lease and identity files, logs the line and goes on to preflight. a
  `pats clean` once the lease is stale and no temp dir names the run removes both files in one event.
- a run of an earlier boot: nothing removed.
- a run whose lease is stale but whose identity is another boot's: nothing removed, though the judge says dead.
- `pats clean --dry-run` of a dead run: its lease and identity file in one line, the identity not again as an
  orphan.
- a run with an identity file and no lease file: nothing removed. `pats clean` removes the identity file once old
  enough, and lists it under `--dry-run`.
- an identity file pats could not write (a word unreadable): no file, the run is never swept.
- a lease file of the new pats read by the old parser: host and pid as before.
- a lease of this host whose pid is alive: nothing removed.
- a lease of another host, a stale one, one with no identity file: nothing removed, nothing logged.
- a run of another namespace, same boot, pid gone: nothing removed.
- a held lxd run of this boot and namespace, pid gone, server lease fresh: its instance, bridge and acl removed,
  its marker kept, and `pats clean` then removes the marker with its aliases.
- a dead run on places a and b, the run using only a: a's resources removed, b's kept, the lease kept, and a run
  using b later removes b's.
- a place the run does not use: not listed.
- a place that cannot be listed: a warning, the run goes on.
- a remove that fails: a warning, the run's other resources after it in order kept, the run goes on.
- a remove that meets "not found": counts as removed.
- `--dry-run`: nothing removed.
- `PATS_NO_SWEEP=1`: no list is made.
- a `pats clean` holding the machine lock: the sweep waits up to its bound, then warns and goes on.
- a list that hangs: the sweep ends at 60s with its warning, and preflight starts.
