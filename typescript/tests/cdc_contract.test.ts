import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { create, fromBinary, toBinary } from "@bufbuild/protobuf";
import { AnySchema, TimestampSchema } from "@bufbuild/protobuf/wkt";
import { describe, expect, test } from "vitest";

import {
  ChangeRecordSchema,
  ChangedFieldMaskSchema,
  type CloudEvent_CloudEventAttributeValue,
  CloudEvent_CloudEventAttributeValueSchema,
  CloudEventSchema,
  DataCollectionSchema,
  DecimalValueSchema,
  FieldPathSchema,
  ListValueSchema,
  NullValueSchema,
  OpaqueDataSchema,
  Operation,
  RecordFieldSchema,
  RecordSchema,
  SchemaReferenceSchema,
  SourceExtensionSchema,
  SourcePositionSchema,
  TransactionContextSchema,
  type Value,
  ValueSchema,
} from "../src/index.js";

const EVENT_TYPE = "io.invariantprotocol.cdc.v1.change";
const CHANGE_RECORD_TYPE_URL = "type.googleapis.com/invariant.cdc.v1.ChangeRecord";
const FIXTURE_VERSION = "3.6.1.Final";
const here = dirname(fileURLToPath(import.meta.url));
const fixtureRoot = resolve(here, `../../testdata/cdc/debezium/${FIXTURE_VERSION}`);

function timestamp(seconds: bigint, nanos = 0) {
  return create(TimestampSchema, { seconds, nanos });
}

function value(kind: Value["kind"], typeName = "") {
  return create(ValueSchema, { typeName, kind });
}

function field(name: string, fieldValue: Value) {
  return create(RecordFieldSchema, { name, value: fieldValue });
}

function richChangeRecord() {
  return create(ChangeRecordSchema, {
    operation: Operation.UPDATE,
    key: create(RecordSchema, {
      fields: [field("id", value({ case: "uint64Value", value: 18_446_744_073_709_551_615n }))],
    }),
    after: create(RecordSchema, {
      fields: [
        field("explicit_null", value({ case: "nullValue", value: create(NullValueSchema) })),
        field("unsigned", value({ case: "uint64Value", value: 18_446_744_073_709_551_615n })),
        field(
          "amount",
          value(
            {
              case: "decimalValue",
              value: create(DecimalValueSchema, {
                value: "12345678901234567890.123456789",
                scale: 9,
                precision: 29,
              }),
            },
            "org.apache.kafka.connect.data.Decimal",
          ),
        ),
        field("binary", value({ case: "bytesValue", value: Uint8Array.from([0x00, 0xff, 0x10, 0x62, 0x69]) })),
        field(
          "occurred_at",
          value(
            { case: "timestampValue", value: timestamp(1_721_234_567n, 987_654_321) },
            "io.debezium.time.NanoTimestamp",
          ),
        ),
        field(
          "items",
          value({
            case: "listValue",
            value: create(ListValueSchema, {
              values: [
                value({ case: "stringValue", value: "first" }),
                value({ case: "nullValue", value: create(NullValueSchema) }),
                value({ case: "uint64Value", value: 9_007_199_254_740_993n }),
              ],
            }),
          }),
        ),
        field(
          "address",
          value({
            case: "recordValue",
            value: create(RecordSchema, {
              fields: [
                field("city", value({ case: "stringValue", value: "Oakland" })),
                field("zip", value({ case: "uint32Value", value: 94_607 })),
              ],
            }),
          }),
        ),
        // "omitted" is deliberately absent: absence is not a null Value.
      ],
    }),
    dataCollection: create(DataCollectionSchema, { id: "inventory.public.customers" }),
    schemaReference: create(SchemaReferenceSchema, {
      uri: "urn:example:schema:customers",
      version: "42",
      fingerprint: Uint8Array.from([0x12, 0x34, 0x56, 0x78]),
    }),
    sourcePosition: create(SourcePositionSchema, {
      stream: "source-stream-7",
      format: "application/vnd.debezium.source-position+json",
      value: new TextEncoder().encode('{"opaque":"position","lsn":24023128}'),
    }),
    transaction: create(TransactionContextSchema, {
      id: "tx-123",
      totalOrder: 9_007_199_254_740_993n,
      dataCollectionOrder: 7n,
    }),
    sourceTime: timestamp(1_721_234_567n, 123_456_789),
    captureTime: timestamp(1_721_234_568n, 1),
    changedFields: create(ChangedFieldMaskSchema, {
      paths: [
        create(FieldPathSchema, { segments: ["amount"] }),
        create(FieldPathSchema, { segments: ["address", "city"] }),
      ],
    }),
    sourceExtension: create(SourceExtensionSchema, {
      representation: {
        case: "opaqueData",
        value: create(OpaqueDataSchema, {
          mediaType: "application/json",
          schema: "https://debezium.io/schemas/3.6/source/postgresql",
          data: new TextEncoder().encode('{"connector":"postgresql","future_source_field":{"x":1}}'),
        }),
      },
    }),
  });
}

