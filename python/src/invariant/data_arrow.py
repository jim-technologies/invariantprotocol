"""Project canonical Invariant data schemas and protobuf values into Arrow.

PyArrow is imported only when an Arrow API is called so the RPC runtime does
not require the optional ``data`` dependency.
"""

from __future__ import annotations

import json
import re
from collections.abc import Iterable
from decimal import Decimal
from importlib import import_module
from typing import TYPE_CHECKING, Any
from uuid import UUID

from google.protobuf import descriptor as protobuf_descriptor
from google.protobuf import descriptor_pb2, json_format
from google.protobuf.message import DecodeError, Message

from invariant.gen.invariant.data.v1 import annotations_pb2, schema_pb2
from invariant.gen.invariant.data.v1.schema_pb2 import (
    DatasetSchema,
    DataType,
    Field,
    MappingDiagnostic,
)

if TYPE_CHECKING:
    import pyarrow

_DESCRIPTION = "invariant.description"
_ENUM_CLOSED = "invariant.enum.closed"
_ENUM_VALUES = "invariant.enum.values"
_LOGICAL_TYPE = "invariant.logical_type"
_ONEOF = "invariant.oneof"
_PRESENCE = "invariant.presence"
_PROTO_DEFAULT = "invariant.proto.default"
_PROTO_FULL_NAME = "invariant.proto.full_name"
_PROTO_HAS_DEFAULT = "invariant.proto.has_default"
_PROTO_JSON_NAME = "invariant.proto.json_name"
_PROTO_NUMBER_PATH = "invariant.proto.number_path"
_PROTOBUF_TYPE = "invariant.protobuf_type"
_STABLE_ID = "invariant.stable_id"
_PARQUET_FIELD_ID = "PARQUET:field_id"
_WRAPPER_TYPES = {
    "google.protobuf.BoolValue",
    "google.protobuf.BytesValue",
    "google.protobuf.DoubleValue",
    "google.protobuf.FloatValue",
    "google.protobuf.Int32Value",
    "google.protobuf.Int64Value",
    "google.protobuf.StringValue",
    "google.protobuf.UInt32Value",
    "google.protobuf.UInt64Value",
}
_INT64_MIN = -(1 << 63)
_INT64_MAX = (1 << 63) - 1
_INT32_MAX = (1 << 31) - 1
_NANOS_PER_SECOND = 1_000_000_000
_TIMESTAMP_SECONDS_MIN = -62_135_596_800
_TIMESTAMP_SECONDS_MAX = 253_402_300_799
_DURATION_SECONDS_MAX = 315_576_000_000
_DEFAULT_RECORD_BATCH_SIZE = 256
_PRIMITIVE_KINDS = {
    protobuf_descriptor.FieldDescriptor.TYPE_DOUBLE: schema_pb2.PRIMITIVE_KIND_DOUBLE,
    protobuf_descriptor.FieldDescriptor.TYPE_FLOAT: schema_pb2.PRIMITIVE_KIND_FLOAT,
    protobuf_descriptor.FieldDescriptor.TYPE_INT64: schema_pb2.PRIMITIVE_KIND_INT64,
    protobuf_descriptor.FieldDescriptor.TYPE_UINT64: schema_pb2.PRIMITIVE_KIND_UINT64,
    protobuf_descriptor.FieldDescriptor.TYPE_INT32: schema_pb2.PRIMITIVE_KIND_INT32,
    protobuf_descriptor.FieldDescriptor.TYPE_FIXED64: schema_pb2.PRIMITIVE_KIND_FIXED64,
    protobuf_descriptor.FieldDescriptor.TYPE_FIXED32: schema_pb2.PRIMITIVE_KIND_FIXED32,
    protobuf_descriptor.FieldDescriptor.TYPE_BOOL: schema_pb2.PRIMITIVE_KIND_BOOL,
    protobuf_descriptor.FieldDescriptor.TYPE_STRING: schema_pb2.PRIMITIVE_KIND_STRING,
    protobuf_descriptor.FieldDescriptor.TYPE_BYTES: schema_pb2.PRIMITIVE_KIND_BYTES,
    protobuf_descriptor.FieldDescriptor.TYPE_UINT32: schema_pb2.PRIMITIVE_KIND_UINT32,
    protobuf_descriptor.FieldDescriptor.TYPE_SFIXED32: schema_pb2.PRIMITIVE_KIND_SFIXED32,
    protobuf_descriptor.FieldDescriptor.TYPE_SFIXED64: schema_pb2.PRIMITIVE_KIND_SFIXED64,
    protobuf_descriptor.FieldDescriptor.TYPE_SINT32: schema_pb2.PRIMITIVE_KIND_SINT32,
    protobuf_descriptor.FieldDescriptor.TYPE_SINT64: schema_pb2.PRIMITIVE_KIND_SINT64,
}
_WRAPPER_KINDS = {
    "google.protobuf.BoolValue": schema_pb2.PRIMITIVE_KIND_BOOL,
    "google.protobuf.BytesValue": schema_pb2.PRIMITIVE_KIND_BYTES,
    "google.protobuf.DoubleValue": schema_pb2.PRIMITIVE_KIND_DOUBLE,
    "google.protobuf.FloatValue": schema_pb2.PRIMITIVE_KIND_FLOAT,
    "google.protobuf.Int32Value": schema_pb2.PRIMITIVE_KIND_INT32,
    "google.protobuf.Int64Value": schema_pb2.PRIMITIVE_KIND_INT64,
    "google.protobuf.StringValue": schema_pb2.PRIMITIVE_KIND_STRING,
    "google.protobuf.UInt32Value": schema_pb2.PRIMITIVE_KIND_UINT32,
    "google.protobuf.UInt64Value": schema_pb2.PRIMITIVE_KIND_UINT64,
}
_JSON_KINDS = {
    "google.protobuf.Any": schema_pb2.JSON_KIND_ANY,
    "google.protobuf.Struct": schema_pb2.JSON_KIND_STRUCT,
    "google.protobuf.Value": schema_pb2.JSON_KIND_VALUE,
    "google.protobuf.ListValue": schema_pb2.JSON_KIND_LIST_VALUE,
}


