#!/usr/bin/env bash
# Pins scripts/check_breaking.sh: the baseline it hands to buf, when it skips,
# and when it honours or refuses INVARIANT_ALLOW_PROTO_BREAK. Each case runs the
# real script in a throwaway Git fixture with a stub `buf` that records its
# arguments and exits with a chosen status, so the test needs no network, no
# buf module and nothing beyond bash and git.

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
tmp="$(mktemp -d "${TMPDIR:-/tmp}/check-breaking-test.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT

# Isolate the fixtures from the caller's Git configuration and CI context.
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=fixture GIT_AUTHOR_EMAIL=fixture@example.invalid
export GIT_COMMITTER_NAME=fixture GIT_COMMITTER_EMAIL=fixture@example.invalid
unset GITHUB_BASE_REF INVARIANT_ALLOW_PROTO_BREAK

mkdir "$tmp/bin"
cat >"$tmp/bin/buf" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >"$BUF_STUB_LOG"
exit "${BUF_STUB_EXIT:-0}"
STUB
chmod +x "$tmp/bin/buf"
export PATH="$tmp/bin:$PATH" BUF_STUB_LOG="$tmp/buf.log"

repo="$tmp/repo"
failures=0

# fixture: a repository whose history is v0.1.0, v0.2.0, a later commit, a
# reachable pre-release tag, and a newer release tag on an unmerged branch.
fixture() {
  rm -rf "$repo"
  mkdir -p "$repo/scripts" "$repo/proto"
  cp "$here/check_breaking.sh" "$repo/scripts/"
  git -C "$repo" init -q -b main
  git -C "$repo" commit -q --allow-empty -m one
  git -C "$repo" tag v0.1.0
  git -C "$repo" commit -q --allow-empty -m two
  git -C "$repo" tag -a v0.2.0 -m v0.2.0
  git -C "$repo" switch -q -c side
  git -C "$repo" commit -q --allow-empty -m side
  git -C "$repo" tag v0.9.0
  git -C "$repo" switch -q main
  git -C "$repo" commit -q --allow-empty -m three
  git -C "$repo" tag v0.3.0-rc.1
  set_version 0.3.0
  set_changelog "## Unreleased" "### Fixed" "- A fix."
}

set_version() { printf '%s\n' "$1" >"$repo/VERSION"; }

set_changelog() {
  { printf '# Changelog\n\n'; printf '%s\n\n' "$@"; printf '## v0.2.0\n\n### Breaking\n\n- Old.\n'; } >"$repo/CHANGELOG.md"
}

# check NAME WANT_STATUS WANT_BUF_ARGS WANT_OUTPUT [NAME=VALUE...]
# An empty WANT_BUF_ARGS means buf must not run; WANT_OUTPUT is a fixed
# string the combined output must contain (empty: anything).
check() {
  local name="$1" want_status="$2" want_buf="$3" want_output="$4" status=0 got_buf=""
  shift 4
  rm -f "$BUF_STUB_LOG"
  (cd "$repo" && env "$@" scripts/check_breaking.sh) >"$tmp/out" 2>&1 || status=$?
  [[ -f "$BUF_STUB_LOG" ]] && got_buf="$(cat "$BUF_STUB_LOG")"
  if [[ "$status" == "$want_status" && "$got_buf" == "$want_buf" ]] &&
    grep -qF -- "$want_output" "$tmp/out"; then
    printf 'ok   %s\n' "$name"
    return
  fi
  failures=$((failures + 1))
  printf 'FAIL %s: exit %s (want %s), buf [%s] (want [%s]), output wants [%s]\n' \
    "$name" "$status" "$want_status" "$got_buf" "$want_buf" "$want_output"
  sed 's/^/       | /' "$tmp/out"
}

against_tag() { printf 'breaking --against ../.git#tag=%s,subdir=proto' "$1"; }

fixture
check "an ordinary commit compares against the newest reachable release tag" \
  0 "$(against_tag v0.2.0)" "against v0.2.0"
check "a pull request compares against its base branch" \
  0 "breaking --against ../.git#ref=origin/main,subdir=proto" "origin/main" GITHUB_BASE_REF=main
check "a reported break fails the check" \
  100 "$(against_tag v0.2.0)" "" BUF_STUB_EXIT=100

git -C "$repo" tag -a v0.3.0 -m v0.3.0
check "the release commit compares against the previous release" \
  0 "$(against_tag v0.2.0)" "against v0.2.0"
git -C "$repo" commit -q --allow-empty -m four
check "the commit after a release compares against that release" \
  0 "$(against_tag v0.3.0)" "against v0.3.0"

rm -rf "$repo" && mkdir -p "$repo/scripts" "$repo/proto"
cp "$here/check_breaking.sh" "$repo/scripts/"
git -C "$repo" init -q -b main && git -C "$repo" commit -q --allow-empty -m only
set_version 0.1.0
check "no reachable release tag skips buf loudly" 0 "" "skipping buf breaking"

fixture
set_changelog "## Unreleased" "### Breaking" "- Renamed a field."
check "the flag accepts a break the unreleased section documents" \
  0 "$(against_tag v0.2.0)" "accepting the breaks" BUF_STUB_EXIT=100 INVARIANT_ALLOW_PROTO_BREAK=1
check "the flag never hides a buf failure that is not a break" \
  1 "$(against_tag v0.2.0)" "" BUF_STUB_EXIT=1 INVARIANT_ALLOW_PROTO_BREAK=1
check "the flag only accepts the value 1" \
  1 "" "set it to 1" BUF_STUB_EXIT=100 INVARIANT_ALLOW_PROTO_BREAK=yes
set_version 1.0.0
check "the flag is refused from 1.0" \
  1 "" "needs a new major version" BUF_STUB_EXIT=100 INVARIANT_ALLOW_PROTO_BREAK=1

fixture
check "the flag is refused without a Breaking entry" \
  1 "" "no '### Breaking' entry" BUF_STUB_EXIT=100 INVARIANT_ALLOW_PROTO_BREAK=1
set_changelog "## Unreleased" "### Breaking" "Prose is not an entry." "### Fixed" "- A fix."
check "the flag is refused when Breaking has no list entry" \
  1 "" "no '### Breaking' entry" BUF_STUB_EXIT=100 INVARIANT_ALLOW_PROTO_BREAK=1
set_changelog "## v0.3.0 — 2026-01-01" "### Breaking" "- Renamed a field."
check "the flag accepts the release being cut before it is tagged" \
  0 "$(against_tag v0.2.0)" "accepting the breaks" BUF_STUB_EXIT=100 INVARIANT_ALLOW_PROTO_BREAK=1
git -C "$repo" tag -a v0.3.0 -m v0.3.0
check "the flag accepts the release commit once it is tagged" \
  0 "$(against_tag v0.2.0)" "accepting the breaks" BUF_STUB_EXIT=100 INVARIANT_ALLOW_PROTO_BREAK=1
git -C "$repo" commit -q --allow-empty -m four
check "the flag is refused once that release has shipped" \
  1 "" "is already released" BUF_STUB_EXIT=100 INVARIANT_ALLOW_PROTO_BREAK=1
set_changelog "## v0.2.9" "### Breaking" "- Renamed a field."
check "the flag is refused for a section that is not the current VERSION" \
  1 "" "must be '## Unreleased' or '## v0.3.0'" BUF_STUB_EXIT=100 INVARIANT_ALLOW_PROTO_BREAK=1

if ((failures > 0)); then
  echo "check_breaking_test: ${failures} case(s) failed" >&2
  exit 1
fi
echo "check_breaking_test: all cases passed"
