# a stub opencode that answers its version and preflight, and in a pair marks
# its start in the workdir and never returns.
FROM docker.io/library/ubuntu:24.04
RUN printf '%s\n' '#!/bin/sh' \
        'case "$1" in (--version|-v) echo opencode 1.0.0; exit 0 ;; esac' \
        '[ -z "$PATS_PREFLIGHT" ] || exit 0' \
        'touch pats-spread-started' \
        'exec sleep 600' > /usr/local/bin/opencode && \
    chmod 755 /usr/local/bin/opencode
