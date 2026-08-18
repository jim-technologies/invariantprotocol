import datetime

from google.protobuf import any_pb2 as _any_pb2
from google.protobuf import timestamp_pb2 as _timestamp_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class Operation(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    OPERATION_UNSPECIFIED: _ClassVar[Operation]
    OPERATION_CREATE: _ClassVar[Operation]
    OPERATION_UPDATE: _ClassVar[Operation]
    OPERATION_DELETE: _ClassVar[Operation]
    OPERATION_SNAPSHOT_READ: _ClassVar[Operation]
    OPERATION_TRUNCATE: _ClassVar[Operation]
    OPERATION_SOURCE_MESSAGE: _ClassVar[Operation]
OPERATION_UNSPECIFIED: Operation
OPERATION_CREATE: Operation
OPERATION_UPDATE: Operation
OPERATION_DELETE: Operation
OPERATION_SNAPSHOT_READ: Operation
OPERATION_TRUNCATE: Operation
OPERATION_SOURCE_MESSAGE: Operation

class ChangeRecord(_message.Message):
    __slots__ = ("operation", "key", "data_collection", "schema_reference", "source_position", "transaction", "source_time", "capture_time", "source_extension", "source_message", "full", "delta")
    OPERATION_FIELD_NUMBER: _ClassVar[int]
    KEY_FIELD_NUMBER: _ClassVar[int]
    DATA_COLLECTION_FIELD_NUMBER: _ClassVar[int]
    SCHEMA_REFERENCE_FIELD_NUMBER: _ClassVar[int]
    SOURCE_POSITION_FIELD_NUMBER: _ClassVar[int]
    TRANSACTION_FIELD_NUMBER: _ClassVar[int]
    SOURCE_TIME_FIELD_NUMBER: _ClassVar[int]
    CAPTURE_TIME_FIELD_NUMBER: _ClassVar[int]
    SOURCE_EXTENSION_FIELD_NUMBER: _ClassVar[int]
    SOURCE_MESSAGE_FIELD_NUMBER: _ClassVar[int]
    FULL_FIELD_NUMBER: _ClassVar[int]
    DELTA_FIELD_NUMBER: _ClassVar[int]
    operation: Operation
    key: Record
    data_collection: DataCollection
    schema_reference: SchemaReference
    source_position: SourcePosition
    transaction: TransactionContext
    source_time: _timestamp_pb2.Timestamp
    capture_time: _timestamp_pb2.Timestamp
    source_extension: SourceExtension
    source_message: Value
    full: FullChange
    delta: DeltaChange
    def __init__(self, operation: _Optional[_Union[Operation, str]] = ..., key: _Optional[_Union[Record, _Mapping]] = ..., data_collection: _Optional[_Union[DataCollection, _Mapping]] = ..., schema_reference: _Optional[_Union[SchemaReference, _Mapping]] = ..., source_position: _Optional[_Union[SourcePosition, _Mapping]] = ..., transaction: _Optional[_Union[TransactionContext, _Mapping]] = ..., source_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., capture_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., source_extension: _Optional[_Union[SourceExtension, _Mapping]] = ..., source_message: _Optional[_Union[Value, _Mapping]] = ..., full: _Optional[_Union[FullChange, _Mapping]] = ..., delta: _Optional[_Union[DeltaChange, _Mapping]] = ...) -> None: ...

class FullChange(_message.Message):
    __slots__ = ("before", "after", "changed_fields")
    BEFORE_FIELD_NUMBER: _ClassVar[int]
    AFTER_FIELD_NUMBER: _ClassVar[int]
    CHANGED_FIELDS_FIELD_NUMBER: _ClassVar[int]
    before: Record
    after: Record
    changed_fields: ChangedFieldMask
    def __init__(self, before: _Optional[_Union[Record, _Mapping]] = ..., after: _Optional[_Union[Record, _Mapping]] = ..., changed_fields: _Optional[_Union[ChangedFieldMask, _Mapping]] = ...) -> None: ...

class DeltaChange(_message.Message):
    __slots__ = ("result", "patch", "delete")
    RESULT_FIELD_NUMBER: _ClassVar[int]
    PATCH_FIELD_NUMBER: _ClassVar[int]
    DELETE_FIELD_NUMBER: _ClassVar[int]
    result: Record
    patch: RecordPatch
    delete: DeleteDelta
    def __init__(self, result: _Optional[_Union[Record, _Mapping]] = ..., patch: _Optional[_Union[RecordPatch, _Mapping]] = ..., delete: _Optional[_Union[DeleteDelta, _Mapping]] = ...) -> None: ...

class DeleteDelta(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class RecordPatch(_message.Message):
    __slots__ = ("changes",)
    CHANGES_FIELD_NUMBER: _ClassVar[int]
    changes: _containers.RepeatedCompositeFieldContainer[FieldChange]
    def __init__(self, changes: _Optional[_Iterable[_Union[FieldChange, _Mapping]]] = ...) -> None: ...

class FieldChange(_message.Message):
    __slots__ = ("path", "before", "after")
    PATH_FIELD_NUMBER: _ClassVar[int]
    BEFORE_FIELD_NUMBER: _ClassVar[int]
    AFTER_FIELD_NUMBER: _ClassVar[int]
    path: FieldPath
    before: FieldState
    after: FieldState
    def __init__(self, path: _Optional[_Union[FieldPath, _Mapping]] = ..., before: _Optional[_Union[FieldState, _Mapping]] = ..., after: _Optional[_Union[FieldState, _Mapping]] = ...) -> None: ...

class FieldState(_message.Message):
    __slots__ = ("value", "absent")
    VALUE_FIELD_NUMBER: _ClassVar[int]
    ABSENT_FIELD_NUMBER: _ClassVar[int]
    value: Value
    absent: Absent
    def __init__(self, value: _Optional[_Union[Value, _Mapping]] = ..., absent: _Optional[_Union[Absent, _Mapping]] = ...) -> None: ...

class Absent(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class DataCollection(_message.Message):
    __slots__ = ("id",)
    ID_FIELD_NUMBER: _ClassVar[int]
    id: str
    def __init__(self, id: _Optional[str] = ...) -> None: ...

class SchemaReference(_message.Message):
    __slots__ = ("uri", "version", "fingerprint")
    URI_FIELD_NUMBER: _ClassVar[int]
    VERSION_FIELD_NUMBER: _ClassVar[int]
    FINGERPRINT_FIELD_NUMBER: _ClassVar[int]
    uri: str
    version: str
    fingerprint: bytes
    def __init__(self, uri: _Optional[str] = ..., version: _Optional[str] = ..., fingerprint: _Optional[bytes] = ...) -> None: ...

class SourcePosition(_message.Message):
    __slots__ = ("stream", "format", "value")
    STREAM_FIELD_NUMBER: _ClassVar[int]
    FORMAT_FIELD_NUMBER: _ClassVar[int]
    VALUE_FIELD_NUMBER: _ClassVar[int]
    stream: str
    format: str
    value: bytes
    def __init__(self, stream: _Optional[str] = ..., format: _Optional[str] = ..., value: _Optional[bytes] = ...) -> None: ...

class TransactionContext(_message.Message):
    __slots__ = ("id", "total_order", "data_collection_order")
    ID_FIELD_NUMBER: _ClassVar[int]
    TOTAL_ORDER_FIELD_NUMBER: _ClassVar[int]
    DATA_COLLECTION_ORDER_FIELD_NUMBER: _ClassVar[int]
    id: str
    total_order: int
    data_collection_order: int
    def __init__(self, id: _Optional[str] = ..., total_order: _Optional[int] = ..., data_collection_order: _Optional[int] = ...) -> None: ...

class ChangedFieldMask(_message.Message):
    __slots__ = ("paths",)
    PATHS_FIELD_NUMBER: _ClassVar[int]
    paths: _containers.RepeatedCompositeFieldContainer[FieldPath]
    def __init__(self, paths: _Optional[_Iterable[_Union[FieldPath, _Mapping]]] = ...) -> None: ...

class FieldPath(_message.Message):
    __slots__ = ("segments",)
    SEGMENTS_FIELD_NUMBER: _ClassVar[int]
    segments: _containers.RepeatedScalarFieldContainer[str]
    def __init__(self, segments: _Optional[_Iterable[str]] = ...) -> None: ...

class SourceExtension(_message.Message):
    __slots__ = ("proto_data", "opaque_data")
    PROTO_DATA_FIELD_NUMBER: _ClassVar[int]
    OPAQUE_DATA_FIELD_NUMBER: _ClassVar[int]
    proto_data: _any_pb2.Any
    opaque_data: OpaqueData
    def __init__(self, proto_data: _Optional[_Union[_any_pb2.Any, _Mapping]] = ..., opaque_data: _Optional[_Union[OpaqueData, _Mapping]] = ...) -> None: ...

class OpaqueData(_message.Message):
    __slots__ = ("media_type", "schema", "data")
    MEDIA_TYPE_FIELD_NUMBER: _ClassVar[int]
    SCHEMA_FIELD_NUMBER: _ClassVar[int]
    DATA_FIELD_NUMBER: _ClassVar[int]
    media_type: str
    schema: str
    data: bytes
    def __init__(self, media_type: _Optional[str] = ..., schema: _Optional[str] = ..., data: _Optional[bytes] = ...) -> None: ...

class Record(_message.Message):
    __slots__ = ("fields",)
    FIELDS_FIELD_NUMBER: _ClassVar[int]
    fields: _containers.RepeatedCompositeFieldContainer[RecordField]
    def __init__(self, fields: _Optional[_Iterable[_Union[RecordField, _Mapping]]] = ...) -> None: ...

class RecordField(_message.Message):
    __slots__ = ("name", "value")
    NAME_FIELD_NUMBER: _ClassVar[int]
    VALUE_FIELD_NUMBER: _ClassVar[int]
    name: str
    value: Value
    def __init__(self, name: _Optional[str] = ..., value: _Optional[_Union[Value, _Mapping]] = ...) -> None: ...

class Value(_message.Message):
    __slots__ = ("type_name", "null_value", "bool_value", "int32_value", "int64_value", "uint32_value", "uint64_value", "float32_value", "float64_value", "string_value", "bytes_value", "decimal_value", "timestamp_value", "record_value", "list_value", "map_value")
    TYPE_NAME_FIELD_NUMBER: _ClassVar[int]
    NULL_VALUE_FIELD_NUMBER: _ClassVar[int]
    BOOL_VALUE_FIELD_NUMBER: _ClassVar[int]
    INT32_VALUE_FIELD_NUMBER: _ClassVar[int]
    INT64_VALUE_FIELD_NUMBER: _ClassVar[int]
    UINT32_VALUE_FIELD_NUMBER: _ClassVar[int]
    UINT64_VALUE_FIELD_NUMBER: _ClassVar[int]
    FLOAT32_VALUE_FIELD_NUMBER: _ClassVar[int]
    FLOAT64_VALUE_FIELD_NUMBER: _ClassVar[int]
    STRING_VALUE_FIELD_NUMBER: _ClassVar[int]
    BYTES_VALUE_FIELD_NUMBER: _ClassVar[int]
    DECIMAL_VALUE_FIELD_NUMBER: _ClassVar[int]
    TIMESTAMP_VALUE_FIELD_NUMBER: _ClassVar[int]
    RECORD_VALUE_FIELD_NUMBER: _ClassVar[int]
    LIST_VALUE_FIELD_NUMBER: _ClassVar[int]
    MAP_VALUE_FIELD_NUMBER: _ClassVar[int]
    type_name: str
    null_value: NullValue
    bool_value: bool
    int32_value: int
    int64_value: int
    uint32_value: int
    uint64_value: int
    float32_value: float
    float64_value: float
    string_value: str
    bytes_value: bytes
    decimal_value: DecimalValue
    timestamp_value: _timestamp_pb2.Timestamp
    record_value: Record
    list_value: ListValue
    map_value: MapValue
    def __init__(self, type_name: _Optional[str] = ..., null_value: _Optional[_Union[NullValue, _Mapping]] = ..., bool_value: _Optional[bool] = ..., int32_value: _Optional[int] = ..., int64_value: _Optional[int] = ..., uint32_value: _Optional[int] = ..., uint64_value: _Optional[int] = ..., float32_value: _Optional[float] = ..., float64_value: _Optional[float] = ..., string_value: _Optional[str] = ..., bytes_value: _Optional[bytes] = ..., decimal_value: _Optional[_Union[DecimalValue, _Mapping]] = ..., timestamp_value: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., record_value: _Optional[_Union[Record, _Mapping]] = ..., list_value: _Optional[_Union[ListValue, _Mapping]] = ..., map_value: _Optional[_Union[MapValue, _Mapping]] = ...) -> None: ...

class NullValue(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class DecimalValue(_message.Message):
    __slots__ = ("value", "scale", "precision")
    VALUE_FIELD_NUMBER: _ClassVar[int]
    SCALE_FIELD_NUMBER: _ClassVar[int]
    PRECISION_FIELD_NUMBER: _ClassVar[int]
    value: str
    scale: int
    precision: int
    def __init__(self, value: _Optional[str] = ..., scale: _Optional[int] = ..., precision: _Optional[int] = ...) -> None: ...

class ListValue(_message.Message):
    __slots__ = ("values",)
    VALUES_FIELD_NUMBER: _ClassVar[int]
    values: _containers.RepeatedCompositeFieldContainer[Value]
    def __init__(self, values: _Optional[_Iterable[_Union[Value, _Mapping]]] = ...) -> None: ...

class MapValue(_message.Message):
    __slots__ = ("entries",)
    ENTRIES_FIELD_NUMBER: _ClassVar[int]
    entries: _containers.RepeatedCompositeFieldContainer[MapEntry]
    def __init__(self, entries: _Optional[_Iterable[_Union[MapEntry, _Mapping]]] = ...) -> None: ...

class MapEntry(_message.Message):
    __slots__ = ("key", "value")
    KEY_FIELD_NUMBER: _ClassVar[int]
    VALUE_FIELD_NUMBER: _ClassVar[int]
    key: Value
    value: Value
    def __init__(self, key: _Optional[_Union[Value, _Mapping]] = ..., value: _Optional[_Union[Value, _Mapping]] = ...) -> None: ...
