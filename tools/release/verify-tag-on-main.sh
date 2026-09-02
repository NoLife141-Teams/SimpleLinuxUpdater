#!/usr/bin/env bash
set -euo pipefail

release_tag="${RELEASE_TAG:?RELEASE_TAG is required}"
release_sha="${RELEASE_SHA:?RELEASE_SHA is required}"
main_ref="refs/remotes/origin/main"

if [[ ! "$release_tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "Refusing to release: invalid release tag ${release_tag}." >&2
  exit 1
fi
if [[ ! "$release_sha" =~ ^[0-9a-f]{40}$ ]]; then
  echo "Refusing to release: invalid release SHA ${release_sha}." >&2
  exit 1
fi

git fetch --no-tags origin +refs/heads/main:"${main_ref}"
git fetch --force --no-tags origin "+refs/tags/${release_tag}:refs/tags/${release_tag}"

release_commit="$(git rev-parse --verify "${release_sha}^{commit}")"
tag_commit="$(git rev-parse --verify "refs/tags/${release_tag}^{commit}")"
if [[ "$release_commit" != "$tag_commit" ]]; then
  echo "Refusing to release: tag ${release_tag} does not resolve to ${release_commit}." >&2
  exit 1
fi
if ! git merge-base --is-ancestor "${release_commit}" "${main_ref}"; then
  echo "Refusing to release: tagged commit ${release_commit} is not in origin/main history." >&2
  exit 1
fi

echo "Tagged commit ${release_commit} is in origin/main history."
