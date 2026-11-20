# an image cache for init

status: proposed. `init.md` is what `pats init` builds and pins, and `lxd.md` how the lxd driver builds an image.

## problem

`pats init` builds an lxd sandbox's images on the server it finds, and a ci runner is a new server every time. so a
ci run pays for the whole build on every job:

- a project's rock sandbox (snapd, lxd and rockcraft in a system container) took 6m41s and 6m34s to build in two jobs
  of one ci run, about a sixth of each job. the two jobs built the same image, on two runners.
- publishing with zstd (`lxd.md`) takes about two minutes off that. the rest is the provisioning itself, which only
  not running it removes.
- the image is the same until its inputs change: its alias is a key over them (`init.md`, lxd specifics). nothing
  carries it from one server to the next.

## proposal

one flag on `pats init`, naming a directory of image files:

```
pats init --image-cache <dir>
```

when init needs an lxd image by an alias the server does not have, it looks in the directory for a file named as
that alias, and imports it. that is an image init would otherwise build, and one a `pats.yaml` names that init
would otherwise fail on.

- **pats only reads the directory.** it writes nothing there, removes nothing, keeps no list of it and takes no
  lock on it. what is in it, how it got there and when it is emptied are its owner's: a ci's cache step, a person.
- **the owner fills it**, with `lxc image export`, after an init that built (below).
- **for an image pats builds, the name is the key.** its alias is `pats-img-<key>`, and the key is a hash of what
  the image is made from: the base image's fingerprint, the provision script and `bin/`, the instance type, the
  pats version. all of that is known before a build, which is how init already tells that a server has an image
  and skips it. the directory is asked the same question. so a directory restored under any ci cache key, a stale
  one included, is fine: a file whose name is not wanted is not used.
- **for an image pats does not build, the name is the one `pats.yaml` gives**: a sandbox's `image:`, or its
  `base:`, when that is an alias on the server (below).
- **`--force`** takes no built image from the directory: it builds, as it does when the server has the image.
- no other command reads the directory, and it is no key of `pats.yaml` or of the machine file: where a ci keeps
  its files says nothing about an experiment or about a tool.

### the files

```
<dir>/
  pats-img-698f4b620d581910e96af84b.tar.zst
  pats-img-fe3a125f461d122e90ec9586.tar.zst
  my-rock.tar.zst
```

- **one file an image, in one flat directory**: the tarball `lxc image export` writes, as it is, under the name
  that command gives it when told the alias.
- **a file's name is the alias**, then `.tar` and at most one of `.gz`, `.zst`, `.xz`, `.bz2`, `.lzma`. init does
  not read the directory's listing for meaning: it has an alias, and asks for those six names.
- **an alias the directory is asked for is one path element**: no `/` or `\`, no leading `.`, not empty, and not
  `.` or `..`. one that is not is never looked for, so no name in a `pats.yaml` reaches a file outside the
  directory. an lxd alias with a `/` in it is a sound alias and has no file here.
- **and it is not all hex digits.** such a reference may be the beginning of a fingerprint, which lxd and pats
  both take (`lxd.fingerprint`). it is resolved as today and never looked for here, so one that names two images
  stays the error it is, and no alias is made under it.
- **a regular file only**: a symlink, a directory or anything else under that name is not used, and is warned
  about. the look and the open are two steps, and whoever can change the directory between them is its writer,
  who is trusted further than that already (limits).
- **only a unified image**: one file. a split one (a vm's, and a container's that was made so) exports as two,
  under other names, which are never asked for. its sandbox builds, or fails, as today.
- two files for one alias (two extensions): the one of the newest modification time.
- **the kind is lxd's by the format**, not by the name: an lxd alias may be anything. another driver's files,
  later, are told apart by their ending (below).
- **the image's properties** travel inside the tarball (`metadata.yaml`), and an import restores them: for an
  image pats built, the role, the inputs and the component stamps, all set at the publish. checked on 5.21: an
  image exported and imported had every property back, and its fingerprint.

### filling it

what an init built is on the server under its alias, so its owner exports each by that name:

```sh
r=$(lxc remote get-default)    # or the sandbox's `remote:`
lxc image list "$r:" --project pats --format json |
  jq -r '.[].aliases[].name | select(test("^pats-img-[0-9a-f]{24}$"))' |
  while read -r a; do lxc image export "$r:$a" "$dir/$a" --project pats; done
