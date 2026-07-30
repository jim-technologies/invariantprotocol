# Protobuf-derived data schemas

Invariant treats protobuf as the only authored **logical** type contract. A
`FileDescriptorSet` is compiled into a versioned `invariant.data.v1.SchemaBundle`,
and target renderers derive Arrow, Parquet, Iceberg, PostgreSQL, or ClickHouse
schemas from that bundle. Lance and LanceDB use the Arrow projection directly;
there is no separate Lance logical schema.

```text
.proto + portable data annotations + source comments
          |
          v
descriptor.binpb
          |
          v
versioned SchemaBundle (committed evolution state, never hand-edited)
     /          |          |          |             \
  Arrow      Parquet    Iceberg    PostgreSQL    ClickHouse
    |                                  |          declarations
    v                                  v
Lance SDK                        Atlas diff/migrate
```

This is deliberately narrower than claiming that protobuf is a physical
storage format. Protobuf defines value domains, presence, names, numeric field
identity, nesting, lists, maps, enums, and comments. It does not define table
partitioning, clustering, indexes, foreign keys, retention, catalog commits,
or migration safety. Those remain deployment policy.

## Why the generated bundle exists

Arrow and Parquet describe fields structurally, while Iceberg attaches a
globally unique integer identity to every struct field, list element, map key,
and map value. Those identities must not be reused after a field is removed.
Protobuf field numbers give us stable source identity, but nested collection
children need additional identities and history.

The generated bundle is that history, unlike the reproducible descriptor image,
which contains only the current compiled protobuf graph. On the first compile,
top-level protobuf field numbers are retained where possible and
nested/container identities are allocated deterministically. Later compiles use
the previous bundle to:

- retain identities and storage names across same-number protobuf field renames
  when the old protobuf name remains reserved;
- allocate new identities monotonically;
- tombstone every removed identity permanently;
- reject reuse of a retired protobuf numeric path; and
- keep list elements and map keys/values globally unique.

Evolution validation also rejects changes to presence, declared defaults,
oneof membership, or a portable logical refinement for an existing numeric
path. Enum evolution is additive:
existing name/number pairs must remain present. To rename an enum value, retain
the old name as an alias instead of removing or renumbering it.

Commit the bundle and review its diff, but do not edit it. Developers continue
to author protobuf only. A same-number field rename must reserve the old
protobuf name; removing a field must reserve its number and name. These
reservations must remain present while the containing message remains directly
reachable. Removing an ancestor field requires reservations for that ancestor,
not for every descendant that becomes unreachable with it. These requirements
keep storage history in the authored protobuf contract.

Always render each target from the bundle. Do not chain serialized target
artifacts (an emitted Arrow IPC file into Parquet, for example), because target
formats do not necessarily round-trip every piece of canonical source
metadata. The Parquet renderer's direct bundle mapping followed by the
official in-process Arrow-to-Parquet schema bridge is deliberate; it does not
use the emitted Arrow IPC artifact as evolution state.

Arrow, Parquet, Iceberg, and ClickHouse artifacts each describe one dataset, so
their commands require `--message` when a bundle contains multiple roots.
PostgreSQL is a database desired state: `postgres` without `--message` emits
every dataset table in deterministic source-message order. Its optional
`--message` is the controlled single-table override.

## Dataset roots

Not every protobuf message is a durable row. RPC requests, responses, helper
messages, and recursive trees should not silently become tables. Compilation
therefore discovers only messages explicitly marked as dataset roots:

```proto
import "invariant/data/v1/annotations.proto";

message LedgerEvent {
  option (invariant.data.v1.dataset) = {};

  optional string event_id = 1 [
    (invariant.data.v1.field) = { uuid: {} }
  ];
  optional string amount = 2 [(invariant.data.v1.field) = {
    decimal: {
      precision: 18
      scale: 4
    }
  }];
  optional bytes checksum = 3 [(invariant.data.v1.field) = {
    fixed_bytes: { byte_length: 32 }
  }];
  repeated float embedding = 4 [(invariant.data.v1.field) = {
    fixed_list: { length: 1536 }
  }];
}
```

Omit `--message` for normal compilation. Repeated
`--message fully.qualified.Name` flags remain an explicit selection mechanism
for one-off or controlled builds, and take precedence when supplied; they are
not a second schema language. Once a bundle has been committed, its root set is
append-only: new roots may be added, but omitting a previous root fails
compilation instead of silently discarding that dataset's identity and
tombstone history. A root-message rename cannot be inferred safely; retain the
old message/root while adding the new one, or start a distinct bundle.

