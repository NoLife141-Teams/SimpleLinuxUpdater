#!/usr/bin/env bash
# Only temporary containers and a uniquely named volume are touched.
set -euo pipefail
image="${1:?image required}"
platform="${2:-linux/amd64}"
volume="$(docker volume create)"
container=""
cleanup() {
  if [[ -n "$container" ]]; then docker rm -f "$container" >/dev/null; fi
  docker volume rm "$volume" >/dev/null
}
trap cleanup EXIT
start() {
  container="$(docker run -d --platform "$platform" "$@" -p 127.0.0.1::8080 -v "$volume:/data" "$image")"
  port="$(docker port "$container" 8080/tcp | awk -F: '{print $NF}')"
  url="http://127.0.0.1:$port"
  for ((attempt=0; attempt<90; attempt++)); do
    if curl --connect-timeout 2 --max-time 5 -fsSL "$url/login" >/dev/null 2>&1; then break; fi
    [[ "$(docker inspect -f '{{.State.Running}}' "$container")" == true ]]
    sleep 1
  done
  curl --connect-timeout 2 --max-time 5 -fsSL "$url/login" >/dev/null
  # Inspect PID 1: docker exec defaults to root on an entrypoint privilege-drop image.
  docker exec "$container" sh -ec 'test "$(awk "/^Uid:/ {print \$2}" /proc/1/status)" = "$(id -u app)"; test "$(id -u app)" != 0; test -s /data/servers.db'
}
start
curl -fsS -H "Origin: $url" -H 'Content-Type: application/json' -d '{"username":"smoke-user","password":"Smoke-only-Password-482!"}' "$url/api/auth/setup" >/dev/null
# Recreate with explicit non-root user, preserving the initialized volume.
docker rm -f "$container" >/dev/null
container=""
start --user app
curl -fsS "$url/api/auth/status" | python3 -c 'import json,sys; assert json.load(sys.stdin)["setup_required"] is False'
curl -fsS -H "Origin: $url" -H 'Content-Type: application/json' -d '{"username":"smoke-user","password":"Smoke-only-Password-482!"}' "$url/api/auth/login" >/dev/null
printf 'Docker smoke passed: %s (%s), non-root restart and persisted account.\n' "$image" "$platform"
