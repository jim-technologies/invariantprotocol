# CDC v2 replay fixtures

This directory contains two deterministic protobuf histories for the
`invariant.cdc.v2.ChangeRecord` contract. Both files serialize an
`io.cloudevents.v1.CloudEventBatch`, use the same CloudEvent `source + id`
identities, and describe the same occurrences:

| Index | Position | Operation | Replay effect |
| ---: | --- | --- | --- |
| 0 | `0001` | `SNAPSHOT_READ` | Anchor key `42` with the wide initial record. |
| 1 | `0002` | `UPDATE` | Change `profile.display_name`; make `nickname` explicitly null. |
| 2 | `0002` | `UPDATE` retry | Exact retry of index 1; deduplication leaves state unchanged. |
| 3 | `0003` | `UPDATE` | Change `profile.level`; remove `nickname` again. |
| 4 | `0004` | `DELETE` | Remove key `42`. |
| 5 | `0005` | `CREATE` | Anchor key `84`. |
| 6 | `0006` | `TRUNCATE` | Clear the collection. |
| 7 | `0007` | `SOURCE_MESSAGE` | Preserve a source message without changing row state. |

`full.binpb` repeats complete before/after images. `delta.binpb` uses complete
snapshot/create anchors, sparse exact update patches, and a delete marker. The
initial record deliberately carries a large unchanged decimal, binary data, a
nanosecond timestamp, `uint64`, a list containing explicit null, a map with a
non-string key, and nested records. Lists and maps are atomic patch values;
patch paths traverse records only.

`CloudEventBatch` is only a deterministic conformance-file container. Its
members remain independent CloudEvents, and their array order is not a generic
CloudEvents or transport ordering guarantee. This fixture's replay sequence is
declared by `manifest.json` together with the records' shared
`source_position.stream` and fixture positions.

`manifest.json` records the operation/position sequence, retry index, expected
keys at each opaque position, deterministic file sizes, and SHA-256 digests.
Regenerate all machine-owned files from the repository root:

```bash
go run ./scripts/generate_cdc_v2_fixtures.go
```

These are semantic replay fixtures. They do not prescribe a transport, state
store, or checkpoint implementation, and they do not make protobuf byte order
part of the CDC contract.