def arrow_schema(dataset: DatasetSchema) -> tuple[pyarrow.Schema, list[MappingDiagnostic]]:
    """Map one validated SchemaBundle dataset to a real PyArrow schema.

    The returned schema is directly accepted by ``pyarrow.Table`` and
    ``pyarrow.parquet``. Use :func:`arrow_table` for eager values or
    :func:`arrow_record_batch_reader` for a row-bounded stream.
    """
    if dataset is None:
        raise ValueError("arrow: dataset schema is required")

    pa = _pyarrow()
    fields = []
    diagnostics: list[MappingDiagnostic] = []
    for field in dataset.fields:
        mapped, field_diagnostics = _map_field(pa, field, field.name)
        fields.append(mapped)
        diagnostics.extend(field_diagnostics)

    metadata = _compact_metadata(
        {
            _DESCRIPTION: dataset.description,
            "invariant.dataset": dataset.name,
            "invariant.last_field_id": str(dataset.last_field_id),
            "invariant.source_message": dataset.source_message,
        }
    )
    return pa.schema(fields, metadata=metadata), diagnostics


def arrow_table(
    dataset: DatasetSchema,
    messages: Iterable[Message],
) -> tuple[pyarrow.Table, list[MappingDiagnostic]]:
    """Convert generated protobuf messages into a typed PyArrow table.

    ``dataset`` remains the mapping authority: message reflection supplies
    values, never a second inferred schema. Map entries are sorted because
    protobuf maps are unordered while Arrow maps retain entry order.
    """
    pa = _pyarrow()
    schema, diagnostics = arrow_schema(dataset)
    validated_descriptors: dict[int, Any] = {}
    field_protos: dict[int, tuple[Any, dict[str, descriptor_pb2.FieldDescriptorProto]]] = {}
    rows = [_message_row(dataset, message, validated_descriptors, field_protos) for message in messages]
    batch = _record_batch(pa, schema, rows)
    return pa.Table.from_batches([batch], schema=schema), diagnostics


def arrow_record_batch_reader(
    dataset: DatasetSchema,
    messages: Iterable[Message],
    *,
    batch_size: int = _DEFAULT_RECORD_BATCH_SIZE,
) -> tuple[pyarrow.RecordBatchReader, list[MappingDiagnostic]]:
    """Lazily convert generated protobuf messages into typed Arrow batches.

    At most ``batch_size`` messages and their intermediate Python values are
    converted at once. This is a row bound, not a byte bound: one protobuf
    message can itself contain arbitrarily large variable-width values. The
    returned reader is single-pass, like every Arrow ``RecordBatchReader``.
    """
    if isinstance(batch_size, bool) or not isinstance(batch_size, int):
        raise TypeError("arrow: batch_size must be an integer")
    if batch_size <= 0:
        raise ValueError("arrow: batch_size must be positive")
    if dataset is None:
        raise ValueError("arrow: dataset schema is required")

    pa = _pyarrow()
    dataset_snapshot = DatasetSchema()
    dataset_snapshot.CopyFrom(dataset)
    schema, diagnostics = arrow_schema(dataset_snapshot)

    def batches() -> Iterable[Any]:
        iterator = iter(messages)
        last_descriptor: Any = None
        while True:
            validated_descriptors: dict[int, Any] = (
                {} if last_descriptor is None else {id(last_descriptor): last_descriptor}
            )
            field_protos: dict[int, tuple[Any, dict[str, descriptor_pb2.FieldDescriptorProto]]] = {}
            rows = []
            for _ in range(batch_size):
                try:
                    message = next(iterator)
                except StopIteration:
                    break
                rows.append(_message_row(dataset_snapshot, message, validated_descriptors, field_protos))
            if not rows:
                return
            last_descriptor = message.DESCRIPTOR
            batch = _record_batch(pa, schema, rows)
            del field_protos, message, rows, validated_descriptors
            yield batch
            del batch

    return pa.RecordBatchReader.from_batches(schema, batches()), diagnostics


def _record_batch(
    pa: Any,
    schema: Any,
    rows: list[dict[str, Any]],
) -> Any:
    if len(schema) == 0:
        row_markers = pa.StructArray.from_arrays(
            [],
            fields=[],
            mask=pa.array([False] * len(rows), type=pa.bool_()),
        )
        return pa.RecordBatch.from_struct_array(row_markers).replace_schema_metadata(schema.metadata)
    columns = [_arrow_array(pa, [row[field.name] for row in rows], field.type) for field in schema]
    return pa.RecordBatch.from_arrays(columns, schema=schema)


def _pyarrow() -> Any:
    try:
        return import_module("pyarrow")
    except ModuleNotFoundError as error:
        if error.name != "pyarrow":
            raise
        raise ModuleNotFoundError(
            "Apache Arrow support requires the optional data dependency; "
            "install invariant-protocol with its 'data' extra"
        ) from error


def _arrow_array(pa: Any, values: list[Any], data_type: Any) -> Any:
    """Build nested arrays explicitly so canonical extension types compose."""
    if pa.types.is_fixed_size_list(data_type):
        flattened: list[Any] = []
        for value in values:
            flattened.extend([None] * data_type.list_size if value is None else value)
        items = _arrow_array(pa, flattened, data_type.value_type)
        return pa.FixedSizeListArray.from_arrays(
            items,
            type=data_type,
            mask=pa.array([value is None for value in values], type=pa.bool_()),
        )

    if pa.types.is_list(data_type):
        offsets = [0]
        flattened = []
        for value in values:
            if value is not None:
                flattened.extend(value)
            offsets.append(len(flattened))
        items = _arrow_array(pa, flattened, data_type.value_type)
        return pa.ListArray.from_arrays(
            pa.array(offsets, type=pa.int32()),
            items,
            type=data_type,
            mask=pa.array([value is None for value in values], type=pa.bool_()),
        )

    if pa.types.is_map(data_type):
        offsets = [0]
        keys: list[Any] = []
        map_items: list[Any] = []
        for value in values:
            if value is not None:
                keys.extend(key for key, _ in value)
                map_items.extend(item for _, item in value)
            offsets.append(len(keys))
        return pa.MapArray.from_arrays(
            pa.array(offsets, type=pa.int32()),
            _arrow_array(pa, keys, data_type.key_type),
            _arrow_array(pa, map_items, data_type.item_type),
            type=data_type,
            mask=pa.array([value is None for value in values], type=pa.bool_()),
        )

    if pa.types.is_struct(data_type):
        fields = list(data_type)
        children = [
            _arrow_array(
                pa,
                [None if value is None else value[field.name] for value in values],
                field.type,
            )
            for field in fields
        ]
        return pa.StructArray.from_arrays(
            children,
            fields=fields,
            mask=pa.array([value is None for value in values], type=pa.bool_()),
        )

    return pa.array(values, type=data_type)