Discovery covers the complete supplied `FileDescriptorSet`, including imported
files. An annotation is therefore graph-wide intent: an annotated dependency is
also selected. When combining independently governed descriptor graphs, pass
explicit `--message` roots if that is not the desired bundle boundary.

Dataset and field storage names begin as deterministic snake_case projections
of protobuf names. Subsequent compiles retain the committed names by numeric
identity when the old protobuf field name remains reserved, so a source field
rename does not silently rename a physical column. Generated bundle names are
compiler-owned rather than an undocumented override mechanism. Compilation
rejects an empty name or a collision after normalization, both across selected
datasets and in every reachable nested struct.

Invariant repository policy assigns `51974` from Protobuf's
organization-internal range to the two aggregate data options on their
respective `MessageOptions` and `FieldOptions` extension spaces. It is not
globally registered or guaranteed collision-free outside the supplied
descriptor graph. The controlled, Git-based distribution model makes that a
practical tradeoff, and one aggregate per scope avoids consuming a new number
for every future portable refinement. The compiler rejects a conflicting
declaration visible in the input `FileDescriptorSet`. Consumers merging
independently governed descriptor sets should treat the number as assigned to
Invariant.

Field options express only semantics that every supported target can carry
exactly or diagnose explicitly:

- `decimal`: `string` carrier, precision 1–38, scale no greater than precision;
- `uuid`: `string` carrier containing canonical UUID text; and
- `fixed_bytes`: `bytes` carrier with a non-zero width no greater than
  2,147,483,647; and
- `fixed_list`: non-map repeated `float` or `double` carrier with an exact
  element count from 1 through 2,147,483,647.

A refined singular field must have explicit or oneof presence and cannot have
a declared protobuf default. Otherwise protobuf's implicit empty string or
bytes value would become an invalid value in the refined domain. A repeated
semantic refinement is valid and applies per element. `fixed_list` instead
refines the collection itself and cannot be combined with a semantic element
refinement. It is rejected on singular fields, maps, and carriers other than
`float` or `double`.

A fixed list is not a convenient default vector. Every runtime value must
contain exactly the declared number of elements. Because protobuf does not
track presence for repeated fields, an omitted value and an explicit empty
value are both invalid when the declared length is nonzero; neither is padded
or converted into a zero vector. Conversion errors identify the canonical
dataset storage name and nested field path.

The annotation source is distributed from Git with the runtime packages. Pin
the same repository revision and expose its `proto/` directory as a local Buf
workspace/module dependency or `protoc -I` import root. No protobuf registry is
required.

## SchemaBundle versioning

IR version 4 adds fixed cardinality to the portable `ListType`; mapping version
3 defines its target behavior. It retains v3's exact protobuf source
provenance, active stable IDs, and permanent tombstones. Go, Python, Rust, and
TypeScript readers automatically migrate the exact historical
IR-v3/mapping-v2 pair in memory. Unknown fields, an impossible legacy
`fixed_length`, any other version pair, and all future versions fail closed.
The build-time CLI rewrites a supported bundle deterministically:

```bash
go run ./go/cmd/invariant-schema migrate \
  --bundle data.schema.binpb \
  --output data.schema.binpb
```

Regenerate or run that explicit migration with the matching compiler when the
contract version changes; never hand-edit the artifact.

Before removing explicit `--message` flags, annotate every existing root.
Explicit names replace annotation discovery when supplied; the two sets are not
unioned. Removing a committed root annotation then fails the append-only root
check. Adding, removing, or changing a field refinement on an active numeric
path is a logical-shape change and requires a new protobuf field number. This
includes changing a fixed-list dimension. Reserve the old field number and
name so its stable identities become tombstones, then add the new dimension
under a new number. An existing root can be renamed only by retaining the old
message/root while adding the new one, or by starting a distinct bundle.

## Canonical mapping rules

The bundle retains exact protobuf scalar spellings (`int64` versus `sint64`,
for example) even when a target uses the same logical value type for both.
Enums remain numeric with their full number/name/alias metadata, avoiding data
changes when a symbol is renamed. Open enums preserve unknown future numeric
values; closed enums retain their declared numeric domain and receive target
checks where the target can enforce it.

