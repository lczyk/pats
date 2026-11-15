#!/bin/bash
# lxd_bench.sh times what a pats lxd pair spends its time on: an image pull,
# an instance made from it (unpack, and any uid shift), started, published
# (what composing an image ends in), and made again from the cache.
set -u

t() {
    local s e rc
    s=$(date +%s.%N)
    "$@" > /tmp/bench.out 2>&1
    rc=$?
    e=$(date +%s.%N)
    awk -v s="$s" -v e="$e" -v c="$*" -v rc="$rc" 'BEGIN { printf "%-48s %7.1fs rc=%s\n", c, e - s, rc }'
    [ "$rc" = 0 ] || tail -n 5 /tmp/bench.out
}

echo "== $1"
echo "images.compression_algorithm=$(lxc config get images.compression_algorithm)"
[ -z "${COMPRESSION:-}" ] || lxc config set images.compression_algorithm "$COMPRESSION"
[ -z "${NESTING:-}" ] || lxc profile set default security.nesting=true
lxc info | grep -iE 'apparmor' || true
lxc storage list -f csv
lxc info | grep -E '^ *(driver|driver_version|storage|storage_version|kernel_version):'
lxc info | sed -n '/kernel_features:/,/^[a-z]/p' | grep -E 'idmapped|shiftfs|uevent|seccomp'
head -n 3 /proc/self/uid_map
findmnt -no FSTYPE,SOURCE /var/snap/lxd/common/lxd 2> /dev/null || findmnt -no FSTYPE,SOURCE /

t lxc image copy ubuntu:24.04 local: --alias bench
t lxc init bench b1
t lxc start b1
t lxc exec b1 -- cloud-init status --wait
echo "-- b1 boot"
lxc exec b1 -- systemd-analyze blame | head -n 8
lxc exec b1 -- systemctl --failed --no-legend
lxc exec b1 -- networkctl --no-legend
lxc exec b1 -- ip -br addr
lxc exec b1 -- journalctl -b --no-pager -u apparmor -u snapd | grep -iE 'error|fail|denied|cannot' | head -n 8
lxc exec b1 -- sh -c 'ls /sys/kernel/security/apparmor 2>&1 | head -n 3; cat /proc/self/attr/current 2>&1'
t lxc stop b1
t lxc publish b1 --alias bench-pub
t lxc init bench b2
t lxc init bench-pub b3
t lxc delete -f b1 b2 b3
lxc image list -f csv -c lfs