def _map_field(pa: Any, field: Field, path: str) -> tuple[Any, list[MappingDiagnostic]]:
    if field is None:
        raise ValueError(f"arrow: field at {path!r} is required")
    if not field.HasField("type"):
        raise ValueError(f"arrow: field {path!r} has no logical type")

    mapped_type, children, compatibility, message = _map_type(pa, field.type, path)
    metadata = _compact_metadata(
        {
            _DESCRIPTION: field.description,
            _LOGICAL_TYPE: _logical_type_name(field.type),
            _ONEOF: field.oneof,
            _PRESENCE: schema_pb2.Presence.Name(field.presence),
            _PROTO_FULL_NAME: field.proto_full_name,
            _PROTO_HAS_DEFAULT: str(field.has_default).lower(),
            _PROTO_JSON_NAME: field.json_name,
            _PROTO_NUMBER_PATH: ".".join(str(number) for number in field.proto_number_path),
            _PROTOBUF_TYPE: field.type.protobuf_type,
            _STABLE_ID: str(field.stable_id),
            _PARQUET_FIELD_ID: str(field.stable_id),
        }
    )
    if field.has_default:
        metadata[_PROTO_DEFAULT] = field.protobuf_default

    kind = field.type.WhichOneof("kind")
    if kind == "enum":
        metadata[_ENUM_CLOSED] = str(field.type.enum.closed).lower()
        metadata[_ENUM_VALUES] = json.dumps(
            [
                {
                    key: value
                    for key, value in {
                        "name": enum_value.name,
                        "number": enum_value.number,
                        "description": enum_value.description,
                    }.items()
                    if value != ""
                }
                for enum_value in field.type.enum.values
            ],
            ensure_ascii=False,
            separators=(",", ":"),
        )

    if field.oneof:
        message += (
            f"; Arrow records membership in oneof {field.oneof!r} as metadata but does not enforce mutual exclusivity"
        )
        if compatibility == schema_pb2.MAPPING_COMPATIBILITY_LOSSLESS:
            compatibility = schema_pb2.MAPPING_COMPATIBILITY_RANGE_WIDENED

    diagnostic = MappingDiagnostic(field_path=path, compatibility=compatibility, message=message)
    mapped = pa.field(field.name, mapped_type, nullable=field.nullable, metadata=metadata)
    return mapped, [diagnostic, *children]


def _map_type(
    pa: Any, data_type: DataType, path: str
) -> tuple[Any, list[MappingDiagnostic], schema_pb2.MappingCompatibility, str]:
    kind = data_type.WhichOneof("kind")
    if kind == "primitive":
        mapped = _map_primitive(pa, data_type.primitive.kind)
        primitive = schema_pb2.PrimitiveKind.Name(data_type.primitive.kind).removeprefix("PRIMITIVE_KIND_").lower()
        return (
            mapped,
            [],
            schema_pb2.MAPPING_COMPATIBILITY_LOSSLESS,
            f"protobuf {primitive} maps losslessly to Arrow {mapped}",
        )
    if kind == "enum":
        if data_type.enum.closed:
            return (
                pa.int32(),
                [],
                schema_pb2.MAPPING_COMPATIBILITY_RANGE_WIDENED,
                "closed protobuf enum numbers map to unconstrained Arrow int32; "
                "symbols, aliases, and the closed value set are field metadata",
            )
        return (
            pa.int32(),
            [],
            schema_pb2.MAPPING_COMPATIBILITY_LOSSLESS,
            "open protobuf enum numbers map losslessly to Arrow int32; symbols and aliases are field metadata",
        )
    if kind == "struct":
        fields = []
        diagnostics: list[MappingDiagnostic] = []
        for child in data_type.struct.fields:
            mapped, child_diagnostics = _map_field(pa, child, _join_path(path, child.name))
            fields.append(mapped)
            diagnostics.extend(child_diagnostics)
        return (
            pa.struct(fields),
            diagnostics,
            schema_pb2.MAPPING_COMPATIBILITY_LOSSLESS,
            "protobuf message maps to an Arrow struct",
        )
    if kind == "list":
        element, diagnostics = _map_field(pa, data_type.list.element, f"{path}[]")
        element = element.with_name("item")
        fixed_length = data_type.list.fixed_length
        if fixed_length:
            if fixed_length > _INT32_MAX:
                raise ValueError(f"arrow: fixed-list field {path!r} length must be between 1 and {_INT32_MAX} elements")
            return (
                pa.list_(element, list_size=fixed_length),
                diagnostics,
                schema_pb2.MAPPING_COMPATIBILITY_LOSSLESS,
                f"fixed-cardinality protobuf repeated field maps losslessly to Arrow fixed_size_list[{fixed_length}]",
            )
        return (
            pa.list_(element),
            diagnostics,
            schema_pb2.MAPPING_COMPATIBILITY_LOSSLESS,
            "protobuf repeated field maps to an Arrow list",
        )
    if kind == "map":
        key, key_diagnostics = _map_field(pa, data_type.map.key, f"{path}.key")
        value, value_diagnostics = _map_field(pa, data_type.map.value, f"{path}.value")
        key = key.with_name("key").with_nullable(False)
        value = value.with_name("value")
        return (
            pa.map_(key, value),
            [*key_diagnostics, *value_diagnostics],
            schema_pb2.MAPPING_COMPATIBILITY_LOSSLESS,
            "protobuf map maps to an Arrow map with typed key and value children",
        )
    if kind == "timestamp":
        if data_type.timestamp.unit != schema_pb2.TIME_UNIT_NANOSECOND or data_type.timestamp.timezone != "UTC":
            raise ValueError(f"arrow: field {path!r} has unsupported timestamp unit or timezone")
        return (
            pa.timestamp("ns", tz="UTC"),
            [],
            schema_pb2.MAPPING_COMPATIBILITY_RANGE_REDUCED,
            "Arrow timestamp[ns, tz=UTC] preserves nanosecond precision but its int64 range is narrower than "
            "protobuf Timestamp",
        )
    if kind == "duration":
        if data_type.duration.unit != schema_pb2.TIME_UNIT_NANOSECOND:
            raise ValueError(f"arrow: field {path!r} has unsupported duration unit")
        return (
            pa.duration("ns"),
            [],
            schema_pb2.MAPPING_COMPATIBILITY_RANGE_REDUCED,
            "Arrow duration[ns] preserves nanosecond precision but its int64 range is narrower than protobuf Duration",
        )
    if kind == "json":
        return (
            pa.json_(pa.string()),
            [],
            schema_pb2.MAPPING_COMPATIBILITY_RANGE_REDUCED,
            f"protobuf {schema_pb2.JsonKind.Name(data_type.json.kind)} is encoded as RFC 8259 text in Arrow's "
            f"canonical JSON extension type; {_json_range_reduction(data_type.json.kind)}",
        )
    if kind == "decimal":
        precision, scale = _decimal_parameters(data_type, path)
        mapped = pa.decimal128(precision, scale)
        return (
            mapped,
            [],
            schema_pb2.MAPPING_COMPATIBILITY_REPRESENTATION_CHANGED,
            f"canonical decimal text is decoded into Arrow {mapped}; precision and scale are preserved but the physical representation changes",
        )
    if kind == "uuid":
        uuid_type = getattr(pa, "uuid", None)
        if uuid_type is None:
            raise ValueError(f"arrow: field {path!r} requires a PyArrow release with UUID support")
        return (
            uuid_type(),
            [],
            schema_pb2.MAPPING_COMPATIBILITY_REPRESENTATION_CHANGED,
            "canonical UUID text is decoded into Arrow's UUID extension type over fixed-size binary[16]",
        )
    if kind == "fixed_bytes":
        byte_length = _fixed_bytes_length(data_type, path)
        mapped = pa.binary(byte_length)
        return (
            mapped,
            [],
            schema_pb2.MAPPING_COMPATIBILITY_LOSSLESS,
            f"exact-width protobuf bytes map losslessly to Arrow {mapped}",
        )
    raise ValueError(f"arrow: field {path!r} has an unspecified logical type")


