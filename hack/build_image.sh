#!/usr/bin/env bash
# Build one sandbox image if its input hash differs from the stamp.
# Usage: build_image.sh sandbox-<ver>-<arch>      # e.g. sandbox-26.04-amd64
set -euo pipefail

cd "$(dirname "$0")/.."

DOCKER="${DOCKER:-docker}"
REGISTRY="${REGISTRY:-ghcr.io/lczyk/pats}"
VERSION=$(cat VERSION)

name="$1"
arch="${name##*-}"
rest="${name%-*}"
ver="${rest##*-}"
flavour="${rest%-*}"

stamp=".stamp/$name"
new=$(hack/hash_inputs.sh "$name")
cur=$(cat "$stamp" 2>/dev/null || true)

if [ "$new" = "$cur" ]; then
    echo "==> ${flavour}:${ver}-${arch} up-to-date (stamp matches)"
    exit 0
fi

echo "==> building ${flavour}:${ver}-${arch} (inputs changed)"

case "$flavour" in
    sandbox)
        "$DOCKER" build \
            --tag "$REGISTRY/sandbox:$ver-$arch" \
            --file "images/Dockerfile.sandbox-$ver" \
            --platform "linux/$arch" \
            .
        # also apply the tags a pats built from this checkout resolves to --
        # :v<version> (the pinned default) and :latest -- plus the plain
        # ubuntu tag for a pats.yaml that names it, so each resolves offline.
        # multiarch manifests are assembled separately at publish time.
        for tag in "v$VERSION" latest "$ver"; do
            "$DOCKER" tag "$REGISTRY/sandbox:$ver-$arch" "$REGISTRY/sandbox:$tag"
        done
        ;;
    *)
        echo "unknown flavour: $flavour" >&2
        exit 2
        ;;
esac

echo "$new" > "$stamp"
