# spread allocate for the bread backend, sourced by spread.yaml (ADDRESS and
# FATAL are spread's). a plain bread container (ubuntu + sshd) driving the
# host's docker through its socket, so the sandboxes pats starts are siblings
# of it on the host daemon. that daemon resolves bind mounts against the host,
# so pats's state and projects live in a host dir bound at the same path in
# the container; /etc/pats-spread-dir names it for the tasks.
set -e

# the docker cli and buildx, copied in from docker's own cli image: bread
# ships neither, and pats builds with buildkit.
DOCKER_CLI_IMAGE=docker.io/library/docker:28.5.1-cli@sha256:9190b0613792e658a7783cf14b2d5ace5941bb68ede7276922ea36ee457d76ad

ver=$(printf '%s\n' "$SPREAD_SYSTEM" | cut -d- -f2)   # ubuntu-26.04-arm64 -> 26.04
arch=$(printf '%s\n' "$SPREAD_SYSTEM" | cut -d- -f3)  # ubuntu-26.04-arm64 -> arm64
image="ghcr.io/lczyk/spread-bread/bread:$ver"

# bridge: the container's bridge ip, routable where the daemon is local (linux).
# publish: sshd on a port of 127.0.0.1, for docker desktop's vm (macos).
mode="${BREAD_NET:-}"
if [ -z "$mode" ]; then
    case "$(uname -s)" in
        (Darwin) mode=publish ;;
        (*) mode=bridge ;;
    esac
fi
case "$mode" in
    (bridge) publish_flag="" ;;
    (publish) publish_flag="-p 127.0.0.1::22" ;;
    (*) FATAL "BREAD_NET must be bridge or publish, got: $mode" ;;
esac

# the name is the container's hostname too, so it heads the owner label of
# everything pats starts from inside (discard sweeps by it).
name="pats-spread-$ver-$arch-$$-$RANDOM"

# built by `make spread`: spread runs this with too bare an environment to
# find a go toolchain.
bin=".spread/pats-linux-$arch"
[ -x "$bin" ] || FATAL "no $bin: run spread through make spread"

# under TMPDIR, which docker desktop shares with its vm, as it does /tmp.
shared="${PATS_SPREAD_SHARED:-${TMPDIR:-/tmp}/pats-spread}"
shared="${shared%/}/$name"
mkdir -p "$shared"

# a failed allocation is never handed to discard: undo it here, until ADDRESS.
cli="$name-cli"
trap 'docker rm -f "$name" "$cli" > /dev/null 2>&1; rm -rf "$shared"' EXIT

docker run \
    --rm \
    --platform "linux/$arch" \
    --hostname "$name" \
    -v /var/run/docker.sock:/var/run/docker.sock \
    -v "$shared:$shared" \
    -e DEBIAN_FRONTEND=noninteractive \
    $publish_flag \
    --label pats-spread \
    --name "$name" \
    -d "$image"

# a container that dies on start would otherwise leave spread waiting on its
# own timeout, which says nothing about why.
sshd_up=""
for ((i = 0; i < 60; i++)); do
    if docker exec "$name" pgrep -x sshd > /dev/null 2>&1; then
        sshd_up=1
        break
    fi
    if ! docker inspect "$name" --format '{{.State.Status}}' 2> /dev/null | grep -qx running; then
        docker logs "$name" >&2 2>&1 || true
        FATAL "$name is not running"
    fi
    sleep 1
done
if [ -z "$sshd_up" ]; then
    docker logs "$name" >&2 2>&1 || true
    FATAL "sshd did not come up in $name after 60s"
fi

docker create --platform "linux/$arch" --name "$cli" "$DOCKER_CLI_IMAGE" > /dev/null
docker exec "$name" mkdir -p /usr/local/libexec/docker/cli-plugins
docker cp "$cli:/usr/local/bin/docker" - | docker cp - "$name:/usr/local/bin/"
docker cp "$cli:/usr/local/libexec/docker/cli-plugins/docker-buildx" - |
    docker cp - "$name:/usr/local/libexec/docker/cli-plugins/"
docker rm "$cli" > /dev/null
docker cp "$bin" "$name:/usr/local/bin/pats"
docker exec "$name" sh -c "printf '%s\n' '$shared' > /etc/pats-spread-dir"

if [ "$mode" = publish ]; then
    port=$(docker port "$name" 22 | head -n1 | cut -d: -f2)
    [ -n "$port" ] || FATAL "no published sshd port for $name"
    address="127.0.0.1:$port"
else
    address=$(docker inspect "$name" --format '{{.NetworkSettings.Networks.bridge.IPAddress}}')
fi
trap - EXIT
ADDRESS "$address"
