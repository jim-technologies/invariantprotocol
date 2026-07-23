from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class Presence(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    PRESENCE_UNSPECIFIED: _ClassVar[Presence]
    PRESENCE_IMPLICIT: _ClassVar[Presence]
    PRESENCE_EXPLICIT: _ClassVar[Presence]
    PRESENCE_REQUIRED: _ClassVar[Presence]
    PRESENCE_ONEOF: _ClassVar[Presence]
    PRESENCE_REPEATED: _ClassVar[Presence]
    PRESENCE_MAP: _ClassVar[Presence]
    PRESENCE_NOT_APPLICABLE: _ClassVar[Presence]

class SyntheticRole(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    SYNTHETIC_ROLE_UNSPECIFIED: _ClassVar[SyntheticRole]
    SYNTHETIC_ROLE_PROTO_FIELD: _ClassVar[SyntheticRole]
    SYNTHETIC_ROLE_LIST_ELEMENT: _ClassVar[SyntheticRole]
    SYNTHETIC_ROLE_MAP_KEY: _ClassVar[SyntheticRole]
    SYNTHETIC_ROLE_MAP_VALUE: _ClassVar[SyntheticRole]

class PrimitiveKind(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    PRIMITIVE_KIND_UNSPECIFIED: _ClassVar[PrimitiveKind]
    PRIMITIVE_KIND_DOUBLE: _ClassVar[PrimitiveKind]
    PRIMITIVE_KIND_FLOAT: _ClassVar[PrimitiveKind]
    PRIMITIVE_KIND_INT64: _ClassVar[PrimitiveKind]
    PRIMITIVE_KIND_UINT64: _ClassVar[PrimitiveKind]
    PRIMITIVE_KIND_INT32: _ClassVar[PrimitiveKind]
    PRIMITIVE_KIND_FIXED64: _ClassVar[PrimitiveKind]
    PRIMITIVE_KIND_FIXED32: _ClassVar[PrimitiveKind]
    PRIMITIVE_KIND_BOOL: _ClassVar[PrimitiveKind]
    PRIMITIVE_KIND_STRING: _ClassVar[PrimitiveKind]
    PRIMITIVE_KIND_BYTES: _ClassVar[PrimitiveKind]
    PRIMITIVE_KIND_UINT32: _ClassVar[PrimitiveKind]
    PRIMITIVE_KIND_SFIXED32: _ClassVar[PrimitiveKind]
    PRIMITIVE_KIND_SFIXED64: _ClassVar[PrimitiveKind]
    PRIMITIVE_KIND_SINT32: _ClassVar[PrimitiveKind]
    PRIMITIVE_KIND_SINT64: _ClassVar[PrimitiveKind]

class TimeUnit(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    TIME_UNIT_UNSPECIFIED: _ClassVar[TimeUnit]
    TIME_UNIT_NANOSECOND: _ClassVar[TimeUnit]

class JsonKind(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    JSON_KIND_UNSPECIFIED: _ClassVar[JsonKind]
    JSON_KIND_ANY: _ClassVar[JsonKind]
    JSON_KIND_STRUCT: _ClassVar[JsonKind]
    JSON_KIND_VALUE: _ClassVar[JsonKind]
    JSON_KIND_LIST_VALUE: _ClassVar[JsonKind]

class MappingCompatibility(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    MAPPING_COMPATIBILITY_UNSPECIFIED: _ClassVar[MappingCompatibility]
    MAPPING_COMPATIBILITY_LOSSLESS: _ClassVar[MappingCompatibility]
    MAPPING_COMPATIBILITY_RANGE_WIDENED: _ClassVar[MappingCompatibility]
    MAPPING_COMPATIBILITY_PRECISION_REDUCED: _ClassVar[MappingCompatibility]
    MAPPING_COMPATIBILITY_REPRESENTATION_CHANGED: _ClassVar[MappingCompatibility]
    MAPPING_COMPATIBILITY_UNSUPPORTED: _ClassVar[MappingCompatibility]
    MAPPING_COMPATIBILITY_RANGE_REDUCED: _ClassVar[MappingCompatibility]
PRESENCE_UNSPECIFIED: Presence
PRESENCE_IMPLICIT: Presence
PRESENCE_EXPLICIT: Presence
PRESENCE_REQUIRED: Presence
PRESENCE_ONEOF: Presence
PRESENCE_REPEATED: Presence
PRESENCE_MAP: Presence
PRESENCE_NOT_APPLICABLE: Presence
SYNTHETIC_ROLE_UNSPECIFIED: SyntheticRole
SYNTHETIC_ROLE_PROTO_FIELD: SyntheticRole
SYNTHETIC_ROLE_LIST_ELEMENT: SyntheticRole
SYNTHETIC_ROLE_MAP_KEY: SyntheticRole
SYNTHETIC_ROLE_MAP_VALUE: SyntheticRole
PRIMITIVE_KIND_UNSPECIFIED: PrimitiveKind
PRIMITIVE_KIND_DOUBLE: PrimitiveKind
PRIMITIVE_KIND_FLOAT: PrimitiveKind
PRIMITIVE_KIND_INT64: PrimitiveKind
PRIMITIVE_KIND_UINT64: PrimitiveKind
PRIMITIVE_KIND_INT32: PrimitiveKind
PRIMITIVE_KIND_FIXED64: PrimitiveKind
PRIMITIVE_KIND_FIXED32: PrimitiveKind
PRIMITIVE_KIND_BOOL: PrimitiveKind
PRIMITIVE_KIND_STRING: PrimitiveKind
PRIMITIVE_KIND_BYTES: PrimitiveKind
PRIMITIVE_KIND_UINT32: PrimitiveKind
PRIMITIVE_KIND_SFIXED32: PrimitiveKind
PRIMITIVE_KIND_SFIXED64: PrimitiveKind
PRIMITIVE_KIND_SINT32: PrimitiveKind
PRIMITIVE_KIND_SINT64: PrimitiveKind
TIME_UNIT_UNSPECIFIED: TimeUnit
TIME_UNIT_NANOSECOND: TimeUnit
JSON_KIND_UNSPECIFIED: JsonKind
JSON_KIND_ANY: JsonKind
JSON_KIND_STRUCT: JsonKind
JSON_KIND_VALUE: JsonKind
JSON_KIND_LIST_VALUE: JsonKind
MAPPING_COMPATIBILITY_UNSPECIFIED: MappingCompatibility
MAPPING_COMPATIBILITY_LOSSLESS: MappingCompatibility
MAPPING_COMPATIBILITY_RANGE_WIDENED: MappingCompatibility
MAPPING_COMPATIBILITY_PRECISION_REDUCED: MappingCompatibility
MAPPING_COMPATIBILITY_REPRESENTATION_CHANGED: MappingCompatibility
MAPPING_COMPATIBILITY_UNSUPPORTED: MappingCompatibility
MAPPING_COMPATIBILITY_RANGE_REDUCED: MappingCompatibility

class SchemaBundle(_message.Message):
    __slots__ = ("ir_version", "mapping_version", "source_descriptor_sha256", "datasets")
    IR_VERSION_FIELD_NUMBER: _ClassVar[int]
    MAPPING_VERSION_FIELD_NUMBER: _ClassVar[int]
    SOURCE_DESCRIPTOR_SHA256_FIELD_NUMBER: _ClassVar[int]
    DATASETS_FIELD_NUMBER: _ClassVar[int]
    ir_version: int
    mapping_version: int
    source_descriptor_sha256: bytes
    datasets: _containers.RepeatedCompositeFieldContainer[DatasetSchema]
    def __init__(self, ir_version: _Optional[int] = ..., mapping_version: _Optional[int] = ..., source_descriptor_sha256: _Optional[bytes] = ..., datasets: _Optional[_Iterable[_Union[DatasetSchema, _Mapping]]] = ...) -> None: ...

class DatasetSchema(_message.Message):
    __slots__ = ("source_message", "name", "description", "fields", "last_field_id", "retired_fields")
    SOURCE_MESSAGE_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    DESCRIPTION_FIELD_NUMBER: _ClassVar[int]
    FIELDS_FIELD_NUMBER: _ClassVar[int]
    LAST_FIELD_ID_FIELD_NUMBER: _ClassVar[int]
    RETIRED_FIELDS_FIELD_NUMBER: _ClassVar[int]
    source_message: str
    name: str
    description: str
    fields: _containers.RepeatedCompositeFieldContainer[Field]
    last_field_id: int
    retired_fields: _containers.RepeatedCompositeFieldContainer[RetiredField]
    def __init__(self, source_message: _Optional[str] = ..., name: _Optional[str] = ..., description: _Optional[str] = ..., fields: _Optional[_Iterable[_Union[Field, _Mapping]]] = ..., last_field_id: _Optional[int] = ..., retired_fields: _Optional[_Iterable[_Union[RetiredField, _Mapping]]] = ...) -> None: ...

class RetiredField(_message.Message):
    __slots__ = ("identity", "stable_id")
    IDENTITY_FIELD_NUMBER: _ClassVar[int]
    STABLE_ID_FIELD_NUMBER: _ClassVar[int]
    identity: str
    stable_id: int
    def __init__(self, identity: _Optional[str] = ..., stable_id: _Optional[int] = ...) -> None: ...

class Field(_message.Message):
    __slots__ = ("proto_full_name", "proto_number_path", "name", "stable_id", "presence", "nullable", "oneof", "description", "type", "synthetic_role", "has_default", "protobuf_default", "json_name")
    PROTO_FULL_NAME_FIELD_NUMBER: _ClassVar[int]
    PROTO_NUMBER_PATH_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    STABLE_ID_FIELD_NUMBER: _ClassVar[int]
    PRESENCE_FIELD_NUMBER: _ClassVar[int]
    NULLABLE_FIELD_NUMBER: _ClassVar[int]
    ONEOF_FIELD_NUMBER: _ClassVar[int]
    DESCRIPTION_FIELD_NUMBER: _ClassVar[int]
    TYPE_FIELD_NUMBER: _ClassVar[int]
    SYNTHETIC_ROLE_FIELD_NUMBER: _ClassVar[int]
    HAS_DEFAULT_FIELD_NUMBER: _ClassVar[int]
    PROTOBUF_DEFAULT_FIELD_NUMBER: _ClassVar[int]
    JSON_NAME_FIELD_NUMBER: _ClassVar[int]
    proto_full_name: str
    proto_number_path: _containers.RepeatedScalarFieldContainer[int]
    name: str
    stable_id: int
    presence: Presence
    nullable: bool
    oneof: str
    description: str
    type: DataType
    synthetic_role: SyntheticRole
    has_default: bool
    protobuf_default: str
    json_name: str
    def __init__(self, proto_full_name: _Optional[str] = ..., proto_number_path: _Optional[_Iterable[int]] = ..., name: _Optional[str] = ..., stable_id: _Optional[int] = ..., presence: _Optional[_Union[Presence, str]] = ..., nullable: _Optional[bool] = ..., oneof: _Optional[str] = ..., description: _Optional[str] = ..., type: _Optional[_Union[DataType, _Mapping]] = ..., synthetic_role: _Optional[_Union[SyntheticRole, str]] = ..., has_default: _Optional[bool] = ..., protobuf_default: _Optional[str] = ..., json_name: _Optional[str] = ...) -> None: ...

class DataType(_message.Message):
    __slots__ = ("protobuf_type", "primitive", "enum", "struct", "list", "map", "timestamp", "duration", "json", "decimal", "uuid", "fixed_bytes")
    PROTOBUF_TYPE_FIELD_NUMBER: _ClassVar[int]
    PRIMITIVE_FIELD_NUMBER: _ClassVar[int]
    ENUM_FIELD_NUMBER: _ClassVar[int]
    STRUCT_FIELD_NUMBER: _ClassVar[int]
    LIST_FIELD_NUMBER: _ClassVar[int]
    MAP_FIELD_NUMBER: _ClassVar[int]
    TIMESTAMP_FIELD_NUMBER: _ClassVar[int]
    DURATION_FIELD_NUMBER: _ClassVar[int]
    JSON_FIELD_NUMBER: _ClassVar[int]
    DECIMAL_FIELD_NUMBER: _ClassVar[int]
    UUID_FIELD_NUMBER: _ClassVar[int]
    FIXED_BYTES_FIELD_NUMBER: _ClassVar[int]
    protobuf_type: str
    primitive: PrimitiveType
    enum: EnumType
    struct: StructType
    list: ListType
    map: MapType
    timestamp: TimestampType
    duration: DurationType
    json: JsonType
    decimal: DecimalType
    uuid: UuidType
    fixed_bytes: FixedBytesType
    def __init__(self, protobuf_type: _Optional[str] = ..., primitive: _Optional[_Union[PrimitiveType, _Mapping]] = ..., enum: _Optional[_Union[EnumType, _Mapping]] = ..., struct: _Optional[_Union[StructType, _Mapping]] = ..., list: _Optional[_Union[ListType, _Mapping]] = ..., map: _Optional[_Union[MapType, _Mapping]] = ..., timestamp: _Optional[_Union[TimestampType, _Mapping]] = ..., duration: _Optional[_Union[DurationType, _Mapping]] = ..., json: _Optional[_Union[JsonType, _Mapping]] = ..., decimal: _Optional[_Union[DecimalType, _Mapping]] = ..., uuid: _Optional[_Union[UuidType, _Mapping]] = ..., fixed_bytes: _Optional[_Union[FixedBytesType, _Mapping]] = ...) -> None: ...

class DecimalType(_message.Message):
    __slots__ = ("precision", "scale")
    PRECISION_FIELD_NUMBER: _ClassVar[int]
    SCALE_FIELD_NUMBER: _ClassVar[int]
    precision: int
    scale: int
    def __init__(self, precision: _Optional[int] = ..., scale: _Optional[int] = ...) -> None: ...

class UuidType(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class FixedBytesType(_message.Message):
    __slots__ = ("byte_length",)
    BYTE_LENGTH_FIELD_NUMBER: _ClassVar[int]
    byte_length: int
    def __init__(self, byte_length: _Optional[int] = ...) -> None: ...

class PrimitiveType(_message.Message):
    __slots__ = ("kind",)
    KIND_FIELD_NUMBER: _ClassVar[int]
    kind: PrimitiveKind
    def __init__(self, kind: _Optional[_Union[PrimitiveKind, str]] = ...) -> None: ...

class EnumType(_message.Message):
    __slots__ = ("full_name", "values", "closed")
    FULL_NAME_FIELD_NUMBER: _ClassVar[int]
    VALUES_FIELD_NUMBER: _ClassVar[int]
    CLOSED_FIELD_NUMBER: _ClassVar[int]
    full_name: str
    values: _containers.RepeatedCompositeFieldContainer[EnumValue]
    closed: bool
    def __init__(self, full_name: _Optional[str] = ..., values: _Optional[_Iterable[_Union[EnumValue, _Mapping]]] = ..., closed: _Optional[bool] = ...) -> None: ...

class EnumValue(_message.Message):
    __slots__ = ("name", "number", "description")
    NAME_FIELD_NUMBER: _ClassVar[int]
    NUMBER_FIELD_NUMBER: _ClassVar[int]
    DESCRIPTION_FIELD_NUMBER: _ClassVar[int]
    name: str
    number: int
    description: str
    def __init__(self, name: _Optional[str] = ..., number: _Optional[int] = ..., description: _Optional[str] = ...) -> None: ...

class StructType(_message.Message):
    __slots__ = ("fields",)
    FIELDS_FIELD_NUMBER: _ClassVar[int]
    fields: _containers.RepeatedCompositeFieldContainer[Field]
    def __init__(self, fields: _Optional[_Iterable[_Union[Field, _Mapping]]] = ...) -> None: ...

class ListType(_message.Message):
    __slots__ = ("element",)
    ELEMENT_FIELD_NUMBER: _ClassVar[int]
    element: Field
    def __init__(self, element: _Optional[_Union[Field, _Mapping]] = ...) -> None: ...

class MapType(_message.Message):
    __slots__ = ("key", "value")
    KEY_FIELD_NUMBER: _ClassVar[int]
    VALUE_FIELD_NUMBER: _ClassVar[int]
    key: Field
    value: Field
    def __init__(self, key: _Optional[_Union[Field, _Mapping]] = ..., value: _Optional[_Union[Field, _Mapping]] = ...) -> None: ...

class TimestampType(_message.Message):
    __slots__ = ("unit", "timezone")
    UNIT_FIELD_NUMBER: _ClassVar[int]
    TIMEZONE_FIELD_NUMBER: _ClassVar[int]
    unit: TimeUnit
    timezone: str
    def __init__(self, unit: _Optional[_Union[TimeUnit, str]] = ..., timezone: _Optional[str] = ...) -> None: ...

class DurationType(_message.Message):
    __slots__ = ("unit",)
    UNIT_FIELD_NUMBER: _ClassVar[int]
    unit: TimeUnit
    def __init__(self, unit: _Optional[_Union[TimeUnit, str]] = ...) -> None: ...

class JsonType(_message.Message):
    __slots__ = ("kind",)
    KIND_FIELD_NUMBER: _ClassVar[int]
    kind: JsonKind
    def __init__(self, kind: _Optional[_Union[JsonKind, str]] = ...) -> None: ...

class MappingDiagnostic(_message.Message):
    __slots__ = ("field_path", "compatibility", "message")
    FIELD_PATH_FIELD_NUMBER: _ClassVar[int]
    COMPATIBILITY_FIELD_NUMBER: _ClassVar[int]
    MESSAGE_FIELD_NUMBER: _ClassVar[int]
    field_path: str
    compatibility: MappingCompatibility
    message: str
    def __init__(self, field_path: _Optional[str] = ..., compatibility: _Optional[_Union[MappingCompatibility, str]] = ..., message: _Optional[str] = ...) -> None: ...
