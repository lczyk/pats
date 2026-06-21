#!/usr/bin/env bash
# Compute a single sha256 over all inputs that affect a given sandbox image
# build. Drives the stamp short-circuit in build_image.sh.
#
# Usage: hash_inputs.sh sandbox-<ver>-<arch>      # e.g. sandbox-26.04-amd64
# Stdout: hex digest only.
set -euo pipefail

cd "$(dirname "$0")/.."

name="$1"
arch="${name##*-}"
rest="${name%-*}"
ver="${rest##*-}"
flavour="${rest%-*}"

case "$flavour" in
    sandbox)
        inputs=(
            "images/Dockerfile.sandbox-$ver"
        )
        ;;
    *)
        echo "unknown flavour: $flavour" >&2
        exit 2
        ;;
esac

sha256sum "${inputs[@]}" | sha256sum | cut -d' ' -f1
