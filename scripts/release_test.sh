#!/usr/bin/env bash
# Pins scripts/release.sh: every guard refuses before anything is tagged, and a
# release that passes them creates the annotated vVERSION tag at HEAD and pushes
# it. Each case runs the real script in a throwaway clone of a local bare
# "origin", with stub release checks, so the test needs no network, no
# credential and nothing beyond bash, git and python3.

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
tmp="$(mktemp -d "${TMPDIR:-/tmp}/release-test.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT

# Isolate the fixtures from the caller's Git configuration.
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=fixture GIT_AUTHOR_EMAIL=fixture@example.invalid
export GIT_COMMITTER_NAME=fixture GIT_COMMITTER_EMAIL=fixture@example.invalid
unset RELEASE_STUB_FAIL

origin="$tmp/origin.git"
repo="$tmp/repo"
tag=v0.3.0
failures=0

# fixture: origin holds v0.2.0 and a pushed release commit for 0.3.0; repo is
# a clean clone of it. RELEASE_STUB_FAIL=<name> makes that release check fail.
fixture() {
  rm -rf "$origin" "$repo"
  git init -q --bare -b main "$origin"
  git clone -q "$origin" "$repo" 2>/dev/null
  git -C "$repo" symbolic-ref HEAD refs/heads/main
  mkdir -p "$repo/scripts"
  cp "$here/release.sh" "$repo/scripts/"
  local check
  for check in check_versions check_feature_parity; do
    printf 'import os, sys\nif os.environ.get("RELEASE_STUB_FAIL") == "%s":\n    sys.exit("stub %s failed")\n' \
      "$check" "$check" >"$repo/scripts/$check.py"
  done
  # shellcheck disable=SC2016 # the stub expands its own variable
  printf '#!/usr/bin/env bash\nif [[ "${RELEASE_STUB_FAIL:-}" == check_breaking ]]; then\n  echo "stub check_breaking failed" >&2\n  exit 1\nfi\n' \
    >"$repo/scripts/check_breaking.sh"
  chmod +x "$repo/scripts/check_breaking.sh"
  set_release 0.2.0 "## v0.2.0"
  git -C "$repo" tag -a v0.2.0 -m v0.2.0
  set_release 0.3.0 "## v0.3.0 — 2026-01-01"
  git -C "$repo" push -q origin main v0.2.0
}

# set_release VERSION HEADING commits a release state (not pushed).
set_release() {
  printf '%s\n' "$1" >"$repo/VERSION"
  printf '# Changelog\n\n%s\n\n- A change.\n' "$2" >"$repo/CHANGELOG.md"
  git -C "$repo" add -A
  git -C "$repo" commit -q -m "release: $1"
}

on_origin() { [[ -n "$(git -C "$origin" tag --list "$tag")" ]]; }

# check NAME WANT_STATUS WANT_OUTPUT WANT_ON_ORIGIN [NAME=VALUE...]
# WANT_OUTPUT is a fixed string the combined output must contain;
# WANT_ON_ORIGIN (yes/no) is whether origin holds the tag afterwards.
check() {
  local name="$1" want_status="$2" want_output="$3" want_origin="$4" status=0 got_origin=no
  shift 4
  (cd "$repo" && env "$@" scripts/release.sh) >"$tmp/out" 2>&1 || status=$?
  on_origin && got_origin=yes
  if [[ "$status" == "$want_status" && "$got_origin" == "$want_origin" ]] &&
    grep -qF -- "$want_output" "$tmp/out"; then
    printf 'ok   %s\n' "$name"
    return
  fi
  failures=$((failures + 1))
  printf 'FAIL %s: exit %s (want %s), tag on origin %s (want %s), output wants [%s]\n' \
    "$name" "$status" "$want_status" "$got_origin" "$want_origin" "$want_output"
  sed 's/^/       | /' "$tmp/out"
}

no_local_tag() {
  if git -C "$repo" rev-parse --quiet --verify "refs/tags/$tag" >/dev/null; then
    failures=$((failures + 1))
    printf 'FAIL %s: a refused release left tag %s behind\n' "$1" "$tag"
  fi
}

fixture
check "a release creates and pushes the annotated tag" 0 "published $tag" yes
if [[ "$(git -C "$repo" cat-file -t "$tag")" != tag ||
  "$(git -C "$repo" rev-parse "$tag^{commit}")" != "$(git -C "$repo" rev-parse HEAD)" ||
  "$(git -C "$origin" rev-parse "$tag")" != "$(git -C "$repo" rev-parse "$tag")" ]]; then
  failures=$((failures + 1))
  echo "FAIL the published tag is not the annotated tag of HEAD on both sides"
fi
check "a second run refuses the tag it already made" 1 "already exists locally" yes

fixture
printf '9.9\n' >"$repo/VERSION"
git -C "$repo" commit -q -am "bad version" && git -C "$repo" push -q origin main
check "a malformed VERSION refuses" 1 "MAJOR.MINOR.PATCH" no

fixture
touch "$repo/untracked"
check "an untracked file refuses" 1 "the tree is dirty" no
no_local_tag "an untracked file refuses"

fixture
printf -- '- Edited.\n' >>"$repo/CHANGELOG.md"
check "a modified tracked file refuses" 1 "the tree is dirty" no

fixture
git -C "$repo" commit -q --allow-empty -m unpushed
check "an unpushed commit refuses" 1 "not pushed to origin/main" no
no_local_tag "an unpushed commit refuses"

fixture
set_release 0.3.0 "## Unreleased" && git -C "$repo" push -q origin main
check "an unreleased first heading refuses" 1 "must be '## $tag', not '## Unreleased'" no

fixture
set_release 0.3.0 "## v0.3.1" && git -C "$repo" push -q origin main
check "a first heading for another version refuses" 1 "must be '## $tag'" no

fixture
git -C "$repo" tag -a "$tag" -m "$tag"
check "an existing local tag refuses" 1 "already exists locally" no

fixture
git -C "$repo" tag -a "$tag" -m "$tag"
git -C "$repo" push -q origin "$tag"
git -C "$repo" tag -d "$tag" >/dev/null
check "a tag that exists only on origin refuses" 1 "already exists on origin" yes
no_local_tag "a tag that exists only on origin refuses"

for failing in check_versions check_feature_parity check_breaking; do
  fixture
  check "a failing $failing refuses before tagging" 1 "stub $failing failed" no RELEASE_STUB_FAIL="$failing"
  no_local_tag "a failing $failing refuses before tagging"
done

if ((failures > 0)); then
  echo "release_test: ${failures} case(s) failed" >&2
  exit 1
fi
echo "release_test: all cases passed"
