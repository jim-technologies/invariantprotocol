#!/usr/bin/env bash
set -euo pipefail

image='clickhouse/clickhouse-server:26.7.1.1315@sha256:d7556a3841027651307b5aa08d72b5c467d0241d3db5b67d9e158ef3975626f5'
container_name="invariant-clickhouse-$$"
container_id=''

cleanup() {
  if [[ -n "$container_id" ]]; then
    docker rm --force --volumes "$container_id" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

docker info >/dev/null
container_id="$(
  docker run --detach \
    --name "$container_name" \
    --env CLICKHOUSE_SKIP_USER_SETUP=1 \
    --publish 127.0.0.1::8123 \
    "$image"
)"

binding="$(docker port "$container_id" 8123/tcp)"
port="${binding##*:}"
endpoint="http://127.0.0.1:$port"

ready=false
for _ in $(seq 1 60); do
  if curl --fail --silent --show-error "$endpoint/ping" >/dev/null 2>&1; then
    ready=true
    break
  fi
  sleep 1
done
if [[ "$ready" != true ]]; then
  docker logs "$container_id" >&2
  printf 'ClickHouse did not become ready at %s\n' "$endpoint" >&2
  exit 1
fi

INVARIANT_CLICKHOUSE_URL="$endpoint" \
  GOFLAGS=-mod=readonly \
  go test -count=1 -run '^TestClickHouseDDLAndValueRoundTrip$' ./go/data/clickhouse
