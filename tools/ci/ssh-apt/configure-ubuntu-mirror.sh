#!/bin/sh
set -eu
# CI-only Ubuntu mirror; retain suites, components and APT signature policy.
apt_root=${1:-/etc/apt}
if [ "$apt_root" = /etc/apt ]; then
    if getent hosts mirrors.edge.kernel.org >/dev/null; then
        echo "Ubuntu CI mirror DNS lookup succeeded"
    else
        echo "Ubuntu CI mirror DNS lookup failed" >&2
    fi
fi
for source in "$apt_root/sources.list" "$apt_root"/*mirrors*.txt "$apt_root"/sources.list.d/*.list "$apt_root"/sources.list.d/*.sources; do
    [ -f "$source" ] || continue
    sed -i -E 's#(https?://)(([a-z0-9-]+\.)?archive\.ubuntu\.com|security\.ubuntu\.com)/ubuntu#\1mirrors.edge.kernel.org/ubuntu#g' "$source"
done
