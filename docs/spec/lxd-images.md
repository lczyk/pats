# lxd images: build once, decide when to rebuild

status: proposed. the command name is not final (`pats lxd-init` below; `pats init-lxd` or a flag on a general
`pats init` would do as well). `lxd.md` is the driver's design; this changes how its images are made and found.

## problem

a cold lxd run builds three images before the first pair: the pats sandbox layer on `ubuntu:26.04`, the project's
own `provision` layer on top, and the egress proxy image. on the target server that is several minutes, and the
first instance from each fresh image is slow again while the storage pool unpacks it. today every `pats run`
recomputes each image's key and builds on a miss, so any change to an input rebuilds, unasked, at the start of a run:

- a refreshed `ubuntu:26.04` (`lxc image info` fingerprint; a released series refreshes every week or two) rebuilds
  both sandbox layers;
- an edited `provision` script or anything in its `bin/` rebuilds the project layer;
- any new pats binary rebuilds the proxy image (keyed on the binary's bytes): per release for users, per commit
  while developing;
- a change to `provisionerVersion` rebuilds everything.

superseded images are never deleted (about 2.5 GB per base refresh, 190 MB per pats binary), and two projects share
a layer only by accident of identical inputs.

## what we want

1. the slow build runs when the user says so, once, and runs reuse its result.
2. a run never rebuilds an image that exists. when a local input (the provision material, the pats version) has
   changed since it was built, the run says so and names the command that rebuilds; the user decides when. a
   refreshed base is picked up by the next build, not checked on runs.
3. a run whose image is missing (a fresh server, a purge, a clean) builds it, as today: there is nothing to reuse.
4. projects on the same pats version share what does not depend on the project: the pats sandbox layer and the proxy
   image. a project's own layer is its own.
5. superseded images can be removed without guessing what another project still uses.

## design

### the command

`pats lxd-init [--sandbox <id>]... [--remote <r>]` builds every lxd image the config's sandboxes need (or the named
ones), from the current inputs, and pins what it built. it is today's build path, run on purpose: same progress
lines, same logs (to `.pats/lxd-init/<sandbox>.log`), same key. a layer whose current key already has an image is
reused and pinned, not rebuilt.

`--force` rebuilds regardless (a suspect image). the alias claim as it is (`alias create`, and a failure read as
"another run built it first") would keep the old image, so a forced build re-points the shared alias at the new
fingerprint instead: `PUT /1.0/images/aliases/<name>` through `lxc query`, one api call, no window where the alias is
missing. runs already pinned to the old fingerprint keep it (pins are by fingerprint, below) until their own
`lxd-init`.

### pins

a pin is per (remote, sandbox), in `.pats/lxd-images.json` in the project, next to `.pats/runs/`:

```json
{
  "armstrong": {
    "mason": {
      "fingerprint": "<the image's fingerprint>",
      "alias": "pats-img-<key>",
      "layers": ["<fingerprint of the pats sandbox layer>", "<fingerprint of the project layer>"],
      "proxy": {"fingerprint": "<fp>", "alias": "pats-img-<key>"},
      "pats_version": "1.0.0-beta.4",
      "pats_build": "release",
      "built": "2026-11-07T11:00:00Z",
      "inputs": {"base": "<fingerprint of ubuntu:26.04 when built>", "provision": "<sha256 of material and argv>"}
    }
  }
}
```

- **by fingerprint.** an alias is mutable (another checkout's `--force` re-points it); a fingerprint is the image.
  the alias is kept for people reading the file and for `lxc` commands.
- **under the resolved remote.** a config with no `remote:` pins under the cli's default as resolved at setup (the
  driver already pins it there), not under "", so a later change of default does not silently move the pin. remote
  names are client-local: two names for one server give two pins and, if their inputs differ, two images. harmless.
- **who writes it.** `pats lxd-init`, and a run that had to build a missing image (want 3). nothing else; in
  particular a run never rewrites a pin to record a check. writes take a lock on `.pats/lxd-images.lock`, re-read
  the file, change only their own (remote, sandbox) entries, and replace the file by rename.

