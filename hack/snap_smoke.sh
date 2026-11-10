#!/usr/bin/env bash
# check an installed pats snap without a daemon: the build tags it was packed
# with, and the load errors a reduced build gives a config it cannot run.
set -e

PATS="${PATS:-pats}"
TAGS="nohost,nobwrap,nopodman"

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

main() {
    local version
    version="$("${PATS}" --version)"
    printf 'using %s (%s)\n' "$(command -v "${PATS}")" "${version}"
    printf '%s\n' "${version}" | grep -qF "(tags: ${TAGS})" || _fail "tags: ${version}"
    printf 'ok    tags\n'

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
}

main "$@"