def _json_range_reduction(kind: schema_pb2.JsonKind) -> str:
    if kind == schema_pb2.JSON_KIND_ANY:
        return (
            "standard protobuf JSON requires each populated Any type URL to resolve to a known message descriptor; "
            "embedded Struct, Value, and ListValue numbers must also be finite"
        )
    if kind in {schema_pb2.JSON_KIND_STRUCT, schema_pb2.JSON_KIND_VALUE, schema_pb2.JSON_KIND_LIST_VALUE}:
        return (
            "standard protobuf JSON requires Struct, Value, and ListValue numbers to be finite; "
            "NaN and infinities are not representable"
        )
    return "standard protobuf JSON requires an explicitly supported dynamic JSON kind"


def _map_primitive(pa: Any, kind: schema_pb2.PrimitiveKind) -> Any:
    if kind == schema_pb2.PRIMITIVE_KIND_DOUBLE:
        return pa.float64()
    if kind == schema_pb2.PRIMITIVE_KIND_FLOAT:
        return pa.float32()
    if kind in {
        schema_pb2.PRIMITIVE_KIND_INT64,
        schema_pb2.PRIMITIVE_KIND_SFIXED64,
        schema_pb2.PRIMITIVE_KIND_SINT64,
    }:
        return pa.int64()
    if kind in {schema_pb2.PRIMITIVE_KIND_UINT64, schema_pb2.PRIMITIVE_KIND_FIXED64}:
        return pa.uint64()
    if kind in {
        schema_pb2.PRIMITIVE_KIND_INT32,
        schema_pb2.PRIMITIVE_KIND_SFIXED32,
        schema_pb2.PRIMITIVE_KIND_SINT32,
    }:
        return pa.int32()
    if kind in {schema_pb2.PRIMITIVE_KIND_UINT32, schema_pb2.PRIMITIVE_KIND_FIXED32}:
        return pa.uint32()
    if kind == schema_pb2.PRIMITIVE_KIND_BOOL:
        return pa.bool_()
    if kind == schema_pb2.PRIMITIVE_KIND_STRING:
        return pa.string()
    if kind == schema_pb2.PRIMITIVE_KIND_BYTES:
        return pa.binary()
    raise ValueError(f"arrow: unsupported primitive kind {schema_pb2.PrimitiveKind.Name(kind)}")


def _validate_message_descriptor(
    dataset: DatasetSchema,
    descriptor: Any,
    field_protos: dict[int, tuple[Any, dict[str, descriptor_pb2.FieldDescriptorProto]]],
) -> None:
    """Prove that generated values use the descriptor that produced the bundle.

    A protobuf full name alone is not an identity: two generated modules can
    contain different revisions of the same message. The SchemaBundle retains
    enough logical descriptor information to reject that split before PyArrow
    has an opportunity to coerce a stale value into the canonical schema.
    """
    if descriptor is None:
        raise TypeError("arrow: value does not expose a protobuf message descriptor")
    if descriptor.full_name != dataset.source_message:
        raise TypeError(
            f"arrow: dataset {dataset.source_message!r} cannot convert protobuf message {descriptor.full_name!r}"
        )
    _validate_message_fields(dataset.fields, descriptor, (), dataset.source_message, field_protos)


