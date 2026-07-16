# Protobuf-derived data schemas

Invariant treats protobuf as the only authored **logical** type contract. A
`FileDescriptorSet` is compiled into a versioned `invariant.data.v1.SchemaBundle`,
and target renderers derive Arrow, Parquet, Iceberg, or PostgreSQL schemas from
that bundle.

```text
.proto + source comments
          |
          v
descriptor.binpb
          |
          v
versioned SchemaBundle (generated, never hand-edited)
     /          |          |          \
  Arrow      Parquet    Iceberg    PostgreSQL DDL
                                      |
                                      v
                                Atlas diff/migrate
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

The generated bundle is that history. On the first compile, top-level protobuf
field numbers are retained where possible and nested/container identities are
allocated deterministically. Later compiles use the previous bundle to:

- retain identities across protobuf field renames;
- allocate new identities monotonically;
- tombstone every removed identity permanently;
- reject reuse of a retired protobuf numeric path; and
- keep list elements and map keys/values globally unique.

Evolution validation also rejects changes to presence, declared defaults, or
oneof membership for an existing numeric path. Enum evolution is additive:
existing name/number pairs must remain present. To rename an enum value, retain
the old name as an alias instead of removing or renumbering it.

Commit the bundle and review its diff, but do not edit it. Developers continue
to author protobuf only. Reserving removed protobuf field numbers and names is
still required, just as it is for ordinary wire compatibility.

Always render each target from the bundle. Do not chain serialized target
artifacts (an emitted Arrow IPC file into Parquet, for example), because target
formats do not necessarily round-trip every piece of canonical source
metadata. The Parquet renderer's direct bundle mapping followed by the
official in-process Arrow-to-Parquet schema bridge is deliberate; it does not
use the emitted Arrow IPC artifact as evolution state.

## Dataset roots

Not every protobuf message is a durable row. RPC requests, responses, helper
messages, and recursive trees should not silently become tables. Compilation
therefore requires explicit fully-qualified root message names. This selection
is build configuration, not another type definition. Once a bundle has been
committed, its root set is append-only: new roots may be added, but omitting a
previous root fails compilation instead of silently discarding that dataset's
identity and tombstone history.

Dataset and field storage names are deterministic snake_case projections of
protobuf names. Compilation rejects an empty projected name or a collision
after that lossy normalization, both across selected datasets and in every
reachable nested struct, rather than emitting a bundle that target renderers
cannot interpret consistently.

Custom protobuf dataset/column options are intentionally not published yet.
Public custom options are extensions, and Protobuf's guidance is to obtain a
globally registered extension number for a public declaration. Invariant will
not ship a temporary ad-hoc number that could collide in a downstream
descriptor set. Inferable
logical schemas work without annotations; physical policies remain downstream
until a registered annotation surface is justified.

## Canonical mapping rules

The bundle retains exact protobuf scalar spellings (`int64` versus `sint64`,
for example) even when a target uses the same logical value type for both.
Enums remain numeric with their full number/name/alias metadata, preserving
unknown future values and avoiding data changes when a symbol is renamed.

| Protobuf logical value | Arrow / Parquet | Iceberg | PostgreSQL |
| --- | --- | --- | --- |
| signed 32-bit | `int32` / `INT32` | `int` | `integer` |
| signed 64-bit | `int64` / `INT64` | `long` | `bigint` |
| `uint32`, `fixed32` | `uint32` / `UINT_32` | `long` | `bigint` |
| `uint64`, `fixed64` | `uint64` / `UINT_64` | `decimal(20,0)` | `numeric(20,0)` |
| `float`, `double` | native 32/64-bit float | `float`, `double` | `real`, `double precision` |
| `bool`, `string`, `bytes` | native equivalents | native equivalents | `boolean`, `text`, `bytea` |
| enum | `int32` plus enum metadata | `int` | `integer` |
| nested message | struct/group | struct | `jsonb` in v1 |
| repeated field | list | list | `jsonb` in v1 |
| map | typed map | typed map | `jsonb` in v1 |
| `Timestamp` | UTC nanoseconds | `timestamptz_ns` | `timestamptz` (microseconds) |
| `Duration` | duration/int64 nanoseconds | long nanoseconds | bigint nanoseconds |
| `Any`, `Struct`, `Value`, `ListValue` | JSON representation | string JSON | `jsonb` |

Renderers return a mapping diagnostic for every logical node. Widening, range
reduction, precision reduction, representation changes, and unsupported
mappings are therefore explicit rather than silent. Arrow, Parquet, and
Iceberg nanosecond timestamps use an `int64` count, so they preserve precision
but cover less than protobuf Timestamp's year 0001–9999 range. The same range
limit applies when protobuf Duration becomes an `int64` nanosecond count.
PostgreSQL timestamps cover the protobuf range but cannot retain nanoseconds.
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
PostgreSQL enforces top-level oneofs with a check constraint, but its `text`
and `jsonb` types cannot represent U+0000 even though that code point is valid
inside a protobuf string.

Presence is compiled from native descriptors rather than guessed from syntax:

- implicit proto3/Editions scalars are non-null and use protobuf defaults;
- explicit optional fields and singular messages are nullable;
- required proto2/Editions fields are non-null;
- oneof members are nullable and retain their oneof group;
- repeated/map containers are non-null with empty defaults; and
- list elements and map keys/values are non-null.

The Iceberg renderer records both `initial-default` and `write-default` for
implicit scalar/enum fields and for empty repeated/map containers. Those
format-v3 defaults make a newly added required column read as the same value
protobuf assigns to data written before the field existed. Presence-bearing
fields remain optional and default to null. Proto2 or Editions `required`
fields are rejected by the Iceberg renderer: protobuf does not provide a safe
historical value for a missing required field, even when its descriptor names a
getter default.

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
map entries are sorted for deterministic Arrow ordering. Ordinary
`pyarrow.parquet.write_table()` can then write the table; Invariant does not
wrap or replace the standard writer.

Recursive message graphs fail compilation. Silently turning a recursive type
into an untyped object would stop protobuf from being the canonical contract.
Reachable messages with proto2 extension ranges also fail: an extension can add
a field outside the message declaration, so omitting it would make the bundle
an incomplete representation of the protobuf contract.

## PostgreSQL and Atlas

Invariant emits complete, semicolon-terminated PostgreSQL DDL. Atlas can read
that SQL directly through its external-schema data source, then own inspection,
diffing, migration generation, and application. HCL is not an intermediate
format and is not another source of truth.

SQL has no portable field-identity mechanism. Although the bundle retains a
protobuf field rename by numeric identity, a generated SQL identifier change
still looks like a drop/add to Atlas. Treat the output as desired state and
review or annotate the downstream migration when preserving an existing column
through a rename. The same caveat applies to renamed fields embedded inside a
`jsonb` value.

The SQL renderer quotes identifiers, preserves comments, stores complex values
as `jsonb` in v1, and adds an at-most-one check for each top-level oneof. It does
not invent primary keys, foreign keys, uniqueness, indexes, normalization, or
partitioning.

## Deliberate v1 boundary

This release compiles and renders **schemas**, and Python can convert matching
generated protobuf messages into a `pyarrow.Table`. Invariant does not choose
file layout, write data files, commit an Iceberg catalog, apply database DDL,
generate migrations, or choose partitions. Those operations combine schema
with runtime and deployment policy and remain consumer-owned.

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
and [Atlas external schemas](https://atlasgo.io/atlas-schema/projects).
