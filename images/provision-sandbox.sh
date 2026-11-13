#!/bin/sh
# provisions the pats sandbox: ubuntu + the tooling agents habitually reach
# for (git, curl, python3, ripgrep, node). run as root, once per image: by
# Dockerfile.sandbox-26.04 for the published docker image, and by the lxd
# driver on a fresh ubuntu instance, so both carry the same tools.
#
# no harness cli is installed here: pats places the ones a sandbox's agents
# run when it makes the sandbox's image (docs/spec/harness-images.md).
set -eu

export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    git \
    nodejs \
    npm \
    python3 \
    ripgrep
apt-get clean
rm -rf /var/lib/apt/lists/*
