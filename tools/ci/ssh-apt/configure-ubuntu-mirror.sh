#!/bin/sh
set -eu
# CI-only Ubuntu mirror; retain suites, components and APT signature policy.
apt_root=${1:-/etc/apt}
for source in "$apt_root/sources.list" "$apt_root"/sources.list.d/*.list "$apt_root"/sources.list.d/*.sources; do
    [ -f "$source" ] || continue
    sed -i -E 's#(https?://)(archive\.ubuntu\.com|security\.ubuntu\.com)/ubuntu#\1azure.archive.ubuntu.com/ubuntu#g' "$source"
done
