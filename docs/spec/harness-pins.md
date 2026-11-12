# harness versions per pats release

status: superseded by `harness-images.md`: the pins are the `clis` of the machine config's defaults
(`internal/machine/defaults.yaml`), each with a sha256 per arch, and `images/harness-versions`, the script's
reading of it and the dockerfile's build args are gone. what follows is as it was implemented before that: the pins
file, the provision script and dockerfile reading it, the lxd image key and the hash inputs. the per-release bump is a manual step until the "later" items land; the release workflow around it is out of
scope here.
`init.md` relies on it: its component-version check compares an image's stamped harness versions against what the
running pats release pins.

## problem

the sandbox installs its harness clis at `@latest`: `images/provision-sandbox.sh` runs `npm install -g
@anthropic-ai/claude-code@${CLAUDE_CLI_VERSION:-latest}` and the same for copilot, codex and opencode, and
`images/Dockerfile.sandbox-26.04` defaults the four build args to `latest`. so what an image holds depends on when it
was built:

- the published docker sandbox image of a release and an lxd image provisioned from the same script a week later carry
  different harnesses;
- two `pats init` runs of one release on two machines may too;
- a score cannot be traced to the harness that produced it, beyond what the stamps (`init.md`) record.

## proposal

- **one file of pins.** `images/harness-versions` holds one `NAME=version` line per harness
  (`CLAUDE_CLI_VERSION=2.3.1`), exact versions, no ranges or `latest`. the provision script reads it for its defaults,
  the dockerfile copies it in before the provision step, the lxd provisioner pushes it with the script, and
  `hack/hash_inputs.sh` lists it as a sandbox image input beside the dockerfile and the script (or a pins-only bump
  reads as up to date there). an env var or build arg already set still wins over the file (the dockerfile's build
  args lose their `latest` default, so unset means the file), for a project that wants another version in its own
  layer; the stamps record what it got.
- **part of the image key.** the file's contents go into the lxd image key, so a pin change rebuilds the lxd image at
  the next `pats init`, as a provision edit does. the container drivers pull the published sandbox image, so they get
  a pin change with the release that carries it.
- **bumped on each release.** before a release, each pin moves to that harness's latest stable version (npm's `latest`
  dist-tag), the e2e jobs run against the bumped set, and the release notes list the versions. a release that has to
  hold one back says so in the notes.
- **between releases** the pins stay put. a dev build gets what the file in its tree says, which is the next release's
  set once bumped.

## later, not designed here

- a make target or script that bumps the file to the current `latest` tags and shows the diff.
- where the bump sits in the release workflow (a step in a release checklist, a ci job opening a pull request, a
  scheduled check), and what tests gate it.
- whether a harness that updates often (weekly) needs bumps between pats releases, as a pats patch release.