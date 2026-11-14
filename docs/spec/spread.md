# system tests with spread

status: implemented. `spread.yaml` at the root, its systems made by `hack/spread/`, its tasks in `tests/spread/`.
`make spread` runs it. ci runs the bread suite in `lint_and_test.yml` and the snap suite in `snap.yml`, which
`publish` waits on.

## purpose

unit tests fake the engines, and the e2e tests (`PATS_LXD_E2E`, `PATS_BWRAP_E2E`) drive one engine from go. what
neither does is run pats the way a user does, on a whole system: the cli from `init` to `clean`, the snap through its
plugs, an engine set up the way an admin sets it up, a firewall in the way. spread
(github.com/canonical/spread) runs shell tasks on disposable systems that a backend makes and removes, so each task
starts from a known machine and may change it as it likes.

## backends

both are adhoc: a script makes the system and hands spread its ssh address, another removes it.

### bread: pats from this tree, on docker

a plain bread container (github.com/lczyk/spread-bread: ubuntu and sshd) per system, on ubuntu 24.04 and 26.04 for
amd64 and arm64, driving the host's docker through its socket. cheap: no vm, no nesting, the host's image cache.

- **what goes in:** pats built by `make spread` (static, per arch, into `.spread/`), and the docker cli and buildx
  copied out of docker's own cli image, pinned by digest. bread ships neither, and pats builds with buildkit.
- **one path, both sides:** the sandboxes pats starts are siblings of the bread container on the host daemon, which
  resolves their bind mounts against the host. so allocate binds a host dir at the same path in the container,
  `$PATS_SPREAD_SHARED/<name>` (default under `$TMPDIR`, which docker desktop shares with its vm), names it in
  `/etc/pats-spread-dir`, and the suite puts `HOME` and the projects there.
- **ownership:** the container's hostname is its name, so it heads the owner label (`pats.owner`,
  `<host>/<uid>/<client id>`) of all that pats starts from it. workers on one daemon never clean each other's runs.
- **removal:** discard runs `pats clean` inside, then removes whatever still carries the container's owner label,
  then the shared dir from inside (its files are root's on a linux host), then the container. a failed allocation is
  undone by allocate itself, since spread never hands it to discard.
- **reaching it:** the bridge address on linux, a published port of 127.0.0.1 on macos (`BREAD_NET` overrides).

no systemd and no snapd, and the sandboxes share the host's network: the snap, lxd and firewalls are the lxd
backend's.

### lxd: the snap, and the engines nested

an ubuntu system container per release (22.04, 24.04, 26.04) on the local lxd, of the host's arch, with
`security.nesting` and the mknod and setxattr intercepts, so docker and lxd run inside it. allocate pushes in the snap
under test (`.spread/pats_<arch>.snap`) and pats from this tree, and opens root logins by password for spread.

- **as a user would:** the suite installs the snap and makes a `tester` user; each engine is installed as an admin
  would and the tester put in its group (`tests/spread/lib/engine`), and pats runs as the tester (`as-tester`).
- **tasks share a system:** `restore-each` (`lib/reset-system`) removes the engine snaps and ufw, stops the apt
  docker and drops its state, flushes netfilter, deletes the bridges and turns forwarding off, since one task's
  firewall rules would decide the next one's. the apt docker stays installed: purged and installed again, it never
  gets its socket unit back.
- **firewall preconditions:** docker 28 and later drop forwarded traffic only when they turn forwarding on themselves,
  so the docker variant turns it off before docker starts.
- **local lxd only:** a remote's containers sit on the server's bridge, out of reach of the machine running spread.

## suites

`tests/spread/lib/` holds what both use: `pair.sh <engine>` (the smoke project's pair, its steps in a step sandbox of
that engine, its stub agent under docker, scored and cleaned, and an image with a harness cli placed in it) and that
project.

- **bread:** `pair` (the docker pair) and `kill-clean` (pats killed mid-pair leaves its sandbox, a sibling on the
  daemon, and `pats clean` removes it).
- **snap:** `basics` (build tags, a static binary, the version stamp, the load errors of a reduced build) on every
  release; on 24.04 and 26.04, `docker` (the plug on an apt docker, the plug on the docker snap, and a pats that is
  not the snap against the docker snap), `lxd` (a pair behind an egress policy, on the lts and the latest lxd) and
  `firewall` (under ufw, and under docker's FORWARD policy, an lxd pair fails with pats's hint, and the rules it
  prints make it pass).

## running it

`make spread` runs the bread suite on the host's arch; `SYSTEMS=..` narrows it (`bread:ubuntu-26.04-arm64`,
`lxd:`), `ARGS=..` passes flags (`-v`, `-reuse`, `-debug`). the lxd suite needs a local lxd and the snap in
`.spread/` (`snapcraft pack`), which ci takes from the build job. `make spread-smoke` runs the snap suite's smoke
jobs (`SPREAD_SMOKE`: `basics` everywhere, docker pairs on the newest ubuntu).

in `snap.yml`, `publish` waits on the snap suite:

- **smoke:** on every pull request and push to main, so edge ships on it. no lxd pair: one takes ~8m nested, most of
  it pats composing and pulling lxd images, and the lxd e2e tests cover the driver on every change.
- **whole:** on a release tag, so beta ships on it, on a pull request that changes `VERSION` (the release's, so a red
  suite is found before the tag, which has to be `VERSION`), weekly and by hand. each release on its own runner
  (`SYSTEMS=lxd:ubuntu-24.04`), since nested engines on one runner slow each other down.

spread shows a passing task's output to no one, so the snap suite stamps its steps' times (`tests/spread/lib/stamp`),
the lxd discard pulls them into `.spread/stamps/`, and ci prints them.

## later

- the shared dir, once settled, upstream in bread's own socket-mode allocate.
- a privileged bread backend for bwrap and podman, which unshare and mount inside the system.
- lxd vms where there is kvm, for what a container cannot show.
