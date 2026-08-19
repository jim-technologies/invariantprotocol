#!/usr/bin/env bash
# Fail-closed release guard (MAKEFILE-CONTRACT.md `make release`).
# Verifies release readiness, states what a release publishes, and refuses to
# create the tag itself: the annotated tag is created manually after the
# release commit's CI workflow passes on main (see AGENTS.md). Runs from a
# maintainer's machine; CI never publishes.

set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "$root"

version="$(tr -d '[:space:]' < VERSION)"
tag="v${version}"

if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "release: VERSION must be MAJOR.MINOR.PATCH, got ${version}" >&2
  exit 1
fi

if [[ -n "$(git status --porcelain --untracked-files=all)" ]]; then
  echo "release: refusing a dirty tree; commit or stash everything first." >&2
  exit 1
fi

git fetch --quiet origin main
if ! git merge-base --is-ancestor HEAD origin/main; then
  echo "release: refusing an unpushed tree; HEAD must be on origin/main." >&2
  exit 1
fi

if git rev-parse --quiet --verify "refs/tags/${tag}" >/dev/null; then
  echo "release: tag ${tag} already exists locally; bump VERSION first." >&2
  exit 1
fi
if [[ -n "$(git ls-remote --tags origin "refs/tags/${tag}")" ]]; then
  echo "release: tag ${tag} already exists on origin; bump VERSION first." >&2
  exit 1
fi

python3 scripts/check_versions.py
python3 scripts/check_feature_parity.py --release

cat <<EOF
release: ready to publish ${tag}.

Publishing is one annotated repository tag; every language SDK installs from
Git at that tag with the shared root VERSION (Go modules resolve
github.com/jim-technologies/invariantprotocol directly; npm, uv/pip, and
cargo use their Git-dependency syntax). Packages are deliberately never
published to npm, PyPI, or crates.io.

release: refusing to create the tag automatically. After this release
commit's CI workflow passes on main, publish with:

  git tag -a ${tag} -m "${tag}"
  git push origin ${tag}
EOF
exit 1
