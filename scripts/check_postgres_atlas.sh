#!/usr/bin/env bash
set -euo pipefail

docker info >/dev/null

tmp_dir="$(mktemp -d)"
container_name="invariant-atlas-${PPID}-${RANDOM}"
postgres_image="postgres:18.4@sha256:32ca0af8e77bfb8c6610c488e4691f83f972a3e9e64d3b02facf3ab111ad5500"
cleanup() {
  docker rm --force "$container_name" >/dev/null 2>&1 || true
  rm -rf "$tmp_dir"
}
trap cleanup EXIT

go run ./go/cmd/invariant-schema sql \
  --bundle testdata/data.schema.binpb \
  --message data.v1.CanonicalRecord \
  --output "$tmp_dir/schema.sql"

docker run --detach \
  --name "$container_name" \
  --env POSTGRES_PASSWORD=postgres \
  --env POSTGRES_DB=invariant \
  --health-cmd "pg_isready --username postgres --dbname invariant" \
  --health-interval 1s \
  --health-timeout 5s \
  --health-retries 30 \
  --publish 127.0.0.1::5432 \
  "$postgres_image" >/dev/null

for _ in {1..30}; do
  if ! health="$(docker inspect --format '{{.State.Health.Status}}' "$container_name")"; then
    docker logs "$container_name" || true
    exit 1
  fi
  if [[ "$health" == "healthy" ]]; then
    break
  fi
  if [[ "$health" == "unhealthy" ]]; then
    docker logs "$container_name"
    exit 1
  fi
  sleep 1
done
if [[ "$health" != "healthy" ]]; then
  docker logs "$container_name"
  exit 1
fi

docker exec "$container_name" createdb --username postgres dev
published_address="$(docker port "$container_name" 5432/tcp)"
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
grep -Fq 'CREATE TABLE "data_v1_canonical_record"' "$tmp_dir/inspected.sql"
grep -Fq '"data_v1_canonical_record_choice_oneof_check"' "$tmp_dir/inspected.sql"

atlas schema diff \
  --from "$target_url" \
  --to "$schema_url" \
  --dev-url "$dev_url" \
  --format '{{ sql . "" }}' >"$tmp_dir/diff.sql"
test ! -s "$tmp_dir/diff.sql"