| Protobuf logical value | Arrow / Parquet | Iceberg | PostgreSQL | ClickHouse |
| --- | --- | --- | --- | --- |
| signed 32-bit | `int32` / `INT32` | `int` | `integer` | `Int32` |
| signed 64-bit | `int64` / `INT64` | `long` | `bigint` | `Int64` |
| `uint32`, `fixed32` | `uint32` / `UINT_32` | `long` | `bigint` | `UInt32` |
| `uint64`, `fixed64` | `uint64` / `UINT_64` | `decimal(20,0)` | `numeric(20,0)` | `UInt64` |
| `float`, `double` | native 32/64-bit float | `float`, `double` | `real`, `double precision` | `Float32`, `Float64` |
| `bool`, `string`, `bytes` | native equivalents | native equivalents | `boolean`, `text`, `bytea` | `Bool`, `String`, `String` |
| enum | `int32` plus enum metadata | `int` | `integer` | `Int32` |
| nested message | struct/group | struct | `jsonb` | named `Tuple` |
| repeated field | list | list | `jsonb` | `Array(T)` |
| fixed `float`/`double` list | `FixedSizeList<T>[N]` / physical `LIST` | optional unconstrained list, no default | `jsonb` plus exact top-level cardinality check | `Array(T)` plus exact length check |
| map | typed map | typed map | `jsonb` | `Map(K,V)` plus unique-key check |
| `Timestamp` | UTC nanoseconds | `timestamptz_ns` | `timestamptz` (microseconds) | `DateTime64(9,'UTC')` |
| `Duration` | duration/int64 nanoseconds | long nanoseconds | bigint nanoseconds | `Int64` nanoseconds |
| `Any`, `Struct`, `Value`, `ListValue` | JSON representation | string JSON | `jsonb` | ProtoJSON `String` |
| annotated decimal text | `decimal128` / `DECIMAL` | `decimal` | `numeric` | `Decimal(P,S)` |
| annotated canonical UUID text | `arrow.uuid` / `UUID` | `uuid` | `uuid` | `UUID` |
| annotated exact-width bytes | fixed-size binary / `FIXED_LEN_BYTE_ARRAY` | `fixed` | `bytea` plus length check | `FixedString(N)` |

Renderers return a mapping diagnostic for every logical node. Widening, range
reduction, precision reduction, representation changes, and unsupported
mappings are therefore explicit rather than silent. Arrow, Parquet, and
Iceberg nanosecond timestamps use an `int64` count, so they preserve precision
but cover less than protobuf Timestamp's year 0001–9999 range. The same range
limit applies when protobuf Duration becomes an `int64` nanosecond count.
PostgreSQL timestamps cover the protobuf range but cannot retain nanoseconds.
ClickHouse `DateTime64(9,'UTC')` retains nanoseconds but covers approximately
1677-09-21 through 2262-04-11, so its Timestamp diagnostic is range-reduced.
Iceberg schemas require format version 3 when they use nanosecond timestamps or
Invariant's non-null protobuf defaults.

The dynamic JSON family uses standard protobuf JSON rather than inventing a
second encoding. That is a deliberate range reduction from the protobuf wire
domain: each populated `Any` type URL must resolve to a known message
descriptor, and numbers stored in `Struct`, `Value`, or `ListValue` must be
finite because ProtoJSON cannot encode `NaN` or infinities in those positions.
Target diagnostics report that reduction, and Python value-conversion errors
identify the canonical field path and protobuf source field.

Diagnostics also cover constraints that a target type does not enforce. A
closed enum projected onto an unconstrained integer, or a oneof projected to
independent Arrow/Parquet/Iceberg fields, widens the target's valid state set.
Arrow preserves fixed-list shape exactly. Parquet is produced through Arrow's
official bridge, but its physical `LIST` cannot enforce length; Iceberg also
has no fixed-cardinality list and emits an optional unconstrained list with no
invented historical default. Both report `RANGE_WIDENED` and require
SchemaBundle-aware value validation. PostgreSQL reports widening because its
top-level JSONB length constraint cannot enforce the protobuf floating-point
element domain; a fixed list nested inside JSONB cannot receive an independent
length constraint. ClickHouse's `Array(T)` is lossless only together with its
generated exact-length check.
PostgreSQL enforces top-level oneofs with a check constraint, but its `text`
and `jsonb` types cannot represent U+0000 even though that code point is valid
inside a protobuf string. ClickHouse emits recursive checks for valid UTF-8
protobuf strings, closed enum numbers, unique protobuf map keys, required
presence, and oneof discriminators.

