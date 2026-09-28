#!/usr/bin/env bash
# Publish one Invariant release (MAKEFILE-CONTRACT.md `make release`).
#
# Distribution is Git-only: every language SDK installs from the single
# annotated root tag vVERSION, so publishing is creating and pushing that tag.
# Packages are deliberately never published to npm, PyPI, or crates.io.
#
# Run it from a maintainer's machine once the release commit is on main and
# its CI gate has passed; CI never publishes, it only reruns the gate on the
# pushed tag. The guards come first and fail closed: a clean tree including
# untracked files, HEAD pushed to origin/main, VERSION equal to the first
# CHANGELOG.md heading, and tag vVERSION absent locally and on origin. Then the
# release-only checks run (version alignment, strict feature parity, protobuf
# compatibility), and only then is the tag created and pushed.
#
# scripts/release_test.sh pins this behaviour.

set -euo pipefail
shopt -s inherit_errexit

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "$root"

refuse() {
  echo "release: refusing: $*" >&2
  exit 1
}

version="$(tr -d '[:space:]' < VERSION)"
tag="v${version}"

[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] ||
  refuse "VERSION must be MAJOR.MINOR.PATCH, got ${version}"

dirty="$(git status --porcelain --untracked-files=all)"
[[ -z "$dirty" ]] || refuse "the tree is dirty; commit or stash everything first"

git fetch --quiet origin main
git merge-base --is-ancestor HEAD origin/main ||
  refuse "HEAD is not pushed to origin/main"

heading="$(grep -m 1 '^## ' CHANGELOG.md || true)"
[[ "$heading" == "## ${tag}" || "$heading" == "## ${tag} "* ]] ||
  refuse "the first CHANGELOG.md heading must be '## ${tag}', not '${heading}'"

if git rev-parse --quiet --verify "refs/tags/${tag}" >/dev/null; then
  refuse "tag ${tag} already exists locally; bump VERSION first"
fi
remote_tag="$(git ls-remote --tags origin "refs/tags/${tag}")"
[[ -z "$remote_tag" ]] || refuse "tag ${tag} already exists on origin; bump VERSION first"

python3 scripts/check_versions.py
python3 scripts/check_feature_parity.py --release
scripts/check_breaking.sh

git tag -a "$tag" -m "$tag"
git push origin "refs/tags/${tag}"
echo "release: published ${tag}; Go, Python, Rust, and TypeScript install from this one Git tag."