`proxy` is there only for a sandbox with an egress policy; `pats_build` is `release`, or a dev build's commit with
`-dirty` when its tree was (the same test as the proxy key, below). a sandbox with `image:` names a prebuilt image:
`pats lxd-init` skips it and nothing pins it.

`pats run` resolves a sandbox's image through its pin, not through the key:

- pinned and the fingerprint is on the server: use it. compare the inputs that cost nothing to read: the provision
  material (hashed locally, as the key is today) and the pats version. if either differs, one warning per sandbox:
  `sandbox "mason": its lxd image was built from <older provision | pats 1.0.0-beta.3>; pats lxd-init rebuilds it`
  -- and the run goes on with the pinned image. a refreshed `ubuntu:26.04` is not checked on a run (one more round
  trip to the image server, for a weekly event); `pats lxd-init` picks it up whenever it runs.
- the proxy is the exception to warning and going on: it is pats's own code on the other end of the env protocol
  (`PATS_EGRESS_*`), and a proxy from another build may not speak this one's. so a pinned proxy is used only when
  the run's `pats_build` matches the pin's. otherwise the run resolves the proxy by its own key, as today (a cached
  image, or a build of about a minute: `ubuntu-minimal` plus one binary), and says so. the pin is left as it is: the
  next run of the pin's own build still finds its proxy, and resolution by key is a cache hit after the first build.
  a dev build therefore gets a fresh proxy per commit, as it does today; a release run, one per release.
- not pinned, or the pinned fingerprint is gone from the server: build it as today (want 3), pin it, and say so
  (an `image:` sandbox is neither: it runs its named image, as today):
  `sandbox "mason": no lxd image on armstrong, building it (pats lxd-init does this ahead of a run)`.

a pin is local state, like the run history: gitignored with `.pats/`. two checkouts of one project each pin; they
share images when their inputs match, since the alias is still the content key.

### names

the alias stays the content key, so identical inputs share across projects and folders without coordination
(want 4), and a pin is an exact reference. to make what is on a server readable, each image also gets properties
(`lxc image list` shows them, `lxc image show` prints them): `pats.role` (`sandbox` | `project` | `proxy`),
`pats.version`, `pats.sandbox` (the sandbox id, project layers only) and `pats.inputs` (as in the pin). a second,
human alias was considered (`pats-1.0.0-beta.4-sandbox`) and left out: one alias per image keeps the claim protocol
(run-private alias, then `alias create`) as it is, and the properties carry the same information.

the pats version goes into the key of the two shared layers and of every project layer, not only into a property, so
only the same pats version shares them (want 4): a new release gets fresh base layers even when `provision-sandbox.sh`
did not change, which is the conservative choice since the release may change what pats expects inside. a project
layer on the pats sandbox layer would inherit the version through that layer's fingerprint anyway; one on a custom
`base:` would not, so the version goes into its key directly, and every layer pats builds follows the same rule. the
proxy key becomes the pats version plus the target arch, not the binary's bytes: a release's proxy is shared by every
project on that release. a dev build keeps the binary-bytes key, since its proxy code may differ from the release's
under the same version string: the `VERSION` file names the last release on every commit after it. telling them apart:
a module install of a tagged version (the documented way to get pats) carries no vcs stamp, so `version.Info.CommitSHA
== ""` with a release-shaped version is a release; a clone build (`make install`) carries `vcs.revision` and takes the
bytes key even at the tag. when unsure, the bytes key, never the version key.

the gain is narrower than it looks: a released user whose proxy binary comes from ghcr (the registry fetch) already
gets the same bytes as everyone on that release. the version key helps a released user on linux whose own binary
goes in, whose bytes differ by go toolchain. small, but the rule above is a few lines.

dev builds share the pats sandbox layer across commits (the key has the version, not the commit). what pats bakes
into every image (the provision epilogue, the `ProvisionMount` protocol) is covered by `provisionerVersion`, bumped
by hand when it changes, as today. accepted: mixing the commit into dev keys would make every dev commit's
`lxd-init` rebuild the slowest layer.

### removing what is superseded

`pats clean --images` removes `pats-img-*` images on the server that are:

- not pinned by this project (its pins, by fingerprint);
- not run-private (`pats-img-<key>-<token>`: a build between publish and claim, which the run-scoped clean already
  owns);