Presence is compiled from native descriptors rather than guessed from syntax:

- implicit proto3/Editions scalars are non-null and use protobuf defaults;
- explicit optional fields and singular messages are nullable;
- required proto2/Editions fields are non-null;
- oneof members are nullable and retain their oneof group;
- ordinary repeated/map containers are non-null with empty defaults;
- fixed lists are non-null but have no empty default; and
- list elements and map keys/values are non-null.

ClickHouse uses the following deterministic physical presence convention:

- explicit scalar-like fields, including Timestamp, Duration, decimal, UUID,
  fixed bytes, and ProtoJSON, use `Nullable(T)`;
- explicit composite fields use
  `Tuple(present Bool, value T)` because `Nullable(Array)` and `Nullable(Map)`
  are unsupported and `Nullable(Tuple)` is still disabled beta behavior;
- required fields use the same tuple wrapper plus a `CHECK present`, preventing
  ClickHouse from replacing an omitted required column with its type default;
- each real oneof adds `__invariant_oneof_<oneof>_case Int32`, where `0`
  means unset and a nonzero value is the selected protobuf field number; every
  member uses `Tuple(present Bool, value T)`, and checks require its presence
  bit to agree with the discriminator; and
- ordinary repeated and map containers remain non-null with empty defaults;
  fixed lists have no default and require the generated length check.

This convention distinguishes absence from empty/default values and avoids
session settings. The member presence bit is authoritative alongside the
discriminator, so a synthetic discriminator is never the only persisted
presence state. The `__invariant_` storage-name prefix is reserved for
renderer-generated columns and rejected on authored fields. All identifiers
are backtick quoted without normalizing the committed storage name.

Decimal values use one canonical fixed-scale spelling: optional `-`, an integer
of `0` or a non-zero digit followed by digits, and—when scale is non-zero—a
decimal point followed by exactly `scale` digits. Leading `+`, exponent form,
leading zeroes, whitespace, omitted or truncated fractional digits, and
negative zero are rejected. UUID values use lowercase hyphenated `8-4-4-4-12`
text. Fixed bytes must have exactly the annotated width, and fixed lists must
have exactly the declared number of elements. The Python PyArrow bridge
enforces these rules while converting records; other data writers must enforce
the same logical domain at their boundary.

The Iceberg renderer records both `initial-default` and `write-default` for
implicit scalar/enum fields and for empty ordinary repeated/map containers.
Those format-v3 defaults make a newly added required column read as the same
value protobuf assigns to data written before the field existed. A fixed list
has no valid non-empty protobuf default, so Iceberg widens it to an optional
list without defaults. Presence-bearing fields remain optional and default to
null. Proto2 or Editions `required` fields are rejected by the Iceberg
renderer: protobuf does not provide a safe historical value for a missing
required field, even when its descriptor names a getter default.

Go, Python, Rust, and TypeScript readers validate `ir_version` and
`mapping_version` before exposing a decoded bundle. A newer artifact therefore
fails clearly instead of being interpreted using stale mapping rules.

Python applications that work with data can map a validated dataset directly
to the native PyArrow schema used by Arrow and Parquet:

```python
from invariant import arrow_schema, arrow_table, find_dataset, parse_schema_bundle

bundle = parse_schema_bundle(open("data.schema.binpb", "rb").read())
dataset = find_dataset(bundle, "example.v1.Event")
if dataset is None:
    raise ValueError("missing example.v1.Event dataset")

schema, diagnostics = arrow_schema(dataset)
table, diagnostics = arrow_table(dataset, generated_messages)
```

PyArrow is an optional `data` dependency and is loaded only when an Arrow API is
called. The returned `pyarrow.Schema` can be passed directly to
`pyarrow.Table` and `pyarrow.parquet`; the diagnostics are the same explicit
per-node compatibility contract as the Go Arrow renderer. `arrow_table()`
converts generated messages with the matching protobuf full name into typed
Arrow arrays using that same schema. Presence-bearing absent values become
null, inactive oneof members become null, enums remain numeric, timestamps and
durations retain nanoseconds within Arrow's signed-64-bit range, and protobuf
map entries are sorted for deterministic Arrow ordering. A fixed list becomes
a native `pyarrow.FixedSizeListType` and `FixedSizeListArray`; conversion
rejects short, long, omitted, and empty values before PyArrow or a storage SDK
can coerce them. Ordinary `pyarrow.parquet.write_table()` can then write the
table; Invariant does not wrap or replace the standard writer.

