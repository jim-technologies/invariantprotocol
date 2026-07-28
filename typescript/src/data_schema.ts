import { clone, fromBinary, toBinary } from "@bufbuild/protobuf";

import {
  type DatasetSchema,
  type Field,
  type SchemaBundle,
  SchemaBundleSchema,
} from "./gen/invariant/data/v1/schema_pb.js";

export * from "./gen/invariant/data/v1/schema_pb.js";

export const SCHEMA_IR_VERSION = 4;
export const SCHEMA_MAPPING_VERSION = 3;

/** Parse a supported serialized invariant.data.v1.SchemaBundle. */
export function parseSchemaBundle(data: Uint8Array): SchemaBundle {
  const bundle = fromBinary(SchemaBundleSchema, data);
  return migrateSchemaBundle(bundle);
}

/** Upgrade the one supported historical SchemaBundle version in memory. */
export function migrateSchemaBundle(bundle: SchemaBundle): SchemaBundle {
  if (bundle.irVersion === SCHEMA_IR_VERSION && bundle.mappingVersion === SCHEMA_MAPPING_VERSION) {
    return bundle;
  }
  if (bundle.irVersion !== 3 || bundle.mappingVersion !== 2) {
    throw new Error(
      `unsupported SchemaBundle version pair ir_version=${bundle.irVersion} mapping_version=${bundle.mappingVersion}; ` +
        `expected 3/2 or ${SCHEMA_IR_VERSION}/${SCHEMA_MAPPING_VERSION}`,
    );
  }

  const withUnknown = toBinary(SchemaBundleSchema, bundle);
  const withoutUnknown = toBinary(SchemaBundleSchema, bundle, { writeUnknownFields: false });
  if (
    withUnknown.length !== withoutUnknown.length ||
    withUnknown.some((value, index) => value !== withoutUnknown[index])
  ) {
    throw new Error("migrate SchemaBundle: legacy artifact contains fields unknown to this migrator");
  }

  const validateFields = (fields: Field[], parent: string): void => {
    for (const field of fields) {
      const path = parent ? `${parent}.${field.name}` : field.name;
      switch (field.type?.kind.case) {
        case "struct":
          validateFields(field.type.kind.value.fields, path);
          break;
        case "list": {
          const list = field.type.kind.value;
          if (list.fixedLength !== 0) {
            throw new Error(
              `SchemaBundle mapping_version 2 field ${JSON.stringify(path)} contains fixed_length ` +
                `${list.fixedLength}, which was introduced in mapping_version ${SCHEMA_MAPPING_VERSION}`,
            );
          }
          if (list.element) validateFields([list.element], `${path}[]`);
          break;
        }
        case "map": {
          const map = field.type.kind.value;
          if (map.key) validateFields([map.key], `${path}.key`);
          if (map.value) validateFields([map.value], `${path}.value`);
          break;
        }
      }
    }
  };
  for (const dataset of bundle.datasets) validateFields(dataset.fields, dataset.name);

  const migrated = clone(SchemaBundleSchema, bundle);
  migrated.irVersion = SCHEMA_IR_VERSION;
  migrated.mappingVersion = SCHEMA_MAPPING_VERSION;
  return migrated;
}

/** Reject a bundle whose IR or mapping rules this package cannot interpret. */
export function validateSchemaBundle(bundle: SchemaBundle): void {
  if (bundle.irVersion !== SCHEMA_IR_VERSION) {
    throw new Error(`unsupported SchemaBundle ir_version ${bundle.irVersion}; expected ${SCHEMA_IR_VERSION}`);
  }
  if (bundle.mappingVersion !== SCHEMA_MAPPING_VERSION) {
    throw new Error(
      `unsupported SchemaBundle mapping_version ${bundle.mappingVersion}; expected ${SCHEMA_MAPPING_VERSION}`,
    );
  }
}

/** Serialize a schema bundle to protobuf wire bytes. */
export function serializeSchemaBundle(bundle: SchemaBundle): Uint8Array {
  validateSchemaBundle(bundle);
  return toBinary(SchemaBundleSchema, bundle);
}

/** Find a dataset by its fully-qualified protobuf source message name. */
export function findDataset(bundle: SchemaBundle, sourceMessage: string): DatasetSchema | undefined {
  return bundle.datasets.find((dataset) => dataset.sourceMessage === sourceMessage);
}