def _validate_message_fields(
    logical_fields: Any,
    descriptor: Any,
    parent_number_path: tuple[int, ...],
    path: str,
    field_protos: dict[int, tuple[Any, dict[str, descriptor_pb2.FieldDescriptorProto]]],
) -> None:
    logical_by_number: dict[int, Field] = {}
    for logical in logical_fields:
        if logical.synthetic_role != schema_pb2.SYNTHETIC_ROLE_PROTO_FIELD:
            _descriptor_mismatch(path, f"contains non-protobuf field {logical.name!r}")
        if len(logical.proto_number_path) != len(parent_number_path) + 1:
            _descriptor_mismatch(path, f"field {logical.name!r} has an invalid protobuf number path")
        number = logical.proto_number_path[-1]
        if tuple(logical.proto_number_path[:-1]) != parent_number_path:
            _descriptor_mismatch(path, f"field {logical.name!r} has an invalid protobuf number path")
        if number in logical_by_number:
            _descriptor_mismatch(path, f"duplicates protobuf field number {number}")
        logical_by_number[number] = logical

    descriptor_by_number = descriptor.fields_by_number
    logical_numbers = set(logical_by_number)
    descriptor_numbers = set(descriptor_by_number)
    if logical_numbers != descriptor_numbers:
        missing = sorted(descriptor_numbers - logical_numbers)
        extra = sorted(logical_numbers - descriptor_numbers)
        _descriptor_mismatch(path, f"field numbers differ: missing={missing}, extra={extra}")

    for number, logical in logical_by_number.items():
        actual = descriptor_by_number[number]
        field_path = _join_path(path, logical.name)
        if logical.proto_full_name != actual.full_name:
            _descriptor_mismatch(
                field_path,
                f"protobuf field name is {actual.full_name!r}, expected {logical.proto_full_name!r}",
            )
        if logical.json_name != actual.json_name:
            _descriptor_mismatch(
                field_path,
                f"protobuf JSON name is {actual.json_name!r}, expected {logical.json_name!r}",
            )

        field_proto = _field_proto(actual, field_protos)
        expected_presence, expected_nullable, expected_oneof = _descriptor_presence(actual, field_proto)
        if logical.presence != expected_presence or logical.nullable != expected_nullable:
            _descriptor_mismatch(
                field_path,
                "protobuf presence does not match the SchemaBundle",
            )
        if logical.oneof != expected_oneof:
            _descriptor_mismatch(
                field_path,
                f"protobuf oneof is {expected_oneof!r}, expected {logical.oneof!r}",
            )
        has_default = field_proto.HasField("default_value")
        if logical.has_default != has_default:
            _descriptor_mismatch(field_path, "protobuf declared-default presence does not match the SchemaBundle")
        if has_default and logical.protobuf_default != field_proto.default_value:
            _descriptor_mismatch(
                field_path,
                f"protobuf default is {field_proto.default_value!r}, expected {logical.protobuf_default!r}",
            )

        number_path = (*parent_number_path, number)
        _validate_field_type(logical, actual, number_path, field_path, field_protos)
        _validate_field_refinement(logical, field_proto, field_path)


def _validate_field_type(
    logical: Field,
    actual: Any,
    number_path: tuple[int, ...],
    path: str,
    field_protos: dict[int, tuple[Any, dict[str, descriptor_pb2.FieldDescriptorProto]]],
) -> None:
    if not logical.HasField("type"):
        _descriptor_mismatch(path, "SchemaBundle field has no logical type")

    if _is_map_field(actual):
        if logical.type.WhichOneof("kind") != "map" or logical.type.protobuf_type:
            _descriptor_mismatch(path, "protobuf map does not match the SchemaBundle logical type")
        entry = actual.message_type
        _validate_synthetic_field(
            logical.type.map.key,
            entry.fields_by_name["key"],
            number_path,
            f"{path}.key",
            schema_pb2.SYNTHETIC_ROLE_MAP_KEY,
            f"{actual.full_name}.key",
            field_protos,
        )
        _validate_synthetic_field(
            logical.type.map.value,
            entry.fields_by_name["value"],
            number_path,
            f"{path}.value",
            schema_pb2.SYNTHETIC_ROLE_MAP_VALUE,
            f"{actual.full_name}.value",
            field_protos,
        )
        return

    if actual.is_repeated:
        if logical.type.WhichOneof("kind") != "list" or logical.type.protobuf_type:
            _descriptor_mismatch(path, "protobuf repeated field does not match the SchemaBundle logical type")
        _validate_synthetic_field(
            logical.type.list.element,
            actual,
            number_path,
            f"{path}[]",
            schema_pb2.SYNTHETIC_ROLE_LIST_ELEMENT,
            f"{actual.full_name}[]",
            field_protos,
        )
        return

    _validate_value_type(logical.type, actual, number_path, path, field_protos)


def _validate_field_refinement(
    logical: Field,
    actual: descriptor_pb2.FieldDescriptorProto,
    path: str,
) -> None:
    expected = annotations_pb2.FieldOptions()
    data_type = logical.type
    kind = data_type.WhichOneof("kind")
    if kind == "list":
        if data_type.list.fixed_length:
            expected.fixed_list.length = data_type.list.fixed_length
        else:
            data_type = data_type.list.element.type
            kind = data_type.WhichOneof("kind")

    if kind == "decimal":
        expected.decimal.precision = data_type.decimal.precision
        expected.decimal.scale = data_type.decimal.scale
    elif kind == "uuid":
        expected.uuid.SetInParent()
    elif kind == "fixed_bytes":
        expected.fixed_bytes.byte_length = data_type.fixed_bytes.byte_length

    expected_present = bool(expected.ListFields())
    actual_present = actual.options.HasExtension(annotations_pb2.field)  # type: ignore[arg-type]
    if actual_present != expected_present:
        _descriptor_mismatch(path, "Invariant field refinement does not match the SchemaBundle")
    if actual_present:
        actual_refinement: Any = actual.options.Extensions[annotations_pb2.field]  # type: ignore[index]
        if actual_refinement.SerializeToString(deterministic=True) != expected.SerializeToString(deterministic=True):
            _descriptor_mismatch(path, "Invariant field refinement does not match the SchemaBundle")