One format edge remains explicit: Arrow tables and Arrow IPC preserve the row
count of a valid zero-field dataset, but locked PyArrow 25 writes it to Parquet
as a zero-column table that reopens with zero rows. Do not use Parquet for a
zero-field dataset when row cardinality matters. Invariant does not invent a
hidden physical column or wrap the Parquet writer to disguise that limitation.

The value bridge validates the generated descriptor before reading a row,
including the exact decimal, UUID, fixed-byte, and fixed-list field options.
Changing or removing an annotation while retaining the same protobuf carrier
is rejected as descriptor drift. Nested arrays are built explicitly rather
than delegated to PyArrow's generic row inference, so UUID and JSON extension
values compose inside lists, maps, and structs. Populated `Any` values resolve
through the generated message's own descriptor pool, including bindings built
from an isolated `descriptor.binpb`.

Recursive message graphs fail compilation. Silently turning a recursive type
into an untyped object would stop protobuf from being the canonical contract.
Reachable messages with proto2 extension ranges also fail: an extension can add
a field outside the message declaration, so omitting it would make the bundle
an incomplete representation of the protobuf contract.

## Lance and LanceDB through Arrow

LanceDB accepts Arrow tables and record batches, so the canonical integration
is the existing Arrow projection rather than a `lance` renderer or CLI
command:

```python
import lancedb
from invariant import arrow_table, find_dataset, parse_schema_bundle

bundle = parse_schema_bundle(open("data.schema.binpb", "rb").read())
dataset = find_dataset(bundle, "example.v1.Embedding")
if dataset is None:
    raise ValueError("missing example.v1.Embedding dataset")

table, diagnostics = arrow_table(dataset, generated_messages)
database = await lancedb.connect_async("./data/lance")
lance_table = await database.create_table("embeddings", data=table)
```

The table already contains a native Arrow `FixedSizeList`; application-side
`pa.array(..., type=...)` casting would create a second schema authority and is
unnecessary. The same table can be appended or used as the source of a
`merge_insert`.

Repository qualification locks LanceDB 0.36.0 and PyArrow 25.0.0 in
`python/uv.lock`. Its deterministic local lifecycle creates a table from only
Invariant-generated schema/data, appends, closes and reopens, creates an
HNSW-SQ vector index, searches, performs a standard `merge_insert`, optimizes,
reopens in a fresh Python process, and verifies the schema and both pre-index
and post-index rows. The same boundary round-trips representative non-default
primitive, enum, presence, oneof, nested, list, map, temporal, JSON, decimal,
UUID, and fixed-byte values; JSON is compared semantically because Lance may
normalize insignificant whitespace. It separately verifies that an unenforced
primary key can be set as table configuration without changing SchemaBundle.
Run it directly with:

```bash
flox activate -- make lance-integration
```

Lance manifests, fragments, files, index algorithms and parameters, primary
keys, object-store credentials, compaction policy, and manifest placement are
Lance SDK/table policy. Invariant neither writes the Lance format nor models
those settings. Lance Namespace REST likewise remains an SDK boundary; its
Arrow IPC request bodies can be produced from the same canonical Arrow table.

LanceDB 0.36.0 preserves the `FixedSizeList` element type and dimension,
top-level field nullability, and top-level field metadata after persistence.
It widens the synthetic Arrow value field from non-null to nullable and
normalizes away that child's custom metadata. The reopened physical schema can
therefore admit a null vector element if an application bypasses
`arrow_table()`. Treat this as a Lance-boundary range widening: the committed
SchemaBundle remains the identity and tombstone registry, and `arrow_table()`
remains the value-domain enforcement boundary. Never infer evolution state or
canonical collection-member nullability from a reopened Lance table schema.

The same release creates new tables with Lance data storage format 2.1 by
default, while Arrow `Map` requires format 2.2. A dataset containing protobuf
maps is still generated canonically, but the application must opt the table
into 2.2 as Lance storage policy:

```python
database = await lancedb.connect_async(
    "./data/lance",
    storage_options={"new_table_data_storage_version": "2.2"},
)
```

