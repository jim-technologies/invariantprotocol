# Canonical change data capture

Invariant's CDC contract describes a captured change; it does not prescribe how
the change is captured or delivered. The v1 full-image and Debezium profile
uses an
[`io.cloudevents.v1.CloudEvent`](../proto/io/cloudevents/v1/cloudevents.proto)
whose `proto_data` contains an
[`invariant.cdc.v1.ChangeRecord`](../proto/invariant/cdc/v1/change.proto) packed
as `google.protobuf.Any`:

```text
io.cloudevents.v1.CloudEvent
  required context + optional relationship context
  `- proto_data: google.protobuf.Any
       `- invariant.cdc.v1.ChangeRecord
            operation + key/images + collection + schema + position + time
```

The [v2 dual-representation profile](#cdc-v2-full-and-delta-representations)
uses the same CloudEvents envelope and exact value model while making
`FullChange` and `DeltaChange` explicit alternatives. Its distinct protobuf
package, Any URL, `dataschema`, and CloudEvents type allow v1 consumers to
remain correct and unchanged.

The envelope is the wire-compatible protobuf representation from the latest
stable [CloudEvents v1.0.2 release][ce-release], including its
[protobuf event format][ce-protobuf-format]. The Debezium compatibility profile
and golden fixtures are pinned to the exact stable release
[`3.6.1.Final`][debezium-release]. Neither specification makes a transport,
broker, database, or connector part of the Invariant contract.

Invariant vendors the upstream `cloudevents.proto` so its descriptor image and
four-language generation remain reproducible from one Git revision. The public
protobuf identity `io.cloudevents.v1.CloudEvent`, nested message identities,
and every field number match upstream exactly. A language-specific
`go_package` is distribution metadata, not a second wire contract or a fork of
CloudEvents semantics. A process should load one generated binding for this
protobuf identity: consumers may use an already compatible CloudEvents binding
or Invariant's generated binding, but should not register both copies in one
global protobuf registry.

Normative words such as **must**, **must not**, **should**, and **may** in this
document define the Invariant CDC profile.

## CloudEvents envelope

The [CloudEvents core specification][ce-core] requires `id`, `source`,
`specversion`, and `type`. This profile additionally requires `time`,
`datacontenttype`, and `dataschema` on every canonical CDC event.

| CloudEvents attribute | Protobuf location and canonical value | Meaning |
| --- | --- | --- |
| `id` | `CloudEvent.id`; non-empty | Stable identity of this occurrence within `source` |
| `source` | `CloudEvent.source`; non-empty URI-reference | Stable scope in which the source occurrence was observed |
| `specversion` | `CloudEvent.spec_version = "1.0"` | CloudEvents 1.0; patch releases do not change this serialized value |
| `type` | `CloudEvent.type = "io.invariantprotocol.cdc.v1.change"` | Versioned, reverse-DNS identity of the `ChangeRecord` semantics |
| `time` | `attributes["time"].ce_timestamp` | `source_time` when known, otherwise `capture_time` |
| `datacontenttype` | `attributes["datacontenttype"].ce_string = "application/protobuf"` | The event data is a protobuf message |
| `dataschema` | `attributes["dataschema"].ce_uri = "type.googleapis.com/invariant.cdc.v1.ChangeRecord"` | Absolute schema URI and Any type URL |
| event data | `proto_data` | An Any whose `type_url` is exactly the `dataschema` URI and whose value is the serialized `ChangeRecord` |

`time`, `datacontenttype`, and `dataschema` live in the protobuf envelope's
typed `attributes` map because that is where the upstream CloudEvents protobuf
schema represents optional context attributes. The
[protobuf format requires protobuf event data in `proto_data`][ce-protobuf-data]
and recommends its type URL as `dataschema`.

When a transport needs a media type for the complete serialized envelope, the
CloudEvents protobuf format uses `application/cloudevents+protobuf`. That outer
format is distinct from the payload's required
`datacontenttype = application/protobuf`.

CloudEvents v1.0.2 also defines `CloudEventBatch` with the outer media type
`application/cloudevents-batch+protobuf`. Each member remains an independent
CloudEvent: the batch adds no ordering, transaction, or deduplication
semantics. The shared v2 batch fixture is only a deterministic conformance-file
container; its manifest and declared source stream define the test sequence.

`source` identifies the logical occurrence scope, not a process attempt or a
delivery endpoint. Producers must ensure that `source + id` is unique for each
distinct occurrence. A retry of the same occurrence must retain both values;
it must not generate a fresh random ID. An adapter can use a source-native event
identifier or deterministically derive an ID from an immutable source position
and the disambiguating collection/operation identity. When a source cannot
supply enough stable material, the producing service must durably assign the
ID before its first delivery.

The `v1` component versions the event's semantics. Additive, wire-compatible
protobuf evolution remains within this event type. An incompatible payload
must use a new protobuf package, Any type URL, `dataschema`, and CloudEvents
`type` version.

### Relationship and trace extensions

The following optional CloudEvents extensions use `ce_string` values:

| Extension | Meaning |
| --- | --- |
| `correlationid` | Application-defined identity joining related work |
| `causationid` | Identity of the event or command that caused this event |
| `traceparent` | W3C Trace Context `traceparent` value |
| `tracestate` | W3C Trace Context `tracestate` value |

They are context only. Producers must not place keys, before/after images,
source messages, complete records, source offsets, or connector metadata in
CloudEvent attributes. Trace fields follow [W3C Trace Context][trace-context].
Unknown CloudEvents extensions are forwarded when the surrounding transport
can preserve them, but they never override `ChangeRecord` semantics.

## `ChangeRecord` payload

`ChangeRecord` separates portable meaning from source-specific details.

| Field | Contract |
| --- | --- |
| `operation` | Required canonical operation; producers must not emit `OPERATION_UNSPECIFIED` |
| `key` | Canonical row key when the source exposes one; absent for keyless and collection-wide events |
| `before` | Source-provided image before the operation; optional |
| `after` | Image after the operation; required by create, update, and snapshot-read |
| `data_collection` | Stable source-scoped collection identity; required except for a source message with no meaningful collection |
| `schema_reference` | External schema URI, version, and optional exact fingerprint when known |
| `source_position` | Opaque checkpoint bytes plus their format and optional ordering stream |
| `transaction` | Optional transaction identity and one-based total and per-collection ordering |
| `source_time` | Time the source committed or observed the occurrence, when known |
| `capture_time` | Time the capture process observed or emitted the record |
| `changed_fields` | Optional exact source-name paths known to have changed |
| `source_extension` | Typed external protobuf or opaque encoded source metadata |
| `source_message` | Non-row content, used only for `OPERATION_SOURCE_MESSAGE` |

`capture_time` is required for produced events. `source_time` is required when
the source reports an occurrence or commit time. If it does not, `source_time`
is absent and the envelope `time` uses `capture_time`; every producer for the
same CloudEvents `source` must apply that fallback consistently.

`SourcePosition.value` is deliberately opaque. Generic consumers may retain it
or compare it for equality, but they must not parse, numerically compare, or
order it. `SourcePosition.stream` declares the narrow stream within which the
producer can promise ordering; an empty stream declares no ordering scope.

`ChangedFieldMask` is an optimization, not a substitute for `after`. An absent
mask means that changed fields are unknown. A present mask with no paths means
the producer knows that no record field changed. Each `FieldPath` is a sequence
of exact source field names, so punctuation and nested names do not depend on
protobuf `FieldMask` syntax. Full `after` images are preferred.

### Operation and presence rules

| Canonical operation | Debezium `op` | Key | `before` | `after` | Collection | `source_message` |
| --- | --- | --- | --- | --- | --- | --- |
| `OPERATION_CREATE` | `c` | When available | Normally absent; source-defined if supplied | Required | Required | Prohibited |
| `OPERATION_UPDATE` | `u` | When available | Optional | Required | Required | Prohibited |
| `OPERATION_DELETE` | `d` | Required when the source has a key | Optional | Prohibited | Required | Prohibited |
| `OPERATION_SNAPSHOT_READ` | `r` | When available | Normally absent; source-defined if supplied | Required | Required | Prohibited |
| `OPERATION_TRUNCATE` | `t` | Prohibited | Prohibited | Prohibited | Required | Prohibited |
| `OPERATION_SOURCE_MESSAGE` | `m` | Prohibited | Prohibited | Prohibited | Optional when none is meaningful | Required |

`TRUNCATE` is one collection-wide mutation, not a synthetic set of deletes.
`SOURCE_MESSAGE` states only that a source-native message was captured. Its
`source_message` can be a structured `Value` containing, for example, an exact
prefix and byte content; it must never be projected into row images. Connector
support for `t` and `m` is optional. In Debezium 3.6.1.Final they are notably
documented by the [PostgreSQL connector][debezium-postgres-events], while the
general [Debezium CloudEvents converter documentation][debezium-cloudevents]
lists only `c`, `u`, `d`, and `r`. The compatibility reader accepts `t` and `m`
when a supporting connector supplies them; this contract does not claim that
every connector or converter emits them.

### Presence and exact values

There are three different states and adapters must not collapse them:

1. Omitting a `RecordField` means the source field is absent or unavailable.
2. Including a `RecordField` whose `Value.kind` is `null_value` means the field
   is present and explicitly null.
3. Including a `RecordField` with another `Value.kind` means it is present with
   that exact value.

Likewise, an absent `before` or `after` image differs from a present empty
`Record`. A Debezium envelope-level `"before": null` or `"after": null` maps
to an absent image; a null property inside a present image maps to
`NullValue`. A present `RecordField` with an unset `Value.kind` is invalid.

`Value` avoids an untyped JSON-number round trip:

| Source domain | Canonical representation |
| --- | --- |
| Boolean | `bool_value` |
| Signed integer | Narrowest exact `int32_value` or `int64_value` consistent with the source schema |
| Unsigned integer | `uint32_value` or `uint64_value` |
| IEEE-754 value | `float32_value` or `float64_value` without decimal-text coercion |
| Text | `string_value` |
| Binary | `bytes_value` |
| Base-10 decimal | `decimal_value` with canonical text, scale, and declared precision |
| Instant | `timestamp_value` at nanosecond precision within the protobuf Timestamp range |
| Struct or nested record | `record_value` |
| Array | ordered `list_value`, including explicit null elements |
| Map | ordered `map_value`, including non-string keys and explicit null values |

`type_name` retains an external logical type name or URI. This is important for
Debezium logical decimals and temporal types whose physical Kafka Connect
carrier alone does not express their complete semantics. Non-instant temporal
values retain their exact integer or string carrier plus `type_name`; they are
not incorrectly assigned a timezone. Field names and collection identifiers
are retained exactly rather than normalized. When a connector uses an exact
decimal carrier for an unsigned source value, propagated original-column
metadata selects `uint32_value` or `uint64_value`; a real decimal instead
selects `decimal_value`. Without that source type, an adapter retains the
observable decimal rather than guessing unsignedness. Adapters decode Kafka
Connect's base64 byte representation and unscaled decimal integer directly,
never through binary64.

## Debezium 3.6.1.Final compatibility profile

This profile maps the Debezium data-change envelope described by the
[3.6 connector documentation][debezium-postgres-events] without making a
Debezium runtime or any of its delivery choices a dependency. It covers native
Kafka Connect records and Debezium's structured CloudEvents JSON. It targets
semantic equivalence: operation, presence, values, schema identity, source
position, timestamps, transaction ordering, and uninterpreted source metadata
survive both directions.

### Accepted native envelope shapes

An adapter consumes the record key and record value as separate inputs:

- With Kafka Connect schemas enabled, both have the outer
  `{"schema": ..., "payload": ...}` shape. The adapter maps `payload` and uses
  `schema` to recover exact primitive widths, logical types, optionality, and
  schema identity.
- With schemas disabled, the key and value are their payload objects directly.
  The adapter uses separately supplied source-schema knowledge when exact
  logical types are required.

Schemaful and schemaless shapes are serialization choices, not different CDC
semantics. Schemaless JSON cannot reveal information that its producer already
discarded: for example, the same JSON number might have originated from
different integer widths or a binary decimal. A lossless adapter therefore
must receive the applicable schema or other source-type knowledge when those
distinctions matter. Without it, the adapter preserves the observable JSON
semantics and the available encoding/schema/source metadata in
`source_extension`, but it cannot claim to reconstruct logical type information
that was absent upstream. It must not guess a type through a floating-point
JSON conversion. The official Debezium documentation likewise distinguishes
[JSON records with and without schemas][debezium-serdes].

### Mapping matrix

| Debezium input | `ChangeRecord` | Reverse mapping rule |
| --- | --- | --- |
| Record key `payload`, or direct schemaless key | `key` for row operations | Emit the original key shape and types; no key remains no key. For `m`, preserve the duplicated logical-message prefix in `source_message` instead, never as a row key. |
| `payload.op` | `operation` | `c/u/d/r/t/m` map one-to-one to the operation table above |
| `payload.before` | `before` | Envelope null becomes absent; a present object preserves every present field and explicit field null |
| `payload.after` | `after` | Same presence rule as `before`; operation validation is applied |
| `payload.message` and the prefix-only record key for `m` | `source_message` | Preserve prefix/content and their exact text/bytes; never create a row image or row key. The value's prefix is authoritative and reconstructs both the native `message.prefix` and duplicated native key; mismatched inputs fail. |
| Source collection fields | `data_collection.id` | Compose a stable source-scoped identity using the connector's exact identifiers; do not require a particular database/schema/table vocabulary |
| Key/value Kafka Connect schema or registry identity | `schema_reference` | Preserve its URI/name, version, and fingerprint when available; retain full external schema metadata in `source_extension` when needed for reverse conversion |
| Connector-native offset fields in `payload.source` | `source_position.value` and `format` | Encode the complete checkpoint in its declared external encoding; never promote an LSN, file position, or similar field into a core field |
| Declared source partition or ordered channel | `source_position.stream` | Populate only when the producer can state the ordering scope; it is not globally ordered |
| `payload.transaction.id` | `transaction.id` | Preserve as source-defined text |
| `payload.transaction.total_order` | `transaction.total_order` | Parse and emit as an exact one-based unsigned integer |
| `payload.transaction.data_collection_order` | `transaction.data_collection_order` | Parse and emit as an exact one-based unsigned integer |
| Highest-resolution `payload.source.ts_ns`, `ts_us`, or `ts_ms` | `source_time` | Prefer nanoseconds, then microseconds, then milliseconds; never fabricate missing precision |
| Highest-resolution envelope `payload.ts_ns`, `ts_us`, or `ts_ms` | `capture_time` | Prefer nanoseconds, then microseconds, then milliseconds; preserve which source fields existed in the extension when exact reverse shape matters |
| Changed-field metadata supplied by a source transform | `changed_fields` | Preserve exact field-name paths; otherwise leave absent rather than diffing partial images |
| Complete `payload.source`, unconsumed envelope fields, source schema details, and unknown connector metadata | `source_extension` | Preserve as a typed external protobuf when a descriptor exists, otherwise as opaque bytes with media type and optional schema URI |

Kafka topics, partitions, record offsets, connector names, transaction IDs,
LSNs, binlog coordinates, and database-specific collection components are all
optional profile inputs. None is required by the core protobuf. A connector
adapter can use any of them inside the opaque position or isolated source
extension without assigning them universal meaning.

The source extension is a preservation boundary, not an alternate record.
It must not duplicate complete keys or before/after images and cannot override
`operation`, image presence, or canonical values. When duplicated source facts
are projected into canonical fields, they must agree. Unknown fields are
retained even though generic consumers do not interpret them.

Reverse conversion needs either the preserved Debezium source extension or
explicit target-profile context for source fields and schemas that Debezium
requires. It must fail rather than invent connector metadata. A native
Debezium key/value has no standard slot for every canonical envelope attribute
or optional optimization. For a lossless canonical round trip, an adapter
therefore uses Debezium structured CloudEvents where applicable or retains
`source + id` and any otherwise unrepresentable canonical metadata in an
adapter-owned companion context. That context is not a mandatory broker header
or a new core field; it carries type/shape hints and canonical-only metadata,
not duplicate key or record values. An adapter that emits only the native
semantic intersection can still produce an equivalent `c/u/d/r/t/m` event,
but must not advertise a lossless canonical round trip after discarding the
rest.

### Debezium structured CloudEvents JSON

Debezium 3.6.1.Final's CloudEvents converter emits structured envelopes and can
use JSON or Avro for the envelope and data. This compatibility profile directly
accepts the documented structured JSON form:

- preserve valid incoming `source` and `id` as the canonical event identity;
- read `specversion`, `time`, `datacontenttype`, and `dataschema` according to
  CloudEvents JSON rules;
- map Debezium's CloudEvents `time` to `source_time`: Debezium defines it as
  the time of the change in the source, and it must agree with the corresponding
  `iodebeziumtsms` value when both are present;
- supply `capture_time` from the adapter's deterministic observation context,
  because Debezium structured data contains the row images but not the native
  envelope capture timestamp;
- map `iodebeziumop` and the `data.before`/`data.after` content;
- recover source and transaction metadata from the `iodebezium*` extension
  attributes;
- consume the separately serialized record key, because the Debezium converter
  converts the record value and configures the key converter independently; and
- preserve the original Debezium event type and every unmapped or null extension
  value inside `source_extension`, since the canonical protobuf attribute type
  has no null variant.

The normalized output uses the canonical Invariant `type`, protobuf
`datacontenttype`, `dataschema`, and Any payload. Debezium's documented
`io.debezium.connector.<connector>.DataChangeEvent` type identifies its input
format; it does not replace the canonical output type.

### Auxiliary records are not row changes

Debezium emits records besides data changes. An adapter must classify these
before attempting `ChangeRecord` conversion:

| Input category | Required handling |
| --- | --- |
| Kafka tombstone | A null record value used for compaction is not a second delete. Explicitly pass it through or handle it as a tombstone after the preceding `d` event. |
| Heartbeat | Pass through or handle as a heartbeat/checkpoint signal; never infer a row operation. |
| Schema change | Pass through with its native schema-change payload and a non-ChangeRecord event type; do not map DDL to `TRUNCATE`. |
| Transaction boundary | Pass through `BEGIN`/`END` metadata as boundary records. This differs from transaction enrichment on a data-change record. |

The [Debezium event-changes documentation][debezium-event-changes] explicitly
distinguishes data changes from tombstone, heartbeat, schema-change, and
transaction metadata records. Invariant v1 does not define canonical payload
messages or event-type constants for those auxiliary categories. A service can
retain the original native record or CloudEvent, but it must not label it
`io.invariantprotocol.cdc.v1.change` or pack it as `ChangeRecord`. Dropping an
auxiliary category is an explicit service policy, never an accidental row
conversion.

### Semantic round trips

`Debezium -> canonical -> Debezium` must preserve all information represented
by the input profile. `Canonical -> Debezium -> canonical` must preserve all
canonical fields. Equivalence includes:

- operation and operation-specific presence;
- exact key, image, source-message, nested, array, and map values;
- absent versus explicit null fields;
- decimal text/scale/precision, bytes, temporal meaning, signedness, and
  primitive width when the source schema supplies it;
- collection and schema identity;
- opaque source position and declared ordering scope;
- transaction identity and order;
- source and capture time at the supplied precision;
- stable CloudEvents `source + id`; and
- unknown connector metadata.

Equivalent does not mean byte-identical. JSON object ordering and number text,
Avro schema and binary encoding, protobuf field order, unknown-field placement,
base64 spelling, and default materialization can differ. The contract makes no
claim of byte-identical round trips among JSON, Avro, and protobuf. It also
cannot recover distinctions that were absent from a schemaless input; an
adapter must bring schema knowledge or preserve the typed/raw representation
rather than inventing those distinctions.

## CDC v2 full and delta representations

CDC v2 adds an explicit representation choice without changing the v1 payload
or its Debezium profile. It uses
[`invariant.cdc.v2.ChangeRecord`](../proto/invariant/cdc/v2/change.proto) in the
same upstream `io.cloudevents.v1.CloudEvent` envelope. Every v2 event uses:

| CloudEvents field | Canonical v2 value |
| --- | --- |
| `type` | `io.invariantprotocol.cdc.v2.change` |
| `datacontenttype` | `application/protobuf` |
| `dataschema` | `type.googleapis.com/invariant.cdc.v2.ChangeRecord` |
| `proto_data.type_url` | `type.googleapis.com/invariant.cdc.v2.ChangeRecord` |

The v1 requirements for `id`, `source`, `specversion`, `time`, relationship
extensions, stable retry identity, and the separation of context from event
data apply unchanged. The v2 package is deliberately distinct: a v1 UPDATE
requires a top-level `after`, so making a delta-only UPDATE look like an
additive v1 change would leave an old consumer with an incomplete event. A v2
consumer opts in through the event type and Any URL instead.

V2 retains the same operation, key, collection, schema, source-position,
transaction, timestamp, source-extension, source-message, and exact `Value`
domains. It reserves the former top-level `before`, `after`, and
`changed_fields` names and field numbers. The `representation` oneof contains
either `full` (`FullChange`) or `delta` (`DeltaChange`); it is not valid to
populate both.

### Canonical v2 value equality

Patch validation, replay, and full/delta conversion compare known v2 values by
meaning rather than serialized protobuf bytes:

- the selected `Value.kind`, `type_name`, and optional-field presence are
  exact; distinct scalar kinds never compare equal;
- `Record` fields have unique names and compare without regard to repeated
  field order, recursively at every nesting level;
- `ListValue` elements compare in order;
- `MapValue` keys are unique under this same recursive equality and entries
  compare without regard to order. The repeated entries may retain source
  iteration order on the wire, but that order is not semantic in v2;
- all NaNs within the same float kind compare equal, while positive and
  negative zero are distinct; and
- ordinary protobuf unknown fields do not change known v2 semantics. An older
  consumer that encounters an unknown `Value.kind`, delta effect, or field
  state fails closed instead of treating it as unset or unchanged.

Decimal equality includes exact canonical text, scale, and the presence and
value of declared precision. Bytes, timestamps, nested records, lists, and maps
retain their exact typed domains. These rules define v2 patch/replay semantics
only; they do not reinterpret the unchanged v1 Debezium profile.

### Operation rules

`FullChange` is the self-contained image representation. Its `after` is always
a complete resulting record, never a partial patch. Its operation rules are:

| Operation | `FullChange` rule |
| --- | --- |
| CREATE (`c`) | `after` is required and complete; `before` and `changed_fields` are prohibited |
| UPDATE (`u`) | `after` is required and complete; complete `before` and `changed_fields` are optional |
| DELETE (`d`) | `before` is optional; `after` and `changed_fields` are prohibited |
| SNAPSHOT_READ (`r`) | `after` is required and complete; `before` and `changed_fields` are prohibited |
| TRUNCATE (`t`) | `full` and `delta` are both prohibited |
| SOURCE_MESSAGE (`m`) | `full` and `delta` are both prohibited; top-level `source_message` is required |

`DeltaChange` describes the same row outcomes with a smaller state transition:

| Operation | `DeltaChange.change` rule |
| --- | --- |
| CREATE (`c`) | `result` is required and is the complete created record |
| UPDATE (`u`) | `patch` is required and contains the exact changed fields; an empty patch is a known no-op |
| DELETE (`d`) | `delete` is required and marks the record as absent |
| SNAPSHOT_READ (`r`) | `result` is required and is a complete replay anchor |
| TRUNCATE (`t`) | No representation; applying the operation clears the collection |
| SOURCE_MESSAGE (`m`) | No representation; applying the operation does not change record state |

The existing key and collection rules apply to both representations. A
replay-capable row stream needs a stable key. V2 intentionally has no
`previous_key`:
a source key change is a DELETE of the old key followed by a CREATE of the new
key, ordered together and transaction-linked when that context is available.
`TRUNCATE` is still one collection-wide operation, and `SOURCE_MESSAGE` is
still not a row mutation.

### Exact field transitions

An UPDATE delta is a `RecordPatch` containing `FieldChange` entries. Each
change has a non-empty `FieldPath` plus required `before` and `after`
`FieldState` messages. `FieldState` has exactly one of:

- `value`, containing the exact typed `Value`; or
- `absent`, containing the `Absent` marker.

The four important states therefore remain distinct: unknown is not encoded
as a change, an absent field uses `FieldState.absent`, an explicitly null field
uses `FieldState.value.null_value`, and any other present field uses its exact
typed value. A transition from absent to a value adds a field, value to value
replaces it, and value to absent removes it. Absent to absent and semantically
equal value to value are prohibited; an UPDATE with no changes uses a present,
empty `RecordPatch`.

Paths contain exact record-field name segments and traverse nested `Record`
values only. Within one patch they must be unique and non-overlapping: no path
may equal another or be its ancestor. Consequently patch entry order has no
semantic effect. A missing or non-record intermediate segment is an error; a
producer creates or replaces such a subtree by changing the nearest existing
record field as one complete `Value`.

Lists and maps are atomic at the field boundary. A change to either carries
the complete before and after `ListValue` or `MapValue`; paths never address a
list index or map entry. This can repeat a large collection, but it avoids
index-shift rules, connector-specific map-key equality, and ambiguous dotted
paths. Lists preserve exact source ordering; map entry order remains available
on the wire but is nonsemantic for v2 equality. Both preserve nested typed
values. MongoDB demonstrates why this boundary matters: its native update
description can
report a changed array index, a complete replacement array, or only a new
truncation size depending on the source operation. An adapter using the v2
atomic model must materialize the complete collection before emitting that
field transition. [MongoDB documents these outcome forms explicitly][mongodb-update].

A consumer applies a patch transactionally:

1. Resolve every path against the current record and compare its current state
   with the declared `before` state using canonical `Value` semantics, not
   serialized protobuf bytes.
2. Reject the whole event on a missing base, path/type error, duplicate or
   overlapping path, or before-state mismatch.
3. Only after every precondition succeeds, replace or remove each field using
   its `after` state and commit the resulting record atomically.

This fail-closed rule detects gaps and out-of-order delivery. A duplicate delta
will normally fail its before-state check, so deduplication is mandatory before
application to distinguish an ordinary retry from a broken stream. The
all-or-nothing model follows useful patch precedent without adopting JSON's
number model, JSON Pointer syntax, or command-oriented `move`, `copy`, and
`test` operations. JSON Patch makes sequencing and failure explicit; v2 instead
prohibits overlapping changes so entry order is immaterial. [JSON Patch][json-patch]
The HTTP PATCH specification likewise warns about base-dependent application
and requires atomicity. [HTTP PATCH][http-patch]

JSON Merge Patch is not the v2 representation. It assigns null the meaning of
removal and cannot patch part of an array, so it cannot preserve Invariant's
explicit-null distinction or exact collection semantics. [JSON Merge Patch
documents both limitations][json-merge-patch].

### Replay and state at a source position

For one `(source, data_collection, key)`, the normative forward state machine
is:

- CREATE or SNAPSHOT_READ establishes the complete `after` or `result` state;
- full UPDATE replaces the state with its complete `after`;
- delta UPDATE validates and applies its `patch`;
- DELETE makes the record absent;
- TRUNCATE makes every record in the collection absent; and
- SOURCE_MESSAGE leaves row state unchanged.

Full and delta events can coexist in one v2 stream. A complete full image or
delta `result` is a new replay anchor. Starting with one anchor and applying
the same complete, deduplicated sequence produces the same materialized state
for either representation.

Replay state is scoped by `(CloudEvent.source, data_collection, key)`, and a
TRUNCATE clears only that source's collection. CREATE requires the key not to
be materialized already; a distinct repeated CREATE is an error, while a retry
is removed by `source + id` deduplication first. SNAPSHOT_READ and a full UPDATE
are complete outcome anchors and may replace or establish state. DELETE may
establish absence even when replay begins after the row's prior state. Only a
delta UPDATE patch requires an existing materialized row base.

That guarantee has operational preconditions. A replay-capable delta producer
must provide a stable row key, a CREATE/SNAPSHOT/full-image/checkpoint base
before each delta UPDATE chain, and an ordering scope that contains every
update for that key. Consumers must deduplicate by CloudEvents `source + id`
before state validation, stop on a
gap or mismatch, and never apply an unknown future delta effect or field-state
variant as though it were a no-op. Replaying TRUNCATE relative to partitioned
row changes additionally requires a service-owned collection barrier or source
frontier; the core contract does not invent global order.

To answer “what was this record at this time?”, a service starts from the
latest complete anchor or checkpoint no later than the requested source
frontier and replays in declared source order. `source_time` alone is not an
ordering key: multiple transactions can share a timestamp, capture can be
delayed, and ordering is never global. A time-indexed service must resolve
timestamp ties to source positions and, when it promises transactionally
consistent results, publish only a complete transaction frontier. Source
positions remain opaque to generic consumers; indexing and checkpoint
retention remain service-owned.

A source-position identity uses the full `(CloudEvent.source,
source_position.stream, source_position.format, source_position.value)`
context. Opaque value bytes alone need not be unique across sources, streams,
or formats.

Each UPDATE patch records both field states, so a consumer that has the
resulting record can invert it by swapping `before` and `after`. CREATE and
DELETE are inverted by changing record existence, but an empty `DeleteDelta`
does not independently carry the deleted row. Reverse traversal across a
delete therefore needs the prior materialized state or a checkpoint. Ordinary
historical reconstruction is forward replay from an anchor and does not
require every event to be a standalone database snapshot.

### Full and delta conversion

Conversion preserves the operation and all common event metadata. It is a
state-semantic projection, not a promise that the two protobuf encodings carry
the same redundant evidence:

- full CREATE/SNAPSHOT `after` maps directly to delta `result`, and back;
- full UPDATE maps to a delta patch by recursively comparing a complete prior
  record with the complete `after` image;
- delta UPDATE maps to full by validating the patch against the materialized
  prior record and using the result as complete `after`;
- DELETE maps to or from `DeleteDelta` without a base; a materialized prior
  record is needed only to populate the optional full `before`; and
- TRUNCATE and SOURCE_MESSAGE retain no representation in either direction.

The recursive diff descends only through records. A changed list or map becomes
one atomic field transition. Delta `before` states can validate or invert the
diff, and `changed_fields` for a projected `FullChange` is derived from the
patch paths. If a full UPDATE has no complete `before`, the converter uses its
materialized prior state; without one it must fail instead of guessing removed
fields. Likewise, converting a delta UPDATE without its required base must
fail; delta DELETE can project to a valid full DELETE with `before` absent.

The event remains the same source occurrence during an in-process full/delta
projection, so `source + id`, source position, transaction context, and times
do not change. A service that publishes both encodings decides how subscribers
select one representation; it must not cause a consumer to apply both as two
source mutations.

### Debezium and delta production

The pinned Debezium profile above remains the raw-lossless v1 mapping. A direct
v2 `FullChange` projection is valid only when its semantic images are complete.
Debezium normally provides row images; its changed-field transform adds the
names of changed fields rather than replacing the envelope with an applicable
delta. [Debezium documents that transform as
header enrichment][debezium-event-changes]. PostgreSQL replica identity can
also make `before` contain only key fields or nothing, so a connector-provided
`before` is not necessarily a complete diff base. [Debezium documents those
replica-identity modes][debezium-postgres-replica].

Because v2 `FullChange.before`, when present, is complete, an adapter must not
copy a connector-partial Debezium `before` into that field. It hydrates a
complete prior image or omits semantic `before` and preserves the partial
source evidence in `source_extension`. Producing a delta still requires the
adapter's authoritative materialized state. An incomplete or unavailable-TOAST
`after` cannot be used as a v2 full anchor; the adapter hydrates a complete
outcome or fails.

A Debezium-to-v2-delta adapter is therefore stateful. It keys an authoritative
materialization by the canonical collection and key, consumes events in source
order, verifies any source-provided prior fields, and computes exact field
transitions from the stored record to the complete `after`. It uses CREATE and
snapshot reads as anchors, deletes stored state on `d`, clears collection state
on ordered `t`, and treats `m` as non-row content. If an unavailable value,
partial collection update, missing base, or ordering gap prevents an exact
diff, the adapter emits v2 `FullChange` when a complete image exists or fails;
it never labels an inferred or incomplete patch lossless. Connector metadata
continues to use `source_extension`.

MongoDB provides a useful delta-native comparison: change streams return field
deltas by default, while full-document lookup is optional and can observe a
later majority-committed version. MongoDB states that the event delta still
describes the original change correctly. [MongoDB change streams][mongodb-change-streams]
That is evidence for an outcome-based delta, not a reason to copy MongoDB's
document paths or BSON envelope into the Invariant core.

### Storage and consumption tradeoffs

Delta is usually smaller for wide records with sparse scalar updates. Carrying
both states for changed fields costs more than an after-only patch but enables
base validation and exact inversion. Atomic list and map changes can approach
full-image size, and protobuf path/type overhead can dominate very small rows.
Full images also benefit from transport compression when adjacent records have
similar shapes, so no universal size ratio is promised.

Full UPDATEs are self-contained and naturally support idempotent keyed upserts.
Delta UPDATEs need ordered state, anchors, gap handling, and deduplication; a
log cannot retain only the latest delta for each key and still reconstruct the
record. Periodic full anchors or service-owned checkpoints bound replay cost.
Flink makes the same broad distinction between retract/full-row changelogs and
keyed upsert changelogs, and SQL Server CDC carries before/after rows plus a
changed-column mask. [Flink `RowKind`][flink-row-kind]
[Flink keyed upsert mode][flink-changelog-mode]
[SQL Server CDC][sql-server-cdc]

Invariant standardizes both wire representations and their replay semantics.
It does not choose a storage layout, snapshot interval, compaction policy,
materialized-state engine, or time-travel index for a service.

## Protobuf evolution and forwarding

The CDC messages evolve additively. Existing field numbers and enum numbers
must not be changed or reused; removed fields and values must be reserved.
Consumers must accept protobuf unknown fields. A relay that parses and
reserializes an event must also preserve them; a typed runtime that discards
unknowns is suitable for consuming known semantics, but not for rebuilding a
lossless relay message. A relay that does not recognize the Any `type_url`
forwards the unchanged `Any.type_url` and `Any.value`; it does not unpack and
rebuild it. Likewise, an unknown numeric `Operation` value is not coerced to
`OPERATION_UNSPECIFIED`.

Most protobuf runtimes preserve binary unknown fields, but converting through
JSON or reconstructing a message from only known fields can discard them. A
gateway that cannot retain unknown protobuf data must fail or use an opaque
pass-through path instead of claiming lossless forwarding. See the official
[protobuf unknown-fields guidance][protobuf-unknown-fields].

## Delivery and ownership

- Delivery is at least once unless the selected transport provides and the
  service adopts a stronger guarantee.
- Consumers deduplicate with the CloudEvents `source + id` pair.
- Ordering is scoped only to a declared `source_position.stream` or equivalent
  source partition. There is no global ordering guarantee.
- Generic consumers treat `source_position` as opaque.
- V1 uses full `after` images and optional `changed_fields`; v2 producers select
  `FullChange` or `DeltaChange` without making a partial image ambiguous.
- Checkpoint storage, acknowledgement, retries, backoff, retention, replay,
  capture queries, outbox implementation, and delivery topology are owned by
  the producing and consuming services.

Invariant owns only the protobuf wire contract and these semantics. It does not
own or require a capture engine, outbox, database, broker, connector, schema
registry, checkpoint store, or delivery runtime.

## Conformance profile

Golden fixtures are derived from the official Debezium 3.6 examples and pin
`3.6.1.Final`; they contain no deployment authentication material. Conformance
covers create, update, delete, snapshot-read, truncate, logical-message,
schemaful and schemaless native envelopes, structured CloudEvents JSON,
transaction metadata, source positions, absent/null values, exact decimal,
binary and temporal values, nested data, unknown connector metadata, retry ID
stability, protobuf unknown-field forwarding, and semantic round trips in both
directions.

The exact fixture provenance, input classification, and expected fidelity are
recorded beside the corpus in the
[Debezium 3.6.1.Final fixture guide][cdc-fixtures].

V2 conformance additionally covers both row representations for CREATE,
UPDATE, DELETE, and SNAPSHOT_READ plus representation-neutral TRUNCATE and
SOURCE_MESSAGE, exact absent/null transitions, no-op updates, nested paths,
atomic list/map changes, invalid path overlap and before-state mismatch, stable
retry identity, anchor requirements, ordered replay, full/delta conversion with
state, and equal materialized results across Go, Python, Rust, and TypeScript.

The CDC contract is independent of auditing. This repository does not define
an `AuditEvent`, and this work neither introduces nor changes one. A consuming
descriptor graph that defines an audit event may continue to use CloudEvents
independently; audit events are never wrapped in or reinterpreted as
`ChangeRecord`.

[ce-core]: https://github.com/cloudevents/spec/blob/ce%40v1.0.2/cloudevents/spec.md
[ce-protobuf-data]: https://github.com/cloudevents/spec/blob/ce%40v1.0.2/cloudevents/formats/protobuf-format.md#3-data
[ce-protobuf-format]: https://github.com/cloudevents/spec/blob/ce%40v1.0.2/cloudevents/formats/protobuf-format.md
[ce-release]: https://github.com/cloudevents/spec/tree/ce%40v1.0.2
[cdc-fixtures]: ../testdata/cdc/debezium/3.6.1.Final/README.md
[debezium-cloudevents]: https://debezium.io/documentation/reference/3.6/integrations/cloudevents.html
[debezium-event-changes]: https://debezium.io/documentation/reference/3.6/transformations/event-changes.html
[debezium-postgres-events]: https://debezium.io/documentation/reference/3.6/connectors/postgresql.html#postgresql-events
[debezium-postgres-replica]: https://debezium.io/documentation/reference/3.6/connectors/postgresql.html#postgresql-replica-identity
[debezium-release]: https://debezium.io/releases/3.6/release-notes#release-3.6.1-final
[debezium-serdes]: https://debezium.io/documentation/reference/3.6/integrations/serdes.html
[flink-changelog-mode]: https://nightlies.apache.org/flink/flink-docs-stable/api/java/org/apache/flink/table/connector/ChangelogMode.html
[flink-row-kind]: https://nightlies.apache.org/flink/flink-docs-stable/api/java/org/apache/flink/types/RowKind.html
[http-patch]: https://www.rfc-editor.org/info/rfc5789/
[json-merge-patch]: https://www.rfc-editor.org/info/rfc7396/
[json-patch]: https://www.rfc-editor.org/info/rfc6902/
[mongodb-change-streams]: https://www.mongodb.com/docs/manual/changeStreams/
[mongodb-update]: https://www.mongodb.com/docs/v8.0/reference/change-events/update/
[protobuf-unknown-fields]: https://protobuf.dev/programming-guides/proto3/#unknowns
[sql-server-cdc]: https://learn.microsoft.com/en-us/sql/relational-databases/track-changes/about-change-data-capture-sql-server?view=sql-server-ver17
[trace-context]: https://www.w3.org/TR/trace-context/