def _validate_synthetic_field(
    logical: Field,
    actual: Any,
    number_path: tuple[int, ...],
    path: str,
    role: schema_pb2.SyntheticRole,
    proto_full_name: str,
    field_protos: dict[int, tuple[Any, dict[str, descriptor_pb2.FieldDescriptorProto]]],
) -> None:
    if logical is None:
        _descriptor_mismatch(path, "SchemaBundle is missing a collection child")
    if logical.synthetic_role != role:
        _descriptor_mismatch(path, "SchemaBundle collection child has the wrong role")
    if tuple(logical.proto_number_path) != number_path:
        _descriptor_mismatch(path, "SchemaBundle collection child has the wrong protobuf number path")
    if logical.proto_full_name != proto_full_name:
        _descriptor_mismatch(
            path,
            f"SchemaBundle collection child name is {logical.proto_full_name!r}, expected {proto_full_name!r}",
        )
    expected_name = {
        schema_pb2.SYNTHETIC_ROLE_LIST_ELEMENT: "element",
        schema_pb2.SYNTHETIC_ROLE_MAP_KEY: "key",
        schema_pb2.SYNTHETIC_ROLE_MAP_VALUE: "value",
    }[role]
    if logical.name != expected_name:
        _descriptor_mismatch(
            path, f"SchemaBundle collection child name is {logical.name!r}, expected {expected_name!r}"
        )
    if (
        logical.presence != schema_pb2.PRESENCE_NOT_APPLICABLE
        or logical.nullable
        or logical.oneof
        or logical.has_default
        or logical.protobuf_default
        or logical.json_name
    ):
        _descriptor_mismatch(path, "SchemaBundle collection child has invalid presence or default metadata")
    if not logical.HasField("type"):
        _descriptor_mismatch(path, "SchemaBundle collection child has no logical type")
    _validate_value_type(logical.type, actual, number_path, path, field_protos)


def _validate_value_type(
    logical: DataType,
    actual: Any,
    number_path: tuple[int, ...],
    path: str,
    field_protos: dict[int, tuple[Any, dict[str, descriptor_pb2.FieldDescriptorProto]]],
) -> None:
    primitive_kind = _PRIMITIVE_KINDS.get(actual.type)
    if primitive_kind is not None:
        logical_kind = logical.WhichOneof("kind")
        if logical_kind in {"decimal", "uuid"}:
            if actual.type != protobuf_descriptor.FieldDescriptor.TYPE_STRING or logical.protobuf_type:
                _descriptor_mismatch(path, f"logical {logical_kind} requires a protobuf string carrier")
            return
        if logical_kind == "fixed_bytes":
            if actual.type != protobuf_descriptor.FieldDescriptor.TYPE_BYTES or logical.protobuf_type:
                _descriptor_mismatch(path, "logical fixed_bytes requires a protobuf bytes carrier")
            return
        if logical_kind != "primitive" or logical.primitive.kind != primitive_kind or logical.protobuf_type:
            _descriptor_mismatch(path, "protobuf primitive kind does not match the SchemaBundle")
        return

    if actual.type == protobuf_descriptor.FieldDescriptor.TYPE_ENUM:
        enum = actual.enum_type
        if (
            logical.WhichOneof("kind") != "enum"
            or logical.protobuf_type != enum.full_name
            or logical.enum.full_name != enum.full_name
            or logical.enum.closed != enum.is_closed
        ):
            _descriptor_mismatch(path, "protobuf enum type does not match the SchemaBundle")
        expected_values = [(value.name, value.number) for value in enum.values]
        actual_values = [(value.name, value.number) for value in logical.enum.values]
        if actual_values != expected_values:
            _descriptor_mismatch(path, "protobuf enum values do not match the SchemaBundle")
        return

    if actual.type not in {
        protobuf_descriptor.FieldDescriptor.TYPE_MESSAGE,
        protobuf_descriptor.FieldDescriptor.TYPE_GROUP,
    }:
        _descriptor_mismatch(path, f"protobuf field type {actual.type} is unsupported")

    message = actual.message_type
    full_name = message.full_name
    wrapper_kind = _WRAPPER_KINDS.get(full_name)
    if wrapper_kind is not None:
        if (
            logical.WhichOneof("kind") != "primitive"
            or logical.protobuf_type != full_name
            or logical.primitive.kind != wrapper_kind
        ):
            _descriptor_mismatch(path, "protobuf wrapper type does not match the SchemaBundle")
        return
    if full_name == "google.protobuf.Timestamp":
        if (
            logical.WhichOneof("kind") != "timestamp"
            or logical.protobuf_type != full_name
            or logical.timestamp.unit != schema_pb2.TIME_UNIT_NANOSECOND
            or logical.timestamp.timezone != "UTC"
        ):
            _descriptor_mismatch(path, "protobuf Timestamp does not match the SchemaBundle")
        return
    if full_name == "google.protobuf.Duration":
        if (
            logical.WhichOneof("kind") != "duration"
            or logical.protobuf_type != full_name
            or logical.duration.unit != schema_pb2.TIME_UNIT_NANOSECOND
        ):
            _descriptor_mismatch(path, "protobuf Duration does not match the SchemaBundle")
        return
    json_kind = _JSON_KINDS.get(full_name)
    if json_kind is not None:
        if logical.WhichOneof("kind") != "json" or logical.protobuf_type != full_name or logical.json.kind != json_kind:
            _descriptor_mismatch(path, "protobuf JSON well-known type does not match the SchemaBundle")
        return

    if logical.WhichOneof("kind") != "struct" or logical.protobuf_type != full_name:
        _descriptor_mismatch(path, "protobuf message type does not match the SchemaBundle")
    _validate_message_fields(logical.struct.fields, message, number_path, path, field_protos)