The integration suite qualifies the complete canonical Arrow fixture under
that policy. Invariant does not silently choose a Lance file-format generation
on the application's behalf.

Protobuf and Arrow floats include NaN, but LanceDB's default vector policy
rejects NaN values. Keep the fail-closed `on_bad_vectors="error"` behavior when
protobuf fidelity matters; LanceDB's `drop`, `fill`, and `null` modes alter data
and must be an explicit application decision.

The locked LanceDB 0.36.0 Python API documents MemWAL spec, inspection, and
writer-drain methods, but its documented `LsmWriteSpec` constructor still lives
in the private `_lancedb` extension module. Private extension-module symbols
are not a production contract, so the qualification deliberately does not
import them or claim MemWAL support. MemWAL remains table policy and can be
qualified later without changing SchemaBundle when LanceDB exposes the complete
lifecycle through a stable public module.

## PostgreSQL and Atlas

Invariant emits complete, semicolon-terminated PostgreSQL DDL. Atlas reads that
`file://` SQL desired state directly and uses a PostgreSQL development database
to interpret and diff it. Atlas then owns inspection, migration generation, and
application. HCL is not an intermediate format and is not another source of
truth.

Repository integration runs `scripts/check_postgres_atlas.sh`: it renders the
committed multi-dataset and annotated bundles into one desired state, applies
the result to disposable PostgreSQL 18.4, inspects the live schema, verifies
defaults, nullability, empty list/map defaults, comments, and constraints in
the PostgreSQL catalog, and requires Atlas to report a zero diff. This verifies
the boundary without making production DDL application part of Invariant.

SQL has no portable field-identity mechanism, so the compiler retains the
committed storage name when a protobuf field is renamed under the same numeric
identity and the old name remains reserved. Treat the output as desired state
and review Atlas's diff. Renames inside values represented as `jsonb` remain
application-level data-evolution concerns.

The SQL renderer quotes identifiers, preserves comments, stores complex values
as `jsonb`, adds an at-most-one check for each top-level oneof, and checks
annotated fixed-byte lengths. A top-level fixed list has no empty default and
adds a `jsonb_typeof(...) = 'array'` plus exact
`jsonb_array_length(...) = N` check; PostgreSQL cannot independently enforce a
fixed list embedded in another JSONB value. Native decimal, UUID, and checked
`bytea` mappings apply to top-level singular columns; refinements inside
repeated or nested containers remain part of their `jsonb` representation. It
does not invent primary keys, foreign keys, uniqueness, indexes,
normalization, or partitioning.

## ClickHouse hot schema

`invariant-schema clickhouse` emits a parenthesized table-body fragment for one
dataset:

```bash
go run ./go/cmd/invariant-schema clickhouse \
  --bundle data.schema.binpb \
  --message example.v1.Event \
  --output event.clickhouse.sql
```

The same bundle can emit the versioned, language-neutral hot-to-cold
conversion plan:

```bash
go run ./go/cmd/invariant-schema clickhouse-iceberg \
  --bundle data.schema.binpb \
  --message example.v1.Event \
  --output event.clickhouse-iceberg.json
```

The fragment contains column declarations, source comments on top-level
columns, protobuf-compatible defaults, and the required logical constraints.
It intentionally contains no `CREATE TABLE`, database name, engine,
`ORDER BY`, `PARTITION BY`, TTL, storage policy, index, projection, or codec.
The application combines the fragment with its reviewed physical policy.
`TableSchema.Description` retains the dataset comment; a table-level
ClickHouse `COMMENT` follows the engine clause and therefore remains part of
that application-owned wrapper.

Generated `CHECK` constraints validate inserted data. ClickHouse mutations can
bypass or invalidate checks depending on the mutation path, so applications
that allow `ALTER ... UPDATE` must validate those writes at their boundary as
well; the declarations are schema guards, not an authorization boundary.
The artifact is desired column state, not an `ALTER TABLE` plan. Applications
must review and execute in-place evolution and historical-row backfills,
including changes to a oneof's allowed case set. Per-member presence prevents
an old discriminator value from being reinterpreted as a new member.

