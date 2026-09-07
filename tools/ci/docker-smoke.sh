#!/usr/bin/env bash
# Only temporary containers and a uniquely named volume are touched.
set -euo pipefail
image="${1:?image required}"
platform="${2:-linux/amd64}"
[[ "$platform" == linux/amd64 || "$platform" == linux/arm64 ]]
diagnostics="${DIAGNOSTICS_DIR:-.tmp-smoke/diagnostics}/${platform//\//-}"
mkdir -p "$diagnostics"
phase=initial
volume="$(docker volume create)"
container=""
capture() {
  if [[ -n "$container" ]]; then
    docker logs "$container" >"$diagnostics/$phase.log" 2>&1 || true
    docker inspect -f '{{json .State}}' "$container" >"$diagnostics/$phase-state.json" 2>&1 || true
  fi
}
cleanup() {
  status=$?
  capture
  if [[ -n "$container" ]]; then docker rm -f "$container" >/dev/null || true; fi
  docker volume rm "$volume" >/dev/null || true
  exit "$status"
}
trap cleanup EXIT
start() {
  container="$(docker run -d --platform "$platform" "$@" -p 127.0.0.1::8080 -v "$volume:/data" "$image")"
  port="$(docker port "$container" 8080/tcp | awk -F: '{print $NF}')"
  url="http://127.0.0.1:$port"
  for ((attempt=0; attempt<90; attempt++)); do
    if curl --connect-timeout 2 --max-time 5 -fsSL "$url/login" >/dev/null 2>&1; then break; fi
    [[ "$(docker inspect -f '{{.State.Running}}' "$container")" == true ]] || return 1
    sleep 1
  done
  curl --connect-timeout 2 --max-time 5 -fsSL "$url/login" >/dev/null
  # Inspect PID 1: docker exec defaults to root on an entrypoint privilege-drop image.
  docker exec "$container" sh -ec 'test "$(awk "/^Uid:/ {print \$2}" /proc/1/status)" = "$(id -u app)"; test "$(id -u app)" != 0; test -s /data/servers.db'
}
start
curl --connect-timeout 2 --max-time 5 -fsS -H "Origin: $url" -H 'Content-Type: application/json' -d '{"username":"smoke-user","password":"Smoke-only-Password-482!"}' "$url/api/auth/setup" >/dev/null
# Recreate with explicit non-root user, preserving the initialized volume.
capture
phase=restarted
docker rm -f "$container" >/dev/null
container=""
start --user app
curl --connect-timeout 2 --max-time 5 -fsS "$url/api/auth/status" | python3 -c 'import json,sys; assert json.load(sys.stdin)["setup_required"] is False'
curl --connect-timeout 2 --max-time 5 -fsS -H "Origin: $url" -H 'Content-Type: application/json' -d '{"username":"smoke-user","password":"Smoke-only-Password-482!"}' "$url/api/auth/login" >/dev/null
printf 'Docker smoke passed: %s (%s), non-root restart and persisted account.\n' "$image" "$platform"