def _descriptor_presence(actual: Any, field_proto: descriptor_pb2.FieldDescriptorProto) -> tuple[int, bool, str]:
    if _is_map_field(actual):
        return schema_pb2.PRESENCE_MAP, False, ""
    if actual.is_repeated:
        return schema_pb2.PRESENCE_REPEATED, False, ""
    if actual.is_required:
        return schema_pb2.PRESENCE_REQUIRED, False, ""
    if actual.containing_oneof is not None and not field_proto.proto3_optional:
        return schema_pb2.PRESENCE_ONEOF, True, actual.containing_oneof.name
    if actual.has_presence:
        return schema_pb2.PRESENCE_EXPLICIT, True, ""
    return schema_pb2.PRESENCE_IMPLICIT, False, ""


def _is_map_field(field: Any) -> bool:
    return bool(field.is_repeated and field.message_type is not None and field.message_type.GetOptions().map_entry)


def _field_proto(
    field: Any,
    field_protos: dict[int, tuple[Any, dict[str, descriptor_pb2.FieldDescriptorProto]]],
) -> descriptor_pb2.FieldDescriptorProto:
    file_descriptor = field.file
    descriptor_id = id(file_descriptor)
    cached = field_protos.get(descriptor_id)
    if cached is None or cached[0] is not file_descriptor:
        file_proto = descriptor_pb2.FileDescriptorProto.FromString(file_descriptor.serialized_pb)
        by_name = {}

        def add_message(message: descriptor_pb2.DescriptorProto, prefix: str) -> None:
            full_name = f"{prefix}.{message.name}" if prefix else message.name
            for item in message.field:
                by_name[f"{full_name}.{item.name}"] = item
            for nested in message.nested_type:
                add_message(nested, full_name)

        for message in file_proto.message_type:
            add_message(message, file_proto.package)
        field_protos[descriptor_id] = (file_descriptor, by_name)
    else:
        by_name = cached[1]
    try:
        return by_name[field.full_name]
    except KeyError as error:  # pragma: no cover - invalid runtime descriptors cannot normally be built
        raise ValueError(f"arrow: protobuf descriptor is missing field definition {field.full_name!r}") from error


def _descriptor_mismatch(path: str, reason: str) -> None:
    raise ValueError(f"arrow: protobuf descriptor does not match the SchemaBundle at {path!r}: {reason}")


def _message_row(
    dataset: DatasetSchema,
    message: Message,
    validated_descriptors: dict[int, Any],
    field_protos: dict[int, tuple[Any, dict[str, descriptor_pb2.FieldDescriptorProto]]],
) -> dict[str, Any]:
    descriptor = getattr(message, "DESCRIPTOR", None)
    actual_type = getattr(descriptor, "full_name", None)
    if actual_type != dataset.source_message:
        raise TypeError(f"arrow: dataset {dataset.source_message!r} cannot convert protobuf message {actual_type!r}")
    descriptor_id = id(descriptor)
    if validated_descriptors.get(descriptor_id) is not descriptor:
        _validate_message_descriptor(dataset, descriptor, field_protos)
        validated_descriptors[descriptor_id] = descriptor
    if not message.IsInitialized():
        # Present in the protobuf runtime; types-protobuf omits this legacy
        # compatibility spelling from its base Message stub.
        missing = ", ".join(message.FindInitializationErrors())  # type: ignore[attr-defined]
        raise ValueError(f"arrow: protobuf message {actual_type!r} is missing required fields: {missing}")
    return {field.name: _field_value(message, field, _join_path(dataset.name, field.name)) for field in dataset.fields}


def _field_value(message: Message, field: Field, path: str) -> Any:
    if not field.proto_number_path:
        raise ValueError(f"arrow: field {path!r} has no protobuf number path")
    descriptor = message.DESCRIPTOR.fields_by_number.get(field.proto_number_path[-1])
    if descriptor is None:
        raise ValueError(
            f"arrow: protobuf message {message.DESCRIPTOR.full_name!r} has no field number "
            f"{field.proto_number_path[-1]} for {path!r}"
        )

    if field.presence == schema_pb2.PRESENCE_ONEOF:
        if message.WhichOneof(field.oneof) != descriptor.name:
            return None
    elif field.presence == schema_pb2.PRESENCE_EXPLICIT and not message.HasField(descriptor.name):
        return None

    return _convert_value(getattr(message, descriptor.name), field.type, path, field.proto_full_name)


def _convert_value(value: Any, data_type: DataType, path: str, source: str) -> Any:
    kind = data_type.WhichOneof("kind")
    if kind == "primitive":
        if data_type.protobuf_type in _WRAPPER_TYPES:
            return value.value
        return value
    if kind == "enum":
        return int(value)
    if kind == "struct":
        if not isinstance(value, Message):
            raise TypeError(f"arrow: struct field {path!r} is not a protobuf message")
        return {
            field.name: _field_value(value, field, _join_path(path, field.name)) for field in data_type.struct.fields
        }
    if kind == "list":
        element = data_type.list.element
        fixed_length = data_type.list.fixed_length
        if fixed_length and len(value) != fixed_length:
            raise ValueError(
                f"arrow: fixed-list field {path!r} has {len(value)} elements; expected exactly {fixed_length}"
            )
        return [_convert_value(item, element.type, f"{path}[]", element.proto_full_name) for item in value]
    if kind == "map":
        key = data_type.map.key
        item = data_type.map.value
        return [
            (
                _convert_value(entry_key, key.type, f"{path}.key", key.proto_full_name),
                _convert_value(entry_value, item.type, f"{path}.value", item.proto_full_name),
            )
            for entry_key, entry_value in sorted(value.items(), key=lambda entry: entry[0])
        ]
    if kind == "timestamp":
        return _timestamp_nanoseconds(value.seconds, value.nanos, path)
    if kind == "duration":
        return _duration_nanoseconds(value.seconds, value.nanos, path)
    if kind == "json":
        if not isinstance(value, Message):
            raise TypeError(f"arrow: JSON field {path!r} is not a protobuf message")
        try:
            return json_format.MessageToJson(
                value,
                preserving_proto_field_name=False,
                indent=None,
                sort_keys=True,
                descriptor_pool=value.DESCRIPTOR.file.pool,  # type: ignore[arg-type]
                ensure_ascii=False,
            )
        except (DecodeError, TypeError, ValueError) as error:
            raise ValueError(
                f"arrow: protobuf JSON field {path!r} from {source!r} ({data_type.protobuf_type}) "
                f"is outside the canonical ProtoJSON domain: {_json_range_reduction(data_type.json.kind)}: {error}"
            ) from error
    if kind == "decimal":
        return _decimal_value(value, data_type, path)
    if kind == "uuid":
        return _uuid_value(value, path)
    if kind == "fixed_bytes":
        byte_length = _fixed_bytes_length(data_type, path)
        if not isinstance(value, bytes):
            raise TypeError(f"arrow: fixed-bytes field {path!r} is not protobuf bytes")
        if len(value) != byte_length:
            raise ValueError(
                f"arrow: fixed-bytes field {path!r} has length {len(value)}; expected exactly {byte_length} bytes"
            )
        return value
    raise ValueError(f"arrow: field {path!r} has an unspecified logical type")


