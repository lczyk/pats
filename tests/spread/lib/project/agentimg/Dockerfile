# a stand-in agent image: a stub opencode, so a pair runs with no key. the
# mount needs buildkit, so the build fails unless the snap's buildx is found.
FROM docker.io/library/ubuntu:24.04
RUN --mount=type=tmpfs,target=/scratch \
    printf '#!/bin/sh\necho opencode 1.0.0\n' > /usr/local/bin/opencode && \
    chmod 755 /usr/local/bin/opencode
