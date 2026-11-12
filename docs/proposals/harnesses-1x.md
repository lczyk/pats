# proposal: harnesses on 1.x

status: steps 1 and 2 implemented, the rest proposed. four specs: `docs/spec/machine-config.md` (the file pats reads its harnesses and clis from),
`docs/spec/harness-registry.md` (what a harness is, and `kind: harness` in a `pats.yaml`),
`docs/spec/harness-parse.md` (a harness's output read by a script) and `docs/spec/harness-images.md` (how a
harness's cli gets into an image, and its pin). this is the order they land in and what each step costs.

## why before 1.0

each of these is part of what 1.0 promises to keep: the harness names, the keys of an entry and of a `kind: harness`
item, the calls of a `parse` script, where the machine config is, what the published sandbox image holds, and what
a pin is. after 1.0 these are breaks. what is only additive (a new default, a third table) can wait.

## steps

sizes: S is under a day, M a few days, L a week or more.

1. **the table, from the embedded defaults** (M). the `harnesses` and `clis` tables, their loader and the defaults
   file replace the per-harness switches and maps. the defaults' `clis` hold a name, a version, `login-file` and
   `home-env`, and a test holds that they name the versions of `images/harness-versions`, which the images still
   build from. the go readers stay for this step, chosen by a key only the defaults may use. no behaviour changes;
   the existing tests are the check. everything below builds on it.
2. **`parse`** (M). starlark with `json` and `re`, `out` and its rules, the two passes, the per-line errors, `pats
   harness parse`, and the four readers as scripts in the defaults, each behind the recorded-run tests of the go
   reader it replaces. the go readers and the step-1 key go. `max-cost` loads on every agent, with its
   warning. one new dependency.
3. **the machine's file** (S-M). where it is, the overlay and its errors, `pats config`, the note, `harness_from`
   and `harness_sha256`, the snap's `XDG_CONFIG_HOME`. the `harnesses` table only: a `clis` table in a machine's
   file is an error until step 6, since a pin there would name a version the images do not yet build from. a
   machine can now correct a default's command, hosts or script, and add a harness whose files a `provision`
   brings.
4. **`kind: harness`** (M). items laid over the machine's harnesses, `model` optional where `run` does not name
   `PATS_MODEL`, `PATS_PREFLIGHT`, `pats list`. `pats.example.yaml` gains omp, hermes and a no-op baseline.
5. **artefacts and the cache** (M). `artefacts` on a `clis` entry, the checked fetch, the tree,
   `pats harness pin`. nothing uses the trees yet: the images still hold their clis and still build from
   `images/harness-versions`, which stays until step 6. so this can land and be tried (`pats harness pin` against
   today's pins) before anything depends on it. the machine's `clis` stay closed for the same reason.
6. **placement** (L). the set from the config; the generated dockerfile's `COPY` layers; the lxd layer; bwrap's
   lower dirs; `arch` and `harnesses` in the pin entry; the run's missing and stale checks; the machine's `clis`,
   and the cli's pin in `harness_sha256`; the harness-free published image and provision script;
   `images/harness-versions` goes, and with it its two other readers: the makefile's embed prerequisites take the
   defaults file, and `hack/hash_inputs.sh` drops the pins from the inputs of an image that no longer holds a
   harness. this is the break, in one release: an image made by an older init is stale in every project, and
   `pats init` remakes it.
7. **pins in a project** (S). `version` and `sha256` on an item, on top of 5 and 6.
8. **`pi-openrouter`** (S, plus a recorded run for its script). the first default that is an entry and nothing
   else.

2 comes before 3 so that `parse` is a script from the first release that lets anyone write one. 1 to 4 need nothing
of 5 to 7. 6 is the only step that cannot be split across releases: the image loses its clis, and a machine's file
gains its `clis`, in the release that places them.

## what breaks, in one list

- the published `sandbox` image holds no harness (step 6). a `build:` dockerfile on `FROM ${PATS_SANDBOX}`, and an
  `image:` naming `sandbox:latest` or `sandbox:26.04`, bring their own clis from then on.
- `images/harness-versions` goes (step 6). the pins are the `clis` of the defaults.
- a `provision` that installed its own harness version to override a pin now sits over a cli pats placed; the pin
  moves to an item instead (step 7).
- `max-cost` on an agent whose harness reports no cost as it runs is no longer a load error (step 2). it loads, and
  warns.
- nothing in a `pats.yaml` that uses only the defaults, the default image or `provision:` changes. mason's is such
  a config.

## left out on purpose

- keeping a credential out of the sandbox (a proxy that swaps a placeholder for the real key). not what an eval
  needs.
- harness images in the registry, one per cli, composed at init. it needs an image per cli and release, a second
  mechanism for lxd, and gives nothing the checked fetch does not.
- an install script per harness, run in the image build. see `harness-images.md`, "decided".
- a pins file of its own, and a url on a project's pin. the pin is a `clis` entry's, and where a version comes
  from is the machine's to say (`machine-config.md`).
- one harness starting from another by name. an entry changes the one of its name or is a copy
  (`harness-registry.md`).