def _decimal_parameters(data_type: DataType, path: str) -> tuple[int, int]:
    precision = data_type.decimal.precision
    scale = data_type.decimal.scale
    if precision < 1 or precision > 38:
        raise ValueError(f"arrow: decimal field {path!r} precision must be between 1 and 38")
    if scale > precision:
        raise ValueError(f"arrow: decimal field {path!r} scale must not exceed precision {precision}")
    return precision, scale


def _decimal_value(value: Any, data_type: DataType, path: str) -> Decimal:
    precision, scale = _decimal_parameters(data_type, path)
    if not isinstance(value, str):
        raise TypeError(f"arrow: decimal field {path!r} is not protobuf text")

    if scale == 0:
        canonical = re.fullmatch(r"-?(?:0|[1-9][0-9]*)", value) is not None
    else:
        canonical = re.fullmatch(rf"-?(?:0|[1-9][0-9]*)\.[0-9]{{{scale}}}", value) is not None
    if not canonical:
        raise ValueError(f"arrow: decimal field {path!r} is not canonical decimal({precision}, {scale}) text")

    digits = value.removeprefix("-").replace(".", "")
    if value.startswith("-") and not digits.strip("0"):
        raise ValueError(f"arrow: decimal field {path!r} uses the non-canonical negative-zero spelling")
    significant = digits.lstrip("0")
    if len(significant) > precision:
        raise ValueError(f"arrow: decimal field {path!r} exceeds precision {precision}")
    unscaled = int(digits)
    if unscaled >= 10**precision:  # Defensive: the digit check above avoids unbounded integer parsing.
        raise ValueError(f"arrow: decimal field {path!r} exceeds precision {precision}")
    return Decimal(value)


def _uuid_value(value: Any, path: str) -> UUID:
    if not isinstance(value, str):
        raise TypeError(f"arrow: UUID field {path!r} is not protobuf text")
    try:
        parsed = UUID(value)
    except ValueError as error:
        raise ValueError(f"arrow: UUID field {path!r} is not valid canonical UUID text") from error
    if str(parsed) != value:
        raise ValueError(f"arrow: UUID field {path!r} is not lowercase hyphenated canonical UUID text")
    return parsed


def _fixed_bytes_length(data_type: DataType, path: str) -> int:
    byte_length = data_type.fixed_bytes.byte_length
    if byte_length < 1 or byte_length > _INT32_MAX:
        raise ValueError(f"arrow: fixed-bytes field {path!r} length must be between 1 and {_INT32_MAX} bytes")
    return byte_length


def _checked_nanoseconds(seconds: int, nanos: int, path: str, logical_type: str) -> int:
    value = seconds * _NANOS_PER_SECOND + nanos
    if value < _INT64_MIN or value > _INT64_MAX:
        raise ValueError(
            f"arrow: protobuf {logical_type} field {path!r} is outside Arrow's signed int64 nanosecond range"
        )
    return value


def _timestamp_nanoseconds(seconds: int, nanos: int, path: str) -> int:
    if seconds < _TIMESTAMP_SECONDS_MIN or seconds > _TIMESTAMP_SECONDS_MAX:
        raise ValueError(
            f"arrow: protobuf Timestamp field {path!r} is invalid: seconds must be in "
            f"[{_TIMESTAMP_SECONDS_MIN}, {_TIMESTAMP_SECONDS_MAX}]"
        )
    if nanos < 0 or nanos >= _NANOS_PER_SECOND:
        raise ValueError(f"arrow: protobuf Timestamp field {path!r} is invalid: nanos must be in [0, 999999999]")
    return _checked_nanoseconds(seconds, nanos, path, "Timestamp")


def _duration_nanoseconds(seconds: int, nanos: int, path: str) -> int:
    if seconds < -_DURATION_SECONDS_MAX or seconds > _DURATION_SECONDS_MAX:
        raise ValueError(
            f"arrow: protobuf Duration field {path!r} is invalid: seconds must be in "
            f"[-{_DURATION_SECONDS_MAX}, {_DURATION_SECONDS_MAX}]"
        )
    if nanos <= -_NANOS_PER_SECOND or nanos >= _NANOS_PER_SECOND:
        raise ValueError(
            f"arrow: protobuf Duration field {path!r} is invalid: nanos must be in [-999999999, 999999999]"
        )
    if (seconds > 0 and nanos < 0) or (seconds < 0 and nanos > 0):
        raise ValueError(f"arrow: protobuf Duration field {path!r} is invalid: seconds and nanos signs disagree")
    return _checked_nanoseconds(seconds, nanos, path, "Duration")


def _compact_metadata(values: dict[str, str]) -> dict[str, str]:
    return {key: value for key, value in values.items() if value != ""}


def _join_path(parent: str, child: str) -> str:
    return f"{parent}.{child}" if parent else child


def _logical_type_name(data_type: DataType) -> str:
    kind = data_type.WhichOneof("kind")
    if kind == "list" and data_type.list.fixed_length:
        return "fixed_list"
    return kind or "unspecified"


__all__ = ["arrow_record_batch_reader", "arrow_schema", "arrow_table"]
