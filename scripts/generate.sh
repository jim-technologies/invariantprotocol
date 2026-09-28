#!/usr/bin/env bash
# `make generate`: regenerate every committed build artifact from its source —
# protobuf descriptors and the Go, Python, and TypeScript bindings, the
# SchemaBundle fixtures, the OpenAPI importer fixture, and the CDC v2 fixtures.
# scripts/verify_generate.sh runs this and fails on any difference.

set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "$root"

# Remove only configured generator outputs. Keep Python package markers and
# hand-written files; this makes deleted/renamed protos delete stale bindings.
find go/gen go/tests/gen -type f -name '*.pb.go' -delete
find python/src/invariant/gen python/src/buf python/tests/proto/gen -type f \( -name '*_pb2.py' -o -name '*_pb2.pyi' -o -name '*_pb2_grpc.py' \) -delete
find typescript/src/gen typescript/tests/gen -type f -name '*_pb.ts' -delete

(
  cd proto
  buf build -o descriptor.binpb
  NODE_NO_WARNINGS=1 buf generate descriptor.binpb
  NODE_NO_WARNINGS=1 buf generate --template buf.googleapis.gen.yaml
)
(
  cd python/tests/proto
  buf build -o descriptor.binpb
  buf generate descriptor.binpb
  buf generate --template buf.validate.gen.yaml
  uv run --locked --project ../.. python -m grpc_tools.protoc --descriptor_set_in=descriptor.binpb --grpc_python_out=gen greet.proto
)
(
  cd conformance/proto
  buf build -o descriptor.binpb
  buf generate descriptor.binpb
  uv run --locked --project ../../python python -m grpc_tools.protoc --descriptor_set_in=descriptor.binpb --grpc_python_out=../../python/tests/proto/gen invariantprotocol/conformance/v1/native_cardinality.proto
)
buf build --config buf.data.yaml --path testdata/schema/test/v1/annotated.proto -o testdata/schema/descriptor.binpb
GOFLAGS=-mod=readonly go run ./go/cmd/invariant-schema compile --descriptor testdata/schema/descriptor.binpb --output testdata/schema/schema.binpb
GOFLAGS=-mod=readonly go run ./go/cmd/invariant-schema compile --descriptor python/tests/proto/descriptor.binpb --message data.v1.CanonicalRecord --message data.v1.Proto2Record --output testdata/data.schema.binpb
mkdir -p testdata/openapi/gen/library/v1
GOFLAGS=-mod=readonly go run ./go/cmd/invariant-openapi import --input testdata/openapi/library.yaml --package library.v1 --go-package example.com/project/gen/library/v1 --output testdata/openapi/gen/library/v1/library.proto
(
  cd testdata/openapi
  buf format -w gen/library/v1/library.proto
  buf build -o descriptor.binpb
)
GOFLAGS=-mod=readonly go run ./scripts/generate_cdc_v2_fixtures.go
