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
2. a run never rebuilds an image that exists. when an input has changed since it was built, the run says so and
   names the command that rebuilds; the user decides when.
3. a run whose image is missing (a fresh server, a purge, a clean) builds it, as today: there is nothing to reuse.
4. projects on the same pats version share what does not depend on the project: the pats sandbox layer and the proxy
   image. a project's own layer is its own.
5. superseded images can be removed without guessing what another project still uses.

## design

### the command

`pats lxd-init [--sandbox <id>]... [--remote <r>]` builds every lxd image the config's sandboxes need (or the named
ones), from the current inputs, and records what it built. it is today's build path, run on purpose: same progress
lines, same logs (to `.pats/lxd-init/<sandbox>.log`), same key. it rebuilds a layer only if its current key has no
image; `--force` rebuilds regardless (a refreshed base under the same key cannot happen, since the key carries the
base fingerprint, so `--force` is for a suspect image).

### pins

`pats lxd-init` writes a pin per (remote, sandbox): `.pats/lxd-images.json` in the project, next to `.pats/runs/`.

```json
{
  "armstrong": {
    "mason": {
      "image": "pats-img-<key>",
      "layers": ["pats-img-<sandbox key>", "pats-img-<project key>"],
      "proxy": "pats-img-<proxy key>",
      "pats_version": "1.0.0-beta.4",
      "built": "2026-11-07T11:00:00Z",
      "inputs": {"base": "<fingerprint>", "provision": "<sha256 of the material tree and argv>"}
    }
  }
}
```

`pats run` resolves a sandbox's image through its pin, not through the key:

- pinned and the image is on the server: use it. then check the inputs cheaply (the provision material is hashed
  locally; the pats version is known; the base fingerprint costs one `lxc image info` against the image server, so
  it is checked at most once a day, its last check recorded in the pin). if any differ, one warning per sandbox:
  `sandbox "mason": its lxd image predates <what changed>; pats lxd-init rebuilds it` -- and the run goes on with
  the pinned image.
- not pinned, or the pinned image is gone from the server: build it as today (want 3) and pin it, as if
  `pats lxd-init` had run for that sandbox, with a line saying so.

a pin is local state, like the run history: gitignored with `.pats/`. two checkouts of one project each pin; they
share the images when their inputs match, since the alias is still the content key.

### names

the alias stays the content key, so identical inputs share across projects and folders without coordination
(want 4), and a pin is an exact reference. to make what is on a server readable, each image also gets properties
(`lxc image list` shows them, `lxc image show` prints them): `pats.role` (`sandbox` | `project` | `proxy`),
`pats.version`, `pats.sandbox` (the sandbox id, project layers only) and `pats.inputs` (as in the pin). a second,
human alias was considered (`pats-1.0.0-beta.4-sandbox`) and left out: one alias per image keeps the claim protocol
(run-private alias, then `alias create`) as it is, and the properties carry the same information.

the pats version goes into the key of the two shared layers, not only into a property, so only the same pats version
shares them (want 4): a new release gets fresh base layers even when `provision-sandbox.sh` did not change, which is
the conservative choice since the release may change what pats expects inside. the proxy key becomes the pats
version plus the target arch, not the binary's bytes: a release's proxy is shared by every project on that release.
a dev build keeps the binary-bytes key, since its proxy code may differ from the release's under the same version
string: the `VERSION` file names the last release on every commit after it. telling the two apart needs the build's
vcs stamp (commit, dirty) against the release tag; `internal/version` reads the stamp, and whether it can say "this
is the tagged release" is to be checked.

### removing what is superseded

`pats clean --images [--older <dur>]` removes `pats-img-*` images on the server that are not pinned by this project
and whose `last_used_at` (lxd records it per image, set on every instance created from it) is older than `--older`
(default 30 days). another project's pinned image is used at least whenever that project runs, so a month without a
use means no one ran it. plain `pats clean` stays as it is: it never touches cached images.

## lxd 6

pats is developed and tested against lxd 5.21 (the current lts); the 6.x series is in `latest/stable` and `6/stable`
and becomes the next lts in 2026. what pats relies on, and the risk on 6.x:

- the rest api through `lxc query` (instances, networks, network-acls, images, operations, storage-pools resources,
  `/1.0`): versioned, additive. low risk.
- `lxc image info` text (`Fingerprint:`): the one place pats reads output meant for people. medium; reading the
  alias over `lxc query /1.0/images/aliases/<a>` as json removes it, and should be done regardless.
- image remotes: `ubuntu:26.04` and `ubuntu-minimal:26.04` resolve through the client's remotes. the 6.x docs
  describe image sources as "image registries" (simplestreams, or another lxd server over a cluster link), which may
  change how a fresh client names them. medium, unknown until tried.
- bridge acls, `security.acls.default.*`, nic `name=`, `agent.nic_config`: stable config keys. low.
- `lxc publish`, `alias create`, project features, `profile device get`: low.
- the observed behaviours the driver works around (a delete during a create says not found; acls on a bridge filter
  intra-bridge traffic under `br_netfilter`): may differ either way.

no lxd 6 server is at hand. the plan: the ci lxd e2e job becomes a matrix over `5.21/stable` and `6/stable`
(`canonical/setup-lxd` takes the channel), and the contract tests of `lxd-testing.md` run in both. whatever 6.x
changes in the cli shows there first. where the two differ, the driver asks the server's version (`/1.0`,
`environment.server_version`) once at setup and branches there, not throughout.

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
- whether the daily base-fingerprint check is wanted at all, or the warning should cover only what is local
  (provision material, pats version).
- where pins live when one project drives several servers: per remote in one file (proposed) or a file per remote.
