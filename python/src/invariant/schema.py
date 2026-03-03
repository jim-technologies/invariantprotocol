"""Convert proto message descriptors to JSON Schema."""

from __future__ import annotations

from google.protobuf.descriptor_pb2 import FieldDescriptorProto

from invariant.descriptor import FieldInfo, MessageInfo, ParsedDescriptor

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

# Well-known type mappings
_WKT = {
    "google.protobuf.Timestamp": {"type": "string", "format": "date-time"},
    "google.protobuf.Duration": {
        "type": "string",
        "description": "Duration e.g. '300s', '1.5h'",
    },
    "google.protobuf.Any": {"type": "object"},
    "google.protobuf.Struct": {"type": "object"},
    "google.protobuf.Value": {},
    "google.protobuf.DoubleValue": {"type": "number"},
    "google.protobuf.FloatValue": {"type": "number"},
    "google.protobuf.Int64Value": {"type": "integer"},
    "google.protobuf.UInt64Value": {"type": "integer", "minimum": 0},
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
        return self._message_schema(msg)

    def _message_schema(self, msg: MessageInfo) -> dict:
        properties: dict[str, dict] = {}
        required: list[str] = []

        oneof_fields: set[str] = set()
        for oneof in msg.oneofs:
            for fname in oneof.field_names:
                oneof_fields.add(fname)

        for field in msg.fields:
            if self._is_map_field(field):
                map_msg = self.parsed.messages.get(field.type_name)
                prop = self._map_schema(map_msg) if map_msg else {"type": "object"}
            elif field.label == LABEL_REPEATED:
                prop = {"type": "array", "items": self._field_type_schema(field)}
            else:
                prop = self._field_type_schema(field)

            if field.comment:
                prop["description"] = field.comment

            properties[field.name] = prop

            if (
                field.label != LABEL_REPEATED
                and field.name not in oneof_fields
                and not field.HasField("oneof_index")
                and not field.optional
            ):
                required.append(field.name)

        schema: dict = {
            "type": "object",
            "properties": properties,
            "additionalProperties": False,
        }
        if required:
            schema["required"] = required
        return schema

    def _field_type_schema(self, field: FieldInfo) -> dict:
        t = field.type

        if t in (TYPE_DOUBLE, TYPE_FLOAT):
            return {"type": "number"}
        if t in (TYPE_INT32, TYPE_INT64, TYPE_SINT32, TYPE_SINT64, TYPE_SFIXED32, TYPE_SFIXED64):
            return {"type": "integer"}
        if t in (TYPE_UINT32, TYPE_UINT64, TYPE_FIXED32, TYPE_FIXED64):
            return {"type": "integer", "minimum": 0}
        if t == TYPE_BOOL:
            return {"type": "boolean"}
        if t == TYPE_STRING:
            return {"type": "string"}
        if t == TYPE_BYTES:
            return {"type": "string", "contentEncoding": "base64"}
        if t == TYPE_ENUM:
            return self._enum_schema(field.type_name)
        if t == TYPE_MESSAGE:
            return self._message_type_schema(field.type_name)
        return {}

    def _message_type_schema(self, type_name: str) -> dict:
        if type_name in _WKT:
            return dict(_WKT[type_name])

        msg = self.parsed.messages.get(type_name)
        if msg is None:
            return {"type": "object"}
        return self._message_schema(msg)

    def _enum_schema(self, type_name: str) -> dict:
        enum = self.parsed.enums.get(type_name)
        if enum is None:
            return {"type": "string"}
        return {"type": "string", "enum": [v.name for v in enum.values]}

    def _is_map_field(self, field: FieldInfo) -> bool:
        if field.label != LABEL_REPEATED or field.type != TYPE_MESSAGE:
            return False
        msg = self.parsed.messages.get(field.type_name)
        return msg is not None and msg.is_map_entry

    def _map_schema(self, map_entry_msg: MessageInfo) -> dict:
        value_field = None
        for f in map_entry_msg.fields:
            if f.name == "value":
                value_field = f
                break
        if value_field is None:
            return {"type": "object"}
        return {
            "type": "object",
            "additionalProperties": self._field_type_schema(value_field),
        }
