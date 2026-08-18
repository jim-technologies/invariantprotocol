# ADR 0002: Explicit full-image and delta CDC representations

- Status: Accepted
- Date: 2026-08-17

## Context

ADR 0001 established `CloudEvent<invariant.cdc.v1.ChangeRecord>` as the
transport-neutral CDC contract. V1 follows the image-oriented model used by
Debezium: CREATE and snapshot events carry a complete `after`; UPDATE carries a
complete `after` and optional `before`; DELETE carries an optional `before`.
That representation is self-contained for keyed upserts and maps losslessly to
the pinned Debezium profile.

Image-oriented CDC is not the only established model. MongoDB change streams
return update deltas by default and optionally provide pre/post images. Its
`updateDescription` reports final values for changed fields, removed fields,
and array truncations rather than replaying the source update command.
[MongoDB update events][mongodb-update] SQL Server CDC instead retains update
pre/post rows plus a changed-column mask, while Flink distinguishes full
`UPDATE_BEFORE` and `UPDATE_AFTER` changelog rows and a keyed upsert mode that
can omit the former. [SQL Server CDC][sql-server-cdc]
[Flink `RowKind`][flink-row-kind] [Flink upsert mode][flink-changelog-mode]
There is no single vendor-neutral CDC body that makes one representation
canonical for every source and consumer.

Full images favor stateless consumption, idempotent upserts, late subscribers,
and simple recovery. Deltas favor sparse updates to wide records, explicit
human-readable diffs, validation against prior state, and potentially smaller
event storage. Making a partial `after` mean “unchanged fields were omitted”
would destroy v1's complete-image meaning. Adding a delta-only UPDATE to v1
would be protobuf-wire additive but semantically incompatible: an old consumer
would decode an UPDATE with no required `after`.

JSON patch formats do not solve the typed CDC problem. JSON Merge Patch uses
null as its removal sentinel and cannot patch part of an array, conflicting
with Invariant's explicit-null semantics. [RFC 7396][json-merge-patch] JSON
Patch defines useful ordered and atomic processing rules, but its JSON Pointer
paths, array-index mutation, command operations, and JSON value domain add
complexity without preserving Invariant's decimal, bytes, unsigned, temporal,
and typed-map domains. [RFC 6902][json-patch]

## Decision

### Introduce a distinct v2 payload identity

The dual-representation contract is `invariant.cdc.v2.ChangeRecord`, packed in
the unchanged upstream `io.cloudevents.v1.CloudEvent` protobuf envelope:

| CloudEvents field | V2 value |
| --- | --- |
| `specversion` | `1.0` |
| `type` | `io.invariantprotocol.cdc.v2.change` |
| `datacontenttype` | `application/protobuf` |
| `dataschema` | `type.googleapis.com/invariant.cdc.v2.ChangeRecord` |
| `proto_data.type_url` | `type.googleapis.com/invariant.cdc.v2.ChangeRecord` |

The required `id`, `source`, and `time` attributes and optional correlation,
causation, and W3C trace extensions retain the ADR 0001 rules. `source + id`
identifies the same source occurrence across retries. Record data and
connector-specific metadata remain out of CloudEvent attributes.

V1 remains supported and unchanged. V2 copies the common canonical value and
metadata types into its own versioned package. It retains the common top-level
field-number layout, reserves the v1 top-level `before`, `after`, and
`changed_fields` names and numbers, and adds this explicit oneof:

```text
ChangeRecord.representation
  full  -> FullChange
  delta -> DeltaChange
```

A producer never populates both. Consumers select v2 through the CloudEvents
type and Any URL before interpreting the representation.

### Give both representations the same operation outcomes

The common operation is authoritative. Representation does not create a second
CRUD vocabulary.

| Operation | `FullChange` | `DeltaChange` |
| --- | --- | --- |
| CREATE (`c`) | Complete `after`; no `before` or mask | Complete `result` anchor |
| UPDATE (`u`) | Complete `after`; optional complete `before` and mask | `RecordPatch`, including an empty known no-op |
| DELETE (`d`) | Optional `before`; no `after` or mask | `DeleteDelta` marker |
| SNAPSHOT_READ (`r`) | Complete `after`; no `before` or mask | Complete `result` anchor |
| TRUNCATE (`t`) | No representation | No representation |
| SOURCE_MESSAGE (`m`) | No representation; top-level message required | No representation; top-level message required |

Keys, collection identity, source positions, transaction context, timestamps,
and source extensions have the same meaning in either representation.
`TRUNCATE` clears one collection and is not expanded into synthetic row
deletes. `SOURCE_MESSAGE` has no row-state effect. V2 has no `previous_key`;
sources represent a key change as an ordered DELETE of the old key and CREATE
of the new key, with transaction context when available.

### Make deltas exact, typed outcomes

An UPDATE `RecordPatch` contains `FieldChange` entries. Each entry contains an
exact non-empty record-field path and required `before` and `after`
`FieldState` values. `FieldState` explicitly selects either:

- `value`, which can itself select `NullValue` or any other canonical `Value`;
  or
- `absent`, using the `Absent` marker.

