# SQLite schema projection

`invariant-schema sqlite --bundle schema.binpb` renders desired-state DDL for
all datasets, or selects one with `--message`. The existing protobuf contract
and derived SchemaBundle remain the logical authority. The command neither
opens a database nor applies migrations. There is no new IR version, inferred
key/index, hidden column, or runtime dependency on a database driver.

The projection uses SQLite INTEGER, REAL, TEXT and BLOB with storage-class
checks. Ordinary SQLite affinity still applies before checks; callers must bind
the intended representation. In particular, never convert unsigned 64-bit or
decimal values through a floating-point number before binding their text.
This follows SQLite's documented [affinity rules](https://www.sqlite.org/datatype3.html).

| Logical domain | Representation and checks |
| --- | --- |
| Signed integers | INTEGER, with exact 32-bit checks where applicable |
| Unsigned 32-bit | INTEGER, 0 through 4294967295 |
| Unsigned 64-bit | Canonical decimal TEXT, 0 through 18446744073709551615; lexical ordering is not numeric ordering |
| Boolean | INTEGER, 0 or 1 |
| Enum | INTEGER with signed 32-bit bounds; closed enums also restrict declared numbers |
| Float / double | REAL; NaN becomes NULL in SQLite, losing present-NaN identity; float32 domain validation remains the writer's responsibility |
| String / bytes | TEXT / BLOB, preserving embedded zero bytes |
| Timestamp | INTEGER Unix microseconds within protobuf's complete year range; sub-microsecond precision is reduced explicitly by the writer |
| Duration | INTEGER nanoseconds; the int64 range is narrower than protobuf Duration |
| Decimal | Exact canonical fixed-scale TEXT, precision/scale/sign/zero checks; arithmetic is application-owned |
| UUID | Lowercase canonical hexadecimal TEXT with exact hyphen positions |
| Fixed bytes | BLOB with exact byte length |
| Message, list, map, dynamic JSON | ProtoJSON TEXT with syntax and top-level shape checks; top-level fixed lists additionally enforce length |

NULL preserves explicit presence. Implicit fields use protobuf defaults;
collections default to empty JSON except fixed lists, which have no invented
valid default. Proto2 declared defaults remain metadata and do not erase
absence. Oneof checks allow at most one non-NULL member. Empty messages cannot
be represented as zero-column SQLite tables and are refused. Identifiers retain
their stored spelling and are quoted; case-insensitive collisions and the
reserved `sqlite_` table prefix fail. Comments become safely prefixed SQL line
comments, not catalog records.

JSON uses `json_valid()` and guarded shape checks from SQLite's standard
[JSON functions](https://www.sqlite.org/json1.html). JSON text rejects literal
NUL; encoded strings may contain `\u0000`. Nested protobuf types, numeric
bounds, presence, oneofs, refinements and Any descriptor resolution remain
writer obligations. Diagnostics name every logical node and call out this
widening, as well as temporal precision/range and floating-point limitations.
SQL NULL and the JSON value `null` remain distinct.

`make validate` exercises the renderer against the declared local SQLite CLI,
including actual writes at integer boundaries, presence, oneofs, refinements,
malformed JSON and rejected overflows. `make sqlite-integration` exercises the
CLI through compiled bundle fixtures and persisted reopen. Flox pins 3.53.3,
the newest available catalog build on the qualification date; upstream 3.53.4
is newer. This fixture tool pin is not an application runtime recommendation or
a claim about any consuming medallion-table engine.

This is an additive build tool available to all language consumers. Existing
bundles, PostgreSQL/ClickHouse projections, deployed schemas and persisted data
are unchanged. Before a consumer adopts the output, it must review these
representations, keys/indexes and its own Atlas migration and rollback. Merely
removing this unused projection requires no data rollback.
