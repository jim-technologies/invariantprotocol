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
#
# Deliberate breaks: while VERSION is 0.x a release may break the protobuf
# contract on purpose. INVARIANT_ALLOW_PROTO_BREAK=1 accepts the breaks buf
# reports (buf still prints every one), but only when the newest CHANGELOG.md
# section is unreleased (`## Unreleased`, or `## vX.Y.Z` for the release being
# cut) and has a `### Breaking` subsection with at least one entry. Every other
# use of the flag is refused, so it cannot outlive the release that documents
# the break. CI reads the flag from the repository variable of the same name.
#
# scripts/check_breaking_test.sh pins this behaviour.

set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "$root"

version="$(tr -d '[:space:]' < VERSION)"
release_tag="v${version}"
head="$(git rev-parse HEAD)"

refuse() {
  echo "breaking: refusing INVARIANT_ALLOW_PROTO_BREAK: $*" >&2
  exit 1
}

allow_break=false
case "${INVARIANT_ALLOW_PROTO_BREAK:-}" in
  "") ;;
  1)
    if [[ "$version" != 0.* ]]; then
      refuse "VERSION is ${version}; from 1.0 a protobuf break needs a new major version"
    fi
    heading="$(grep -m 1 '^## ' CHANGELOG.md || true)"
    case "$heading" in
      "## Unreleased") ;;
      "## ${release_tag}" | "## ${release_tag} "*)
        if git rev-parse --quiet --verify "refs/tags/${release_tag}" >/dev/null &&
          [[ "$(git rev-list -n 1 "$release_tag")" != "$head" ]]; then
          refuse "the newest CHANGELOG.md section, ${release_tag}, is already released"
        fi
        ;;
      *)
        refuse "the newest CHANGELOG.md section must be '## Unreleased' or '## ${release_tag}', not '${heading}'"
        ;;
    esac
    entries="$(awk '
      /^## / { section++ }
      section == 1 && /^### / { breaking = ($0 ~ /^### Breaking[[:space:]]*$/); next }
      section == 1 && breaking && /^- / { count++ }
      END { print count + 0 }
    ' CHANGELOG.md)"
    if ((entries == 0)); then
      refuse "the newest CHANGELOG.md section has no '### Breaking' entry"
    fi
    allow_break=true
    ;;
  *)
    refuse "set it to 1 or leave it unset, not '${INVARIANT_ALLOW_PROTO_BREAK}'"
    ;;
esac

# GitHub Actions sets GITHUB_BASE_REF only for pull_request events.
if [[ -n "${GITHUB_BASE_REF:-}" ]]; then
  against="../.git#ref=origin/${GITHUB_BASE_REF},subdir=proto"
  label="the pull request base origin/${GITHUB_BASE_REF}"
else
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
  against="../.git#tag=${baseline},subdir=proto"
  label="$baseline"
fi

echo "breaking: buf breaking against ${label}"
cd proto
status=0
buf breaking --against "$against" || status=$?
# buf exits 100 when it found breaking changes and 1 when it could not run.
if ((status == 100)) && [[ "$allow_break" == true ]]; then
  echo "breaking: accepting the breaks above; INVARIANT_ALLOW_PROTO_BREAK=1 and CHANGELOG.md documents them under ### Breaking" >&2
  exit 0
fi
exit "$status"