```

- **the server is named**: the one the sandbox is on, which is the client's default remote unless the sandbox has
  a `remote:`. a loop that asks another server exports nothing, or another project's images.
- the aliases are read as json: the table and csv forms print one alias an image and fold the rest away. the
  pattern leaves out a run-private alias, which has a token after the key.
- on a ci runner that is every image the init made: both layers of each sandbox, and the proxy.
- the `pats` project and the `pats-img-<key>` alias are what this leans on. they are in `lxd.md` already, and stay
  as they are from here.
- the loop is a ci's, or the action's that wraps pats. pats ships it as text, in the readme's ci notes.
- an image made by hand is exported the same way, by its own alias: `lxc image export my-rock "$dir/my-rock"`.

### using a file

- **an image the server already has under the alias** is used, as today. the directory is not looked at.
- **a file that imports is taken to be the image its name says.** pats checks what it can afterwards (below) and
  no more: the name is its writer's word.
- **the steps**, for a file found:
  1. init hashes the file. for a unified image that sha256 is the fingerprint the server will give it (checked on
     5.21), so init knows it before the server does.
  2. if the server has no image of that fingerprint, the file is imported, with no alias. an import the server
     refuses is not yet a failure: init asks for the fingerprint once more, and goes on if it is there now.
     another init importing the same file at the same moment ends so.
  3. the alias is created on the fingerprint, by `image alias create`, which makes an alias or fails and never
     re-points one. init pins the fingerprint, as it does for an image it found.
- **not `lxc image import --alias`**: the client imports and then sets the alias, replacing one that is there. a
  build of the same key that claimed the alias in between would be re-pointed.
- **no run-private alias**, unlike a build (`lxd.md`). a build has to find what it just published. an import knows
  the fingerprint from step 1.
- **the alias is there by step 3**: on this fingerprint, another init imported the same file, and all is well. on
  another, another run built the key first, or the user's alias was made meanwhile: that image is used, as after a
  build, and the imported one is left with no alias.
- **an image init was about to build is checked once it is in**: its `pats.inputs` property has to be the key
  init wants, and its `pats.version` this pats's. both are set at the publish and come back with the import. a
  file under the wrong name fails this before step 3: a warning that names it, and a build. the check goes by what
  init asked for, not by the name: an image a `pats.yaml` names, whatever it is called, has nothing to be checked
  against.
- **a step that fails** (the file cannot be read or hashed, the server refuses the import, the check above): a
  warning that names the file, and the file stays. init then asks the server for the alias once more, since
  another init may have made it meanwhile, and uses it if it is there. if not, an image pats builds is built, and
  one it does not is the error it is today.
- **the import deletes nothing from the server.** what it brings in is afterwards an image like the rest: one
  init would have built is pinned as built, so a later init that supersedes it may remove it, as it may one it
  built (`init.md`, removing superseded images). one a `pats.yaml` names is pinned as found, and is never pats's
  to remove.
- an image left there with no alias (the check failed, the alias was
  another's by step 3, the init was cut short after step 2) is found again by its fingerprint at step 2 of the
  next init with that file. until then `pats clean --all` takes it, as it takes every image of the project. while
  it is there it is one more image of the project: a reference written as the beginning of a fingerprint, which
  it happens to share, no longer names one image.
- **the run's marks** (`user.pats.run`, `user.pats.owner`, `user.pats.created`) stay the exporting machine's.
  `pats clean` reads them only beside a run-private alias (`src/sandbox/lxd_clean.go`), and an import has none.
- **what the flag cannot promise**: a file that imports, passes what can be checked, and is still not a usable
  image fails the init later, at the stamping or the first start, where a build would have passed. that is a
  hand-made image that does not work, or a file put under a name it is not. the fix is to delete the file, and
  the alias the import made on the server, which the next init would otherwise use without a look at the
  directory.
- and an image an import left on the server with no alias (above) can make the beginning of a fingerprint, which
  some `pats.yaml` on that server uses as a reference, name two images: an init with that reference, this one
  included, then fails until the image is removed. a reference by alias or by a whole fingerprint is not touched.
- short of those two, the flag never fails an init that would have passed without it: a directory or a file that
  cannot be read is a warning, and init goes on as with no flag.

### an image pats did not build

a sandbox may name an image made elsewhere: `image:`, used as it is, or `base:`, which pats provisions on top of
(`lxd.md`). under lxd either is an alias in the `pats` project, a fingerprint, or `remote:alias`.

- **an alias the server lacks** is looked for in the directory, by the rule above, and imported under that alias.
  so a hand-made image is a file in the directory and a line in `pats.yaml`:

  ```yaml
  rock-lxd:
    driver: lxd
    image: my-rock        # <dir>/my-rock.tar.zst, when the server has no such alias
  ```

- **the directory is asked last**, where a local reference is resolved (`lxd.fingerprint`), after what that tries
  today has failed: the alias, then a unique beginning of a fingerprint. so a reference that resolves today
  resolves the same. an `image:` is resolved there when init pins it, and a `base:` when the build on it works out
  its key. nothing asks for a base sooner: the arch init places a cli for is the server's, whatever the base
  (`lxd.Arch`).
- **only an alias.** a fingerprint names bytes the server has or has not, and a `remote:alias` is copied from its
  remote, as today.
- **with no file either**, init fails as it does today, and the error says the directory was looked in.
- **the alias on the server wins**, always: a newer file in the directory does not replace an image the server
  has under that name, and `--force` does not either, since the alias is the user's and not pats's to re-point. a
  ci runner starts with none. on a server that keeps its images, the user deletes the alias to take a new file.
- **the pin holds the fingerprint**, as for any `image:`, with every field it has today. a run goes by its pin
  and never reads the directory. so a file replaced under one name changes nothing until an init on a server
  without the alias imports it, which pins the new fingerprint.
- **an imported image is then one the server has**: init reads its component stamps as it does any `image:`'s,
  and one with none, or without the harness's cli, is what it is today.
- what `image:` means is unchanged: pats adds nothing to it, no provision and no harness cli
  (`internal/eval/place.go`), and gives it the `pats` user at a start if it has none. a `base:` from the directory
  is built on as any base is, and its fingerprint is in the key of what is built.

### the layers

an lxd sandbox is up to three images, each looked for on its own (`init.md`, `internal/eval/build.go`):

- **pats's own layer**, when the sandbox has no `base:` or pats places a harness cli in it.
- **the project's layer**, when the sandbox has a `provision`, and also when it has no layer of pats's own: a
  `base:` with nothing to place and no `provision` is still built once, as one layer.
- **the proxy**, when it has an egress policy.

what follows from the keys:

- **a key needs its base resolved first.** with no `base:`, the lowest layer's base is a remote's image
  (`ubuntu:26.04`), and init asks the remote for its fingerprint whether or not the layer then comes from the
  directory. so the directory saves the builds, not the network: such an init still needs the remote to answer
  with every file to hand. a `base:` that is an alias on the server, or in the directory, asks no remote.
- the project layer's key holds the fingerprint of the layer under it, so it is not known until that layer is on
  the server. an import keeps the fingerprint, since it is the tarball's hash. so the lower layer comes in from
  the directory first, the upper one's key follows from it, and its file is then found under that name.
- so each layer a sandbox has is in the directory, and each is imported: the upper one's key cannot be made
  without the lower one's fingerprint, and the pin lists the lower one. a run itself asks only for the image it
  starts from (`usePins`, `internal/eval/init.go`).
- a lower layer that was built instead has, as a rule, a new fingerprint. the upper one's key is then new, no file
  has its name, and it builds as well.
- **`--force`** builds each layer whatever its key (the flag goes to each), and does not ask the directory for
  the image it is about to build. its base is resolved as ever, the directory included: a `base:` alias the
  server lacks is the user's image, forced or not.

## the directory is its owner's

- **anything in it may be deleted, added or replaced** whenever no init is running.
- **pats adds nothing, so nothing grows by pats's hand.** an export loop run over a directory that was restored
  adds to what is there, and a changed provision script or a new pats version then leaves the images before it
  behind. a ci that keys its cache on what changes the images (`pats.yaml`, the provision scripts, the pats
  version), restores under that key alone and exports only after a miss, never carries an old image.
- a ci's cache has its own ends too: github drops one unused for 7 days, and keeps 10 GiB a repository.

## what it costs

measured on lxd 5.21, zfs, with the rock sandbox's image (1134 MiB with zstd):

- an export is a copy out of the server's store. a 193 MiB image took 9s to a client on another machine.
- an import of that image took 6s, and the first instance from the 1134 MiB image 21s.
- against that, the build it replaces is about five minutes after the zstd change.

init reads each file it uses once more, to hash it.

not measured: the hashing, the same on a `dir` pool, which a ci runner's `lxd init --auto` gives, and what the
ci's own cache takes to store and fetch two gibibytes.

## other drivers, later

a file an image, in the format its engine reads and under the name its engine knows the image by. for lxd that is
lxd's tarball, and stays so: canonical lxd reads no oci image (`init.md`, "oci images and pebble"), and a
provisioned system container is not an application image.

- **docker and podman**: an oci archive, one tar holding an image's layout, with an ending of its own to its
  name (`.oci.tar`). it is what `podman save --format oci-archive` writes and what both engines load. which
  `docker save` writes one, and whether a load keeps the image id, which is what a pin holds, differ by engine and
  version: to be checked for each before it is promised. the key would be the hash of its build's inputs, which a
  built image is labelled with now (`pats.context-hash`, `src/sandbox/build.go`).
- an archive holds every layer of its image, so two images on one base store it twice. the price of a file each.
- **bwrap**: its images are registry images fetched into its own store (`bwrap.md`). an oci archive could be a
  source for one, for a runner with no registry access.
- not now: a container build in the measured ci was a minute, and an engine has its own build cache.

nothing above is tried. what the first version fixes for them is small: a flat directory, a file an image, under
the name its engine knows the image by, with an ending that says the format.

## limits

- **the first run builds.** and two jobs of one run that miss the same image both build it.
- **github scopes a cache to its branch**: a pull request reads its base branch's and writes its own, so the base
  branch has to have run once. and it stores a directory only under a key it does not have yet, so a ci whose key
  never changes never stores a new image.
- **whoever can write the directory is trusted with the lxd server and what runs on it.** a file is a whole
  filesystem and lxd's metadata for it, under a name the writer chose, and pats checks nothing of it. the server
  parses the file at the import, before any sandbox starts. and the image lands under an alias in the `pats`
  project that every project on that server then uses by that name: a content key, or a name like `my-rock`. it is
  trusted as the provision script is, and further.
- **whoever can read the directory has the image**: everything the provision script left in it. a secret a
  provision bakes in is in the file, and in the ci's cache with it. so a provision that needs a secret does not
  leave it in the image, and a project whose image is not for a fork to read does not cache it where a fork's run
  restores.
- **a remote server** works and is slow: the export comes down to the client and the import goes back up.
- **split images**, a vm's among them, are not used (above).

## not doing

- **pats writing the directory.** an export is one command a ci already has, and a directory pats writes is one
  pats has to keep whole between two inits and a killed one.
- **removing files.** by age, by size, or a bad one: each needs pats to know what in the directory is in use. the
  owner knows, and `rm` is enough.
- **a hash in the file's name**, to check a file against. the owner's export would have to compute it and spell
  it. init hashes the file itself, to know its fingerprint, and that tells it what the file is, not what it was
  meant to be.
- **an oci image layout** for the directory: blobs by digest under an index, an lxd image in it as an artifact. it
  would let other tools list and move the cache, and it needs an index and the removal of blobs no entry names.
- **a registry as the store.** a file is pushed to one with any tool for artifacts, by whoever wants it there.
- **`lxc image copy` from a shared lxd server.** a ci runner has none to copy from.
- **caching the builder's package downloads.** it shortens a build and still runs it.
- **an automatic location.** the cache dir (`cache-dir.md`) is the machine's, and a runner's is empty. a ci names
  the directory it restores.

## changes

- `cmd/pats`, `internal/eval`: `--image-cache` on `init`, into the driver's options.
- `src/sandbox`: in `lxd.fingerprint`, the directory asked last for a local alias. `lxd.image` asks for its own
  alias that way before a build, and under a forced build asks the server only: today it resolves the alias
  before it looks at the flag. `image import` and `image alias create` on the lxd runner, and the check of a
  `pats-img-` image's properties.
- the lxd fake: `image import`, with a fingerprint that is the bytes' hash, and an alias it creates or refuses.
- docs: `init.md` (the flag), `lxd.md` (the import beside the build), the readme's ci notes with the export loop
  and the cache key.

## tests

- a file named as the alias is imported and nothing is built: no `launch`, no `publish`, no run-private alias, no
  `import --alias`. the alias is made by `alias create` on the file's hash, and the pin is the one a build would
  have written.
- no file of that name: a build, as with no flag. the directory is as it was: nothing added, nothing changed.
- an image the server has under the alias: the directory is not read.
- both layers of a sandbox come from the directory on a new server, the upper one found under a key made from the
  lower one's fingerprint.
- a damaged file: a warning that names it, a build, and the file still there.
- a cache attempt that fails after another init made the alias: that alias is used, nothing is built, and an
  `image:` does not fail.
- an image left with no alias, sharing its first digits with another: a reference by those digits no longer
  resolves, and one by alias or by the whole fingerprint does.
- a symlink, a directory and a fifo under the name: not opened, warned about, and a build.
- an import cut short before its alias, then an init with the same file: the alias is created on the image the
  server has, by the file's hash, and nothing is imported or built.
- two inits importing one file, each having seen the server without it: one import is taken, the other refused,
  and both go on to the alias and end pinned on one fingerprint. so for an `image:` as for a built image.
- the export loop names its server: with a sandbox on a `remote:`, it lists and exports there.
- the alias taken between the lookup and the claim, by a build of the key: that image is used and its alias still
  names it, nothing is built, and the imported image is left with no alias.
- a `pats-img-` file that holds another key's image, or another pats version's: a warning, a build, and no alias
  made on it.
- a reference that resolves today resolves the same with the flag: an alias, a full fingerprint, a unique
  beginning of one. the directory is asked only after those.
- a sandbox with a `base:` and no cli to place has no layer of pats's own, and one with a layer of pats's own and
  no `provision` no project layer: only the files of the layers it has are asked for.
- the export loop's pattern takes `pats-img-` and 24 digits, and not a run-private alias, nor the second alias of
  an image that has two.
- a file under another name is never opened: another key, a `.squashfs`, the two files of a split image, a file
  with no extension.
- `image: my-rock`, the server without the alias, `my-rock.tar.zst` in the directory: imported under `my-rock`,
  pinned by its fingerprint, nothing built and nothing placed in it. the same for a `base:`, which is then built
  on, with its fingerprint in the key.
- `image: my-rock` with the alias on the server: the directory is not read, with `--force` too.
- `image: my-rock` with neither: the error of today, naming the directory.
- an alias that is not one path element (`../x`, `a/b`, `.x`, `..`): no file is looked for, inside the directory
  or out of it. a fingerprint, a `remote:alias` and a reference of hex digits alone (`abc123`, with a file of
  that name there): the same, and the last resolves or fails as with no flag.
- `image: pats-img-` and 24 digits, a file of that name with no pats properties: imported and pinned as found,
  with no check.
- an imported image init would have built is pinned as built, and one a `pats.yaml` names as found.
- the file under `my-rock` replaced by another image, on a new server: the next init pins the new fingerprint.
- two files for one alias: the newer is used.
- a file of another pats version, another arch or another provision script has another name, and is not used.
- `--force` builds both layers with a file for each in the directory, and imports neither. with a `base:` alias
  the server lacks, it imports that from the directory and builds on it.
- a `base:` with no cli to place and no `provision`: one layer, looked for under its key and imported.
- nothing in the directory is ever written, removed or renamed by an init.
- a directory that cannot be read: a warning, and the init builds and pins as with no flag.
- a vm sandbox and a docker one do not read the directory.
- against a real server (`PATS_LXD_E2E`): an image exported by its alias, removed and imported from that file has
  its fingerprint and the properties set at its publish back; the export loop above writes the names init reads.

## decided, open to change

- **a flag, and no variable.** pats has no variable for a single key (`machine-file.md`, not doing). an action
  that wraps pats writes the `init` line itself and can add the flag to it. and a directory that decides which
  image an agent runs in belongs in the command a ci's log shows, not in its environment. a variable can be added
  later, and could not be taken away.
- **the lower layer is imported**, though a run starts from the upper one alone and hashing the lower one's file
  would give its fingerprint for the key. before init writes a pin it asks that every image the pin lists is on
  the server, the layers among them (`missingImage`, `internal/eval/init.go`), and the next change to a provision
  script builds on the lower layer. what skipping it would save is one import, not measured.
- **pats says nothing about what to export.** on a ci runner the server is new, so every `pats-img-` alias in the
  project is this init's, and the loop is exact. on a server several projects share it exports theirs as well,
  which is not what the flag is for. a list from `init` is an addition for whoever has that problem.

## open questions

- whether a unified image's fingerprint is its tarball's sha256 on lxd 6.x, as it is on 5.21. the import's first
  step rests on it, and the real-server test above is what says.
