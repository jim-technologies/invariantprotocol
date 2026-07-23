"""Convert proto message descriptors to JSON Schema."""

from __future__ import annotations

import copy
from typing import Any

from google.protobuf.descriptor_pb2 import FieldDescriptorProto

from invariant.descriptor import ParsedDescriptor
from invariant.gen.invariant.v1 import types_pb2 as invpb

# Proto field type constants
TYPE_DOUBLE = FieldDescriptorProto.TYPE_DOUBLE
TYPE_FLOAT = FieldDescriptorProto.TYPE_FLOAT
TYPE_INT64 = FieldDescriptorProto.TYPE_INT64
TYPE_UINT64 = FieldDescriptorProto.TYPE_UINT64
TYPE_INT32 = FieldDescriptorProto.TYPE_INT32
TYPE_FIXED64 = FieldDescriptorProto.TYPE_FIXED64
TYPE_FIXED32 = FieldDescriptorProto.TYPE_FIXED32
TYPE_BOOL = FieldDescriptorProto.TYPE_BOOL
TYPE_STRING = FieldDescriptorProto.TYPE_STRING
TYPE_MESSAGE = FieldDescriptorProto.TYPE_MESSAGE
TYPE_BYTES = FieldDescriptorProto.TYPE_BYTES
TYPE_UINT32 = FieldDescriptorProto.TYPE_UINT32
TYPE_ENUM = FieldDescriptorProto.TYPE_ENUM
TYPE_SFIXED32 = FieldDescriptorProto.TYPE_SFIXED32
TYPE_SFIXED64 = FieldDescriptorProto.TYPE_SFIXED64
TYPE_SINT32 = FieldDescriptorProto.TYPE_SINT32
TYPE_SINT64 = FieldDescriptorProto.TYPE_SINT64

LABEL_REPEATED = FieldDescriptorProto.LABEL_REPEATED

_SIGNED_64_PATTERN = r"^(0|-?[1-9][0-9]*)$"
_UNSIGNED_64_PATTERN = r"^(0|[1-9][0-9]*)$"
_FLOAT_WRAPPERS = {"google.protobuf.DoubleValue", "google.protobuf.FloatValue"}

# Well-known type mappings
_WKT: dict[str, dict[str, Any]] = {
    "google.protobuf.Timestamp": {"type": "string", "format": "date-time"},
    "google.protobuf.Duration": {
        "type": "string",
        "pattern": r"^-?(?:0|[1-9][0-9]*)(?:\.[0-9]{1,9})?s$",
    },
    "google.protobuf.Any": {"type": "object"},
    "google.protobuf.Struct": {"type": "object"},
    "google.protobuf.Value": {},
    "google.protobuf.ListValue": {"type": "array", "items": {}},
    "google.protobuf.FieldMask": {"type": "string"},
    "google.protobuf.Empty": {"type": "object", "additionalProperties": False},
    "google.protobuf.Int64Value": {"type": "string", "pattern": _SIGNED_64_PATTERN},
    "google.protobuf.UInt64Value": {"type": "string", "pattern": _UNSIGNED_64_PATTERN},
    "google.protobuf.Int32Value": {"type": "integer"},
    "google.protobuf.UInt32Value": {"type": "integer", "minimum": 0},
    "google.protobuf.BoolValue": {"type": "boolean"},
    "google.protobuf.StringValue": {"type": "string"},
    "google.protobuf.BytesValue": {"type": "string", "contentEncoding": "base64"},
}


