# Debezium 3.6.1.Final CDC fixtures

This directory is a pinned, shared conformance corpus for the Invariant CDC
contract. The native fixtures pair Debezium's separately serialized record key
and value in the test-only shape `{ "key": ..., "value": ... }`. Manifest
entries encoded as `cloudevents-structured-json` point to raw CloudEvents
structured JSON objects instead. Their separately serialized record key is the
raw JSON object named by the entry's `key_path`; it is deliberately not
embedded in the CloudEvent. `manifest.json` is the machine-readable index and
is the authority for classifying a fixture before interpreting it.

## Version and provenance

- Debezium is pinned to exactly `3.6.1.Final`, not a floating container tag or
  documentation alias. The exact upstream source tag is
  <https://github.com/debezium/debezium/tree/v3.6.1.Final>, and the release
  series is recorded at <https://debezium.io/releases/3.6/>.
- Native PostgreSQL envelope, transaction, truncate, logical-message, and
  tombstone shapes are adapted from the official Debezium 3.6 PostgreSQL
  connector documentation:
  <https://debezium.io/documentation/reference/3.6/connectors/postgresql.html>.
- The schemaful MySQL shape and its precise decimal, unsigned `BIGINT`, binary,
  and temporal mappings are adapted from the official Debezium 3.6 MySQL
  connector documentation:
  <https://debezium.io/documentation/reference/3.6/connectors/mysql.html>.
- The structured CloudEvents shape is adapted from Debezium's official 3.6
  CloudEvents integration example:
  <https://debezium.io/documentation/reference/3.6/integrations/cloudevents.html>.
- CloudEvents behavior is pinned to the stable `ce@v1.0.2` specification and
  JSON format:
  <https://github.com/cloudevents/spec/blob/ce%40v1.0.2/cloudevents/spec.md> and
  <https://github.com/cloudevents/spec/blob/ce%40v1.0.2/cloudevents/formats/json-format.md>.

The examples are adaptations, not captured production records. Names, values,
times, schema URIs, source positions, and identifiers are deterministic test
data. Email addresses use the reserved `.invalid` domain. No connection
configuration, credentials, real topic names, or private deployment names are
included. A valid `dataschema` attribute and deliberately unknown
`iodebeziumfuturetoken` extension were added to the structured example to
exercise preservation behavior.

## Fidelity cases

`native-create-schemaful.json` is deliberately schemaful so its bytes and
logical types remain distinguishable. Kafka Connect JSON encodes byte values as
base64. `account_balance` is the signed unscaled integer `123456` at scale `2`
(`1234.56`); `unsigned_counter` is `18446744073709551615` represented through
Debezium's precise Decimal carrier plus the documented propagated source type
`BIGINT UNSIGNED`, so it maps canonically to `uint64_value`; and `raw_payload`
is the byte sequence `00 ff 10`. The event also carries a microsecond timestamp,
an array, a nested record, and an `int64` beyond JavaScript's safe-integer
range. Readers must not route those numbers through IEEE-754 binary64.

Within that schemaful `after` image, `nickname` is explicitly null while the
known optional field `middle_name` is absent. That distinction must survive a
semantic round trip. A schemaless event can preserve the representation it
actually carries, but it cannot reconstruct logical type information that its
upstream serializer discarded; adapters retain the original schema/encoding
and connector fields in the isolated source extension.

`future_connector_metadata` and `iodebeziumfuturetoken` stand for connector
metadata unknown to an adapter. They must survive intact without becoming
CloudEvents routing attributes in the canonical output.
`future_envelope_metadata` and `future_transaction_metadata` likewise exercise
opaque preservation for unknown native envelope and transaction members.

## Event identity and auxiliary records

`structured-cloudevent-snapshot.json` and
`structured-cloudevent-snapshot-retry.json` are byte-identical. Their identical
CloudEvent `source` and `id` model delivery of the same event more than once;
they are not two distinct changes.

Files prefixed with `auxiliary-` are intentionally not data-change envelopes.
A Kafka tombstone is not a second delete, a heartbeat is not a row mutation,
schema-change metadata is not a `ChangeRecord`, and transaction `BEGIN`/`END`
records are not the transaction's row changes. Tests and adapters must dispatch
them using the `category` in `manifest.json` or pass them through explicitly.

The corpus asserts semantic, not byte-identical, interoperability. JSON, Avro,
and protobuf encodings have different wire representations, and field order is
not part of the asserted round trip.
