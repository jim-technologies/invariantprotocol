#!/usr/bin/env bash
# Protobuf compatibility slice of `make validate` and `make release`.
#
# `buf breaking` needs a baseline older than the change under test. Comparing
# against origin/main is a no-op on main after a push: HEAD is compared with
# itself, so nothing between two releases can ever fail. The baseline is
# therefore the newest plain-SemVer v* tag reachable from HEAD, which is what a
# consumer pinning the previous release sees. On the release commit itself,
# once the release tag points at HEAD, the preceding release is the baseline
# instead. A pull request run compares against its base branch, so it judges
# the pull request's own change. With no reachable release tag (a shallow or
# single-ref clone) the check is skipped loudly rather than passed silently.

set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "$root"

# GitHub Actions sets GITHUB_BASE_REF only for pull_request events.
if [[ -n "${GITHUB_BASE_REF:-}" ]]; then
  against="../.git#ref=origin/${GITHUB_BASE_REF},subdir=proto"
  echo "breaking: buf breaking against the pull request base origin/${GITHUB_BASE_REF}"
  cd proto
  exec buf breaking --against "$against"
fi

release_tag="v$(tr -d '[:space:]' < VERSION)"
head="$(git rev-parse HEAD)"
baseline=""
while IFS= read -r tag; do
  [[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || continue
  if [[ "$tag" == "$release_tag" && "$(git rev-list -n 1 "$tag")" == "$head" ]]; then
    continue
  fi
  baseline="$tag"
  break
done < <(git tag --merged HEAD --list 'v[0-9]*' --sort=-version:refname)

if [[ -z "$baseline" ]]; then
  echo "breaking: no prior release tag is reachable from HEAD; skipping buf breaking" >&2
  exit 0
fi

echo "breaking: buf breaking against ${baseline}"
cd proto
exec buf breaking --against "../.git#tag=${baseline},subdir=proto"
