#!/usr/bin/env bash
set -euo pipefail
: "${GO_COVERAGE_THRESHOLD:=73.0}"
coverage_summary="$(go tool cover -func=coverage.out | tail -n 1)"
measured="$(printf '%s\n' "$coverage_summary" | awk '{ value = $NF; sub(/%$/, "", value); print value }')"

if [[ ! "$measured" =~ ^[0-9]+([.][0-9]+)?$ ]] || [[ ! "$GO_COVERAGE_THRESHOLD" =~ ^[0-9]+([.][0-9]+)?$ ]]; then
  echo "::error::Unable to parse total Go coverage or threshold"
  exit 1
fi

printf 'Measured Go coverage: %s%%; required minimum: %s%%\n' "$measured" "$GO_COVERAGE_THRESHOLD"
if ! awk -v measured="$measured" -v threshold="$GO_COVERAGE_THRESHOLD" \
  'BEGIN { exit !(measured + 0 >= threshold + 0) }'; then
  echo "::error::Measured Go coverage ${measured}% is below required minimum ${GO_COVERAGE_THRESHOLD}%"
  exit 1
fi
