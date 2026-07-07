import { ScalarType, type DescField, type DescMessage } from "@bufbuild/protobuf";

import { type ParsedDescriptor } from "./descriptor.js";

export type JsonSchema = Record<string, unknown>;

const WKT: Record<string, JsonSchema> = {
  "google.protobuf.Timestamp": { type: "string", format: "date-time" },
  "google.protobuf.Duration": { type: "string", description: "Duration e.g. '300s', '1.5h'" },
  "google.protobuf.Any": { type: "object" },
  "google.protobuf.Struct": { type: "object" },
  "google.protobuf.Value": {},
  "google.protobuf.DoubleValue": { type: "number" },
  "google.protobuf.FloatValue": { type: "number" },
  "google.protobuf.Int64Value": { type: "integer" },
  "google.protobuf.UInt64Value": { type: "integer", minimum: 0 },
  "google.protobuf.Int32Value": { type: "integer" },
  "google.protobuf.UInt32Value": { type: "integer", minimum: 0 },
  "google.protobuf.BoolValue": { type: "boolean" },
  "google.protobuf.StringValue": { type: "string" },
  "google.protobuf.BytesValue": { type: "string", contentEncoding: "base64" },
};

export class SchemaGenerator {
  constructor(private readonly parsed: ParsedDescriptor) {}

  messageToSchema(fullName: string): JsonSchema {
    const msg = this.parsed.getMessage(fullName);
    if (!msg) {
      return { type: "object" };
    }
    return this.messageSchema(msg);
  }

  private messageSchema(msg: DescMessage): JsonSchema {
    const properties: Record<string, JsonSchema> = {};
    const required: string[] = [];

    for (const field of msg.fields) {
      const prop = this.fieldSchema(field);
      const comment = this.parsed.commentForField(msg.typeName, field);
      if (comment) {
        prop.description = comment;
      }
      properties[field.name] = prop;

      if (isRequired(field)) {
        required.push(field.name);
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

  private fieldSchema(field: DescField): JsonSchema {
    switch (field.fieldKind) {
      case "list":
        return { type: "array", items: this.listItemSchema(field) };
      case "map":
        return { type: "object", additionalProperties: this.mapValueSchema(field) };
      case "scalar":
        return scalarSchema(field.scalar);
      case "enum":
        return this.enumSchema(field.enum.typeName);
      case "message":
        return this.messageTypeSchema(field.message);
    }
  }

  private listItemSchema(field: DescField & { fieldKind: "list" }): JsonSchema {
    switch (field.listKind) {
      case "scalar":
        return scalarSchema(field.scalar);
      case "enum":
        return this.enumSchema(field.enum.typeName);
      case "message":
        return this.messageTypeSchema(field.message);
    }
  }

  private mapValueSchema(field: DescField & { fieldKind: "map" }): JsonSchema {
    switch (field.mapKind) {
      case "scalar":
        return scalarSchema(field.scalar);
      case "enum":
        return this.enumSchema(field.enum.typeName);
      case "message":
        return this.messageTypeSchema(field.message);
    }
  }

  private messageTypeSchema(message: DescMessage): JsonSchema {
    if (WKT[message.typeName]) {
      return { ...WKT[message.typeName] };
    }
    return this.messageSchema(message);
  }

  private enumSchema(typeName: string): JsonSchema {
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
      return { type: "number" };
    case ScalarType.INT32:
    case ScalarType.INT64:
    case ScalarType.SINT32:
    case ScalarType.SINT64:
    case ScalarType.SFIXED32:
    case ScalarType.SFIXED64:
      return { type: "integer" };
    case ScalarType.UINT32:
    case ScalarType.UINT64:
    case ScalarType.FIXED32:
    case ScalarType.FIXED64:
      return { type: "integer", minimum: 0 };
    case ScalarType.BOOL:
      return { type: "boolean" };
    case ScalarType.STRING:
      return { type: "string" };
    case ScalarType.BYTES:
      return { type: "string", contentEncoding: "base64" };
  }
}

function isRequired(field: DescField): boolean {
  return field.fieldKind !== "list" && field.fieldKind !== "map" && field.oneof === undefined && !field.proto.proto3Optional;
}
