#!/usr/bin/env bash
# pair.sh <engine> runs suite <engine> (docker, lxd) of the project beside
# this script with $PATS, in a copy of it under the home: its steps in a step
# sandbox of that engine, its stub agent in a docker one. it checks what the
# steps left, the score and that clean finds nothing left, and makes an image
# of that engine with a harness cli placed in it.
set -e

PATS="${PATS:-pats}"
FIXTURE="$(cd "$(dirname "$0")/project" && pwd)"
STAMP="$(dirname "$0")/stamp"

_fail() {
    printf 'FAIL  %s\n' "$1" >&2
    exit 1
}

main() {
    local engine="$1" dir pair out want
    case "${engine}" in
        (docker|lxd) ;;
        (*) _fail "unknown engine: ${engine}" ;;
    esac
    printf 'using %s (%s)\n' "$(command -v "${PATS}")" "$("${PATS}" --version)"
    # under the home: a strict snap sees nothing of the host's /tmp.
    dir="$(mktemp -d -p "${HOME}")"
    cp -R "${FIXTURE}/." "${dir}"
    cd "${dir}"
    git init -q .
    "${STAMP}" "pair ${engine}: init agent and steps"
    "${PATS}" init --sandbox agent --sandbox "steps-${engine}" || _fail "${engine}: init"
    # a cli fetched into pats's cache and placed in an image pats composes:
    # its stamp in the pin is the cli run inside that image.
    "${STAMP}" "pair ${engine}: init placed"
    "${PATS}" init --sandbox "placed-${engine}" || _fail "${engine}: init of a placed cli"
    grep -qE '"pi": "[0-9]+\.[0-9]+' .pats/images.json || _fail "${engine}: the placed cli has no stamp"
    "${STAMP}" "pair ${engine}: run"
    OPENROUTER_API_KEY=fake-key-for-spread "${PATS}" run -S "${engine}" -v --offline || _fail "${engine}: run"
    "${STAMP}" "pair ${engine}: score and clean"
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
    # an engine this system lacks is skipped with a warning, and another
    # system's runs on a shared daemon are another machine's: neither is
    # left behind.
    out="$(printf '%s\n' "${out}" |
        grep -v -e '^WARN [a-z]*: skipped (' -e '^INFO [a-z]*: left [0-9]* run(s) this clean cannot judge' || true)"
    [ -z "${out}" ] || _fail "${engine}: left behind: ${out}"
    cd - > /dev/null
    rm -rf "${dir}"
    "${STAMP}" "pair ${engine}: done"
    printf 'ok    %s pair\n' "${engine}"
}

main "$@"
