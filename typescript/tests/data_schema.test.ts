import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { create } from "@bufbuild/protobuf";
import { describe, expect, test } from "vitest";

import {
  findDataset,
  parseSchemaBundle,
  Presence,
  SchemaBundleSchema,
  serializeSchemaBundle,
  SyntheticRole,
} from "../src/index.js";

const here = dirname(fileURLToPath(import.meta.url));
const goldenBundle = resolve(here, "../../testdata/data.schema.binpb");

describe("canonical data schema", () => {
  test("reads the shared cross-language bundle", () => {
    const encoded = new Uint8Array(readFileSync(goldenBundle));
    const bundle = parseSchemaBundle(encoded);

    expect(bundle.irVersion).toBe(1);
    expect(bundle.mappingVersion).toBe(1);
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

    const labels = fields.get("labels");
    expect([labels?.stableId, labels?.presence]).toEqual([19, Presence.REPEATED]);
    expect(labels?.type?.kind.case).toBe("list");
    const element = labels?.type?.kind.case === "list" ? labels.type.kind.value.element : undefined;
    expect([element?.stableId, element?.presence, element?.syntheticRole]).toEqual([
      31,
      Presence.NOT_APPLICABLE,
      SyntheticRole.LIST_ELEMENT,
    ]);

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
    const bundle = create(SchemaBundleSchema, { irVersion: 1, mappingVersion: 1 });
    if (version === "ir") bundle.irVersion = 2;
    else bundle.mappingVersion = 2;

    expect(() => parseSchemaBundle(serializeSchemaBundle(bundle))).toThrow(`unsupported SchemaBundle ${version}_version`);
  });
});
