#!/usr/bin/env bash
# `make connect-interop`: build the Go, Python, and Rust Connect interop
# servers and exercise each with the official Connect-ES client.

set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "$root"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

rust_target="${CARGO_TARGET_DIR:-rust/target}"
case "$rust_target" in
  /*) ;;
  *) rust_target="$root/$rust_target" ;;
esac

GOFLAGS=-mod=readonly go build -o "$tmp/go-connect-interop" ./go/tests/connectinterop
uv sync --locked --project python
cargo build --manifest-path rust/Cargo.toml --locked --target-dir "$rust_target" --example connect_interop_server
INVARIANT_CONNECT_INTEROP_GO="$tmp/go-connect-interop" \
  INVARIANT_CONNECT_INTEROP_RUST="$rust_target/debug/examples/connect_interop_server" \
  node typescript/tests/connect_interop.ts
