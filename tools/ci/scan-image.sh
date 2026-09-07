#!/usr/bin/env bash
set -euo pipefail
image="${1:?image required}"
platform="${2:-linux/amd64}"
[[ "$platform" == linux/amd64 || "$platform" == linux/arm64 ]]
diagnostics="${DIAGNOSTICS_DIR:-.tmp-smoke/diagnostics}"
mkdir -p "$diagnostics"
report="$diagnostics/trivy-${platform//\//-}.json"
root="$(mktemp -d)"
trap 'rm -rf "$root"' EXIT
if [[ "$image" == *@sha256:* ]]; then docker pull --platform "$platform" "$image"; fi
docker save --platform "$platform" "$image" -o "$root/image.tar"
# Trivy 0.74.0, immutable multiarch digest. No Docker socket or credentials mounted.
status=0
docker run --rm -v "$root:/scan:ro" \
  aquasec/trivy:0.74.0@sha256:62b1e65e8869bc4b4c6aa4fa2b21595256c7c2f6018a9d9ad61caf87187c1969 \
  image --input /scan/image.tar --scanners vuln --pkg-types os,library \
  --format json --severity HIGH,CRITICAL --exit-code 1 >"$report" \
  2>"$diagnostics/trivy-${platform//\//-}.log" || status=$?
cat "$diagnostics/trivy-${platform//\//-}.log"
# Fail closed if the scanner silently misses the OS or application binary.
python3 - "$report" <<'PY'
import json, sys
with open(sys.argv[1], encoding='utf-8') as stream:
    results = json.load(stream).get('Results', [])
assert any(r.get('Class') == 'os-pkgs' for r in results), 'OS packages missing from scan'
assert any(r.get('Type') == 'gobinary' and r.get('Target', '').endswith('/webserver') for r in results), 'Go application binary missing from scan'
PY
exit "$status"
