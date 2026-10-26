#!/bin/sh
# Runs exclusively inside the task-owned disposable container.
set -eu
test -f /run/slu-fixture-owner
mkdir -p /tmp/slu-debs /tmp/slu-repo
native=$(dpkg --print-architecture)
make_package() {
    name=$1; version=$2; arch=$3; extra=${4:-}
    tree=/tmp/slu-build-$name-$version-$arch
    mkdir -p "$tree/DEBIAN" "$tree/usr/share/slu-fixture"
    printf 'Package: %s\nVersion: %s\nArchitecture: %s\nMaintainer: Disposable Fixture <fixture@example.invalid>\nDescription: package deferral fixture\n%s\n' "$name" "$version" "$arch" "$extra" > "$tree/DEBIAN/control"
    printf '%s\n' "$version" > "$tree/usr/share/slu-fixture/$name-$arch"
    dpkg-deb --root-owner-group --build "$tree" "/tmp/slu-debs/$name-$version-$arch.deb" >/dev/null
}
for name in slu-deferred slu-independent slu-dependent slu-admin-held slu-admin-pinned; do
    make_package "$name" 1 "$native"
    extra=''; if [ "$name" = slu-dependent ]; then extra='Depends: slu-deferred (>= 2)'; fi
    make_package "$name" 2 "$native" "$extra"
    dpkg -i "/tmp/slu-debs/$name-1-$native.deb" >/dev/null
done
make_package slu-replacement 2 "$native" 'Conflicts: slu-deferred'
make_package slu-victim 1 "$native"
dpkg -i "/tmp/slu-debs/slu-victim-1-$native.deb" >/dev/null
make_package slu-safe-replacement 2 "$native" 'Conflicts: slu-victim'
dpkg --add-architecture i386
for arch in "$native" i386; do
    make_package slu-multi 1 "$arch" 'Multi-Arch: same'
    make_package slu-multi 2 "$arch" 'Multi-Arch: same'
    dpkg -i "/tmp/slu-debs/slu-multi-1-$arch.deb" >/dev/null
done
cp /tmp/slu-debs/*.deb /tmp/slu-repo/
: > /tmp/slu-repo/Packages
for deb in /tmp/slu-repo/*.deb; do
    dpkg-deb -f "$deb" | sed '/^$/d' >> /tmp/slu-repo/Packages
    printf 'Filename: %s\nSize: %s\nSHA256: %s\n\n' "./${deb##*/}" "$(stat -c %s "$deb")" "$(sha256sum "$deb" | cut -d ' ' -f 1)" >> /tmp/slu-repo/Packages
done
rm -f /etc/apt/sources.list /etc/apt/sources.list.d/*
printf 'deb [trusted=yes] file:/tmp/slu-repo ./\n' > /etc/apt/sources.list.d/slu-fixture.list
apt-get update
apt-mark hold slu-admin-held >/dev/null
printf 'Package: slu-admin-pinned\nPin: version 1\nPin-Priority: 1001\n' > /etc/apt/preferences.d/administrator
printf '%s\n' "$native" > /run/slu-native
sha256sum /etc/apt/preferences.d/administrator > /run/slu-pin-before
apt-mark showhold > /run/slu-holds-before
