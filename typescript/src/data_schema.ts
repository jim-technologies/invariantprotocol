import { fromBinary, toBinary } from "@bufbuild/protobuf";

import { type DatasetSchema, type SchemaBundle, SchemaBundleSchema } from "./gen/invariant/data/v1/schema_pb.js";

export * from "./gen/invariant/data/v1/schema_pb.js";

export const SCHEMA_IR_VERSION = 3;
export const SCHEMA_MAPPING_VERSION = 2;

/** Parse a supported serialized invariant.data.v1.SchemaBundle. */
export function parseSchemaBundle(data: Uint8Array): SchemaBundle {
  const bundle = fromBinary(SchemaBundleSchema, data);
  validateSchemaBundle(bundle);
  return bundle;
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
  return toBinary(SchemaBundleSchema, bundle);
}

/** Find a dataset by its fully-qualified protobuf source message name. */
export function findDataset(bundle: SchemaBundle, sourceMessage: string): DatasetSchema | undefined {
  return bundle.datasets.find((dataset) => dataset.sourceMessage === sourceMessage);
}
