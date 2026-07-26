#!/usr/bin/env bash

set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
descriptor="$root/testdata/openapi/descriptor.binpb"
# Go must see the generated package inside the repository's locked module.
# The EXIT trap removes this directory on success and on ordinary failures.
tmp="$(mktemp -d "$root/.openapi-codegen.XXXXXX")"

cleanup() {
  chmod -R u+w "$tmp" 2>/dev/null || true
  rm -rf "$tmp"
}
trap cleanup EXIT

cd "$root"
buf generate "$descriptor" \
  --template testdata/openapi/buf.codegen.gen.yaml \
  --output "$tmp"

echo "==> Go"
go_package="$tmp/go/library/v1"
GOFLAGS=-mod=readonly go test "./${go_package#"$root/"}"
(
  cd "$go_package"
  GOFLAGS=-mod=readonly go doc . RegisterLibraryServiceServer >/dev/null
  GOFLAGS=-mod=readonly go doc . LibraryServiceServer >/dev/null
)

echo "==> Python"
uv run --locked --project python python -m grpc_tools.protoc \
  --descriptor_set_in="$descriptor" \
  --grpc_python_out="$tmp/python" \
  library/v1/library.proto
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH="$tmp/python" \
  uv run --locked --project python python - <<'PY'
from library.v1 import library_pb2, library_pb2_grpc

assert library_pb2.Book.DESCRIPTOR.full_name == "library.v1.Book"
assert library_pb2_grpc.LibraryServiceServicer is not None
assert library_pb2_grpc.add_LibraryServiceServicer_to_server is not None
PY

echo "==> Rust"
rust_smoke="$tmp/rust-smoke"
cargo fetch --quiet --manifest-path rust/Cargo.toml --locked
cargo new --quiet --lib --name invariant_openapi_codegen_smoke "$rust_smoke"
cp testdata/openapi/rust-smoke/build.rs "$rust_smoke/build.rs"
cp testdata/openapi/rust-smoke/lib.rs "$rust_smoke/src/lib.rs"
cp rust/Cargo.lock "$rust_smoke/Cargo.lock"
(
  cd "$rust_smoke"
  cargo add --quiet --offline invariant-protocol --path "$root/rust"
  cargo add --quiet --offline prost@0.14 prost-types@0.14 tonic@0.14 tonic-prost@0.14
  cargo add --quiet --offline --build invariant-protocol-codegen --path "$root/rust/codegen"
  cargo add --quiet --offline --build prost@0.14 prost-types@0.14
  INVARIANT_OPENAPI_DESCRIPTOR="$descriptor" cargo check --quiet --locked --offline
)

echo "==> TypeScript"
typescript_out="$tmp/typescript-js"
./node_modules/.bin/tsc \
  --outDir "$typescript_out" \
  --target ES2023 \
  --module NodeNext \
  --moduleResolution NodeNext \
  --strict \
  --skipLibCheck \
  "$tmp/typescript/library/v1/library_pb.ts"
INVARIANT_OPENAPI_TYPESCRIPT="$typescript_out/library/v1/library_pb.js" \
  node --input-type=module - <<'JS'
import { pathToFileURL } from "node:url";

const generated = await import(pathToFileURL(process.env.INVARIANT_OPENAPI_TYPESCRIPT).href);
if (generated.LibraryService?.typeName !== "library.v1.LibraryService") {
  throw new Error("generated LibraryService descriptor is unavailable");
}
JS

echo "OpenAPI-import code generation passed for Go, Python, Rust, and TypeScript"
