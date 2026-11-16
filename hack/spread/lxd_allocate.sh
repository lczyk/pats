# spread allocate for the lxd backend, sourced by spread.yaml (ADDRESS and
# FATAL are spread's). an ubuntu system container on the local lxd, nested so
# docker and lxd run inside it, with the snap under test and pats from this
# tree pushed in. spread logs in as root with the system's password.
set -e

ver="${SPREAD_SYSTEM#ubuntu-}"  # ubuntu-24.04 -> 24.04
case "$(uname -m)" in
    (x86_64) arch=amd64 ;;
    (aarch64|arm64) arch=arm64 ;;
    (*) FATAL "no lxd systems on $(uname -m)" ;;
esac
snap=".spread/pats_$arch.snap"
bin=".spread/pats-linux-$arch"
[ -f "$snap" ] || FATAL "no $snap: snapcraft pack, then put the snap there"
[ -x "$bin" ] || FATAL "no $bin: run spread through make spread"

# lxd names take no dots.
name="pats-spread-${ver//./-}-$$-$RANDOM"

# a failed allocation is never handed to discard: undo it here, until ADDRESS.
trap 'lxc delete --force "$name" > /dev/null 2>&1' EXIT

# nesting and the two syscall intercepts are what docker in a container needs.
lxc launch "ubuntu:$ver" "$name" \
    -c security.nesting=true \
    -c security.syscalls.intercept.mknod=true \
    -c security.syscalls.intercept.setxattr=true

lxc exec "$name" -- cloud-init status --wait > /dev/null || true
lxc exec "$name" -- snap wait system seed.loaded

# the image's own sshd settings refuse root and passwords; a drop-in read
# first wins.
lxc exec "$name" -- sh -ec "
    command -v sshd > /dev/null || apt-get install -y -q openssh-server > /dev/null
    printf 'root:%s\n' '$SPREAD_SYSTEM_PASSWORD' | chpasswd
    printf 'PermitRootLogin yes\nPasswordAuthentication yes\n' > /etc/ssh/sshd_config.d/00-spread.conf
    systemctl restart ssh
"

lxc file push "$snap" "$name/var/tmp/pats.snap"
lxc file push --mode 0755 "$bin" "$name/usr/local/bin/pats"

address=""
for ((i = 0; i < 60; i++)); do
    address=$(lxc list "$name" --format csv -c 4 | cut -d' ' -f1)
    [ -n "$address" ] && break
    sleep 1
done
[ -n "$address" ] || FATAL "$name got no ipv4 address after 60s"
lxc config set "$name" user.spread.address="$address"

trap - EXIT
ADDRESS "$address"
