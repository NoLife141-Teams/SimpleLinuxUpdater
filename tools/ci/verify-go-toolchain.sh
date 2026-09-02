#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$repo_root"

canonical_count="$(awk '$1 == "go" { count++ } END { print count + 0 }' go.mod)"
canonical_version="$(awk '$1 == "go" { print $2 }' go.mod)"
if [[ "$canonical_count" != "1" ]] || ! grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' <<<"$canonical_version"; then
  echo "go.mod must declare exactly one patch-level Go version." >&2
  exit 1
fi

builder_pattern='^FROM[[:space:]]+golang:[0-9]+\.[0-9]+\.[0-9]+-alpine[[:space:]]+AS[[:space:]]+builder[[:space:]]*$'
builder_count="$(grep -Ec "$builder_pattern" Dockerfile || true)"
builder_version="$(sed -nE 's/^FROM[[:space:]]+golang:([0-9]+\.[0-9]+\.[0-9]+)-alpine[[:space:]]+AS[[:space:]]+builder[[:space:]]*$/\1/p' Dockerfile)"
if [[ "$builder_count" != "1" ]]; then
  echo "Dockerfile must contain exactly one patch-level golang alpine builder image." >&2
  exit 1
fi
if [[ "$builder_version" != "$canonical_version" ]]; then
  echo "Go toolchain drift: Dockerfile uses ${builder_version}, go.mod requires ${canonical_version}." >&2
  exit 1
fi

echo "Go toolchain aligned at ${canonical_version}."
