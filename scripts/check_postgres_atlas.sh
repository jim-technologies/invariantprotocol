#!/usr/bin/env bash
set -euo pipefail

docker info >/dev/null

tmp_dir="$(mktemp -d)"
container_name="invariant-atlas-${PPID}-${RANDOM}"
container_id=""
postgres_image="postgres:18.4@sha256:32ca0af8e77bfb8c6610c488e4691f83f972a3e9e64d3b02facf3ab111ad5500"
cleanup() {
  if [[ -n "$container_id" ]]; then
    docker rm --force --volumes "$container_id" >/dev/null 2>&1 || true
  fi
  rm -rf "$tmp_dir"
}
trap cleanup EXIT

{
  GOFLAGS=-mod=readonly go run ./go/cmd/invariant-schema sql \
    --bundle testdata/data.schema.binpb
  printf '\n'
  GOFLAGS=-mod=readonly go run ./go/cmd/invariant-schema sql \
    --bundle testdata/schema/schema.binpb
} >"$tmp_dir/schema.sql"

container_id="$(docker run --detach \
  --name "$container_name" \
  --env POSTGRES_PASSWORD=postgres \
  --env POSTGRES_DB=invariant \
  --health-cmd "pg_isready --username postgres --dbname invariant" \
  --health-interval 1s \
  --health-timeout 5s \
  --health-retries 30 \
  --publish 127.0.0.1::5432 \
  "$postgres_image")"

for _ in {1..30}; do
  if ! health="$(docker inspect --format '{{.State.Health.Status}}' "$container_id")"; then
    docker logs "$container_id" || true
    exit 1
  fi
  if [[ "$health" == "healthy" ]]; then
    break
  fi
  if [[ "$health" == "unhealthy" ]]; then
    docker logs "$container_id"
    exit 1
  fi
  sleep 1
done
if [[ "$health" != "healthy" ]]; then
  docker logs "$container_id"
  exit 1
fi

docker exec "$container_id" createdb --username postgres dev
published_address="$(docker port "$container_id" 5432/tcp)"
published_port="${published_address##*:}"
target_url="postgres://postgres:postgres@127.0.0.1:$published_port/invariant?sslmode=disable&search_path=public"
dev_url="postgres://postgres:postgres@127.0.0.1:$published_port/dev?sslmode=disable&search_path=public"
schema_url="file://$tmp_dir/schema.sql"

atlas schema apply \
  --url "$target_url" \
  --to "$schema_url" \
  --dev-url "$dev_url" \
  --auto-approve

atlas schema inspect \
  --url "$target_url" \
  --format '{{ sql . }}' >"$tmp_dir/inspected.sql"
grep -Fq 'CREATE TABLE "schema_test_v1_annotated_record"' "$tmp_dir/inspected.sql"
grep -Fq '"amount" numeric(18,4)' "$tmp_dir/inspected.sql"
grep -Fq '"record_id" uuid' "$tmp_dir/inspected.sql"
grep -Fq '"schema_test_v1_annotated_record_digest_fixed_bytes_check"' "$tmp_dir/inspected.sql"
grep -Fq '"schema_test_v1_annotated_record_reference_oneof_check"' "$tmp_dir/inspected.sql"
grep -Fq 'CREATE TABLE "data_v1_canonical_record"' "$tmp_dir/inspected.sql"
grep -Fq '"double_value" double precision NOT NULL DEFAULT 0' "$tmp_dir/inspected.sql"
grep -Fq '"optional_note" text NULL' "$tmp_dir/inspected.sql"
grep -Fq "\"labels\" jsonb NOT NULL DEFAULT '[]'" "$tmp_dir/inspected.sql"
grep -Fq "\"counters\" jsonb NOT NULL DEFAULT '{}'" "$tmp_dir/inspected.sql"
grep -Fq 'CREATE TABLE "data_v1_proto2_record"' "$tmp_dir/inspected.sql"

assert_query() {
  local query="$1"
  local expected="$2"
  local actual
  actual="$(
    docker exec "$container_id" \
      psql --username postgres --dbname invariant --tuples-only --no-align --command "$query"
  )"
  if [[ "$actual" != "$expected" ]]; then
    printf 'PostgreSQL catalog assertion failed.\nExpected: %s\nActual:   %s\n' "$expected" "$actual" >&2
    return 1
  fi
}

assert_query \
  "SELECT string_agg(tablename, ',' ORDER BY tablename) FROM pg_tables WHERE schemaname = 'public';" \
  "data_v1_canonical_record,data_v1_proto2_record,schema_test_v1_annotated_record"
assert_query \
  "SELECT is_nullable || '|' || COALESCE(column_default, '<null>') FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'data_v1_canonical_record' AND column_name = 'double_value';" \
  "NO|0"
assert_query \
  "SELECT is_nullable || '|' || COALESCE(column_default, '<null>') FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'data_v1_canonical_record' AND column_name = 'optional_note';" \
  "YES|<null>"
assert_query \
  "SELECT is_nullable || '|' || COALESCE(column_default, '<null>') FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'data_v1_canonical_record' AND column_name = 'labels';" \
  "NO|'[]'::jsonb"
assert_query \
  "SELECT is_nullable || '|' || COALESCE(column_default, '<null>') FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'data_v1_canonical_record' AND column_name = 'counters';" \
  "NO|'{}'::jsonb"
assert_query \
  "SELECT is_nullable || '|' || COALESCE(column_default, '<null>') FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'data_v1_proto2_record' AND column_name = 'id';" \
  "NO|<null>"
assert_query \
  "SELECT is_nullable || '|' || COALESCE(column_default, '<null>') FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'data_v1_proto2_record' AND column_name = 'label';" \
  "YES|<null>"
assert_query \
  "SELECT obj_description('public.data_v1_canonical_record'::regclass, 'pg_class');" \
  "CanonicalRecord exercises every protobuf shape supported by data projection."
assert_query \
  "SELECT col_description('public.data_v1_canonical_record'::regclass, ordinal_position::integer) FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'data_v1_canonical_record' AND column_name = 'double_value';" \
  "A double-precision value."
assert_query \
  "SELECT obj_description('public.schema_test_v1_annotated_record'::regclass, 'pg_class');" \
  "AnnotatedRecord exercises the complete authored-proto data-schema path."
assert_query \
  "SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid = 'public.schema_test_v1_annotated_record'::regclass AND conname = 'schema_test_v1_annotated_record_digest_fixed_bytes_check';" \
  "CHECK ((octet_length(digest) = 24))"
assert_query \
  "SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid = 'public.schema_test_v1_annotated_record'::regclass AND conname = 'schema_test_v1_annotated_record_reference_oneof_check';" \
  "CHECK ((num_nonnulls(external_id, sequence) <= 1))"

atlas schema diff \
  --from "$target_url" \
  --to "$schema_url" \
  --dev-url "$dev_url" \
  --format '{{ sql . "" }}' >"$tmp_dir/diff.sql"
test ! -s "$tmp_dir/diff.sql"