Named tuples retain finite nested messages. Empty messages use one constrained
synthetic unit element because ClickHouse has no empty Tuple. `Map(K,V)` keeps
the protobuf key/value types and adds a recursive uniqueness check because
ClickHouse otherwise permits duplicate keys. Closed enums add an allowed-number
check; open enums remain unconstrained `Int32`, preserving unknown numbers.
ClickHouse reserves `null` as a Tuple element name, so that otherwise
non-canonical storage name is rejected with an unsupported diagnostic. Tuple
syntax has no per-element comments: nested comments remain canonical in
SchemaBundle, while ClickHouse `COMMENT` clauses are emitted for top-level
columns.
`FixedString(N)` is emitted only for widths 1–256 so the schema needs no
`allow_suspicious_fixed_string_types` setting. Every publisher must still
validate exact width before insertion because ClickHouse pads shorter
`FixedString` input with NUL bytes. A fixed list maps to `Array(Float32)` or
`Array(Float64)`, omits the ordinary empty-array default, and adds an exact
`length(...) = N` constraint.

The Go API is:

```go
schema, diagnostics, err := clickhouse.Schema(dataset)
fragment := schema.ColumnDeclarations()

projection, projectionDiagnostics, err := clickhouse.ProjectToIceberg(dataset)
encoded, err := clickhouse.ProjectionJSON(projection)
```

`ProjectToIceberg` is a structural publishing plan over the existing
`iceberg.Schema(dataset)` representation. Each node carries `{value}` and,
for oneofs, `{case}` expression templates plus a separate presence predicate;
publishers can therefore emit null for absent optional composite values without
enabling beta `Nullable(Tuple)` behavior. Unsigned 64-bit leaves use exactly
`accurateCast({value}, 'Decimal(20, 0)')`: the maximum UInt64 value
18,446,744,073,709,551,615 is below the Decimal(20,0) maximum and is converted
without a signed or floating-point intermediate. `uint32`/`fixed32` use
`toInt64`, and timestamps use `toUnixTimestamp64Nano`.
Its diagnostics include both the ClickHouse source mapping and Iceberg target
mapping, so range or representation differences on either side remain visible.
A fixed-list projection records `fixed_length` while its Iceberg target remains
the explicitly widened unconstrained list; publishers validate the length
before evaluating the conversion expressions.

This plan is not a writer, buffer, watermark manager, direct ClickHouse-to-
Iceberg `INSERT`, or catalog integration. The application evaluates the plan
while publishing rows and owns retries, transactions, table format/version,
and catalog commits. A protobuf `required` field remains unsupported by the
Iceberg projection because historical Iceberg rows have no safe missing value,
even though the native ClickHouse schema can enforce it.

The ordinary Go test skips the live boundary unless
`INVARIANT_CLICKHOUSE_URL` is set. The repository command starts an exact
pinned ClickHouse image and runs the guarded DDL/value round trip:

```bash
flox activate -- make clickhouse-integration
```

That test wraps the generated declarations in a test-owned `MergeTree`,
round-trips absent/present/default values plus decimal, UUID, temporal, enum,
and every ProtoJSON shape; validates oneof and required constraints; rejects
duplicate map keys, invalid UTF-8, and invalid JSON; and executes the exact
UInt64, UInt32, and timestamp Iceberg conversion expressions.

## Deliberate schema boundary

This release compiles and renders **schemas**, and Python can convert matching
generated protobuf messages into a `pyarrow.Table` accepted directly by
LanceDB. Invariant does not choose file layout, write Parquet or Lance data
files, commit an Iceberg catalog, apply database DDL, generate database
migrations, or choose partitions. Those operations combine schema with runtime
and deployment policy and remain consumer-owned.

The Iceberg output is a schema snapshot, not an evolution transaction. Its
`initial-default` and `write-default` values preserve protobuf's implicit
defaults, but a catalog integration must compare the committed table state,
apply additions through Iceberg's evolution API, require format version 3 where
needed, and commit the catalog transaction itself.

The format references behind these decisions are the
[Arrow columnar format](https://arrow.apache.org/docs/format/Columnar.html),
[Parquet logical types](https://parquet.apache.org/docs/file-format/types/logicaltypes/),
[Iceberg schema evolution rules](https://iceberg.apache.org/docs/latest/evolution/#schema-evolution),
[protobuf field presence](https://protobuf.dev/programming-guides/field_presence/),
[Atlas SQL schema sources](https://atlasgo.io/atlas-schema/sql), and
[ClickHouse data types](https://clickhouse.com/docs/reference/data-types).
Lance integration follows the
[LanceDB Arrow table API](https://docs.lancedb.com/tables/create)
and leaves file-format behavior to the
[Lance SDK](https://lance.org/).
