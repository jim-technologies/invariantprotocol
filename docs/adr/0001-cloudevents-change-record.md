# ADR 0001: CloudEvents envelope with a typed CDC payload

- Status: Accepted
- Date: 2026-08-17

## Context

Change data capture needs two kinds of interoperability. Event infrastructure
needs stable identity, origin, time, type, schema, and trace context without
understanding a database record. Data consumers need exact operation, presence,
value, schema, source-position, and transaction semantics without depending on
a particular capture implementation.

CloudEvents already defines the first boundary. Its latest stable
[v1.0.2 core specification][ce-core] requires `source + id` identity and a
versioned event `type`; its [protobuf event format][ce-protobuf] provides an
upstream `io.cloudevents.v1.CloudEvent` with typed protobuf data in
`google.protobuf.Any`. Debezium's native data-change envelope provides the
second boundary for a widely used CDC ecosystem, including `c`, `u`, `d`, and
`r` operations, transaction enrichment, connector-native positions, and
source/capture timestamps. Some connectors also supply `t` and `m` operations.

Copying Debezium's complete envelope into the core would make its connector and
delivery vocabulary part of Invariant. Using an untyped JSON object would lose
presence and exact decimal, binary, temporal, unsigned, array, map, and nested
record semantics. Adding CDC fields to CloudEvent context would make generic
routing metadata large, source-specific, and difficult to evolve.

## Decision

Canonical CDC is `CloudEvent<ChangeRecord>`:

1. Reuse the wire-compatible CloudEvents v1.0.2
   `io.cloudevents.v1.CloudEvent` schema. Do not fork or wrap it.
   Vendor that schema for deterministic repository generation while retaining
   its public protobuf identity and exact wire layout; language package paths
   are local distribution metadata.
2. Pack `invariant.cdc.v1.ChangeRecord` in `CloudEvent.proto_data` as
   `google.protobuf.Any`.
3. Require this canonical envelope profile:

   | Attribute | Value |
   | --- | --- |
   | `specversion` | `1.0` |
   | `type` | `io.invariantprotocol.cdc.v1.change` |
   | `datacontenttype` | `application/protobuf` |
   | `dataschema` and Any `type_url` | `type.googleapis.com/invariant.cdc.v1.ChangeRecord` |

   `id`, `source`, and `time` are also required and source-defined under the
   rules in the [CDC contract](../cdc.md). Optional `correlationid`,
   `causationid`, `traceparent`, and `tracestate` extensions carry relationship
   context only.
4. Keep the payload transport-neutral. It carries the operation, canonical key
   and images, data-collection identity, schema reference, opaque source
   position, optional transaction and changed-field information, source and
   capture times, and an isolated source extension.
5. Represent values as a typed recursive protobuf union. A missing
   `RecordField` is absent; a present `RecordField` with `NullValue` is
   explicitly null. Exact decimal, bytes, timestamps, unsigned integers,
   arrays, maps, and nested records never need an intermediate floating-point
   JSON representation.
6. Map Debezium operations one-to-one:

   | Debezium | Canonical |
   | --- | --- |
   | `c` | `OPERATION_CREATE` |
   | `u` | `OPERATION_UPDATE` |
   | `d` | `OPERATION_DELETE` |
   | `r` | `OPERATION_SNAPSHOT_READ` |
   | `t` | `OPERATION_TRUNCATE` |
   | `m` | `OPERATION_SOURCE_MESSAGE` |

   `t` and `m` are accepted only when the source connector supports them.
   `SOURCE_MESSAGE` is never represented as a row mutation.
7. Pin the conformance profile and fixtures to exact stable Debezium
   [`3.6.1.Final`][debezium-release]. Accept schemaful and schemaless native
   envelopes and documented [structured CloudEvents JSON][debezium-ce].
   Preserve connector-specific source data in `source_extension`, never in the
   CloudEvent context or portable core fields.
8. Treat tombstones, heartbeats, schema changes, and transaction boundaries as
   explicitly classified auxiliary records. They can pass through under
   service policy, but they do not use the canonical change event type or
   `ChangeRecord` payload.
9. Promise semantic, not byte-identical, round trips across Debezium and the
   canonical contract. JSON, Avro, and protobuf may encode equivalent values
   differently.

## Consequences

Generic event infrastructure can route, deduplicate, and trace CDC events using
standard CloudEvents context while record consumers depend on one exact,
versioned protobuf payload. `source + id` remains stable across retries.
Ordering can be declared for a source stream without suggesting global order,
and generic consumers retain source positions without learning their format.

Adapters carry real work. A Debezium adapter must read the separate key, honor
operation-specific presence, recover types from the Kafka Connect or registry
schema where available, choose the highest supplied timestamp precision, and
retain unknown source metadata. A schemaless serialization cannot disclose a
type distinction that was already erased, so exact adapters need independent
schema knowledge or an opaque typed/raw preservation path.

Protobuf evolves additively. Relays preserve unknown fields and unknown Any
payloads as bytes instead of rebuilding only known fields. Incompatible payload
semantics require a new package, Any URL, `dataschema`, and event-type version.

Delivery is at least once unless a service adopts a stronger transport
guarantee. Deduplication uses `source + id`; ordering is limited to a declared
source stream. Capture, outbox, checkpoints, retries, retention, transport, and
delivery topology remain service-owned.

## Rejected alternatives

### Make the Debezium envelope canonical

This would expose connector names, source-specific offset fields, and delivery
assumptions as universal concepts. It would also make evolution depend on an
external connector implementation. Debezium instead remains a first-class,
lossless compatibility profile.

### Put records and connector offsets in CloudEvent attributes

CloudEvent attributes are small, inspectable event context with a restricted
type system. Complete records and connector source structures belong in event
data. The typed payload and isolated source extension preserve them without
polluting routing context.

### Use JSON as the canonical payload

Untyped JSON cannot by itself preserve integer width and signedness, exact
decimal scale, arbitrary bytes, map key types, or absent versus explicit null
across all supported encodings. Protobuf supplies explicit presence, exact
scalar domains, unknown-field forwarding, and generated artifacts in every
repository language.

### Model auxiliary Debezium records as row mutations

A compaction tombstone is not a second delete, a heartbeat is not an update,
DDL is not `TRUNCATE`, and a transaction boundary is not transaction-enriched
row data. Conflating them would produce false facts, so v1 leaves their payload
contracts outside `ChangeRecord`.

## Boundary

Invariant owns the wire contract and semantics only. It does not own capture,
outbox, checkpointing, retries, retention, or delivery. No transport, broker,
database, connector, or registry is required.

This repository does not define an `AuditEvent`; this decision neither creates
nor changes one. Any separately defined audit contract continues to use
CloudEvents independently and is never coupled to `ChangeRecord`.

[ce-core]: https://github.com/cloudevents/spec/blob/ce%40v1.0.2/cloudevents/spec.md
[ce-protobuf]: https://github.com/cloudevents/spec/blob/ce%40v1.0.2/cloudevents/formats/protobuf-format.md
[debezium-ce]: https://debezium.io/documentation/reference/3.6/integrations/cloudevents.html
[debezium-release]: https://debezium.io/releases/3.6/release-notes#release-3.6.1-final
