#!/usr/bin/env bash
set -euo pipefail

release_sha="${GITHUB_SHA:?GITHUB_SHA is required}"
main_ref="refs/remotes/origin/main"
release_commit="$(git rev-parse "${release_sha}^{commit}")"

git fetch --no-tags origin +refs/heads/main:"${main_ref}"
if ! git merge-base --is-ancestor "${release_commit}" "${main_ref}"; then
  echo "Refusing to release: tagged commit ${release_commit} is not in origin/main history." >&2
  exit 1
fi

echo "Tagged commit ${release_commit} is in origin/main history."
