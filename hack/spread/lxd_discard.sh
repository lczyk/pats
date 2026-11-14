# spread discard for the lxd backend, sourced by spread.yaml. everything pats
# made ran nested in the container, so deleting it takes them all.
set -e

# by the address allocate stamped on it: the nested engines add others.
name=$(lxc list --format csv -c n,user.spread.address |
    awk -F, -v ip="$SPREAD_SYSTEM_ADDRESS" '$2 == ip { print $1; exit }')
if [ -z "$name" ]; then
    printf 'no pats-spread container at %s\n' "$SPREAD_SYSTEM_ADDRESS" >&2
    exit 1
fi
# the tasks' steps, timed (tests/spread/lib/stamp).
mkdir -p .spread/stamps
lxc file pull "$name/var/tmp/spread-stamps" ".spread/stamps/$name.log" > /dev/null 2>&1 || true
lxc delete --force "$name"
