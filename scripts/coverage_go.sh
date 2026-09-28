#!/usr/bin/env bash
# `make coverage-go`: run the Go tests over authored packages (generated code
# and runnable test fixtures excluded) and enforce the 80% statement floor.

set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "$root"
export GOFLAGS=-mod=readonly

all_packages="$(go list ./go/...)"
packages="$(printf '%s\n' "$all_packages" | grep -Ev '/go/(gen/|tests/(connectinterop|gen|manual)$)')"
profile="$(mktemp)"
trap 'rm -f "$profile"' EXIT

# shellcheck disable=SC2086 # one package path per word
go test -count=1 -covermode=atomic -coverprofile="$profile" $packages
total="$(go tool cover -func="$profile" | awk '/^total:/ {gsub("%", "", $3); print $3}')"
awk -v total="$total" 'BEGIN { printf "Go authored statement coverage: %.1f%% (required: 80.0%%)\n", total; exit !(total >= 80.0) }'
