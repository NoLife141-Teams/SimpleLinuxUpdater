#!/usr/bin/env bash
set -euo pipefail

: "${GH_TOKEN:?GH_TOKEN is required to query GitHub Actions runs}"
: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"
: "${CODEQL_SHA:?CODEQL_SHA is required}"

api_url="${GITHUB_API_URL:-https://api.github.com}"
workflow_path="${CODEQL_WORKFLOW_PATH:-dynamic/github-code-scanning/codeql}"
timeout_seconds="${CODEQL_WAIT_TIMEOUT_SECONDS:-900}"
poll_seconds="${CODEQL_WAIT_POLL_SECONDS:-10}"

for value_name in timeout_seconds poll_seconds; do
  value="${!value_name}"
  if ! [[ "$value" =~ ^[1-9][0-9]*$ ]]; then
    echo "${value_name} must be a positive integer, got ${value@Q}" >&2
    exit 1
  fi
done

for command in curl jq; do
  if ! command -v "$command" >/dev/null 2>&1; then
    echo "required command is unavailable: $command" >&2
    exit 1
  fi
done

deadline=$((SECONDS + timeout_seconds))
runs_url="${api_url}/repos/${GITHUB_REPOSITORY}/actions/runs?head_sha=${CODEQL_SHA}&per_page=100"

while :; do
  response="$(curl --fail --silent --show-error --retry 3 --retry-all-errors \
    -H "Authorization: Bearer ${GH_TOKEN}" \
    -H 'Accept: application/vnd.github+json' \
    -H 'X-GitHub-Api-Version: 2022-11-28' \
    "$runs_url")"

  run="$(jq -c --arg path "$workflow_path" '
    [.workflow_runs[] | select(.path == $path)]
    | sort_by(.created_at // "")
    | last // {}
  ' <<<"$response")"

  run_id="$(jq -r '.id // empty' <<<"$run")"
  status="$(jq -r '.status // "missing"' <<<"$run")"
  conclusion="$(jq -r '.conclusion // empty' <<<"$run")"

  printf 'CodeQL default setup for %s: status=%s conclusion=%s run_id=%s\n' \
    "$CODEQL_SHA" "$status" "${conclusion:-pending}" "${run_id:-none}"

  if [[ -n "$run_id" && "$status" == "completed" ]]; then
    if [[ "$conclusion" == "success" ]]; then
      echo "CodeQL default setup completed successfully."
      exit 0
    fi
    echo "::error::CodeQL default setup run ${run_id} completed with conclusion '${conclusion:-unknown}'."
    exit 1
  fi

  if (( SECONDS >= deadline )); then
    echo "::error::Timed out waiting for CodeQL default setup workflow '${workflow_path}' on ${CODEQL_SHA}."
    exit 1
  fi

  sleep "$poll_seconds"
done