function attribute(attr: CloudEvent_CloudEventAttributeValue["attr"]) {
  return create(CloudEvent_CloudEventAttributeValueSchema, { attr });
}

function cloudEvent(record = richChangeRecord()) {
  return create(CloudEventSchema, {
    id: "server-1:24023128:7",
    source: "urn:invariant:test:source:inventory",
    specVersion: "1.0",
    type: EVENT_TYPE,
    attributes: {
      time: attribute({ case: "ceTimestamp", value: timestamp(1_721_234_567n, 123_456_789) }),
      datacontenttype: attribute({ case: "ceString", value: "application/protobuf" }),
      dataschema: attribute({ case: "ceUri", value: CHANGE_RECORD_TYPE_URL }),
      correlationid: attribute({ case: "ceString", value: "request-42" }),
      causationid: attribute({ case: "ceString", value: "command-11" }),
      traceparent: attribute({
        case: "ceString",
        value: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
      }),
    },
    data: {
      case: "protoData",
      value: create(AnySchema, {
        typeUrl: CHANGE_RECORD_TYPE_URL,
        value: toBinary(ChangeRecordSchema, record),
      }),
    },
  });
}

function fieldValue(record: NonNullable<ReturnType<typeof richChangeRecord>["after"]>, name: string) {
  const found = record.fields.find((candidate) => candidate.name === name)?.value;
  if (found === undefined) throw new Error(`missing record field ${name}`);
  return found;
}

function assertRichValues(record: ReturnType<typeof richChangeRecord>) {
  expect(record.operation).toBe(Operation.UPDATE);
  expect(record.key).toBeDefined();
  expect(record.before).toBeUndefined();
  expect(record.after).toBeDefined();
  expect(record.dataCollection?.id).toBe("inventory.public.customers");
  expect(new TextDecoder().decode(record.sourcePosition?.value)).toBe('{"opaque":"position","lsn":24023128}');
  expect(record.transaction).toMatchObject({
    id: "tx-123",
    totalOrder: 9_007_199_254_740_993n,
    dataCollectionOrder: 7n,
  });
  expect(record.sourceTime).toEqual(timestamp(1_721_234_567n, 123_456_789));

  const after = record.after;
  if (after === undefined) throw new Error("missing after image");
  expect(after.fields.some((candidate) => candidate.name === "omitted")).toBe(false);
  expect(fieldValue(after, "explicit_null").kind.case).toBe("nullValue");
  expect(fieldValue(after, "unsigned").kind).toEqual({
    case: "uint64Value",
    value: 18_446_744_073_709_551_615n,
  });

  const amount = fieldValue(after, "amount");
  expect(amount.kind.case).toBe("decimalValue");
  if (amount.kind.case !== "decimalValue") throw new Error("amount must be a decimal");
  expect(amount.kind.value).toMatchObject({
    value: "12345678901234567890.123456789",
    scale: 9,
    precision: 29,
  });
  expect(fieldValue(after, "binary").kind).toEqual({
    case: "bytesValue",
    value: Uint8Array.from([0x00, 0xff, 0x10, 0x62, 0x69]),
  });
  expect(fieldValue(after, "occurred_at").kind).toEqual({
    case: "timestampValue",
    value: timestamp(1_721_234_567n, 987_654_321),
  });

  const items = fieldValue(after, "items");
  if (items.kind.case !== "listValue") throw new Error("items must be a list");
  expect(items.kind.value.values.map((item) => item.kind.case)).toEqual(["stringValue", "nullValue", "uint64Value"]);
  expect(items.kind.value.values[2]?.kind).toEqual({
    case: "uint64Value",
    value: 9_007_199_254_740_993n,
  });

  const address = fieldValue(after, "address");
  if (address.kind.case !== "recordValue") throw new Error("address must be a nested record");
  expect(fieldValue(address.kind.value, "city").kind).toEqual({ case: "stringValue", value: "Oakland" });
  expect(fieldValue(address.kind.value, "zip").kind).toEqual({ case: "uint32Value", value: 94_607 });

  expect(record.changedFields?.paths.map((path) => path.segments)).toEqual([["amount"], ["address", "city"]]);
  expect(record.sourceExtension?.representation.case).toBe("opaqueData");
  if (record.sourceExtension?.representation.case !== "opaqueData") throw new Error("missing opaque extension");
  expect(new TextDecoder().decode(record.sourceExtension.representation.value.data)).toContain("future_source_field");
}

