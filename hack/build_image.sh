#!/usr/bin/env bash
# Build one sandbox image if its input hash differs from the stamp, then apply
# the tags a pats built from this checkout resolves to.
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

case "$flavour" in
    sandbox) ;;
    *)
        echo "unknown flavour: $flavour" >&2
        exit 2
        ;;
esac

image="$REGISTRY/sandbox:$ver-$arch"
stamp=".stamp/$name"
new=$(hack/hash_inputs.sh "$name")
cur=$(cat "$stamp" 2>/dev/null || true)

if [ "$new" = "$cur" ]; then
    echo "==> ${flavour}:${ver}-${arch} up-to-date (stamp matches)"
else
    echo "==> building ${flavour}:${ver}-${arch} (inputs changed)"
    "$DOCKER" build \
        --tag "$image" \
        --file "images/Dockerfile.sandbox-$ver" \
        --platform "linux/$arch" \
        .
    echo "$new" > "$stamp"
fi

# the arch-less tags -- :v<version> (the pinned default), :latest, and the
# plain ubuntu tag for a pats.yaml that names it -- resolve offline only for
# the host's arch, so only that build applies them. they are re-applied on
# every run: a VERSION bump moves :v<version> without a rebuild. multiarch
# manifests are assembled separately at publish time.
host=$("$DOCKER" version --format '{{.Server.Arch}}')
if [ "$arch" = "$host" ]; then
    for tag in "v$VERSION" latest "$ver"; do
        "$DOCKER" tag "$image" "$REGISTRY/sandbox:$tag"
    done
fi