This is an outcome delta, not a source-command log. Increment, append, move,
copy, and connector-specific update commands are normalized to the values that
actually existed before and after the occurrence. Absent, explicitly null, and
present non-null remain distinct. Absent-to-absent and equal-value transitions
are invalid; a no-op UPDATE uses an empty patch.

V2 canonical equality compares the selected kind, `type_name`, and optional
presence exactly. Record fields are uniquely named and unordered, list elements
are ordered, and map entries are unordered with keys unique under recursive
canonical value equality. Map source iteration order may remain on the wire but
does not create a v2 change. All NaNs of one float kind compare equal, positive
and negative zero remain distinct, and ordinary protobuf unknown fields do not
alter known semantics. Unknown oneof variants fail closed.

Paths traverse exact field-name segments through nested records only. Paths in
one patch are unique and non-overlapping, including ancestor/descendant
overlap, so repeated-entry order is not semantic. Lists and maps are atomic
field values: changing one replaces its complete typed value. This deliberately
forgoes element-level compactness to avoid index-shift ordering, ambiguous
dotted paths, and source-specific map-key equality. MongoDB's multiple array
delta forms demonstrate the adapter complexity that this boundary contains.
[MongoDB update events][mongodb-update]

A patch is applied atomically. The consumer first resolves and validates every
declared `before` state against the current record using canonical value
semantics. Any missing base, invalid traversal, mismatch, overlap, or unknown
effect rejects the whole event. Only then are all `after` states installed.
HTTP PATCH supplies the relevant precedent: base-dependent patches are not
generally idempotent and the whole change set is atomic. [RFC 5789][http-patch]
The Invariant contract borrows those safety properties, not the HTTP method or
wire format.

### Define replay as a state machine with explicit prerequisites

For each `(source, data_collection, key)`, CREATE or SNAPSHOT_READ establishes
a complete record, UPDATE replaces or patches it, and DELETE makes it absent.
TRUNCATE clears collection state; SOURCE_MESSAGE leaves it unchanged. A full
image or delta `result` can re-anchor a mixed v2 stream.

A distinct CREATE for an already materialized key is invalid; stable retries
are deduplicated first. SNAPSHOT_READ and full UPDATE may re-anchor state,
DELETE may establish absence without a prior row, and TRUNCATE affects only the
named collection within the CloudEvent source. Only delta UPDATE requires an
existing row base.

Exact delta replay requires:

1. a stable key for independently materialized row state;
2. a CREATE, snapshot, full image, or service checkpoint base;
3. complete delivery in the declared source ordering scope;
4. deduplication by CloudEvents `source + id` before patch application; and
5. fail-closed handling of gaps, before-state mismatches, and unknown delta
   semantics.

At-least-once delivery makes the fourth rule load-bearing. A repeated delta
normally fails its before-state check; deduplicating first distinguishes an
ordinary retry from a real gap or ordering failure. A collection-wide TRUNCATE
also needs a service-owned ordering barrier relative to partitioned row
streams. Invariant does not claim global order.

State “at time” means state at a resolved source frontier, not a sort by wall
clock alone. A service starts from the latest anchor or checkpoint before the
frontier and applies complete source order. It resolves equal timestamps with
source positions and, if it promises transactional visibility, exposes only
complete transaction frontiers. Source positions remain opaque to generic
consumers; time indexes and checkpoint retention remain service policy.

Field transitions can be inverted by swapping before and after when the
resulting record is available. CREATE and DELETE invert record existence, but
`DeleteDelta` intentionally does not duplicate the removed row. Reverse
traversal across a delete therefore requires prior materialized state or a
checkpoint. The portable guarantee is deterministic forward reconstruction
from an anchor, not a claim that every event is a standalone snapshot.

### Make full/delta projection state-semantic

CREATE and SNAPSHOT_READ project directly between complete `after` and
`result`. An UPDATE projects from full to delta by diffing a complete prior
record against complete `after`; it projects from delta to full by applying the
patch to that prior record. The diff recurses through records and treats lists
and maps atomically. DELETE projects to `DeleteDelta`; reconstructing a full
delete can omit `before`, while populating that optional evidence requires
prior materialized state. TRUNCATE and SOURCE_MESSAGE do not gain a
representation.

UPDATE conversion is total only in the replay context that its patch requires.
A full UPDATE whose `before` is absent uses the materialized state. A converter
with neither a complete `before` nor an authoritative base fails rather than
guessing removals or treating a partial record as complete. Common metadata,
source position, and `source + id` remain attached to the same occurrence.
Publishing both encodings must not cause consumers to apply the occurrence
twice.

### Keep Debezium compatibility full-first and stateful for delta

The pinned Debezium 3.6.1.Final compatibility profile remains the raw-lossless
v1 mapping. Direct projection to v2 `FullChange` additionally requires complete
semantic images. Debezium's changed-record-state
transformation adds changed field names to headers; it does not replace row
images with an applicable typed patch. [Debezium event changes][debezium-changes]
PostgreSQL replica identity can also limit the `before` image to key fields or
omit it. [Debezium PostgreSQL replica identity][debezium-replica]

