#!/usr/bin/env bash
set -euo pipefail

shard=$1
shards=$2

# Go runs top-level tests in the root package mostly serially. Distribute them
# across runners and let Go run all subtests of each selected test as usual.
tests=$(go test -race -list '^(Test|Example|Fuzz)' .)
pattern=$(awk -v shard="$shard" -v shards="$shards" '
  /^(Test|Example|Fuzz)[^[:space:]]*$/ {
    if (test++ % shards == shard - 1) {
      printf "%s%s", separator, $0
      separator = "|"
    }
  }
' <<<"$tests")
if [[ -z "$pattern" ]]; then
  echo "No Go tests selected for race shard $shard of $shards" >&2
  exit 1
fi
go test -race -count=1 -run "^(${pattern})$" .

# Give the smaller packages to one shard, including newly added packages.
if [[ "$shard" == 1 ]]; then
  root_package=$(go list -race .)
  packages=$(go list -race ./...)
  mapfile -t other_packages < <(awk -v root="$root_package" '$0 != root' <<<"$packages")
  if (( ${#other_packages[@]} > 0 )); then
    go test -race -count=1 "${other_packages[@]}"
  fi
fi
