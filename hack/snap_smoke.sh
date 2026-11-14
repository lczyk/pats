#!/usr/bin/env bash
# check an installed pats snap: the build tags it was packed with, that its
# binary is static and stamped as the snap's version says, and the load
# errors a reduced build gives a config it cannot run. with an engine
# named (docker, lxd) it also runs and scores a pair of the project in
# hack/snap-smoke, its steps in a step sandbox of that engine, and makes an
# image of that engine with a harness cli placed in it: that takes the
# engine's plug connected.
#
#   hack/snap_smoke.sh [docker] [lxd]
#
# PAIR_ONLY=1 skips the checks of a reduced build, for a pats that is not the
# snap: the pairs alone, as a clone's bin/pats runs them.
set -e

PATS="${PATS:-pats}"
SNAP_DIR="${SNAP_DIR:-/snap/pats/current}"
TAGS="nohost,nobwrap,nopodman"
FIXTURE="$(cd "$(dirname "$0")/../tests/spread/lib/project" && pwd)"

_fail() {
    printf 'FAIL  %s\n' "$1" >&2
    exit 1
}

# _load_error <name> <config> <want> runs pats on config and wants its error.
_load_error() {
    local name="$1" config="$2" want="$3" dir out
    # under the home: a strict snap sees nothing of the host's /tmp.
    dir="$(mktemp -d -p "${HOME}")"
    printf '%s\n' "${config}" > "${dir}/pats.yaml"
    local rc=0
    out="$(cd "${dir}" && "${PATS}" run 2>&1)" || rc=$?
    rm -rf "${dir}"
    [ "${rc}" -ne 0 ] || _fail "${name}: pats run passed"
    printf '%s\n' "${out}" | grep -qF "${want}" || _fail "${name}: ${out}"
    printf 'ok    %s\n' "${name}"
}

# _pair <engine> runs suite <engine> of the fixture, in a copy of it under the
# home, and checks what its steps left and its score.
_pair() {
    local engine="$1" dir pair out want
    dir="$(mktemp -d -p "${HOME}")"
    cp -R "${FIXTURE}/." "${dir}"
    cd "${dir}"
    git init -q .
    "${PATS}" init --sandbox agent --sandbox "steps-${engine}" || _fail "${engine}: init"
    # a cli fetched into the snap's cache and placed in an image pats
    # composes: its stamp in the pin is the cli run inside that image.
    "${PATS}" init --sandbox "placed-${engine}" || _fail "${engine}: init of a placed cli"
    grep -qE '"pi": "[0-9]+\.[0-9]+' .pats/images.json || _fail "${engine}: the placed cli has no stamp"
    OPENROUTER_API_KEY=fake-key-for-smoke "${PATS}" run -S "${engine}" -v --offline || _fail "${engine}: run"
    pair=".pats/runs/latest/stub/t-${engine}"
    [ -d "${pair}" ] || _fail "${engine}: no pair dir"
    grep -qx 'home=/tmp/pats-step-home' "${pair}/stage.log" || _fail "${engine}: stage.log: $(cat "${pair}/stage.log")"
    # docker steps run as the host uid, lxd ones as the instance's own
    # non-root agent user.
    want="$(id -u)"
    [ "${engine}" = docker ] || want='[1-9][0-9]*'
    grep -qx "${want}" "${pair}/stage-uid" || _fail "${engine}: stage ran as $(cat "${pair}/stage-uid")"
    grep -qx helped "${pair}/collected" || _fail "${engine}: collected: $(cat "${pair}/collected")"
    grep -qF '"steps"' "${pair}/metadata.json" || _fail "${engine}: no steps in metadata"
    out="$("${PATS}" score -S "${engine}" 2>&1)" || _fail "${engine}: score: ${out}"
    printf '%s\n' "${out}" | grep -qE "^t-${engine} +1\.00" || _fail "${engine}: score: ${out}"
    out="$("${PATS}" clean -n 2>&1)" || _fail "${engine}: clean: ${out}"
    # an engine the runner lacks is skipped with a warning, not left behind.
    out="$(printf '%s\n' "${out}" | grep -v '^WARN [a-z]*: skipped (' || true)"
    [ -z "${out}" ] || _fail "${engine}: left behind: ${out}"
    cd - > /dev/null
    rm -rf "${dir}"
    printf 'ok    %s pair\n' "${engine}"
}

# _stamp <version line> checks the installed binary against the snap's own
# version: a dev build (+git<sha7>) carries that commit, a release none, and
# neither is stamped as modified.
_stamp() {
    local version="$1" snap sha
    snap="$(grep '^version:' "${SNAP_DIR}/meta/snap.yaml")" || _fail "stamp: no version in the snap's meta"
    case "${snap}" in
        (*+git*)
            sha="${snap##*+git}"
            sha="${sha%%[!0-9a-f]*}"
            # the version line shows seven characters, whatever git needed.
            sha="${sha:0:7}"
            printf '%s\n' "${version}" | grep -qF " @ ${sha} " || _fail "stamp: ${snap} is not the commit of ${version}"
            ;;
        (*)
            printf '%s\n' "${version}" | grep -qF ' @ ' && _fail "stamp: ${snap} is a release, but carries a commit: ${version}"
            ;;
    esac
    printf '%s\n' "${version}" | grep -qF dirty && _fail "stamp: stamped as modified: ${version}"
    printf 'ok    stamp\n'
}

main() {
    local version engine
    version="$("${PATS}" --version)"
    printf 'using %s (%s)\n' "$(command -v "${PATS}")" "${version}"
    # the pairs alone, with none named, would check nothing and pass.
    if [ -n "${PAIR_ONLY:-}" ] && [ "$#" -eq 0 ]; then
        _fail "PAIR_ONLY needs an engine: docker, lxd"
    fi
    if [ -z "${PAIR_ONLY:-}" ]; then
        printf '%s\n' "${version}" | grep -qF "(tags: ${TAGS})" || _fail "tags: ${version}"
        printf 'ok    tags\n'

        # static, so lxd can push this very binary into its proxy instance.
        readelf -l "${SNAP_DIR}/bin/pats" > /dev/null || _fail "static: readelf cannot read ${SNAP_DIR}/bin/pats"
        readelf -l "${SNAP_DIR}/bin/pats" | grep -q INTERP && _fail "static: pats is not a static binary"
        printf 'ok    static\n'

        _stamp "${version}"

        _load_error "no host driver" '
sandbox: {kind: sandbox, items: {h: {driver: host, sand-not-boxed: true}}}
agents: {kind: agent, items: {a: {harness: opencode-openrouter, model: m, sandbox: h}}}
tasks: {kind: task, items: {t: {prompt: p}}}
suites: {kind: suite, items: {s: {agents: a, tasks: t}}}' \
            'driver "host" is not in this build'

        _load_error "no host steps" '
sandbox: {kind: sandbox, items: {d: {driver: docker, image: docker.io/library/ubuntu:24.04}}}
agents: {kind: agent, items: {a: {harness: opencode-openrouter, model: m, sandbox: d}}}
tasks: {kind: task, items: {t: {prompt: p, stage: echo hi}}}
suites: {kind: suite, items: {s: {agents: a, tasks: t}}}' \
            'this build runs no host steps'
    fi

    for engine in "$@"; do
        case "${engine}" in
            (docker|lxd) _pair "${engine}" ;;
            (*) _fail "unknown engine: ${engine}" ;;
        esac
    done
}

main "$@"
