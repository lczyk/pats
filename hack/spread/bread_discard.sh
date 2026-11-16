# spread discard for the bread backend, sourced by spread.yaml. what pats
# started from the container are its siblings on the host daemon, not its
# children, so they go first: pats clean, then a sweep of whatever still
# carries the container's owner label. the shared dir's files are root's on a
# linux host, so they are removed from inside before the container goes.
set -e

# "host:port" was published (macos), a bare ip is a bridge address.
name=""
case "$SPREAD_SYSTEM_ADDRESS" in
    (*:*)
        port="${SPREAD_SYSTEM_ADDRESS##*:}"
        name=$(docker ps -a --filter label=pats-spread --format '{{.Names}} {{.Ports}}' |
            awk -v m=":$port->22/" 'index($0, m) { print $1; exit }')
        ;;
    (*)
        ids=$(docker ps -aq --filter label=pats-spread)
        if [ -n "$ids" ]; then
            # shellcheck disable=SC2086
            name=$(docker inspect $ids --format '{{.Name}} {{.NetworkSettings.Networks.bridge.IPAddress}}' |
                awk -v ip="$SPREAD_SYSTEM_ADDRESS" '$2 == ip { print substr($1, 2); exit }')
        fi
        ;;
esac
if [ -z "$name" ]; then
    printf 'no pats-spread container at %s\n' "$SPREAD_SYSTEM_ADDRESS" >&2
    exit 1
fi

shared=$(docker exec "$name" cat /etc/pats-spread-dir 2> /dev/null || true)
if [ -n "$shared" ]; then
    docker exec -e HOME="$shared/home" "$name" pats clean || true
fi

# the owner label is <hostname>/<uid>/<client id>, and the hostname is the name.
_owned() {
    awk -v p="$name/" 'index($2, p) == 1 { print $1 }'
}
ids=$(docker ps -a --filter label=pats.owner --format '{{.ID}} {{.Label "pats.owner"}}' | _owned)
if [ -n "$ids" ]; then
    printf 'removing containers pats clean left: %s\n' "$ids"
    # shellcheck disable=SC2086
    docker rm -f $ids > /dev/null
fi
ids=$(docker network ls --filter label=pats.owner --format '{{.ID}} {{.Label "pats.owner"}}' | _owned)
if [ -n "$ids" ]; then
    printf 'removing networks pats clean left: %s\n' "$ids"
    # shellcheck disable=SC2086
    docker network rm $ids > /dev/null
fi

if [ -n "$shared" ]; then
    docker exec "$name" rm -rf "$shared" || true
fi
docker rm -f "$name" > /dev/null
if [ -n "$shared" ]; then
    rmdir "$shared" 2> /dev/null || true
fi
