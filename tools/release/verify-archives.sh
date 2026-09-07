#!/usr/bin/env bash
set -euo pipefail
version="${RELEASE_TAG#v}"
app="SimpleLinuxUpdater_$version"
(cd dist && sha256sum --check checksums.txt)
for target in linux_amd64 linux_arm64 darwin_amd64 darwin_arm64 windows_amd64; do
  if [[ "$target" == windows_amd64 ]]; then
    unzip -t "dist/${app}_${target}.zip"
  else
    tar -tzf "dist/${app}_${target}.tar.gz" >/dev/null
  fi
done
# Execute the exact packaged amd64 distribution on the Linux release runner.
diagnostics="${DIAGNOSTICS_DIR:-.tmp-smoke/archive-diagnostics}"
mkdir -p "$diagnostics"
diagnostics="$(cd "$diagnostics" && pwd)"
root="$(mktemp -d)"
pid=""
cleanup() {
  if [[ -n "$pid" ]]; then kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; fi
  rm -rf "$root"
}
trap cleanup EXIT
tar -xzf "dist/${app}_linux_amd64.tar.gz" -C "$root"
(
  cd "$root/$app"
  exec env DEBIAN_UPDATER_LISTEN_ADDR=127.0.0.1:18081 DEBIAN_UPDATER_DB_PATH="$root/servers.db" ./webserver >"$diagnostics/server.log" 2>&1
) &
pid=$!
for ((attempt=0; attempt<60; attempt++)); do
  if curl --connect-timeout 2 --max-time 5 -fsSL http://127.0.0.1:18081/login >/dev/null 2>&1; then break; fi
  kill -0 "$pid"
  sleep 1
done
curl --connect-timeout 2 --max-time 5 -fsSL http://127.0.0.1:18081/login >/dev/null
