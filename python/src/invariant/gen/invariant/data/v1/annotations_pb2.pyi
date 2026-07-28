from google.protobuf import descriptor_pb2 as _descriptor_pb2
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor
DATASET_FIELD_NUMBER: _ClassVar[int]
dataset: _descriptor.FieldDescriptor
FIELD_FIELD_NUMBER: _ClassVar[int]
field: _descriptor.FieldDescriptor

class DatasetOptions(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class DecimalOptions(_message.Message):
    __slots__ = ("precision", "scale")
    PRECISION_FIELD_NUMBER: _ClassVar[int]
    SCALE_FIELD_NUMBER: _ClassVar[int]
    precision: int
    scale: int
    def __init__(self, precision: _Optional[int] = ..., scale: _Optional[int] = ...) -> None: ...

class UuidOptions(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class FixedBytesOptions(_message.Message):
    __slots__ = ("byte_length",)
    BYTE_LENGTH_FIELD_NUMBER: _ClassVar[int]
    byte_length: int
    def __init__(self, byte_length: _Optional[int] = ...) -> None: ...

class FixedListOptions(_message.Message):
    __slots__ = ("length",)
    LENGTH_FIELD_NUMBER: _ClassVar[int]
    length: int
    def __init__(self, length: _Optional[int] = ...) -> None: ...

class FieldOptions(_message.Message):
    __slots__ = ("decimal", "uuid", "fixed_bytes", "fixed_list")
    DECIMAL_FIELD_NUMBER: _ClassVar[int]
    UUID_FIELD_NUMBER: _ClassVar[int]
    FIXED_BYTES_FIELD_NUMBER: _ClassVar[int]
    FIXED_LIST_FIELD_NUMBER: _ClassVar[int]
    decimal: DecimalOptions
    uuid: UuidOptions
    fixed_bytes: FixedBytesOptions
    fixed_list: FixedListOptions
    def __init__(self, decimal: _Optional[_Union[DecimalOptions, _Mapping]] = ..., uuid: _Optional[_Union[UuidOptions, _Mapping]] = ..., fixed_bytes: _Optional[_Union[FixedBytesOptions, _Mapping]] = ..., fixed_list: _Optional[_Union[FixedListOptions, _Mapping]] = ...) -> None: ...