- unused for 30 days: `last_used_at` (lxd sets it on every instance created from the image) or, for an image no
  instance was ever made from, `uploaded_at`. an image another project built yesterday and has not run yet is
  therefore safe.

another project's pinned image is used whenever that project runs, so a month without a use means it did not run. a
project dormant longer than that loses its images and builds them at its next run, with the line above naming
`pats lxd-init`. 30 days is fixed until someone asks for a flag. plain `pats clean` stays as it is and never touches
cached images.

## tests

the pin logic is local and testable without a server once `lxd-testing.md` lands: pins written, read, merged by two
processes (seam 3 gives two process values on one fake server), a missing fingerprint rebuilt and re-pinned, the
warning on a changed provision or version, `--force` re-pointing the alias, and `clean --images` keeping the young,
the pinned and the run-private. the e2e starts from an empty `.pats/`, so it covers want 3; it runs twice (the second
run must build nothing) to cover want 1 and 2.

## lxd 6

pats is developed and tested against lxd 5.21 (the current lts); the 6.x series is in `latest/stable` and `6/stable`
and becomes the next lts in 2026. what pats relies on, and the risk on 6.x:

- the rest api through `lxc query` (instances, networks, network-acls, images, operations, storage-pools resources,
  `/1.0`): versioned, additive. low risk.
- `lxc image info` text (`Fingerprint:`): the one place pats reads output meant for people. medium; reading the
  alias over `lxc query <remote>:/1.0/images/aliases/<a>` as json removes it (the `ubuntu:` remotes serve the same
  api), and should be done regardless.
- image remotes: `ubuntu:26.04` and `ubuntu-minimal:26.04` resolve through the client's remotes. the 6.x docs
  describe image sources as "image registries" (simplestreams, or another lxd server over a cluster link), which may
  change how a fresh client names them. medium, unknown until tried.
- bridge acls, `security.acls.default.*`, nic `name=`, `agent.nic_config`: stable config keys. low.
- `lxc publish`, `alias create`, project features, `profile device get`: low.
- the observed behaviours the driver works around (a delete during a create says not found; acls on a bridge filter
  intra-bridge traffic under `br_netfilter`): may differ either way.

no lxd 6 server is at hand. the plan: the ci lxd e2e job becomes a matrix over `5.21/stable` and `6/stable`
(`canonical/setup-lxd` takes the channel), and the contract tests of `lxd-testing.md` run in both. whatever 6.x
changes in the cli shows there first, including how `ubuntu:26.04` resolves on a fresh 6.x client. where the two
differ, the driver asks the server's version (`/1.0`, `environment.server_version`) once at setup and branches there,
not throughout.

## oci images and pebble

asked: could the published pats sandbox image (an oci image, docker's) boot directly as an lxd container with pebble
as its entrypoint, and skip provisioning?

- canonical lxd cannot pull an oci image: its image sources are simplestreams servers and other lxd servers (the
  6.x docs, checked 2026-11-07). incus, the fork, runs oci application containers since its 6.3. so on lxd pats
  would convert the image itself: its registry client already fetches and verifies layers; flattening them into a
  rootfs plus an lxd `metadata.yaml` and `lxc image import` is the rest.
- the init: an lxd system container runs `/sbin/init`; `lxc exec` does not need it, so pebble (or `sleep infinity`)
  as pid 1 would do for running commands. what pid 1 would not do is bring the network up: an ubuntu image does that
  with systemd-networkd and netplan. pats already addresses proxy pairs statically; open pairs would need the same
  (a static address from the bridge's range, a default route, a resolver) instead of dhcp.
- not covered at all: vms (an oci image has no kernel or bootloader), and the cases lxd is for (systemd, snapd, a
  real system).
- the gain: no provisioning step for the default sandbox. with pins, provisioning runs once per pats version per
  server, so the gain is one build per release.

so it is not established that it works, and with this design it buys little. recorded as not doing; revisit if
provisioning time per release becomes the bottleneck.

## open questions

- the command's name, and whether it is its own command or `pats init --lxd`.
- whether a run with a missing image should build it (as proposed, want 3) or stop and point at the command.
- where pins live when one project drives several servers: per remote in one file (proposed) or a file per remote.
