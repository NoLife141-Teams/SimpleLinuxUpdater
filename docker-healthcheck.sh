#!/bin/sh
set -eu
if [ "$(id -u)" = 0 ]; then
    exec su-exec app /app/webserver healthcheck
fi
exec /app/webserver healthcheck
