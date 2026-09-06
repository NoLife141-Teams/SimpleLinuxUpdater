#!/bin/sh
set -eu

app_user="app"
app_group="app"

if [ "$(id -u)" = "0" ]; then
    app_uid="$(id -u "$app_user")"
    app_gid="$(id -g "$app_user")"

    # Ownership repair executes as root before privilege drop, but all path
    # traversal/chown operations happen descriptor-relative with no-follow
    # semantics inside the root-owned helper.
    /usr/local/bin/persistence-owner "$app_uid" "$app_gid"

    exec su-exec "$app_user:$app_group" "$@"
fi

exec "$@"