class SchemaGenerator:
    """Convert proto message descriptors to JSON Schema for tool input validation."""

    def __init__(self, parsed: ParsedDescriptor):
        self.parsed = parsed

    def message_to_schema(self, full_name: str) -> dict:
        """Return a JSON Schema dict for the named proto message type."""
        msg = self.parsed.messages.get(full_name)
        if msg is None:
            return {"type": "object"}
        return self._message_schema(msg, set())

    def _message_schema(self, msg: invpb.MessageInfo, visiting: set[str]) -> dict:
        properties: dict[str, dict] = {}
        required: list[str] = []

        oneof_fields: set[str] = set()
        for oneof in msg.oneofs:
            for fname in oneof.field_names:
                oneof_fields.add(fname)

        for field in msg.fields:
            if self._is_map_field(field):
                map_msg = self.parsed.messages.get(field.type_name)
                prop = self._map_schema(map_msg, visiting) if map_msg else {"type": "object"}
            elif field.label == LABEL_REPEATED:
                prop = {"type": "array", "items": self._field_type_schema(field, visiting)}
            else:
                prop = self._field_type_schema(field, visiting)

            if field.comment:
                prop["description"] = field.comment

            property_name = field.json_name or field.name
            properties[property_name] = prop

            if (
                field.label != LABEL_REPEATED
                and field.name not in oneof_fields
                and not field.HasField("oneof_index")
                and not field.optional
            ):
                required.append(property_name)

        schema: dict = {
            "type": "object",
            "properties": properties,
            "additionalProperties": False,
        }
        if required:
            schema["required"] = required
        return schema

    def _field_type_schema(self, field: invpb.FieldInfo, visiting: set[str]) -> dict:
        t = field.type

        if t in (TYPE_DOUBLE, TYPE_FLOAT):
            return _float_schema()
        if t in (TYPE_INT32, TYPE_SINT32, TYPE_SFIXED32):
            return {"type": "integer"}
        if t in (TYPE_UINT32, TYPE_FIXED32):
            return {"type": "integer", "minimum": 0}
        if t in (TYPE_INT64, TYPE_SINT64, TYPE_SFIXED64):
            return {"type": "string", "pattern": _SIGNED_64_PATTERN}
        if t in (TYPE_UINT64, TYPE_FIXED64):
            return {"type": "string", "pattern": _UNSIGNED_64_PATTERN}
        if t == TYPE_BOOL:
            return {"type": "boolean"}
        if t == TYPE_STRING:
            return {"type": "string"}
        if t == TYPE_BYTES:
            return {"type": "string", "contentEncoding": "base64"}
        if t == TYPE_ENUM:
            return self._enum_schema(field.type_name)
        if t == TYPE_MESSAGE:
            return self._message_type_schema(field.type_name, visiting)
        return {}

    def _message_type_schema(self, type_name: str, visiting: set[str]) -> dict:
        if type_name in _FLOAT_WRAPPERS:
            return _float_schema()
        if type_name in _WKT:
            return copy.deepcopy(_WKT[type_name])

        if type_name in visiting:
            return {"type": "object"}
        msg = self.parsed.messages.get(type_name)
        if msg is None:
            return {"type": "object"}
        visiting.add(type_name)
        schema = self._message_schema(msg, visiting)
        visiting.remove(type_name)
        return schema

    def _enum_schema(self, type_name: str) -> dict:
        if type_name == "google.protobuf.NullValue":
            return {"type": "null"}
        enum = self.parsed.enums.get(type_name)
        if enum is None:
            return {"type": "string"}
        return {"type": "string", "enum": [v.name for v in enum.values]}

    def _is_map_field(self, field: invpb.FieldInfo) -> bool:
        if field.label != LABEL_REPEATED or field.type != TYPE_MESSAGE:
            return False
        msg = self.parsed.messages.get(field.type_name)
        return msg is not None and msg.is_map_entry

    def _map_schema(self, map_entry_msg: invpb.MessageInfo, visiting: set[str]) -> dict:
        key_field = None
        value_field = None
        for f in map_entry_msg.fields:
            if f.name == "key":
                key_field = f
            if f.name == "value":
                value_field = f
        if value_field is None:
            return {"type": "object"}
        schema = {
            "type": "object",
            "additionalProperties": self._field_type_schema(value_field, visiting),
        }
        if key_field is not None:
            if key_field.type == TYPE_BOOL:
                schema["propertyNames"] = {"enum": ["false", "true"]}
            elif key_field.type in (TYPE_INT32, TYPE_INT64, TYPE_SINT32, TYPE_SINT64, TYPE_SFIXED32, TYPE_SFIXED64):
                schema["propertyNames"] = {"pattern": _SIGNED_64_PATTERN}
            elif key_field.type in (TYPE_UINT32, TYPE_UINT64, TYPE_FIXED32, TYPE_FIXED64):
                schema["propertyNames"] = {"pattern": _UNSIGNED_64_PATTERN}
        return schema


def _float_schema() -> dict:
    return {
        "oneOf": [
            {"type": "number"},
            {"type": "string", "enum": ["NaN", "Infinity", "-Infinity"]},
        ]
    }
