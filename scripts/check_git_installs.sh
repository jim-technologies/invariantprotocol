#!/usr/bin/env bash

set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
sha="$(git -C "$root" rev-parse HEAD)"
version="$(tr -d '[:space:]' < "$root/VERSION")"
install_ref="$sha"
if [[ "${GITHUB_REF_TYPE:-}" == "tag" ]]; then
  if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "VERSION must be MAJOR.MINOR.PATCH, got ${version}" >&2
    exit 1
  fi
  install_ref="${GITHUB_REF_NAME:?GITHUB_REF_NAME is required for a tag build}"
  expected_tag="v${version}"
  if [[ "$install_ref" != "$expected_tag" ]]; then
    echo "release tag is ${install_ref}; expected ${expected_tag}" >&2
    exit 1
  fi
  checkout_tag_commit="$(git -C "$root" rev-parse "refs/tags/${install_ref}^{commit}")"
  if [[ "$checkout_tag_commit" != "$sha" ]]; then
    echo "release tag ${install_ref} resolves to ${checkout_tag_commit}; expected checked-out commit ${sha}" >&2
    exit 1
  fi
fi
tmp="$(mktemp -d)"

cleanup() {
  chmod -R u+w "$tmp" 2>/dev/null || true
  rm -rf "$tmp"
}
trap cleanup EXIT

# A bare clone ensures every consumer sees only files committed at this exact
# revision, never generated files or other state from the working tree.
source_repo="$tmp/invariantprotocol.git"
git clone --quiet --bare "$root" "$source_repo"
git --git-dir="$source_repo" cat-file -e "${sha}^{commit}"
if [[ "${GITHUB_REF_TYPE:-}" == "tag" ]]; then
  source_tag_commit="$(git --git-dir="$source_repo" rev-parse "refs/tags/${install_ref}^{commit}")"
  if [[ "$source_tag_commit" != "$sha" ]]; then
    echo "cloned release tag ${install_ref} resolves to ${source_tag_commit}; expected ${sha}" >&2
    exit 1
  fi
fi

echo "Checking Git installs from ${install_ref} (${sha})"

echo "==> Go"
go_consumer="$tmp/go-consumer"
mkdir -p "$go_consumer" "$tmp/gopath"
(
  cd "$go_consumer"
  go mod init example.com/invariant-git-install >/dev/null

  git_config="$tmp/gitconfig"
  git config --file "$git_config" \
    --add "url.file://${source_repo}.insteadOf" \
    "https://github.com/jim-technologies/invariantprotocol.git"
  git config --file "$git_config" \
    --add "url.file://${source_repo}.insteadOf" \
    "https://github.com/jim-technologies/invariantprotocol"

  env \
    GIT_ALLOW_PROTOCOL=file:https \
    GIT_CONFIG_GLOBAL="$git_config" \
    GIT_CONFIG_NOSYSTEM=1 \
    GOPATH="$tmp/gopath" \
    GOPRIVATE=github.com/jim-technologies/invariantprotocol \
    GOPROXY=https://proxy.golang.org,direct \
    go get \
      "github.com/jim-technologies/invariantprotocol/go@${install_ref}" \
      "github.com/jim-technologies/invariantprotocol/go/cmd/invariant-openapi@${install_ref}" \
      "github.com/jim-technologies/invariantprotocol/go/cmd/invariant-schema@${install_ref}"
  env \
    GIT_ALLOW_PROTOCOL=file:https \
    GIT_CONFIG_GLOBAL="$git_config" \
    GIT_CONFIG_NOSYSTEM=1 \
    GOPATH="$tmp/gopath" \
    GOPRIVATE=github.com/jim-technologies/invariantprotocol \
    GOPROXY=https://proxy.golang.org,direct \
    go build \
      github.com/jim-technologies/invariantprotocol/go \
      github.com/jim-technologies/invariantprotocol/go/cmd/invariant-openapi \
      github.com/jim-technologies/invariantprotocol/go/cmd/invariant-schema
)

echo "==> Python"
python_venv="$tmp/python-venv"
uv venv --quiet --python 3.14 "$python_venv"
uv pip install --quiet --python "$python_venv/bin/python" \
  "invariant-protocol[data] @ git+file://${source_repo}@${install_ref}#subdirectory=python"
EXPECTED_VERSION="$version" "$python_venv/bin/python" - <<'PY'
import importlib.metadata
import os

import invariant
import pyarrow

assert importlib.metadata.version("invariant-protocol") == os.environ["EXPECTED_VERSION"]
assert invariant.Server is not None
assert invariant.arrow_table is not None
assert pyarrow.__version__
PY

echo "==> Rust"
rust_consumer="$tmp/rust-consumer"
cargo new --quiet --lib --name invariant_git_install "$rust_consumer"
(
  cd "$rust_consumer"
  cargo add --quiet invariant-protocol \
    --git "file://${source_repo}" \
    --rev "$install_ref"
  cargo add --quiet --build invariant-protocol-codegen \
    --git "file://${source_repo}" \
    --rev "$install_ref"
  cargo check --quiet --locked
)

echo "==> TypeScript"
npm_consumer="$tmp/npm-consumer"
mkdir -p "$npm_consumer"
(
  cd "$npm_consumer"
  npm init --yes --silent >/dev/null
  npm install --silent --allow-git=root \
    "git+file://${source_repo}#${install_ref}"
  EXPECTED_VERSION="$version" node --input-type=module <<'JS'
import { SERVER_VERSION, Server } from "@jim-technologies/invariant-protocol";

if (SERVER_VERSION !== process.env.EXPECTED_VERSION) {
  throw new Error(`installed ${SERVER_VERSION}; expected ${process.env.EXPECTED_VERSION}`);
}
if (typeof Server !== "function") {
  throw new Error("Server export is unavailable");
}
JS
)

echo "Git installs passed for Go, Python, Rust, and TypeScript"
