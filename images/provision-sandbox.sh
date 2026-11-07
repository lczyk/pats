#!/bin/sh
# provisions the pats sandbox: ubuntu + the agent harness clis + the tooling
# agents habitually reach for (git, curl, python3, ripgrep). run as root, once
# per image: by Dockerfile.sandbox-26.04 for the published docker image, and
# by the lxd driver on a fresh ubuntu instance, so both carry the same tools.
#
# clis go in globally (npm -g -> /usr/*) so they're on PATH for any uid: the
# docker agent runs as the host uid with no home of its own, so a per-user
# install (~/.local) would be invisible to it. pin cli versions before
# publishing; floating latest for now.
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

npm install -g \
    "@anthropic-ai/claude-code@${CLAUDE_CLI_VERSION:-latest}" \
    "@openai/codex@${CODEX_CLI_VERSION:-latest}" \
    "opencode-ai@${OPENCODE_VERSION:-latest}"
npm cache clean --force