describe("canonical CDC contract", () => {
  test("wraps a typed ChangeRecord in the canonical CloudEvent without loss", () => {
    const event = cloudEvent();
    const decoded = fromBinary(CloudEventSchema, toBinary(CloudEventSchema, event));

    expect([decoded.source, decoded.id]).toEqual(["urn:invariant:test:source:inventory", "server-1:24023128:7"]);
    expect(decoded.specVersion).toBe("1.0");
    expect(decoded.type).toBe(EVENT_TYPE);
    expect(decoded.attributes.datacontenttype?.attr).toEqual({
      case: "ceString",
      value: "application/protobuf",
    });
    expect(decoded.attributes.dataschema?.attr).toEqual({ case: "ceUri", value: CHANGE_RECORD_TYPE_URL });
    expect(decoded.attributes.time?.attr).toEqual({
      case: "ceTimestamp",
      value: timestamp(1_721_234_567n, 123_456_789),
    });
    expect(decoded.attributes.correlationid?.attr).toEqual({ case: "ceString", value: "request-42" });
    expect(decoded.attributes.causationid?.attr).toEqual({ case: "ceString", value: "command-11" });

    expect(decoded.data.case).toBe("protoData");
    if (decoded.data.case !== "protoData") throw new Error("CDC payload must use CloudEvent.proto_data");
    expect(decoded.data.value.typeUrl).toBe(CHANGE_RECORD_TYPE_URL);
    const record = fromBinary(ChangeRecordSchema, decoded.data.value.value);
    assertRichValues(record);

    const retryRecord = richChangeRecord();
    retryRecord.captureTime = timestamp(1_721_234_569n, 2);
    const retry = cloudEvent(retryRecord);
    expect([retry.source, retry.id]).toEqual([decoded.source, decoded.id]);
  });

  test.each([
    [Operation.CREATE, true, false, true, true, false],
    [Operation.UPDATE, true, false, true, true, false],
    [Operation.DELETE, true, true, false, true, false],
    [Operation.SNAPSHOT_READ, true, false, true, true, false],
    [Operation.TRUNCATE, false, false, false, true, false],
    [Operation.SOURCE_MESSAGE, false, false, false, false, true],
  ] as const)(
    "preserves operation %s presence semantics",
    (operation, hasKey, hasBefore, hasAfter, hasCollection, hasMessage) => {
      const image = create(RecordSchema, {
        fields: [field("id", value({ case: "uint64Value", value: 1n }))],
      });
      const record = create(ChangeRecordSchema, {
        operation,
        key: hasKey ? image : undefined,
        before: hasBefore ? image : undefined,
        after: hasAfter ? image : undefined,
        dataCollection: hasCollection ? create(DataCollectionSchema, { id: "inventory.public.customers" }) : undefined,
        sourceMessage: hasMessage
          ? value({ case: "bytesValue", value: new TextEncoder().encode("source-native-message") })
          : undefined,
      });
      const decoded = fromBinary(ChangeRecordSchema, toBinary(ChangeRecordSchema, record));

      expect(decoded.key !== undefined).toBe(hasKey);
      expect(decoded.before !== undefined).toBe(hasBefore);
      expect(decoded.after !== undefined).toBe(hasAfter);
      expect(decoded.dataCollection !== undefined).toBe(hasCollection);
      expect(decoded.sourceMessage !== undefined).toBe(hasMessage);
      if (operation === Operation.SOURCE_MESSAGE) {
        expect([decoded.key, decoded.before, decoded.after]).toEqual([undefined, undefined, undefined]);
      }
    },
  );

  test("preserves a future unknown field while exposing known semantics", () => {
    const record = richChangeRecord();
    // Field 100, varint value 1, stands in for a field added by a future writer.
    const futureWire = new Uint8Array([...toBinary(ChangeRecordSchema, record), 0xa0, 0x06, 0x01]);

    const decoded = fromBinary(ChangeRecordSchema, futureWire);
    assertRichValues(decoded);
    expect(toBinary(ChangeRecordSchema, decoded)).toEqual(futureWire);
  });
});

interface FixtureEntry {
  path: string;
  category: string;
  row_change: boolean;
  operation: string | null;
}

interface FixtureManifest {
  debezium_version: string;
  cloudevents_specification_version: string;
  cloudevents_event_specversion: string;
  fixtures: FixtureEntry[];
}

type JsonObject = { [key: string]: unknown };

const parseJsonWithSource = JSON.parse as unknown as (
  text: string,
  reviver: (key: string, value: unknown, context: { source?: string }) => unknown,
) => unknown;

