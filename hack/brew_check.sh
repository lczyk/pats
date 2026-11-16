#!/usr/bin/env bash
# Audit Formula/pats.rb and install it from a local tap, in the homebrew/brew
# image. The image's brew is the linux one, so this installs the linux
# tarball of the runner's arch.
#
# Usage: brew_check.sh
set -euo pipefail

cd "$(dirname "$0")/.."

IMAGE="homebrew/brew@sha256:b0072bfdebf5934ae24b93b44a1928a88057399b3283ffa0177bb86084fdedfd"

docker run --rm -v "$PWD/Formula:/formula:ro" -e HOMEBREW_NO_AUTO_UPDATE=1 -e HOMEBREW_NO_ANALYTICS=1 \
    "${IMAGE}" bash -euo pipefail -c '
        brew=/home/linuxbrew/.linuxbrew/bin/brew
        tap="$($brew --repository)/Library/Taps/lczyk/homebrew-pats"
        mkdir -p "$tap/Formula"
        cp /formula/pats.rb "$tap/Formula/"
        $brew style lczyk/pats/pats
        $brew audit --strict --online lczyk/pats/pats
        $brew install lczyk/pats/pats
        $brew test lczyk/pats/pats
    '
