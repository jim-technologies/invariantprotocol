import { type DescField, type DescMessage, ScalarType } from "@bufbuild/protobuf";
import { Edition, FieldDescriptorProto_Label } from "@bufbuild/protobuf/wkt";

import type { ParsedDescriptor } from "./descriptor.js";

export type JsonSchema = Record<string, unknown>;

const WKT: Record<string, JsonSchema> = {
  "google.protobuf.Timestamp": { type: "string", format: "date-time" },
  "google.protobuf.Duration": { type: "string", pattern: "^-?(?:0|[1-9][0-9]*)(?:\\.[0-9]{1,9})?s$" },
  "google.protobuf.Any": { type: "object" },
  "google.protobuf.Struct": { type: "object" },
  "google.protobuf.Value": {},
  "google.protobuf.DoubleValue": numberOrNonFiniteSchema(),
  "google.protobuf.FloatValue": numberOrNonFiniteSchema(),
  "google.protobuf.Int64Value": signed64Schema(),
  "google.protobuf.UInt64Value": unsigned64Schema(),
  "google.protobuf.Int32Value": { type: "integer" },
  "google.protobuf.UInt32Value": { type: "integer", minimum: 0 },
  "google.protobuf.BoolValue": { type: "boolean" },
  "google.protobuf.StringValue": { type: "string" },
  "google.protobuf.BytesValue": { type: "string", contentEncoding: "base64" },
  "google.protobuf.FieldMask": { type: "string" },
  "google.protobuf.ListValue": { type: "array", items: {} },
  "google.protobuf.Empty": { type: "object", additionalProperties: false },
};

export class SchemaGenerator {
  constructor(private readonly parsed: ParsedDescriptor) {}

  messageToSchema(fullName: string): JsonSchema {
    const msg = this.parsed.getMessage(fullName);
    if (!msg) {
      return { type: "object" };
    }
    return this.messageSchema(msg, new Set());
  }

  private messageSchema(msg: DescMessage, visiting: Set<string>): JsonSchema {
    const properties: Record<string, JsonSchema> = {};
    const required: string[] = [];

    for (const field of msg.fields) {
      const prop = this.fieldSchema(field, visiting);
      const comment = this.parsed.commentForField(msg.typeName, field);
      if (comment) {
        prop.description = comment;
      }
      properties[field.jsonName] = prop;

      if (isRequired(field)) {
        required.push(field.jsonName);
      }
    }

    const schema: JsonSchema = {
      type: "object",
      properties,
      additionalProperties: false,
    };
    if (required.length > 0) {
      schema.required = required;
    }
    return schema;
  }

  private fieldSchema(field: DescField, visiting: Set<string>): JsonSchema {
    switch (field.fieldKind) {
      case "list":
        return { type: "array", items: this.listItemSchema(field, visiting) };
      case "map": {
        const propertyNames = mapKeySchema(field.mapKey);
        return {
          type: "object",
          additionalProperties: this.mapValueSchema(field, visiting),
          ...(propertyNames === undefined ? {} : { propertyNames }),
        };
      }
      case "scalar":
        return scalarSchema(field.scalar);
      case "enum":
        return this.enumSchema(field.enum.typeName);
      case "message":
        return this.messageTypeSchema(field.message, visiting);
    }
  }

  private listItemSchema(field: DescField & { fieldKind: "list" }, visiting: Set<string>): JsonSchema {
    switch (field.listKind) {
      case "scalar":
        return scalarSchema(field.scalar);
      case "enum":
        return this.enumSchema(field.enum.typeName);
      case "message":
        return this.messageTypeSchema(field.message, visiting);
    }
  }

  private mapValueSchema(field: DescField & { fieldKind: "map" }, visiting: Set<string>): JsonSchema {
    switch (field.mapKind) {
      case "scalar":
        return scalarSchema(field.scalar);
      case "enum":
        return this.enumSchema(field.enum.typeName);
      case "message":
        return this.messageTypeSchema(field.message, visiting);
    }
  }

  private messageTypeSchema(message: DescMessage, visiting: Set<string>): JsonSchema {
    if (WKT[message.typeName]) {
      return { ...WKT[message.typeName] };
    }
    if (visiting.has(message.typeName)) {
      return { type: "object" };
    }
    visiting.add(message.typeName);
    const schema = this.messageSchema(message, visiting);
    visiting.delete(message.typeName);
    return schema;
  }

  private enumSchema(typeName: string): JsonSchema {
    if (typeName === "google.protobuf.NullValue") {
      return { type: "null" };
    }
    const en = this.parsed.getEnum(typeName);
    if (!en) {
      return { type: "string" };
    }
    return { type: "string", enum: en.values.map((value) => value.name) };
  }
}

function scalarSchema(type: ScalarType): JsonSchema {
  switch (type) {
    case ScalarType.DOUBLE:
    case ScalarType.FLOAT:
      return numberOrNonFiniteSchema();
    case ScalarType.INT32:
    case ScalarType.SINT32:
    case ScalarType.SFIXED32:
      return { type: "integer" };
    case ScalarType.INT64:
    case ScalarType.SINT64:
    case ScalarType.SFIXED64:
      return signed64Schema();
    case ScalarType.UINT32:
    case ScalarType.FIXED32:
      return { type: "integer", minimum: 0 };
    case ScalarType.UINT64:
    case ScalarType.FIXED64:
      return unsigned64Schema();
    case ScalarType.BOOL:
      return { type: "boolean" };
    case ScalarType.STRING:
      return { type: "string" };
    case ScalarType.BYTES:
      return { type: "string", contentEncoding: "base64" };
  }
}

function isRequired(field: DescField): boolean {
  return (
    field.fieldKind !== "list" &&
    field.fieldKind !== "map" &&
    field.oneof === undefined &&
    !field.proto.proto3Optional &&
    !(field.parent.file.edition === Edition.EDITION_PROTO2 && field.proto.label === FieldDescriptorProto_Label.OPTIONAL)
  );
}

function signed64Schema(): JsonSchema {
  return { type: "string", pattern: "^(0|-?[1-9][0-9]*)$" };
}

function unsigned64Schema(): JsonSchema {
  return { type: "string", pattern: "^(0|[1-9][0-9]*)$" };
}

function numberOrNonFiniteSchema(): JsonSchema {
  return { oneOf: [{ type: "number" }, { type: "string", enum: ["NaN", "Infinity", "-Infinity"] }] };
}

function mapKeySchema(type: ScalarType): JsonSchema | undefined {
  switch (type) {
    case ScalarType.BOOL:
      return { enum: ["false", "true"] };
    case ScalarType.INT32:
    case ScalarType.INT64:
    case ScalarType.SINT32:
    case ScalarType.SINT64:
    case ScalarType.SFIXED32:
    case ScalarType.SFIXED64:
      return { pattern: "^(0|-?[1-9][0-9]*)$" };
    case ScalarType.UINT32:
    case ScalarType.UINT64:
    case ScalarType.FIXED32:
    case ScalarType.FIXED64:
      return { pattern: "^(0|[1-9][0-9]*)$" };
    default:
      return undefined;
  }
}
