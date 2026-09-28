#!/usr/bin/env bash
# Generated-output staleness slice of `make validate`: regenerate every
# committed build artifact, then fail when any generated path differs from what
# is committed, including new untracked outputs.

set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "$root"

generated=(
  proto/descriptor.binpb
  conformance/proto/descriptor.binpb
  go/gen
  go/tests/gen
  python/src/buf
  python/src/invariant/gen
  python/tests/proto/descriptor.binpb
  python/tests/proto/gen
  testdata/cdc/v2
  testdata/data.schema.binpb
  testdata/openapi/descriptor.binpb
  testdata/openapi/gen/library/v1/library.proto
  testdata/schema/descriptor.binpb
  testdata/schema/schema.binpb
  typescript/src/gen
  typescript/tests/gen
)

scripts/generate.sh

stale="$(git status --porcelain --untracked-files=all -- "${generated[@]}")"
if [[ -n "$stale" ]]; then
  echo "Generated files are out of date. Run 'make generate' and commit the results."
  printf '%s\n' "$stale"
  exit 1
fi
