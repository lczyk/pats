#!/bin/sh
# provisions the pats sandbox: ubuntu + the agent harness clis + the tooling
# agents habitually reach for (git, curl, python3, ripgrep). run as root, once
# per image: by Dockerfile.sandbox-26.04 for the published docker image, and
# by the lxd driver on a fresh ubuntu instance, so both carry the same tools.
#
# clis go in globally (npm -g -> /usr/*) so they're on PATH for any uid: the
# docker agent runs as the host uid with no home of its own, so a per-user
# install (~/.local) would be invisible to it.
#
# cli versions come from harness-versions next to this script, unless the env
# already sets one (a build arg, or a project's own layer pinning another).
set -eu

pins="$(dirname "$0")/harness-versions"
[ -f "$pins" ] || { echo "provision-sandbox: $pins is missing" >&2; exit 1; }
while IFS='=' read -r name ver; do
    case "$name" in
        '' | '#'*) continue ;;
        *[!A-Z_]*) echo "provision-sandbox: bad pin name: $name" >&2; exit 1 ;;
    esac
    eval "cur=\${$name:-}"
    [ -n "$cur" ] || eval "$name=\$ver"
done < "$pins"

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
    "@anthropic-ai/claude-code@${CLAUDE_CLI_VERSION:?no pin}" \
    "@github/copilot@${COPILOT_CLI_VERSION:?no pin}" \
    "@openai/codex@${CODEX_CLI_VERSION:?no pin}" \
    "opencode-ai@${OPENCODE_VERSION:?no pin}"
npm cache clean --force
