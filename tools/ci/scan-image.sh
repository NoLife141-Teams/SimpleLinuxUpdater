#!/usr/bin/env bash
set -euo pipefail
image="${1:?image required}"
root="$(mktemp -d)"
trap 'rm -rf "$root"' EXIT
docker save "$image" -o "$root/image.tar"
# Trivy 0.74.0, immutable multiarch digest. No Docker socket or credentials mounted.
docker run --rm -v "$root:/scan:ro" \
  aquasec/trivy:0.74.0@sha256:62b1e65e8869bc4b4c6aa4fa2b21595256c7c2f6018a9d9ad61caf87187c1969 \
  image --input /scan/image.tar --scanners vuln --pkg-types os \
  --severity HIGH,CRITICAL --exit-code 1