function parseJsonObject(path: string): JsonObject {
  // Node 24 exposes each primitive's original JSON token to the reviver. Keep
  // integer tokens as bigint so Debezium int64/uint64 values never pass
  // through JavaScript's lossy binary64 number representation.
  const parsed = parseJsonWithSource(readFileSync(path, "utf8"), (_key, value, context) => {
    if (typeof value === "number" && context.source !== undefined && /^-?(0|[1-9][0-9]*)$/.test(context.source)) {
      return BigInt(context.source);
    }
    return value;
  });
  if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed))
    throw new Error(`${path} is not an object`);
  return parsed as JsonObject;
}

function jsonObject(value: unknown, path: string): JsonObject {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    throw new Error(`${path} is not an object`);
  }
  return value as JsonObject;
}

function dataChangeOperation(document: JsonObject): string | undefined {
  if (typeof document.iodebeziumop === "string") return document.iodebeziumop;
  if (typeof document.value !== "object" || document.value === null || Array.isArray(document.value)) return undefined;
  let envelope = document.value as JsonObject;
  if (typeof envelope.payload === "object" && envelope.payload !== null && !Array.isArray(envelope.payload)) {
    envelope = envelope.payload as JsonObject;
  }
  return typeof envelope.op === "string" ? envelope.op : undefined;
}

describe("pinned Debezium conformance fixtures", () => {
  test("parses and classifies native and structured data-change events", () => {
    const manifest = JSON.parse(readFileSync(resolve(fixtureRoot, "manifest.json"), "utf8")) as FixtureManifest;
    expect(manifest.debezium_version).toBe(FIXTURE_VERSION);
    expect(manifest.cloudevents_specification_version).toBe("1.0.2");
    expect(manifest.cloudevents_event_specversion).toBe("1.0");

    const expectedOperations = new Map([
      ["native-create-schemaful.json", "c"],
      ["native-update-schemaless.json", "u"],
      ["native-delete-schemaless.json", "d"],
      ["native-snapshot-schemaless.json", "r"],
      ["native-truncate-schemaless.json", "t"],
      ["native-logical-message-schemaless.json", "m"],
      ["structured-cloudevent-snapshot.json", "r"],
      ["structured-cloudevent-snapshot-retry.json", "r"],
    ]);
    for (const [name, operation] of expectedOperations) {
      expect(manifest.fixtures.find((entry) => entry.path === name)?.category).toBe("data_change");
      expect(dataChangeOperation(parseJsonObject(resolve(fixtureRoot, name)))).toBe(operation);
    }

    const schemaful = parseJsonObject(resolve(fixtureRoot, "native-create-schemaful.json"));
    expect(schemaful.value).toMatchObject({ schema: expect.any(Object), payload: expect.any(Object) });
    const schemafulPayload = jsonObject(jsonObject(schemaful.value, "value").payload, "value.payload");
    const schemafulAfter = jsonObject(schemafulPayload.after, "value.payload.after");
    const profile = jsonObject(schemafulAfter.profile, "value.payload.after.profile");
    expect(profile.score).toBe(9_007_199_254_740_993n);
    const schemaless = parseJsonObject(resolve(fixtureRoot, "native-update-schemaless.json"));
    expect(schemaless.value).not.toHaveProperty("schema");

    const structuredBytes = readFileSync(resolve(fixtureRoot, "structured-cloudevent-snapshot.json"));
    const retryBytes = readFileSync(resolve(fixtureRoot, "structured-cloudevent-snapshot-retry.json"));
    expect(structuredBytes).toEqual(retryBytes);
    const structured = parseJsonObject(resolve(fixtureRoot, "structured-cloudevent-snapshot.json"));
    expect(structured.specversion).toBe("1.0");
  });

  test("keeps tombstones, heartbeats, schemas, and transaction boundaries auxiliary", () => {
    const manifest = JSON.parse(readFileSync(resolve(fixtureRoot, "manifest.json"), "utf8")) as FixtureManifest;
    const auxiliary = new Map([
      ["auxiliary-kafka-tombstone.json", "kafka_tombstone"],
      ["auxiliary-heartbeat.json", "heartbeat"],
      ["auxiliary-schema-change.json", "schema_change"],
      ["auxiliary-transaction-begin.json", "transaction_boundary"],
      ["auxiliary-transaction-end.json", "transaction_boundary"],
    ]);

    for (const [name, category] of auxiliary) {
      const entry = manifest.fixtures.find((fixture) => fixture.path === name);
      expect(entry).toMatchObject({ category, row_change: false, operation: null });
      expect(parseJsonObject(resolve(fixtureRoot, name))).toBeTypeOf("object");
    }
  });
});
