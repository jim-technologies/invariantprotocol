import { readFileSync, statSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { create, fromBinary, toBinary } from "@bufbuild/protobuf";
import { TimestampSchema } from "@bufbuild/protobuf/wkt";
import { describe, expect, test } from "vitest";

import { cdcV2, type CloudEvent, CloudEventBatchSchema, CloudEventSchema } from "../src/index.js";

const EVENT_TYPE = "io.invariantprotocol.cdc.v2.change";
const CHANGE_RECORD_TYPE_URL = "type.googleapis.com/invariant.cdc.v2.ChangeRecord";
const here = dirname(fileURLToPath(import.meta.url));
const fixtureRoot = resolve(here, "../../testdata/cdc/v2");
const fullFixture = resolve(fixtureRoot, "full.binpb");
const deltaFixture = resolve(fixtureRoot, "delta.binpb");
const MIN_TIMESTAMP_SECONDS = -62_135_596_800n;
const MAX_TIMESTAMP_SECONDS = 253_402_300_799n;

type HistoryItem = { event: CloudEvent; record: cdcV2.ChangeRecord };
type ReplayFrontier = { source: string; position: cdcV2.SourcePosition };
type Materialized = {
  source: string;
  collection: string;
  key: string;
  record: cdcV2.Record;
};
type State = Map<string, Materialized>;

class ReplayError extends Error {}

function timestamp(seconds: bigint, nanos = 0) {
  return create(TimestampSchema, { seconds, nanos });
}

function cloneRecord(record: cdcV2.Record): cdcV2.Record {
  return fromBinary(cdcV2.RecordSchema, toBinary(cdcV2.RecordSchema, record));
}

function cloneValue(value: cdcV2.Value): cdcV2.Value {
  return fromBinary(cdcV2.ValueSchema, toBinary(cdcV2.ValueSchema, value));
}

function cloneChangeRecord(record: cdcV2.ChangeRecord): cdcV2.ChangeRecord {
  return fromBinary(cdcV2.ChangeRecordSchema, toBinary(cdcV2.ChangeRecordSchema, record));
}

function cloneState(state: State): State {
  return new Map([...state].map(([key, entry]) => [key, { ...entry, record: cloneRecord(entry.record) }]));
}

function bytesSemantic(value: Uint8Array): string {
  return Buffer.from(value).toString("hex");
}

function compareStrings(left: string, right: string): number {
  return left < right ? -1 : left > right ? 1 : 0;
}

function validateTimestamp(value: { seconds: bigint; nanos: number }, name: string): void {
  if (
    value.seconds < MIN_TIMESTAMP_SECONDS ||
    value.seconds > MAX_TIMESTAMP_SECONDS ||
    !Number.isInteger(value.nanos) ||
    value.nanos < 0 ||
    value.nanos > 999_999_999
  ) {
    throw new ReplayError(`${name} is not a valid google.protobuf.Timestamp`);
  }
}

function numberSemantic(value: number): string {
  if (Number.isNaN(value)) return "NaN";
  if (value === Number.POSITIVE_INFINITY) return "Infinity";
  if (value === Number.NEGATIVE_INFINITY) return "-Infinity";
  if (Object.is(value, -0)) return "-0";
  return String(value);
}

function recordFields(record: cdcV2.Record): Map<string, cdcV2.Value> {
  const result = new Map<string, cdcV2.Value>();
  for (const field of record.fields) {
    if (result.has(field.name)) throw new ReplayError(`duplicate record field ${field.name}`);
    if (field.value === undefined || field.value.kind.case === undefined) {
      throw new ReplayError(`record field ${field.name} has no value`);
    }
    result.set(field.name, field.value);
  }
  return result;
}

function recordSemantic(record: cdcV2.Record): string {
  return JSON.stringify(
    [...recordFields(record)]
      .map(([name, value]): [string, string] => [name, valueSemantic(value)])
      .sort((left, right) => compareStrings(left[0], right[0])),
  );
}

function valueSemantic(value: cdcV2.Value): string {
  const kind = value.kind.case;
  let payload: unknown;
  switch (kind) {
    case "nullValue":
      payload = null;
      break;
    case "boolValue":
    case "int32Value":
    case "uint32Value":
    case "stringValue":
      payload = value.kind.value;
      break;
    case "int64Value":
    case "uint64Value":
      payload = value.kind.value.toString();
      break;
    case "float32Value":
    case "float64Value":
      payload = numberSemantic(value.kind.value);
      break;
    case "bytesValue":
      payload = bytesSemantic(value.kind.value);
      break;
    case "decimalValue":
      payload = [value.kind.value.value, value.kind.value.scale, value.kind.value.precision ?? null];
      break;
    case "timestampValue":
      validateTimestamp(value.kind.value, "timestamp_value");
      payload = [value.kind.value.seconds.toString(), value.kind.value.nanos];
      break;
    case "recordValue":
      payload = recordSemantic(value.kind.value);
      break;
    case "listValue":
      payload = value.kind.value.values.map(valueSemantic);
      break;
    case "mapValue": {
      const keys = new Set<string>();
      payload = value.kind.value.entries
        .map((entry): [string, string] => {
          if (entry.key === undefined || entry.value === undefined) throw new ReplayError("map entry is incomplete");
          const key = valueSemantic(entry.key);
          if (keys.has(key)) throw new ReplayError("duplicate canonical map key");
          keys.add(key);
          return [key, valueSemantic(entry.value)];
        })
        .sort((left, right) => compareStrings(left[0], right[0]));
      break;
    }
    default:
      throw new ReplayError("value kind is required");
  }
  return JSON.stringify([value.typeName, kind, payload]);
}

function fieldStateSemantic(state: cdcV2.FieldState): string {
  switch (state.state.case) {
    case "absent":
      return "absent";
    case "value":
      return `value:${valueSemantic(state.state.value)}`;
    default:
      throw new ReplayError("field state is required");
  }
}

function stateSemantic(state: State): string {
  return JSON.stringify(
    [...state.values()]
      .map((entry): [string, string, string, string] => [
        entry.source,
        entry.collection,
        entry.key,
        recordSemantic(entry.record),
      ])
      .sort((left, right) => compareStrings(JSON.stringify(left), JSON.stringify(right))),
  );
}

function timestampSemantic(value: { seconds: bigint; nanos: number } | undefined): string {
  if (value === undefined) return "missing";
  return `${value.seconds}:${value.nanos}`;
}

function validateShape(record: cdcV2.ChangeRecord): void {
  const representation = record.representation.case;
  const hasMessage = record.sourceMessage !== undefined;
  if (record.operation !== cdcV2.Operation.SOURCE_MESSAGE && record.dataCollection === undefined) {
    throw new ReplayError("data_collection is required");
  }
  if (record.captureTime === undefined) throw new ReplayError("capture_time is required");
  validateTimestamp(record.captureTime, "capture_time");
  if (record.sourceTime !== undefined) validateTimestamp(record.sourceTime, "source_time");
  if (record.key !== undefined) recordSemantic(record.key);
  if (record.sourceMessage !== undefined) valueSemantic(record.sourceMessage);

  switch (record.operation) {
    case cdcV2.Operation.CREATE:
    case cdcV2.Operation.SNAPSHOT_READ:
      if (representation === "full") {
        const full = record.representation.value;
        if (full.before !== undefined || full.after === undefined || full.changedFields !== undefined) {
          throw new ReplayError("CREATE and SNAPSHOT_READ require only full.after");
        }
        recordSemantic(full.after);
      } else if (representation === "delta") {
        if (record.representation.value.change.case !== "result") {
          throw new ReplayError("CREATE and SNAPSHOT_READ require delta.result");
        }
        recordSemantic(record.representation.value.change.value);
      } else {
        throw new ReplayError("CREATE and SNAPSHOT_READ require a representation");
      }
      if (hasMessage) throw new ReplayError("row changes prohibit source_message");
      return;
    case cdcV2.Operation.UPDATE:
      if (representation === "full") {
        if (record.representation.value.after === undefined) {
          throw new ReplayError("UPDATE full representation requires after");
        }
        if (record.representation.value.before !== undefined) recordSemantic(record.representation.value.before);
        recordSemantic(record.representation.value.after);
      } else if (representation === "delta") {
        if (record.representation.value.change.case !== "patch") {
          throw new ReplayError("UPDATE delta representation requires patch");
        }
        validatePatch(record.representation.value.change.value);
      } else {
        throw new ReplayError("UPDATE requires a representation");
      }
      if (hasMessage) throw new ReplayError("row changes prohibit source_message");
      return;
    case cdcV2.Operation.DELETE:
      if (representation === "full") {
        const full = record.representation.value;
        if (full.after !== undefined || full.changedFields !== undefined) {
          throw new ReplayError("DELETE prohibits after and changed_fields");
        }
        if (full.before !== undefined) recordSemantic(full.before);
      } else if (representation === "delta") {
        if (record.representation.value.change.case !== "delete") {
          throw new ReplayError("DELETE delta representation requires delete");
        }
      } else {
        throw new ReplayError("DELETE requires a representation");
      }
      if (hasMessage) throw new ReplayError("row changes prohibit source_message");
      return;
    case cdcV2.Operation.TRUNCATE:
      if (representation !== undefined || record.key !== undefined || hasMessage) {
        throw new ReplayError("TRUNCATE prohibits row data");
      }
      return;
    case cdcV2.Operation.SOURCE_MESSAGE:
      if (representation !== undefined || record.key !== undefined || !hasMessage) {
        throw new ReplayError("SOURCE_MESSAGE requires only source_message");
      }
      return;
    default:
      throw new ReplayError("operation must be specified");
  }
}

function validateEvent(event: CloudEvent): cdcV2.ChangeRecord {
  if (event.source.length === 0 || event.id.length === 0) throw new ReplayError("CloudEvent identity is required");
  if (event.specVersion !== "1.0" || event.type !== EVENT_TYPE) throw new ReplayError("unexpected event contract");
  if (event.data.case !== "protoData" || event.data.value.typeUrl !== CHANGE_RECORD_TYPE_URL) {
    throw new ReplayError("event must contain Any<invariant.cdc.v2.ChangeRecord>");
  }
  if (event.attributes.datacontenttype?.attr.case !== "ceString") {
    throw new ReplayError("datacontenttype must be a string");
  }
  if (event.attributes.datacontenttype.attr.value !== "application/protobuf") {
    throw new ReplayError("unexpected datacontenttype");
  }
  if (
    event.attributes.dataschema?.attr.case !== "ceUri" ||
    event.attributes.dataschema.attr.value !== CHANGE_RECORD_TYPE_URL
  ) {
    throw new ReplayError("unexpected dataschema");
  }

  const record = fromBinary(cdcV2.ChangeRecordSchema, event.data.value.value);
  validateShape(record);
  const expectedTime = record.sourceTime ?? record.captureTime;
  const actualTime = event.attributes.time?.attr;
  if (actualTime?.case !== "ceTimestamp" || timestampSemantic(actualTime.value) !== timestampSemantic(expectedTime)) {
    throw new ReplayError("CloudEvent time must match source_time or capture_time fallback");
  }
  validateTimestamp(actualTime.value, "CloudEvent time");
  return record;
}

function readHistory(path: string): HistoryItem[] {
  const batch = fromBinary(CloudEventBatchSchema, readFileSync(path));
  if (batch.events.length === 0) throw new ReplayError(`empty fixture ${path}`);
  return batch.events.map((event) => ({ event, record: validateEvent(event) }));
}

const missing = Symbol("missing");

function findField(record: cdcV2.Record, name: string): { index: number; field: cdcV2.RecordField } | undefined {
  let found: { index: number; field: cdcV2.RecordField } | undefined;
  record.fields.forEach((field, index) => {
    if (field.name !== name) return;
    if (found !== undefined) throw new ReplayError(`duplicate record field ${name}`);
    found = { index, field };
  });
  return found;
}

function lookup(record: cdcV2.Record, path: readonly string[]): cdcV2.Value | typeof missing {
  if (path.length === 0) throw new ReplayError("empty patch path");
  let current = record;
  for (let index = 0; index < path.length; index += 1) {
    const segment = path[index];
    if (segment === undefined) throw new ReplayError("empty patch segment");
    const found = findField(current, segment);
    if (found === undefined) {
      if (index === path.length - 1) return missing;
      throw new ReplayError(`missing ancestor at ${path.slice(0, index + 1).join(".")}`);
    }
    if (found.field.value === undefined) throw new ReplayError(`field ${segment} has no value`);
    if (index === path.length - 1) return found.field.value;
    if (found.field.value.kind.case !== "recordValue") {
      throw new ReplayError(`non-record ancestor at ${path.slice(0, index + 1).join(".")}`);
    }
    current = found.field.value.kind.value;
  }
  throw new ReplayError("empty patch path");
}

function observedState(value: cdcV2.Value | typeof missing): string {
  return value === missing ? "absent" : `value:${valueSemantic(value)}`;
}

function setPath(record: cdcV2.Record, path: readonly string[], after: cdcV2.FieldState): void {
  if (path.length === 0) throw new ReplayError("empty patch path");
  let parent = record;
  for (let index = 0; index < path.length - 1; index += 1) {
    const segment = path[index];
    if (segment === undefined) throw new ReplayError("empty patch segment");
    const found = findField(parent, segment);
    if (found?.field.value?.kind.case !== "recordValue") {
      throw new ReplayError(`${found === undefined ? "missing" : "non-record"} ancestor at ${segment}`);
    }
    parent = found.field.value.kind.value;
  }

  const leaf = path.at(-1);
  if (leaf === undefined) throw new ReplayError("empty patch path");
  const found = findField(parent, leaf);
  if (after.state.case === "absent") {
    if (found === undefined) throw new ReplayError(`cannot remove absent field ${path.join(".")}`);
    parent.fields.splice(found.index, 1);
    return;
  }
  if (after.state.case !== "value") throw new ReplayError("field state is required");
  if (found === undefined) {
    parent.fields.push(create(cdcV2.RecordFieldSchema, { name: leaf, value: cloneValue(after.state.value) }));
  } else {
    found.field.value = cloneValue(after.state.value);
  }
}

function pathsOverlap(left: readonly string[], right: readonly string[]): boolean {
  const common = Math.min(left.length, right.length);
  return left.slice(0, common).every((segment, index) => segment === right[index]);
}

function validatePatch(patch: cdcV2.RecordPatch): string[][] {
  const paths: string[][] = [];
  for (const change of patch.changes) {
    const path = change.path?.segments ?? [];
    if (path.length === 0) throw new ReplayError("empty patch path");
    if (change.before === undefined || change.after === undefined) {
      throw new ReplayError("patch before and after are required");
    }
    if (fieldStateSemantic(change.before) === fieldStateSemantic(change.after)) {
      throw new ReplayError("patch change must describe a real transition");
    }
    for (const existing of paths) {
      if (path.length === existing.length && pathsOverlap(path, existing)) {
        throw new ReplayError("duplicate patch path");
      }
      if (pathsOverlap(path, existing)) throw new ReplayError("overlapping patch paths");
    }
    paths.push([...path]);
  }
  return paths;
}

function applyPatch(base: cdcV2.Record, patch: cdcV2.RecordPatch): cdcV2.Record {
  recordSemantic(base);
  const paths = validatePatch(patch);

  patch.changes.forEach((change, index) => {
    const path = paths[index];
    if (path === undefined || change.before === undefined) throw new ReplayError("invalid patch");
    if (observedState(lookup(base, path)) !== fieldStateSemantic(change.before)) {
      throw new ReplayError(`before mismatch at ${path.join(".")}`);
    }
  });

  const result = cloneRecord(base);
  patch.changes.forEach((change, index) => {
    const path = paths[index];
    if (path === undefined || change.after === undefined) throw new ReplayError("invalid patch");
    setPath(result, path, change.after);
  });
  return result;
}

function stateKey(event: CloudEvent, record: cdcV2.ChangeRecord): string {
  if (record.key === undefined) throw new ReplayError("keyless record cannot be replayed as keyed state");
  if (record.dataCollection === undefined) throw new ReplayError("data_collection is required");
  return JSON.stringify([event.source, record.dataCollection.id, recordSemantic(record.key)]);
}

function baseFor(state: State, event: CloudEvent, record: cdcV2.ChangeRecord): cdcV2.Record | undefined {
  if (record.key === undefined || record.dataCollection === undefined) return undefined;
  return state.get(stateKey(event, record))?.record;
}

function applyEvent(state: State, event: CloudEvent, record: cdcV2.ChangeRecord): void {
  if (record.operation === cdcV2.Operation.SOURCE_MESSAGE) return;
  if (record.operation === cdcV2.Operation.TRUNCATE) {
    if (record.dataCollection === undefined) throw new ReplayError("data_collection is required");
    for (const [key, entry] of state) {
      if (entry.source === event.source && entry.collection === record.dataCollection.id) state.delete(key);
    }
    return;
  }

  const key = stateKey(event, record);
  if (record.operation === cdcV2.Operation.CREATE || record.operation === cdcV2.Operation.SNAPSHOT_READ) {
    if (record.operation === cdcV2.Operation.CREATE && state.has(key)) {
      throw new ReplayError("CREATE base already exists");
    }
    let result: cdcV2.Record | undefined;
    if (record.representation.case === "full") result = record.representation.value.after;
    if (record.representation.case === "delta" && record.representation.value.change.case === "result") {
      result = record.representation.value.change.value;
    }
    if (result === undefined || record.dataCollection === undefined) throw new ReplayError("anchor result is required");
    state.set(key, {
      source: event.source,
      collection: record.dataCollection.id,
      key: recordSemantic(record.key as cdcV2.Record),
      record: cloneRecord(result),
    });
    return;
  }

  const materialized = state.get(key);
  if (record.operation === cdcV2.Operation.UPDATE) {
    if (record.representation.case === "full") {
      const full = record.representation.value;
      if (
        materialized !== undefined &&
        full.before !== undefined &&
        recordSemantic(full.before) !== recordSemantic(materialized.record)
      ) {
        throw new ReplayError("full before mismatch");
      }
      if (full.after === undefined) throw new ReplayError("full after is required");
      if (record.dataCollection === undefined) throw new ReplayError("data_collection is required");
      state.set(key, {
        source: event.source,
        collection: record.dataCollection.id,
        key: recordSemantic(record.key as cdcV2.Record),
        record: cloneRecord(full.after),
      });
    } else if (record.representation.case === "delta" && record.representation.value.change.case === "patch") {
      if (materialized === undefined) throw new ReplayError("row base is missing");
      materialized.record = applyPatch(materialized.record, record.representation.value.change.value);
    } else {
      throw new ReplayError("UPDATE representation is invalid");
    }
    return;
  }
  if (record.operation === cdcV2.Operation.DELETE) {
    if (
      materialized !== undefined &&
      record.representation.case === "full" &&
      record.representation.value.before !== undefined &&
      recordSemantic(record.representation.value.before) !== recordSemantic(materialized.record)
    ) {
      throw new ReplayError("full before mismatch");
    }
    state.delete(key);
    return;
  }
  throw new ReplayError("unsupported operation");
}

function sourcePositionSemantic(source: string, position: cdcV2.SourcePosition | undefined): string {
  if (position === undefined) return "missing";
  return JSON.stringify([source, position.stream, position.format, bytesSemantic(position.value)]);
}

function replay(history: readonly HistoryItem[], stopFrontier?: ReplayFrontier) {
  const state: State = new Map();
  const seen = new Set<string>();
  const snapshots: { identity: string; state: State }[] = [];
  let found = stopFrontier === undefined;
  for (const { event, record } of history) {
    const identity = JSON.stringify([event.source, event.id]);
    if (seen.has(identity)) continue;
    seen.add(identity);
    applyEvent(state, event, record);
    snapshots.push({ identity, state: cloneState(state) });
    if (
      stopFrontier !== undefined &&
      sourcePositionSemantic(event.source, record.sourcePosition) ===
        sourcePositionSemantic(stopFrontier.source, stopFrontier.position)
    ) {
      found = true;
      break;
    }
  }
  if (!found) throw new ReplayError("source position was not found");
  return { state, snapshots };
}

function* walkValue(value: cdcV2.Value): Generator<cdcV2.Value> {
  yield value;
  if (value.kind.case === "recordValue") {
    for (const field of value.kind.value.fields) if (field.value !== undefined) yield* walkValue(field.value);
  } else if (value.kind.case === "listValue") {
    for (const item of value.kind.value.values) yield* walkValue(item);
  } else if (value.kind.case === "mapValue") {
    for (const entry of value.kind.value.entries) {
      if (entry.key !== undefined) yield* walkValue(entry.key);
      if (entry.value !== undefined) yield* walkValue(entry.value);
    }
  }
}

function* historyValues(history: readonly HistoryItem[]): Generator<cdcV2.Value> {
  for (const { record } of history) {
    const images: cdcV2.Record[] = [];
    if (record.key !== undefined) images.push(record.key);
    if (record.representation.case === "full") {
      if (record.representation.value.before !== undefined) images.push(record.representation.value.before);
      if (record.representation.value.after !== undefined) images.push(record.representation.value.after);
    } else if (record.representation.case === "delta") {
      if (record.representation.value.change.case === "result") {
        images.push(record.representation.value.change.value);
      } else if (record.representation.value.change.case === "patch") {
        for (const change of record.representation.value.change.value.changes) {
          if (change.before?.state.case === "value") yield* walkValue(change.before.state.value);
          if (change.after?.state.case === "value") yield* walkValue(change.after.state.value);
        }
      }
    }
    if (record.sourceMessage !== undefined) yield* walkValue(record.sourceMessage);
    for (const image of images) {
      for (const field of image.fields) if (field.value !== undefined) yield* walkValue(field.value);
    }
  }
}

function stringValue(text: string): cdcV2.Value {
  return create(cdcV2.ValueSchema, { kind: { case: "stringValue", value: text } });
}

function nullValue(): cdcV2.Value {
  return create(cdcV2.ValueSchema, {
    kind: { case: "nullValue", value: create(cdcV2.NullValueSchema) },
  });
}

function makeRecord(fields: Record<string, cdcV2.Value>): cdcV2.Record {
  return create(cdcV2.RecordSchema, {
    fields: Object.entries(fields).map(([name, value]) => create(cdcV2.RecordFieldSchema, { name, value })),
  });
}

function present(value: cdcV2.Value): cdcV2.FieldState {
  return create(cdcV2.FieldStateSchema, { state: { case: "value", value: cloneValue(value) } });
}

function absent(): cdcV2.FieldState {
  return create(cdcV2.FieldStateSchema, {
    state: { case: "absent", value: create(cdcV2.AbsentSchema) },
  });
}

function fieldChange(path: string[], before: cdcV2.FieldState, after: cdcV2.FieldState): cdcV2.FieldChange {
  return create(cdcV2.FieldChangeSchema, {
    path: create(cdcV2.FieldPathSchema, { segments: path }),
    before,
    after,
  });
}

function patch(...changes: cdcV2.FieldChange[]): cdcV2.RecordPatch {
  return create(cdcV2.RecordPatchSchema, { changes });
}

function deltaPatch(...changes: cdcV2.FieldChange[]): cdcV2.DeltaChange {
  return create(cdcV2.DeltaChangeSchema, {
    change: { case: "patch", value: patch(...changes) },
  });
}

function minimalRecord(operation: cdcV2.Operation, delta?: cdcV2.DeltaChange): cdcV2.ChangeRecord {
  return create(cdcV2.ChangeRecordSchema, {
    operation,
    key: makeRecord({ id: stringValue("1") }),
    dataCollection: create(cdcV2.DataCollectionSchema, { id: "inventory.records" }),
    captureTime: timestamp(1n),
    representation: delta === undefined ? undefined : { case: "delta", value: delta },
  });
}

function minimalFullRecord(
  operation: cdcV2.Operation,
  before?: cdcV2.Record,
  after?: cdcV2.Record,
): cdcV2.ChangeRecord {
  return create(cdcV2.ChangeRecordSchema, {
    operation,
    key: makeRecord({ id: stringValue("1") }),
    dataCollection: create(cdcV2.DataCollectionSchema, { id: "inventory.records" }),
    captureTime: timestamp(1n),
    representation: {
      case: "full",
      value: create(cdcV2.FullChangeSchema, { before, after }),
    },
  });
}

function sourceMessageRecord(value: cdcV2.Value): cdcV2.ChangeRecord {
  return create(cdcV2.ChangeRecordSchema, {
    operation: cdcV2.Operation.SOURCE_MESSAGE,
    captureTime: timestamp(1n),
    sourceMessage: value,
  });
}

function stateForValue(value: cdcV2.Value | undefined): cdcV2.FieldState {
  return value === undefined ? absent() : present(value);
}

function diffRecords(before: cdcV2.Record, after: cdcV2.Record, prefix: string[] = []): cdcV2.RecordPatch {
  const beforeFields = recordFields(before);
  const afterFields = recordFields(after);
  const changes: cdcV2.FieldChange[] = [];
  const names = [...new Set([...beforeFields.keys(), ...afterFields.keys()])].sort();
  for (const name of names) {
    const oldValue = beforeFields.get(name);
    const newValue = afterFields.get(name);
    if (oldValue !== undefined && newValue !== undefined && valueSemantic(oldValue) === valueSemantic(newValue)) {
      continue;
    }
    if (
      oldValue?.kind.case === "recordValue" &&
      newValue?.kind.case === "recordValue" &&
      oldValue.typeName === newValue.typeName
    ) {
      changes.push(...diffRecords(oldValue.kind.value, newValue.kind.value, [...prefix, name]).changes);
      continue;
    }
    changes.push(fieldChange([...prefix, name], stateForValue(oldValue), stateForValue(newValue)));
  }
  return patch(...changes);
}

function fullToDelta(record: cdcV2.ChangeRecord, base?: cdcV2.Record): cdcV2.ChangeRecord {
  const converted = cloneChangeRecord(record);
  if (converted.operation === cdcV2.Operation.TRUNCATE || converted.operation === cdcV2.Operation.SOURCE_MESSAGE) {
    return converted;
  }
  if (converted.representation.case !== "full") throw new ReplayError("FullChange is required");
  const full = converted.representation.value;
  let delta: cdcV2.DeltaChange;
  if (converted.operation === cdcV2.Operation.CREATE || converted.operation === cdcV2.Operation.SNAPSHOT_READ) {
    if (full.after === undefined) throw new ReplayError("full after is required");
    delta = create(cdcV2.DeltaChangeSchema, {
      change: { case: "result", value: cloneRecord(full.after) },
    });
  } else if (converted.operation === cdcV2.Operation.UPDATE) {
    const prior = base ?? full.before;
    if (prior === undefined || full.after === undefined) throw new ReplayError("full-to-delta base is missing");
    if (base !== undefined && full.before !== undefined && recordSemantic(base) !== recordSemantic(full.before)) {
      throw new ReplayError("full before mismatch");
    }
    delta = create(cdcV2.DeltaChangeSchema, {
      change: { case: "patch", value: diffRecords(prior, full.after) },
    });
  } else if (converted.operation === cdcV2.Operation.DELETE) {
    if (base !== undefined && full.before !== undefined && recordSemantic(base) !== recordSemantic(full.before)) {
      throw new ReplayError("full before mismatch");
    }
    delta = create(cdcV2.DeltaChangeSchema, {
      change: { case: "delete", value: create(cdcV2.DeleteDeltaSchema) },
    });
  } else {
    throw new ReplayError("unsupported operation");
  }
  converted.representation = { case: "delta", value: delta };
  return converted;
}

function deltaToFull(record: cdcV2.ChangeRecord, base?: cdcV2.Record): cdcV2.ChangeRecord {
  const converted = cloneChangeRecord(record);
  if (converted.operation === cdcV2.Operation.TRUNCATE || converted.operation === cdcV2.Operation.SOURCE_MESSAGE) {
    return converted;
  }
  if (converted.representation.case !== "delta") throw new ReplayError("DeltaChange is required");
  const change = converted.representation.value.change;
  let full: cdcV2.FullChange;
  if (
    (converted.operation === cdcV2.Operation.CREATE || converted.operation === cdcV2.Operation.SNAPSHOT_READ) &&
    change.case === "result"
  ) {
    full = create(cdcV2.FullChangeSchema, { after: cloneRecord(change.value) });
  } else if (converted.operation === cdcV2.Operation.UPDATE && change.case === "patch") {
    if (base === undefined) throw new ReplayError("delta-to-full base is missing");
    full = create(cdcV2.FullChangeSchema, {
      before: cloneRecord(base),
      after: applyPatch(base, change.value),
      changedFields: create(cdcV2.ChangedFieldMaskSchema, {
        paths: change.value.changes.map((item) =>
          create(cdcV2.FieldPathSchema, { segments: [...(item.path?.segments ?? [])] }),
        ),
      }),
    });
  } else if (converted.operation === cdcV2.Operation.DELETE && change.case === "delete") {
    full = create(cdcV2.FullChangeSchema, { before: base === undefined ? undefined : cloneRecord(base) });
  } else {
    throw new ReplayError("invalid delta operation");
  }
  converted.representation = { case: "full", value: full };
  return converted;
}

function representationSemantic(record: cdcV2.ChangeRecord): string {
  if (record.representation.case === undefined) return "none";
  if (record.representation.case === "full") {
    const full = record.representation.value;
    return JSON.stringify([
      "full",
      full.before ? recordSemantic(full.before) : null,
      full.after ? recordSemantic(full.after) : null,
      full.changedFields?.paths.map((path) => path.segments).sort() ?? null,
    ]);
  }
  const change = record.representation.value.change;
  if (change.case === "result") return JSON.stringify(["result", recordSemantic(change.value)]);
  if (change.case === "delete") return "delete";
  if (change.case === "patch") {
    return JSON.stringify([
      "patch",
      change.value.changes
        .map((item) => [
          item.path?.segments ?? [],
          item.before ? fieldStateSemantic(item.before) : "missing",
          item.after ? fieldStateSemantic(item.after) : "missing",
        ])
        .sort((left, right) => compareStrings(JSON.stringify(left[0]), JSON.stringify(right[0]))),
    ]);
  }
  throw new ReplayError("delta change is required");
}

function recordIds(state: State): bigint[] {
  return [...state.values()]
    .map((entry) => {
      const id = recordFields(entry.record).get("id");
      if (id?.kind.case !== "int64Value") throw new ReplayError("fixture id must be int64");
      return id.kind.value;
    })
    .sort((left, right) => (left < right ? -1 : left > right ? 1 : 0));
}

describe("CDC v2 full and delta conformance", () => {
  test("decodes canonical CloudEventBatch histories and replays both representations identically", () => {
    const full = readHistory(fullFixture);
    const delta = readHistory(deltaFixture);
    const fullReplay = replay(full);
    const deltaReplay = replay(delta);

    expect(full.map(({ event }) => [event.source, event.id])).toEqual(
      delta.map(({ event }) => [event.source, event.id]),
    );
    expect(fullReplay.snapshots.map(({ identity }) => identity)).toEqual(
      deltaReplay.snapshots.map(({ identity }) => identity),
    );
    expect(fullReplay.snapshots).toHaveLength(7);
    expect(full).toHaveLength(8);
    expect(toBinary(CloudEventSchema, full[1]?.event as CloudEvent)).toEqual(
      toBinary(CloudEventSchema, full[2]?.event as CloudEvent),
    );
    expect(toBinary(CloudEventSchema, delta[1]?.event as CloudEvent)).toEqual(
      toBinary(CloudEventSchema, delta[2]?.event as CloudEvent),
    );
    fullReplay.snapshots.forEach((snapshot, index) => {
      expect(stateSemantic(snapshot.state)).toBe(stateSemantic(deltaReplay.snapshots[index]?.state ?? new Map()));
    });
    expect(stateSemantic(fullReplay.state)).toBe(stateSemantic(deltaReplay.state));

    const operations = new Set(full.map(({ record }) => record.operation));
    expect(operations).toEqual(
      new Set([
        cdcV2.Operation.CREATE,
        cdcV2.Operation.UPDATE,
        cdcV2.Operation.DELETE,
        cdcV2.Operation.SNAPSHOT_READ,
        cdcV2.Operation.TRUNCATE,
        cdcV2.Operation.SOURCE_MESSAGE,
      ]),
    );
    expect(full[0]?.event.attributes.correlationid?.attr).toEqual({
      case: "ceString",
      value: "fixture-replay-history",
    });
    expect(full[1]?.record.transaction).toMatchObject({
      id: "fixture-transaction",
      totalOrder: 2n,
      dataCollectionOrder: 2n,
    });
  });

  test("preserves rich exact values and distinct absent/null transitions", () => {
    const full = readHistory(fullFixture);
    const delta = readHistory(deltaFixture);
    const values = [...historyValues(full)];
    const kinds = new Set(values.map((value) => value.kind.case));
    for (const kind of [
      "nullValue",
      "uint64Value",
      "bytesValue",
      "decimalValue",
      "timestampValue",
      "recordValue",
      "listValue",
      "mapValue",
    ]) {
      expect(kinds).toContain(kind);
    }
    expect(values.filter((value) => value.kind.case === "uint64Value").map((value) => value.kind.value)).toContain(
      18_446_744_073_709_551_615n,
    );
    const decimal = values.find((value) => value.kind.case === "decimalValue");
    expect(decimal?.kind).toMatchObject({
      case: "decimalValue",
      value: { value: "12345678901234567890.123400", scale: 6, precision: 38 },
    });
    const binary = values.find((value) => value.typeName === "example.Binary");
    expect(binary?.kind.case).toBe("bytesValue");
    if (binary?.kind.case !== "bytesValue") throw new Error("binary fixture value is missing");
    expect([...binary.kind.value]).toEqual([0x00, 0x7f, 0x80, 0xff]);
    const temporal = values.find((value) => value.typeName === "example.NanosecondInstant");
    expect(temporal?.kind).toEqual({
      case: "timestampValue",
      value: timestamp(1_723_912_200n, 987_654_321),
    });

    const snapshot = delta[0]?.record;
    if (snapshot?.representation.case !== "delta" || snapshot.representation.value.change.case !== "result") {
      throw new Error("fixture snapshot must be a delta result");
    }
    const initial = snapshot.representation.value.change.value;
    const tags = recordFields(initial).get("tags");
    expect(tags?.kind.case).toBe("listValue");
    if (tags?.kind.case !== "listValue") throw new Error("tags must be a list");
    expect(tags.kind.value.values.map((value) => value.kind.case)).toEqual(["stringValue", "nullValue", "stringValue"]);
    const attributes = recordFields(initial).get("attributes");
    if (attributes?.kind.case !== "mapValue") throw new Error("attributes must be a map");
    expect(attributes.kind.value.entries[1]?.key?.kind).toEqual({ case: "int32Value", value: 7 });

    const transitions = delta.flatMap(({ record }) => {
      if (record.representation.case !== "delta" || record.representation.value.change.case !== "patch") return [];
      return record.representation.value.change.value.changes;
    });
    expect(
      transitions.map((change) => [
        change.before ? change.before.state.case : undefined,
        change.after ? change.after.state.case : undefined,
      ]),
    ).toEqual(
      expect.arrayContaining([
        ["absent", "value"],
        ["value", "absent"],
      ]),
    );
    expect(
      transitions.some(
        (change) => change.after?.state.case === "value" && change.after.state.value.kind.case === "nullValue",
      ),
    ).toBe(true);
    expect(delta[0]?.record.sourceExtension?.representation.case).toBe("opaqueData");
  });

  test("defines canonical Value equality across the exact scalar and composite domain", () => {
    const scalarValues = [
      create(cdcV2.ValueSchema, { kind: { case: "boolValue", value: true } }),
      create(cdcV2.ValueSchema, { kind: { case: "int32Value", value: -2_147_483_648 } }),
      create(cdcV2.ValueSchema, { kind: { case: "int64Value", value: -9_223_372_036_854_775_808n } }),
      create(cdcV2.ValueSchema, { kind: { case: "uint32Value", value: 4_294_967_295 } }),
      create(cdcV2.ValueSchema, { kind: { case: "uint64Value", value: 18_446_744_073_709_551_615n } }),
      ...[Number.NaN, Number.POSITIVE_INFINITY, Number.NEGATIVE_INFINITY, 0, -0].map((value) =>
        create(cdcV2.ValueSchema, { kind: { case: "float32Value", value } }),
      ),
      ...[Number.NaN, Number.POSITIVE_INFINITY, Number.NEGATIVE_INFINITY, 0, -0].map((value) =>
        create(cdcV2.ValueSchema, { kind: { case: "float64Value", value } }),
      ),
      create(cdcV2.ValueSchema, {
        kind: {
          case: "decimalValue",
          value: create(cdcV2.DecimalValueSchema, { value: "123", scale: -2 }),
        },
      }),
      create(cdcV2.ValueSchema, {
        kind: {
          case: "decimalValue",
          value: create(cdcV2.DecimalValueSchema, { value: "123", scale: -2, precision: 9 }),
        },
      }),
    ];
    for (const value of scalarValues) {
      const decoded = fromBinary(cdcV2.ValueSchema, toBinary(cdcV2.ValueSchema, value));
      expect(valueSemantic(decoded)).toBe(valueSemantic(value));
    }

    const recordOne = create(cdcV2.ValueSchema, {
      kind: { case: "recordValue", value: makeRecord({ alpha: stringValue("a"), beta: stringValue("b") }) },
    });
    const recordTwo = create(cdcV2.ValueSchema, {
      kind: { case: "recordValue", value: makeRecord({ beta: stringValue("b"), alpha: stringValue("a") }) },
    });
    expect(valueSemantic(recordOne)).toBe(valueSemantic(recordTwo));

    const mapOne = create(cdcV2.ValueSchema, {
      kind: {
        case: "mapValue",
        value: create(cdcV2.MapValueSchema, {
          entries: [
            create(cdcV2.MapEntrySchema, { key: stringValue("alpha"), value: stringValue("a") }),
            create(cdcV2.MapEntrySchema, { key: stringValue("beta"), value: stringValue("b") }),
          ],
        }),
      },
    });
    const mapTwo = create(cdcV2.ValueSchema, {
      kind: {
        case: "mapValue",
        value: create(cdcV2.MapValueSchema, {
          entries: [
            create(cdcV2.MapEntrySchema, { key: stringValue("beta"), value: stringValue("b") }),
            create(cdcV2.MapEntrySchema, { key: stringValue("alpha"), value: stringValue("a") }),
          ],
        }),
      },
    });
    expect(valueSemantic(mapOne)).toBe(valueSemantic(mapTwo));
    const duplicateMap = cloneValue(mapOne);
    if (duplicateMap.kind.case !== "mapValue") throw new Error("map clone lost its kind");
    duplicateMap.kind.value.entries.push(
      create(cdcV2.MapEntrySchema, { key: stringValue("alpha"), value: stringValue("other") }),
    );
    expect(() => valueSemantic(duplicateMap)).toThrow(/duplicate canonical map key/);

    const listOne = create(cdcV2.ValueSchema, {
      kind: {
        case: "listValue",
        value: create(cdcV2.ListValueSchema, { values: [stringValue("a"), stringValue("b")] }),
      },
    });
    const listTwo = create(cdcV2.ValueSchema, {
      kind: {
        case: "listValue",
        value: create(cdcV2.ListValueSchema, { values: [stringValue("b"), stringValue("a")] }),
      },
    });
    expect(valueSemantic(listOne)).not.toBe(valueSemantic(listTwo));

    const float32NaNOne = fromBinary(cdcV2.ValueSchema, Uint8Array.of(0x45, 0x01, 0x00, 0xc0, 0x7f));
    const float32NaNTwo = fromBinary(cdcV2.ValueSchema, Uint8Array.of(0x45, 0x01, 0x00, 0x80, 0x7f));
    const float64NaNOne = fromBinary(
      cdcV2.ValueSchema,
      Uint8Array.of(0x49, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0xf8, 0x7f),
    );
    const float64NaNTwo = fromBinary(
      cdcV2.ValueSchema,
      Uint8Array.of(0x49, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0xf0, 0x7f),
    );
    expect(valueSemantic(float32NaNOne)).toBe(valueSemantic(float32NaNTwo));
    expect(valueSemantic(float64NaNOne)).toBe(valueSemantic(float64NaNTwo));
    expect(valueSemantic(float32NaNOne)).not.toBe(valueSemantic(float64NaNOne));

    const positiveZero = create(cdcV2.ValueSchema, { kind: { case: "float64Value", value: 0 } });
    const negativeZero = create(cdcV2.ValueSchema, { kind: { case: "float64Value", value: -0 } });
    expect(valueSemantic(positiveZero)).not.toBe(valueSemantic(negativeZero));
    expect(valueSemantic(stringValue("same"))).not.toBe(
      valueSemantic(
        create(cdcV2.ValueSchema, { typeName: "example.String", kind: { case: "stringValue", value: "same" } }),
      ),
    );
    expect(valueSemantic(scalarValues.at(-2) as cdcV2.Value)).not.toBe(
      valueSemantic(scalarValues.at(-1) as cdcV2.Value),
    );

    const known = stringValue("known");
    const knownWithUnknown = fromBinary(
      cdcV2.ValueSchema,
      Uint8Array.from([...toBinary(cdcV2.ValueSchema, known), 0xa0, 0x06, 0x01]),
    );
    expect(valueSemantic(knownWithUnknown)).toBe(valueSemantic(known));
    const unknownVariant = fromBinary(cdcV2.ValueSchema, Uint8Array.of(0xa2, 0x06, 0x00));
    expect(() => valueSemantic(unknownVariant)).toThrow(/kind is required/);
  });

  test("rejects malformed core Values while preserving unknown isolated extension variants", () => {
    const duplicateRecord = create(cdcV2.RecordSchema, {
      fields: [
        create(cdcV2.RecordFieldSchema, { name: "duplicate", value: stringValue("one") }),
        create(cdcV2.RecordFieldSchema, { name: "duplicate", value: stringValue("two") }),
      ],
    });
    expect(() =>
      validateShape(
        sourceMessageRecord(create(cdcV2.ValueSchema, { kind: { case: "recordValue", value: duplicateRecord } })),
      ),
    ).toThrow(/duplicate record field/);

    const incompleteMap = create(cdcV2.ValueSchema, {
      kind: {
        case: "mapValue",
        value: create(cdcV2.MapValueSchema, {
          entries: [create(cdcV2.MapEntrySchema, { key: stringValue("key") })],
        }),
      },
    });
    expect(() => validateShape(sourceMessageRecord(incompleteMap))).toThrow(/map entry is incomplete/);

    const duplicateMap = create(cdcV2.ValueSchema, {
      kind: {
        case: "mapValue",
        value: create(cdcV2.MapValueSchema, {
          entries: [
            create(cdcV2.MapEntrySchema, { key: stringValue("key"), value: stringValue("one") }),
            create(cdcV2.MapEntrySchema, { key: stringValue("key"), value: stringValue("two") }),
          ],
        }),
      },
    });
    expect(() => validateShape(sourceMessageRecord(duplicateMap))).toThrow(/duplicate canonical map key/);

    const invalidTimestamp = create(cdcV2.ValueSchema, {
      kind: { case: "timestampValue", value: timestamp(1n, 1_000_000_000) },
    });
    expect(() => validateShape(sourceMessageRecord(invalidTimestamp))).toThrow(/not a valid/);
    const invalidCaptureTime = sourceMessageRecord(stringValue("message"));
    invalidCaptureTime.captureTime = timestamp(MAX_TIMESTAMP_SECONDS + 1n);
    expect(() => validateShape(invalidCaptureTime)).toThrow(/capture_time/);

    const unknownDelta = fromBinary(cdcV2.DeltaChangeSchema, Uint8Array.of(0xa2, 0x06, 0x00));
    expect(() => validateShape(minimalRecord(cdcV2.Operation.UPDATE, unknownDelta))).toThrow(/requires patch/);
    const unknownFieldState = fromBinary(cdcV2.FieldStateSchema, Uint8Array.of(0xa2, 0x06, 0x00));
    expect(() =>
      validateShape(
        minimalRecord(
          cdcV2.Operation.UPDATE,
          deltaPatch(fieldChange(["name"], unknownFieldState, present(stringValue("after")))),
        ),
      ),
    ).toThrow(/field state is required/);
    const unknownExtensionWire = Uint8Array.of(0xa2, 0x06, 0x00);
    const unknownSourceExtension = sourceMessageRecord(stringValue("message"));
    unknownSourceExtension.sourceExtension = fromBinary(cdcV2.SourceExtensionSchema, unknownExtensionWire);
    expect(() => validateShape(unknownSourceExtension)).not.toThrow();
    expect(toBinary(cdcV2.SourceExtensionSchema, unknownSourceExtension.sourceExtension)).toEqual(unknownExtensionWire);
  });

  test("reconstructs state at opaque positions and keeps the delta fixture smaller", () => {
    const full = readHistory(fullFixture);
    const delta = readHistory(deltaFixture);
    const manifest = JSON.parse(readFileSync(resolve(fixtureRoot, "manifest.json"), "utf8")) as {
      operations: string[];
      positions: string[];
      retry_indexes: number[];
      state_at_position: Record<string, number[]>;
    };
    // CloudEventBatch itself promises no order. These files are replay histories only
    // because the conformance manifest explicitly declares their sequence and retry.
    expect(full.map(({ record }) => cdcV2.Operation[record.operation])).toEqual(manifest.operations);
    expect(
      full.map(({ record }) => new TextDecoder().decode(record.sourcePosition?.value ?? new Uint8Array())),
    ).toEqual(manifest.positions);
    expect(manifest.retry_indexes).toEqual([2]);
    for (const [position, expectedIds] of Object.entries(manifest.state_at_position)) {
      const item = full.find(
        ({ record }) => new TextDecoder().decode(record.sourcePosition?.value ?? new Uint8Array()) === position,
      );
      if (item?.record.sourcePosition === undefined) throw new Error(`fixture position ${position} is missing`);
      const frontier = { source: item.event.source, position: item.record.sourcePosition };
      const fullAtPosition = replay(full, frontier).state;
      const deltaAtPosition = replay(delta, frontier).state;
      expect(stateSemantic(deltaAtPosition)).toBe(stateSemantic(fullAtPosition));
      expect(recordIds(deltaAtPosition)).toEqual(expectedIds.map((id) => BigInt(id)));
    }
    const first = full[0];
    const firstPosition = first?.record.sourcePosition;
    if (first === undefined || firstPosition === undefined) throw new Error("fixture source position is missing");
    expect(() =>
      replay(full, {
        source: first.event.source,
        position: create(cdcV2.SourcePositionSchema, {
          stream: `${firstPosition.stream}:other`,
          format: firstPosition.format,
          value: firstPosition.value,
        }),
      }),
    ).toThrow(/not found/);
    expect(() =>
      replay(full, {
        source: first.event.source,
        position: create(cdcV2.SourcePositionSchema, {
          stream: firstPosition.stream,
          format: `${firstPosition.format};variant=other`,
          value: firstPosition.value,
        }),
      }),
    ).toThrow(/not found/);
    expect(() => replay(full, { source: `${first.event.source}:other`, position: firstPosition })).toThrow(/not found/);

    const collidingEvent = fromBinary(CloudEventSchema, toBinary(CloudEventSchema, first.event));
    collidingEvent.source = `${first.event.source}:other`;
    const collidingHistory = [{ event: collidingEvent, record: cloneChangeRecord(first.record) }, ...full];
    const collisionReplay = replay(collidingHistory, {
      source: first.event.source,
      position: firstPosition,
    });
    expect(collisionReplay.snapshots).toHaveLength(2);
    expect(new Set([...collisionReplay.state.values()].map(({ source }) => source))).toEqual(
      new Set([collidingEvent.source, first.event.source]),
    );
    expect(statSync(deltaFixture).size).toBeLessThan(statSync(fullFixture).size);
  });

  test("derives deltas from full images and materializes equivalent full images", () => {
    const full = readHistory(fullFixture);
    const delta = readHistory(deltaFixture);
    const state: State = new Map();
    const seen = new Set<string>();
    const derivedHistory: HistoryItem[] = [];
    const materializedHistory: HistoryItem[] = [];

    full.forEach((fullItem, index) => {
      const deltaItem = delta[index];
      if (deltaItem === undefined) throw new Error("fixture histories must align");
      const identity = JSON.stringify([fullItem.event.source, fullItem.event.id]);
      if (seen.has(identity)) return;
      seen.add(identity);
      const base = baseFor(state, fullItem.event, fullItem.record);

      const derived = fullToDelta(fullItem.record, base);
      expect(derived.operation).toBe(deltaItem.record.operation);
      expect(representationSemantic(derived)).toBe(representationSemantic(deltaItem.record));
      expect(bytesSemantic(derived.sourcePosition?.value ?? new Uint8Array())).toBe(
        bytesSemantic(deltaItem.record.sourcePosition?.value ?? new Uint8Array()),
      );

      const materialized = deltaToFull(deltaItem.record, base);
      expect(materialized.operation).toBe(fullItem.record.operation);
      expect(representationSemantic(materialized)).toBe(representationSemantic(fullItem.record));
      derivedHistory.push({ event: fullItem.event, record: derived });
      materializedHistory.push({ event: deltaItem.event, record: materialized });
      applyEvent(state, fullItem.event, fullItem.record);
    });

    expect(stateSemantic(replay(derivedHistory).state)).toBe(stateSemantic(replay(full).state));
    expect(stateSemantic(replay(materializedHistory).state)).toBe(stateSemantic(replay(delta).state));
  });

  test("replays outcome events without hidden bases and scopes state to CloudEvent source", () => {
    const state: State = new Map();
    const sourceA = createDummyEvent("urn:test:source:a");
    const sourceB = createDummyEvent("urn:test:source:b");
    const sourceC = createDummyEvent("urn:test:source:c");

    const fullUpdate = minimalFullRecord(cdcV2.Operation.UPDATE, undefined, makeRecord({ name: stringValue("one") }));
    validateShape(fullUpdate);
    applyEvent(state, sourceA, fullUpdate);
    expect([...state.values()].map(({ source }) => source)).toEqual([sourceA.source]);

    const deltaUpdate = minimalRecord(
      cdcV2.Operation.UPDATE,
      deltaPatch(fieldChange(["name"], present(stringValue("one")), present(stringValue("two")))),
    );
    validateShape(deltaUpdate);
    applyEvent(state, sourceA, deltaUpdate);

    const snapshot = minimalRecord(
      cdcV2.Operation.SNAPSHOT_READ,
      create(cdcV2.DeltaChangeSchema, {
        change: { case: "result", value: makeRecord({ name: stringValue("snapshot") }) },
      }),
    );
    validateShape(snapshot);
    applyEvent(state, sourceA, snapshot);
    expect(valueSemantic(lookup(baseFor(state, sourceA, snapshot) as cdcV2.Record, ["name"]) as cdcV2.Value)).toBe(
      valueSemantic(stringValue("snapshot")),
    );

    const duplicateCreate = minimalFullRecord(
      cdcV2.Operation.CREATE,
      undefined,
      makeRecord({ name: stringValue("duplicate") }),
    );
    expect(() => applyEvent(state, sourceA, duplicateCreate)).toThrow(/already exists/);

    const sourceBCreate = minimalFullRecord(
      cdcV2.Operation.CREATE,
      undefined,
      makeRecord({ name: stringValue("source-b") }),
    );
    applyEvent(state, sourceB, sourceBCreate);
    expect(new Set([...state.values()].map(({ source }) => source))).toEqual(new Set([sourceA.source, sourceB.source]));

    const truncate = create(cdcV2.ChangeRecordSchema, {
      operation: cdcV2.Operation.TRUNCATE,
      dataCollection: create(cdcV2.DataCollectionSchema, { id: "inventory.records" }),
      captureTime: timestamp(2n),
    });
    validateShape(truncate);
    applyEvent(state, sourceA, truncate);
    expect([...state.values()].map(({ source }) => source)).toEqual([sourceB.source]);

    const fullDelete = minimalFullRecord(cdcV2.Operation.DELETE);
    validateShape(fullDelete);
    applyEvent(state, sourceB, fullDelete);
    expect(state.size).toBe(0);
    expect(() => applyEvent(state, sourceC, fullDelete)).not.toThrow();

    const deltaDelete = minimalRecord(
      cdcV2.Operation.DELETE,
      create(cdcV2.DeltaChangeSchema, {
        change: { case: "delete", value: create(cdcV2.DeleteDeltaSchema) },
      }),
    );
    expect(() => applyEvent(state, sourceC, deltaDelete)).not.toThrow();
    const projectedFull = deltaToFull(deltaDelete);
    expect(projectedFull.representation.case).toBe("full");
    if (projectedFull.representation.case !== "full") throw new Error("DELETE projection must be full");
    expect(projectedFull.representation.value.before).toBeUndefined();
    expect(representationSemantic(fullToDelta(fullDelete))).toBe(representationSemantic(deltaDelete));

    expect(() => applyEvent(state, sourceC, deltaUpdate)).toThrow(/base is missing/);
  });

  test("treats a nested record type change as one atomic field transition", () => {
    const beforeNested = create(cdcV2.ValueSchema, {
      typeName: "example.ProfileV1",
      kind: { case: "recordValue", value: makeRecord({ name: stringValue("same") }) },
    });
    const afterNested = create(cdcV2.ValueSchema, {
      typeName: "example.ProfileV2",
      kind: { case: "recordValue", value: makeRecord({ name: stringValue("same") }) },
    });
    const before = makeRecord({ profile: beforeNested });
    const full = minimalFullRecord(cdcV2.Operation.UPDATE, before, makeRecord({ profile: afterNested }));
    const delta = fullToDelta(full);
    if (delta.representation.case !== "delta" || delta.representation.value.change.case !== "patch") {
      throw new Error("UPDATE conversion must produce a patch");
    }
    expect(delta.representation.value.change.value.changes).toHaveLength(1);
    expect(delta.representation.value.change.value.changes[0]?.path?.segments).toEqual(["profile"]);
    const materialized = deltaToFull(delta, before);
    if (materialized.representation.case !== "full" || full.representation.case !== "full") {
      throw new Error("UPDATE materialization must produce FullChange");
    }
    expect(recordSemantic(materialized.representation.value.before as cdcV2.Record)).toBe(
      recordSemantic(full.representation.value.before as cdcV2.Record),
    );
    expect(recordSemantic(materialized.representation.value.after as cdcV2.Record)).toBe(
      recordSemantic(full.representation.value.after as cdcV2.Record),
    );
    expect(materialized.representation.value.changedFields?.paths[0]?.segments).toEqual(["profile"]);
  });

  test("applies exact null/absence and atomic list/map field replacements", () => {
    const nullField = applyPatch(
      makeRecord({ id: stringValue("1") }),
      patch(fieldChange(["note"], absent(), present(nullValue()))),
    );
    expect(lookup(nullField, ["note"])).not.toBe(missing);
    const removed = applyPatch(nullField, patch(fieldChange(["note"], present(nullValue()), absent())));
    expect(lookup(removed, ["note"])).toBe(missing);

    const oldList = create(cdcV2.ValueSchema, {
      kind: {
        case: "listValue",
        value: create(cdcV2.ListValueSchema, { values: [stringValue("one"), nullValue()] }),
      },
    });
    const newList = create(cdcV2.ValueSchema, {
      kind: {
        case: "listValue",
        value: create(cdcV2.ListValueSchema, { values: [stringValue("two")] }),
      },
    });
    const replaced = applyPatch(
      makeRecord({ items: oldList }),
      patch(fieldChange(["items"], present(oldList), present(newList))),
    );
    expect(valueSemantic(lookup(replaced, ["items"]) as cdcV2.Value)).toBe(valueSemantic(newList));
    expect(() =>
      applyPatch(
        makeRecord({ items: oldList }),
        patch(fieldChange(["items", "0"], present(stringValue("one")), present(stringValue("two")))),
      ),
    ).toThrow(/non-record ancestor/);

    const oldMap = create(cdcV2.ValueSchema, {
      kind: {
        case: "mapValue",
        value: create(cdcV2.MapValueSchema, {
          entries: [create(cdcV2.MapEntrySchema, { key: stringValue("region"), value: stringValue("north") })],
        }),
      },
    });
    const newMap = create(cdcV2.ValueSchema, {
      kind: {
        case: "mapValue",
        value: create(cdcV2.MapValueSchema, {
          entries: [create(cdcV2.MapEntrySchema, { key: stringValue("region"), value: stringValue("south") })],
        }),
      },
    });
    const mapReplaced = applyPatch(
      makeRecord({ attributes: oldMap }),
      patch(fieldChange(["attributes"], present(oldMap), present(newMap))),
    );
    expect(valueSemantic(lookup(mapReplaced, ["attributes"]) as cdcV2.Value)).toBe(valueSemantic(newMap));
    expect(() =>
      applyPatch(
        makeRecord({ attributes: oldMap }),
        patch(fieldChange(["attributes", "region"], present(stringValue("north")), present(stringValue("south")))),
      ),
    ).toThrow(/non-record ancestor/);
  });

  test("rejects missing bases, mismatches, invalid paths, and ambiguous patches", () => {
    const base = makeRecord({
      name: stringValue("before"),
      profile: create(cdcV2.ValueSchema, {
        kind: { case: "recordValue", value: makeRecord({ city: stringValue("Oslo") }) },
      }),
    });
    const update = minimalRecord(
      cdcV2.Operation.UPDATE,
      deltaPatch(fieldChange(["name"], present(stringValue("before")), present(stringValue("after")))),
    );
    expect(() => applyEvent(new Map(), createDummyEvent(), update)).toThrow(/base is missing/);
    expect(() => deltaToFull(update)).toThrow(/base is missing/);

    const baseBeforeFailure = recordSemantic(base);
    expect(() =>
      applyPatch(base, patch(fieldChange(["name"], present(stringValue("wrong")), present(stringValue("after"))))),
    ).toThrow(/before mismatch/);
    expect(recordSemantic(base)).toBe(baseBeforeFailure);
    expect(() =>
      applyPatch(
        base,
        patch(
          fieldChange(["name"], present(stringValue("before")), present(stringValue("one"))),
          fieldChange(["name"], present(stringValue("before")), present(stringValue("two"))),
        ),
      ),
    ).toThrow(/duplicate patch path/);
    expect(() =>
      applyPatch(
        base,
        patch(
          fieldChange(
            ["profile"],
            present(recordFields(base).get("profile") as cdcV2.Value),
            present(
              create(cdcV2.ValueSchema, {
                kind: { case: "recordValue", value: makeRecord({ city: stringValue("Bergen") }) },
              }),
            ),
          ),
          fieldChange(["profile", "city"], present(stringValue("Oslo")), present(stringValue("Bergen"))),
        ),
      ),
    ).toThrow(/overlapping patch paths/);
    expect(() => applyPatch(base, patch(fieldChange([], absent(), present(stringValue("new")))))).toThrow(
      /empty patch path/,
    );
    expect(() =>
      applyPatch(base, patch(fieldChange(["missing", "leaf"], absent(), present(stringValue("new"))))),
    ).toThrow(/missing ancestor/);
    expect(() => applyPatch(base, patch(fieldChange(["missing"], absent(), absent())))).toThrow(/real transition/);
    expect(() =>
      applyPatch(base, patch(fieldChange(["name"], present(stringValue("before")), present(stringValue("before"))))),
    ).toThrow(/real transition/);
  });

  test("validates before states and leaves full and delta replay state atomic on failure", () => {
    const full = readHistory(fullFixture);
    const state = replay(full.slice(0, 1)).state;
    const stateBeforeFailure = stateSemantic(state);
    const badUpdate = cloneChangeRecord(full[1]?.record as cdcV2.ChangeRecord);
    if (badUpdate.representation.case !== "full") throw new Error("fixture update must use FullChange");
    badUpdate.representation.value.before = makeRecord({ id: stringValue("wrong") });

    expect(() => applyEvent(state, full[1]?.event as CloudEvent, badUpdate)).toThrow(/full before mismatch/);
    expect(stateSemantic(state)).toBe(stateBeforeFailure);

    const delta = readHistory(deltaFixture);
    const deltaState = replay(delta.slice(0, 1)).state;
    const deltaStateBeforeFailure = stateSemantic(deltaState);
    const badPatch = cloneChangeRecord(delta[1]?.record as cdcV2.ChangeRecord);
    if (
      badPatch.representation.case !== "delta" ||
      badPatch.representation.value.change.case !== "patch" ||
      badPatch.representation.value.change.value.changes[0] === undefined
    ) {
      throw new Error("fixture update must use DeltaChange.patch");
    }
    badPatch.representation.value.change.value.changes[0].before = present(stringValue("wrong"));

    expect(() => applyEvent(deltaState, delta[1]?.event as CloudEvent, badPatch)).toThrow(/before mismatch/);
    expect(stateSemantic(deltaState)).toBe(deltaStateBeforeFailure);
  });

  test("deduplicates stable CloudEvent identities before validating a retried delta", () => {
    const delta = readHistory(deltaFixture);
    const poisonedRetry = cloneChangeRecord(delta[2]?.record as cdcV2.ChangeRecord);
    if (
      poisonedRetry.representation.case !== "delta" ||
      poisonedRetry.representation.value.change.case !== "patch" ||
      poisonedRetry.representation.value.change.value.changes[0]?.before === undefined
    ) {
      throw new Error("fixture retry must use DeltaChange.patch");
    }
    poisonedRetry.representation.value.change.value.changes[0].before = present(stringValue("not-the-base"));
    const history = delta.map((item, index) => (index === 2 ? { ...item, record: poisonedRetry } : item));

    expect(stateSemantic(replay(history).state)).toBe(stateSemantic(replay(delta).state));
  });

  test.each([
    minimalRecord(cdcV2.Operation.CREATE, deltaPatch()),
    minimalRecord(
      cdcV2.Operation.UPDATE,
      create(cdcV2.DeltaChangeSchema, { change: { case: "result", value: makeRecord({ id: stringValue("1") }) } }),
    ),
    minimalRecord(
      cdcV2.Operation.DELETE,
      create(cdcV2.DeltaChangeSchema, { change: { case: "result", value: makeRecord({ id: stringValue("1") }) } }),
    ),
    minimalRecord(
      cdcV2.Operation.TRUNCATE,
      create(cdcV2.DeltaChangeSchema, {
        change: { case: "delete", value: create(cdcV2.DeleteDeltaSchema) },
      }),
    ),
    minimalRecord(
      cdcV2.Operation.SOURCE_MESSAGE,
      create(cdcV2.DeltaChangeSchema, {
        change: { case: "delete", value: create(cdcV2.DeleteDeltaSchema) },
      }),
    ),
  ])("rejects invalid operation/representation pair %#", (record) => {
    expect(() => validateShape(record)).toThrow(ReplayError);
  });

  test("accepts keyless wire data but rejects strict keyed replay", () => {
    const record = minimalRecord(
      cdcV2.Operation.CREATE,
      create(cdcV2.DeltaChangeSchema, {
        change: { case: "result", value: makeRecord({ name: stringValue("keyless") }) },
      }),
    );
    record.key = undefined;
    validateShape(record);
    expect(() => applyEvent(new Map(), createDummyEvent(), record)).toThrow(/keyless record/);
  });

  test("accepts future protobuf fields while preserving known semantics", () => {
    const record = readHistory(deltaFixture)[0]?.record;
    if (record === undefined) throw new Error("delta fixture is empty");
    const futureWire = new Uint8Array([...toBinary(cdcV2.ChangeRecordSchema, record), 0xa0, 0x06, 0x01]);
    const decoded = fromBinary(cdcV2.ChangeRecordSchema, futureWire);

    expect(decoded.operation).toBe(record.operation);
    expect(decoded.representation.case).toBe(record.representation.case);
    validateShape(decoded);
    expect(toBinary(cdcV2.ChangeRecordSchema, decoded)).toEqual(futureWire);
  });
});

function createDummyEvent(source = "urn:test"): CloudEvent {
  return create(CloudEventSchema, { source });
}
