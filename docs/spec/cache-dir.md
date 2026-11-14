# cache dir

status: implemented.

## problem

pats keeps machine-wide state in two roots, and on macos they are different directories:

- `os.UserCacheDir()/pats` (`src/sandbox/tmp.go` `TempRoot`, `lease.go` `cacheDir`): the temp root `tmp/` with each
  dir's `.owner` file, the `run-<token>.lease` files, and the client id with its `client-id.lock`. on macos that is
  `~/Library/Caches/pats`, on linux `$XDG_CACHE_HOME/pats` or `~/.cache/pats`.
- a hardcoded `$XDG_CACHE_HOME` or `~/.cache` (`internal/eval/remove.go` `projectsFile`): the projects registry
  `projects` and the machine lock `projects.lock`.

so on a mac half of pats's state is in `~/Library/Caches/pats` and half in `~/.cache/pats`, the docs name
`~/.cache/pats` for both, and a user looking for leftovers finds half of them.

## proposal

- **one root, every os.** `sandbox.CacheDir()` is `$XDG_CACHE_HOME/pats` when `XDG_CACHE_HOME` is set and absolute,
  else `~/.cache/pats`. a relative `XDG_CACHE_HOME` is ignored, as the xdg spec says. `TempRoot`, `cacheDir` and
  `projectsFile` all derive from it, and `os.UserCacheDir` is only used to find the legacy root.
- it stays under `$HOME`, which the bind mounts need: docker desktop and podman machine share `$HOME` with their vm,
  and the docker snap's daemon sees `$HOME` only. the `XDG_CACHE_HOME`-outside-`$HOME` caveat in `tmp.go` stays.
  the docker snap's cli sees less, no hidden dir at the home's top and so not the cache: what it opens itself is
  kept elsewhere (`docker-snap-paths.md`).
- **the legacy root** is what `os.UserCacheDir()/pats` resolves to. it exists only when that call succeeds (on linux
  it fails for a relative `XDG_CACHE_HOME`: no legacy root then) and it is a different directory from the new root,
  compared after resolving symlinks: on linux the two are one, and so are they on a mac with
  `XDG_CACHE_HOME=~/Library/Caches`. with no legacy root, nothing below applies.
- **a relative `XDG_CACHE_HOME` today** puts the projects registry and the machine lock under the cwd, so each
  directory had its own. the new root takes `~/.cache/pats` for them instead. a registry under a relative path is
  not migrated: the next `pats init` of each project registers it again, and until then init's removal of
  superseded container images misses that project's pins. the release notes say to rerun `pats init` once if
  `XDG_CACHE_HOME` was relative.

## versions side by side

an older pats keeps using the legacy root, and may run on the same machine as a new one. the new pats reads what an
old one writes. the reverse does not hold: an old `pats clean` finds no lease for a new run (the lease is in the new
root), judges it by age, and can take a live new run that has been going for over ten minutes. so:

- **the rule**: after upgrading, do not run an old `pats clean` while a new pats runs. the release notes say so, and
  say to upgrade every pats on a machine together. a new pats does not write leases into the legacy root to cover for
  old cleaners: that is a second lease to renew for every run, for as long as anyone keeps an old binary.

what the new pats does with the legacy root:

- **client id.** `loadClientID` takes the new root's `client-id.lock`, and when the new root has no valid client id
  and the legacy root has one, publishes the legacy id in the new root (same validation, same atomic write) before
  returning it. every reader of the client id goes through `loadClientID`, so a clean judging runs sees the migrated
  id as a run does. the owner label (`pats.owner=<host>/<uid>/<client>`) then stays the same across the upgrade, and
  runs an old binary started are judged as this client's, not another machine's.
- **leases.** `readLease` looks in the new root, then the legacy root, and returns the path it read. a container run
  started by an old binary has its lease in the legacy root, and a clean looking only in the new root would judge it
  by age and take it while live. when the run sweep removes a run, it removes the lease at the path it was read
  from, in either root, under the same rules as now (only once every resource is gone; a dry run says it would).
- **the sweep.** `pats clean` runs `CleanTemp` over the legacy root as well as the new one, with the same rules and
  the same `-a` and `-f`. the run sweep's `listed` set applies to both, so a run's legacy lease is the run sweep's,
  as a new one is. the events carry full paths, so the output shows which root a line is about.
- **the legacy root stays.** clean never removes the legacy client id, its lock or the root: an old binary may hold
  the lock or be about to write the id, and removing them safely would need its cooperation. when the legacy root
  holds nothing but those and an empty `tmp/`, clean says so once: `left legacy cache dir ~/Library/Caches/pats: it
  holds only an old client id; remove it once no older pats is installed`. the legacy root is looked at on every clean.
- **clean reads identity, never writes it.** judging a run calls `p.owner()`, which today goes through
  `loadClientID` and may publish a fresh id. clean, dry run or not, uses a read-only lookup instead: the new root's
  id, else the legacy root's, else none, with no writes and no repair of a malformed file. a run with no id to
  compare is judged as another machine's (unknown). runs and init, which make resources, keep `loadClientID` and
  its migration.

## not doing

- **no move of live state.** temp dirs and leases are not moved between roots: a live run holds open paths and bind
  mounts into them.
- **no legacy writes.** see "versions side by side".

## tests

- `CacheDir` with an absolute, a relative and no `XDG_CACHE_HOME`.
- the legacy root is none when it resolves to the new root (a symlink, and the `~/Library/Caches` case).
- client id migrated once under the lock, kept when the new root has a valid one, a malformed legacy id ignored.
- `judge` reads a fresh legacy lease and calls the run alive; the run sweep removes a dead run's legacy lease, and
  keeps it when a resource removal failed.
- `CleanTemp` over the legacy root removes a dead run's dir and keeps a live one's; once only the client id files are
  left it says so and removes nothing; a dry run reports the same events and removes nothing.
- clean writes no client id in either root, with or without `-n`; a relative `XDG_CACHE_HOME` on linux has no legacy
  root and clean and runs use `~/.cache/pats`.
