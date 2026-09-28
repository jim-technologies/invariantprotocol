#!/usr/bin/env bash
set -euo pipefail

task_dir="$(mktemp -d)"
trap 'rm -rf "$task_dir"' EXIT
for bundle in testdata/data.schema.binpb testdata/schema/schema.binpb; do
  GOFLAGS=-mod=readonly go run ./go/cmd/invariant-schema sqlite \
    --bundle "$bundle" >> "$task_dir/schema.sql"
done
sqlite3 -batch -bail "$task_dir/records.sqlite" < "$task_dir/schema.sql"
sqlite3 -batch -bail "$task_dir/records.sqlite" <<'SQL'
INSERT INTO data_v1_canonical_record (uint64_value, choice_name)
VALUES ('18446744073709551615', '');
INSERT INTO schema_test_v1_annotated_record (amount, record_id, digest)
VALUES ('-12345678901234.5678', '00000000-0000-0000-0000-000000000000', zeroblob(24));
INSERT INTO schema_test_v1_lance_record (vector, vector64)
VALUES ('[1,2,3,4,5,6,7,8]', '[1,2,3,4]');
SQL
result="$(sqlite3 -batch -bail "$task_dir/records.sqlite" <<'SQL'
SELECT uint64_value || '|' || typeof(uint64_value) || '|' || (choice_name IS NOT NULL)
FROM data_v1_canonical_record;
SQL
)"
[[ "$result" == '18446744073709551615|text|1' ]]
if sqlite3 -batch -bail "$task_dir/records.sqlite" \
  "UPDATE data_v1_canonical_record SET uint64_value = '18446744073709551616';" \
  > "$task_dir/refusal.log" 2>&1; then
  echo 'SQLite accepted an unsigned overflow' >&2
  exit 1
fi
[[ "$(sqlite3 -batch -bail "$task_dir/records.sqlite" 'PRAGMA integrity_check;')" == ok ]]
printf 'SQLite compiled-bundle CLI, durable reopen and overflow refusal passed.\n'
