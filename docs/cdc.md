# Canonical change data capture

Invariant's CDC contract describes a captured change; it does not prescribe how
the change is captured or delivered. Every canonical CDC event is an
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
- Full `after` images are preferred; `changed_fields` is optional metadata.
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
[debezium-release]: https://debezium.io/releases/3.6/release-notes#release-3.6.1-final
[debezium-serdes]: https://debezium.io/documentation/reference/3.6/integrations/serdes.html
[protobuf-unknown-fields]: https://protobuf.dev/programming-guides/proto3/#unknowns
[trace-context]: https://www.w3.org/TR/trace-context/
