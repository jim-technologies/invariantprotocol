import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { create, toBinary } from "@bufbuild/protobuf";
import { describe, expect, test } from "vitest";

import {
  findDataset,
  migrateSchemaBundle,
  Presence,
  parseSchemaBundle,
  PrimitiveKind,
  SCHEMA_IR_VERSION,
  SCHEMA_MAPPING_VERSION,
  SchemaBundleSchema,
  SyntheticRole,
  serializeSchemaBundle,
} from "../src/index.js";

const here = dirname(fileURLToPath(import.meta.url));
const goldenBundle = resolve(here, "../../testdata/data.schema.binpb");

describe("canonical data schema", () => {
  test("reads the shared cross-language bundle", () => {
    const encoded = new Uint8Array(readFileSync(goldenBundle));
    const bundle = parseSchemaBundle(encoded);

    expect(bundle.irVersion).toBe(SCHEMA_IR_VERSION);
    expect(bundle.mappingVersion).toBe(SCHEMA_MAPPING_VERSION);
    expect([bundle.irVersion, bundle.mappingVersion]).toEqual([4, 3]);
    expect(bundle.datasets.map((dataset) => dataset.sourceMessage)).toEqual([
      "data.v1.CanonicalRecord",
      "data.v1.Proto2Record",
    ]);

    const canonical = findDataset(bundle, "data.v1.CanonicalRecord");
    expect(canonical).toBeDefined();
    const fields = new Map(canonical?.fields.map((field) => [field.name, field]));

    const optionalNote = fields.get("optional_note");
    expect([optionalNote?.stableId, optionalNote?.presence, optionalNote?.nullable]).toEqual([
      17,
      Presence.EXPLICIT,
      true,
    ]);
    expect(optionalNote?.storageNameSource).toBe("optional_note");

    const labels = fields.get("labels");
    expect([labels?.stableId, labels?.presence]).toEqual([19, Presence.REPEATED]);
    expect(labels?.type?.kind.case).toBe("list");
    const element = labels?.type?.kind.case === "list" ? labels.type.kind.value.element : undefined;
    expect([element?.stableId, element?.presence, element?.syntheticRole]).toEqual([
      31,
      Presence.NOT_APPLICABLE,
      SyntheticRole.LIST_ELEMENT,
    ]);
    expect(element?.storageNameSource).toBe("");

    const choiceCount = fields.get("choice_count");
    expect([choiceCount?.stableId, choiceCount?.presence, choiceCount?.oneof]).toEqual([22, Presence.ONEOF, "choice"]);

    const proto2 = findDataset(bundle, "data.v1.Proto2Record");
    expect(proto2).toBeDefined();
    const proto2Fields = new Map(proto2?.fields.map((field) => [field.name, field]));
    const id = proto2Fields.get("id");
    expect([id?.stableId, id?.presence]).toEqual([1, Presence.REQUIRED]);
    const label = proto2Fields.get("label");
    expect([label?.stableId, label?.presence, label?.hasDefault, label?.protobufDefault]).toEqual([
      2,
      Presence.EXPLICIT,
      true,
      "unknown",
    ]);

    expect(parseSchemaBundle(serializeSchemaBundle(bundle))).toEqual(bundle);
    expect(findDataset(bundle, "data.v1.Missing")).toBeUndefined();
  });

  test.each(["ir", "mapping"] as const)("rejects an unsupported %s version", (version) => {
    const bundle = create(SchemaBundleSchema, {
      irVersion: SCHEMA_IR_VERSION,
      mappingVersion: SCHEMA_MAPPING_VERSION,
    });
    if (version === "ir") bundle.irVersion = 1;
    else bundle.mappingVersion = 1;

    expect(() => parseSchemaBundle(toBinary(SchemaBundleSchema, bundle))).toThrow(
      "unsupported SchemaBundle version pair",
    );
  });

  test("migrates only the exact legacy version without losing schema state", () => {
    const legacy = create(SchemaBundleSchema, {
      irVersion: 3,
      mappingVersion: 2,
      sourceDescriptorSha256: new TextEncoder().encode("digest"),
      datasets: [
        {
          sourceMessage: "example.v1.Record",
          name: "example_v1_record",
          lastFieldId: 8,
          fields: [
            {
              name: "values",
              stableId: 7,
              type: {
                kind: {
                  case: "list",
                  value: {
                    element: {
                      name: "element",
                      stableId: 8,
                      type: {
                        kind: {
                          case: "primitive",
                          value: { kind: PrimitiveKind.FLOAT },
                        },
                      },
                    },
                  },
                },
              },
            },
          ],
          retiredFields: [
            {
              identity: "f:6",
              stableId: 6,
              protoFullName: "example.v1.Record.old_value",
              name: "old_value",
              storageNameSource: "old_value",
            },
          ],
        },
      ],
    });

    const migrated = migrateSchemaBundle(legacy);

    expect([migrated.irVersion, migrated.mappingVersion]).toEqual([SCHEMA_IR_VERSION, SCHEMA_MAPPING_VERSION]);
    expect(new TextDecoder().decode(migrated.sourceDescriptorSha256)).toBe("digest");
    expect(migrated.datasets[0]?.lastFieldId).toBe(8);
    expect(migrated.datasets[0]?.fields[0]?.stableId).toBe(7);
    expect(migrated.datasets[0]?.retiredFields[0]).toEqual(
      expect.objectContaining({ identity: "f:6", stableId: 6, name: "old_value" }),
    );
    expect(parseSchemaBundle(toBinary(SchemaBundleSchema, legacy))).toEqual(migrated);
    expect(migrateSchemaBundle(migrated)).toBe(migrated);
  });

  test.each([
    [3, 3],
    [4, 2],
  ])("rejects mixed migration version pair %i/%i", (irVersion, mappingVersion) => {
    const bundle = create(SchemaBundleSchema, { irVersion, mappingVersion });

    expect(() => migrateSchemaBundle(bundle)).toThrow(
      `version pair ir_version=${irVersion} mapping_version=${mappingVersion}`,
    );
  });

  test("rejects fixed lists that could not exist in mapping version 2", () => {
    const legacy = create(SchemaBundleSchema, {
      irVersion: 3,
      mappingVersion: 2,
      datasets: [
        {
          name: "example_v1_record",
          fields: [
            {
              name: "embedding",
              type: {
                kind: {
                  case: "list",
                  value: {
                    element: { name: "element" },
                    fixedLength: 8,
                  },
                },
              },
            },
          ],
        },
      ],
    });

    expect(() => migrateSchemaBundle(legacy)).toThrow(
      'mapping_version 2 field "example_v1_record.embedding" contains fixed_length 8',
    );
  });

  test("rejects unknown legacy wire fields", () => {
    const legacy = create(SchemaBundleSchema, {
      irVersion: 3,
      mappingVersion: 2,
    });
    const encoded = new Uint8Array([...toBinary(SchemaBundleSchema, legacy), 0xf8, 0x07, 0x01]);

    expect(() => parseSchemaBundle(encoded)).toThrow("fields unknown to this migrator");
  });

  test("round-trips portable refined types and fixed-list shape", () => {
    const bundle = create(SchemaBundleSchema, {
      irVersion: SCHEMA_IR_VERSION,
      mappingVersion: SCHEMA_MAPPING_VERSION,
      datasets: [
        {
          sourceMessage: "example.v1.Record",
          name: "example_v1_record",
          fields: [
            { name: "amount", type: { kind: { case: "decimal", value: { precision: 18, scale: 4 } } } },
            { name: "id", type: { kind: { case: "uuid", value: {} } } },
            { name: "digest", type: { kind: { case: "fixedBytes", value: { byteLength: 32 } } } },
            {
              name: "embedding",
              type: {
                kind: {
                  case: "list",
                  value: {
                    element: {
                      name: "element",
                      type: {
                        kind: {
                          case: "primitive",
                          value: { kind: PrimitiveKind.FLOAT },
                        },
                      },
                    },
                    fixedLength: 1536,
                  },
                },
              },
            },
          ],
        },
      ],
    });

    const fields = parseSchemaBundle(serializeSchemaBundle(bundle)).datasets[0]?.fields;
    expect(fields?.[0]?.type?.kind).toEqual({
      case: "decimal",
      value: expect.objectContaining({ precision: 18, scale: 4 }),
    });
    expect(fields?.[1]?.type?.kind.case).toBe("uuid");
    expect(fields?.[2]?.type?.kind).toEqual({ case: "fixedBytes", value: expect.objectContaining({ byteLength: 32 }) });
    expect(fields?.[3]?.type?.kind).toEqual({
      case: "list",
      value: expect.objectContaining({
        fixedLength: 1536,
        element: expect.objectContaining({
          type: expect.objectContaining({
            kind: {
              case: "primitive",
              value: expect.objectContaining({ kind: PrimitiveKind.FLOAT }),
            },
          }),
        }),
      }),
    });
  });
});
