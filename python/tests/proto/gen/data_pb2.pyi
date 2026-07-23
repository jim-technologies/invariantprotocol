import datetime

from google.protobuf import any_pb2 as _any_pb2
from google.protobuf import duration_pb2 as _duration_pb2
from google.protobuf import struct_pb2 as _struct_pb2
from google.protobuf import timestamp_pb2 as _timestamp_pb2
from google.protobuf import wrappers_pb2 as _wrappers_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class DataState(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    DATA_STATE_UNSPECIFIED: _ClassVar[DataState]
    DATA_STATE_READY: _ClassVar[DataState]
    DATA_STATE_ACTIVE: _ClassVar[DataState]
DATA_STATE_UNSPECIFIED: DataState
DATA_STATE_READY: DataState
DATA_STATE_ACTIVE: DataState

class CanonicalRecord(_message.Message):
    __slots__ = ("double_value", "float_value", "int64_value", "uint64_value", "int32_value", "fixed64_value", "fixed32_value", "bool_value", "string_value", "bytes_value", "uint32_value", "sfixed32_value", "sfixed64_value", "sint32_value", "sint64_value", "state", "optional_note", "nested", "labels", "children", "counters", "choice_count", "choice_name", "created_at", "elapsed", "wrapped_count", "attributes", "opaque")
    class CountersEntry(_message.Message):
        __slots__ = ("key", "value")
        KEY_FIELD_NUMBER: _ClassVar[int]
        VALUE_FIELD_NUMBER: _ClassVar[int]
        key: str
        value: int
        def __init__(self, key: _Optional[str] = ..., value: _Optional[int] = ...) -> None: ...
    DOUBLE_VALUE_FIELD_NUMBER: _ClassVar[int]
    FLOAT_VALUE_FIELD_NUMBER: _ClassVar[int]
    INT64_VALUE_FIELD_NUMBER: _ClassVar[int]
    UINT64_VALUE_FIELD_NUMBER: _ClassVar[int]
    INT32_VALUE_FIELD_NUMBER: _ClassVar[int]
    FIXED64_VALUE_FIELD_NUMBER: _ClassVar[int]
    FIXED32_VALUE_FIELD_NUMBER: _ClassVar[int]
    BOOL_VALUE_FIELD_NUMBER: _ClassVar[int]
    STRING_VALUE_FIELD_NUMBER: _ClassVar[int]
    BYTES_VALUE_FIELD_NUMBER: _ClassVar[int]
    UINT32_VALUE_FIELD_NUMBER: _ClassVar[int]
    SFIXED32_VALUE_FIELD_NUMBER: _ClassVar[int]
    SFIXED64_VALUE_FIELD_NUMBER: _ClassVar[int]
    SINT32_VALUE_FIELD_NUMBER: _ClassVar[int]
    SINT64_VALUE_FIELD_NUMBER: _ClassVar[int]
    STATE_FIELD_NUMBER: _ClassVar[int]
    OPTIONAL_NOTE_FIELD_NUMBER: _ClassVar[int]
    NESTED_FIELD_NUMBER: _ClassVar[int]
    LABELS_FIELD_NUMBER: _ClassVar[int]
    CHILDREN_FIELD_NUMBER: _ClassVar[int]
    COUNTERS_FIELD_NUMBER: _ClassVar[int]
    CHOICE_COUNT_FIELD_NUMBER: _ClassVar[int]
    CHOICE_NAME_FIELD_NUMBER: _ClassVar[int]
    CREATED_AT_FIELD_NUMBER: _ClassVar[int]
    ELAPSED_FIELD_NUMBER: _ClassVar[int]
    WRAPPED_COUNT_FIELD_NUMBER: _ClassVar[int]
    ATTRIBUTES_FIELD_NUMBER: _ClassVar[int]
    OPAQUE_FIELD_NUMBER: _ClassVar[int]
    double_value: float
    float_value: float
    int64_value: int
    uint64_value: int
    int32_value: int
    fixed64_value: int
    fixed32_value: int
    bool_value: bool
    string_value: str
    bytes_value: bytes
    uint32_value: int
    sfixed32_value: int
    sfixed64_value: int
    sint32_value: int
    sint64_value: int
    state: DataState
    optional_note: str
    nested: NestedRecord
    labels: _containers.RepeatedScalarFieldContainer[str]
    children: _containers.RepeatedCompositeFieldContainer[NestedRecord]
    counters: _containers.ScalarMap[str, int]
    choice_count: int
    choice_name: str
    created_at: _timestamp_pb2.Timestamp
    elapsed: _duration_pb2.Duration
    wrapped_count: _wrappers_pb2.Int32Value
    attributes: _struct_pb2.Struct
    opaque: _any_pb2.Any
    def __init__(self, double_value: _Optional[float] = ..., float_value: _Optional[float] = ..., int64_value: _Optional[int] = ..., uint64_value: _Optional[int] = ..., int32_value: _Optional[int] = ..., fixed64_value: _Optional[int] = ..., fixed32_value: _Optional[int] = ..., bool_value: _Optional[bool] = ..., string_value: _Optional[str] = ..., bytes_value: _Optional[bytes] = ..., uint32_value: _Optional[int] = ..., sfixed32_value: _Optional[int] = ..., sfixed64_value: _Optional[int] = ..., sint32_value: _Optional[int] = ..., sint64_value: _Optional[int] = ..., state: _Optional[_Union[DataState, str]] = ..., optional_note: _Optional[str] = ..., nested: _Optional[_Union[NestedRecord, _Mapping]] = ..., labels: _Optional[_Iterable[str]] = ..., children: _Optional[_Iterable[_Union[NestedRecord, _Mapping]]] = ..., counters: _Optional[_Mapping[str, int]] = ..., choice_count: _Optional[int] = ..., choice_name: _Optional[str] = ..., created_at: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., elapsed: _Optional[_Union[datetime.timedelta, _duration_pb2.Duration, _Mapping]] = ..., wrapped_count: _Optional[_Union[_wrappers_pb2.Int32Value, _Mapping]] = ..., attributes: _Optional[_Union[_struct_pb2.Struct, _Mapping]] = ..., opaque: _Optional[_Union[_any_pb2.Any, _Mapping]] = ...) -> None: ...

class NestedRecord(_message.Message):
    __slots__ = ("id", "label")
    ID_FIELD_NUMBER: _ClassVar[int]
    LABEL_FIELD_NUMBER: _ClassVar[int]
    id: int
    label: str
    def __init__(self, id: _Optional[int] = ..., label: _Optional[str] = ...) -> None: ...

class RecursiveRecord(_message.Message):
    __slots__ = ("parent",)
    PARENT_FIELD_NUMBER: _ClassVar[int]
    parent: RecursiveRecord
    def __init__(self, parent: _Optional[_Union[RecursiveRecord, _Mapping]] = ...) -> None: ...