V2 `FullChange.before` is complete when present. An adapter hydrates a partial
connector `before` or omits it and retains the source evidence in
`source_extension`; it never promotes a partial image to semantic full state.
Likewise, an incomplete or unavailable-TOAST `after` cannot serve as a v2
anchor and must be hydrated or rejected.

A Debezium adapter emits v2 delta only while maintaining authoritative state in
source order. CREATE and snapshot events seed it. UPDATE compares stored state
with the complete `after`, validates whatever source `before` is available,
and emits exact transitions. DELETE removes state, TRUNCATE clears collection
state at an ordered barrier, and SOURCE_MESSAGE does not mutate it. An
unavailable TOAST value, incomplete collection outcome, missing base, or gap
forces `FullChange` when a complete image exists or a failure. It is never
silently approximated as a replayable delta. Connector metadata remains in the
isolated source extension.

## Consequences

Consumers that want simple upserts can remain on v1 or select v2 full events.
Consumers that want explicit diffs can select v2 delta and implement one small,
typed, deterministic state machine. A mixed v2 stream can fall back to a full
image when an exact compact delta is unavailable and use that image as a new
anchor.

Sparse updates to wide scalar records generally shrink. Before and after states
for changed fields cost more than an after-only patch but permit validation and
inversion. Large list/map changes can be as large as full images. Full records
also compress well in repeated batches, so the contract makes no universal
storage-saving claim.

Delta consumers pay operational costs: materialized state, strict ordering,
deduplication, snapshots/checkpoints, and gap recovery. A broker cannot compact
a delta log to only its latest event per key and retain reconstructability.
Checkpoint cadence, retention, indexing, and materialization remain
service-owned rather than entering the protobuf.

V1 remains stable. V2 uses an intentionally different package, CloudEvents
type, and Any URL, so existing consumers never reinterpret a delta-only event
as a valid v1 UPDATE.

## Rejected alternatives

### Broaden v1 UPDATE to allow a missing `after`

This would be wire additive but semantically breaking. Existing v1 consumers
correctly rely on complete `after` for UPDATE.

### Treat `changed_fields` plus a partial `after` as a delta

The same record would mean complete state in one event and partial state in
another. It would also make omitted fields ambiguous between unchanged,
absent, and unavailable.

### Carry only new values in delta entries

An after-only patch is smaller, but it cannot validate its base, distinguish a
true addition from an overwrite, or invert a transition without separately
recovering the old field. The chosen two-state transition is still compact for
sparse changes and fails closed on a wrong base.

### Adopt JSON Patch or JSON Merge Patch

Their JSON domain cannot preserve all canonical values. Merge Patch conflates
null and removal; JSON Patch adds indexed collection mutation and
command-oriented operations that are unnecessary for captured outcomes.

### Add indexed list and keyed map patch operations

This can reduce individual document-heavy events but introduces sequential
index semantics, map-key equality, additional unknown operations, and more
cross-language replay surface. Atomic collection replacement covers every
canonical value correctly. A later incompatible representation can be added
only with its own explicit version.

### Make delta the only v2 representation

Some sources cannot produce an exact delta without hydration, and some sinks
need stateless upserts or full re-anchors. Requiring both producers and
consumers to run stateful conversion would move complexity into every service.

### Use separate full-record and delta-record event contracts

Separate messages and event types would duplicate the operation vocabulary and
common metadata, awkwardly repeat representation-free TRUNCATE and
SOURCE_MESSAGE, and make a full fallback or re-anchor look like a different
event family in a delta-oriented stream. The v2 oneof keeps both encodings on
one source occurrence, so publishing or projecting the alternate encoding does
not turn it into a second mutation.

## Boundary

Invariant owns the v2 protobuf wire contract, CloudEvents profile, validation,
and replay semantics. Services own capture, hydration, materialized state,
checkpoints, time indexes, retries, retention, transport, and publication
policy. Debezium, MongoDB, SQL Server, Flink, Kafka, and any database remain
compatibility evidence or adapters, not core dependencies.

Audit contracts remain independent and unchanged.

[debezium-changes]: https://debezium.io/documentation/reference/3.6/transformations/event-changes.html
[debezium-replica]: https://debezium.io/documentation/reference/3.6/connectors/postgresql.html#postgresql-replica-identity
[flink-changelog-mode]: https://nightlies.apache.org/flink/flink-docs-stable/api/java/org/apache/flink/table/connector/ChangelogMode.html
[flink-row-kind]: https://nightlies.apache.org/flink/flink-docs-stable/api/java/org/apache/flink/types/RowKind.html
[http-patch]: https://www.rfc-editor.org/info/rfc5789/
[json-merge-patch]: https://www.rfc-editor.org/info/rfc7396/
[json-patch]: https://www.rfc-editor.org/info/rfc6902/
[mongodb-update]: https://www.mongodb.com/docs/v8.0/reference/change-events/update/
[sql-server-cdc]: https://learn.microsoft.com/en-us/sql/relational-databases/track-changes/about-change-data-capture-sql-server?view=sql-server-ver17
