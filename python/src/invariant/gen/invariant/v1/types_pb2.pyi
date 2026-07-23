from google.protobuf.internal import containers as _containers
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class ParsedDescriptor(_message.Message):
    __slots__ = ("services", "messages", "enums")
    class ServicesEntry(_message.Message):
        __slots__ = ("key", "value")
        KEY_FIELD_NUMBER: _ClassVar[int]
        VALUE_FIELD_NUMBER: _ClassVar[int]
        key: str
        value: ServiceInfo
        def __init__(self, key: _Optional[str] = ..., value: _Optional[_Union[ServiceInfo, _Mapping]] = ...) -> None: ...
    class MessagesEntry(_message.Message):
        __slots__ = ("key", "value")
        KEY_FIELD_NUMBER: _ClassVar[int]
        VALUE_FIELD_NUMBER: _ClassVar[int]
        key: str
        value: MessageInfo
        def __init__(self, key: _Optional[str] = ..., value: _Optional[_Union[MessageInfo, _Mapping]] = ...) -> None: ...
    class EnumsEntry(_message.Message):
        __slots__ = ("key", "value")
        KEY_FIELD_NUMBER: _ClassVar[int]
        VALUE_FIELD_NUMBER: _ClassVar[int]
        key: str
        value: EnumInfo
        def __init__(self, key: _Optional[str] = ..., value: _Optional[_Union[EnumInfo, _Mapping]] = ...) -> None: ...
    SERVICES_FIELD_NUMBER: _ClassVar[int]
    MESSAGES_FIELD_NUMBER: _ClassVar[int]
    ENUMS_FIELD_NUMBER: _ClassVar[int]
    services: _containers.MessageMap[str, ServiceInfo]
    messages: _containers.MessageMap[str, MessageInfo]
    enums: _containers.MessageMap[str, EnumInfo]
    def __init__(self, services: _Optional[_Mapping[str, ServiceInfo]] = ..., messages: _Optional[_Mapping[str, MessageInfo]] = ..., enums: _Optional[_Mapping[str, EnumInfo]] = ...) -> None: ...

class ServiceInfo(_message.Message):
    __slots__ = ("name", "full_name", "methods", "comment")
    class MethodsEntry(_message.Message):
        __slots__ = ("key", "value")
        KEY_FIELD_NUMBER: _ClassVar[int]
        VALUE_FIELD_NUMBER: _ClassVar[int]
        key: str
        value: MethodInfo
        def __init__(self, key: _Optional[str] = ..., value: _Optional[_Union[MethodInfo, _Mapping]] = ...) -> None: ...
    NAME_FIELD_NUMBER: _ClassVar[int]
    FULL_NAME_FIELD_NUMBER: _ClassVar[int]
    METHODS_FIELD_NUMBER: _ClassVar[int]
    COMMENT_FIELD_NUMBER: _ClassVar[int]
    name: str
    full_name: str
    methods: _containers.MessageMap[str, MethodInfo]
    comment: str
    def __init__(self, name: _Optional[str] = ..., full_name: _Optional[str] = ..., methods: _Optional[_Mapping[str, MethodInfo]] = ..., comment: _Optional[str] = ...) -> None: ...

class MethodInfo(_message.Message):
    __slots__ = ("name", "input_type", "output_type", "comment", "client_streaming", "server_streaming")
    NAME_FIELD_NUMBER: _ClassVar[int]
    INPUT_TYPE_FIELD_NUMBER: _ClassVar[int]
    OUTPUT_TYPE_FIELD_NUMBER: _ClassVar[int]
    COMMENT_FIELD_NUMBER: _ClassVar[int]
    CLIENT_STREAMING_FIELD_NUMBER: _ClassVar[int]
    SERVER_STREAMING_FIELD_NUMBER: _ClassVar[int]
    name: str
    input_type: str
    output_type: str
    comment: str
    client_streaming: bool
    server_streaming: bool
    def __init__(self, name: _Optional[str] = ..., input_type: _Optional[str] = ..., output_type: _Optional[str] = ..., comment: _Optional[str] = ..., client_streaming: _Optional[bool] = ..., server_streaming: _Optional[bool] = ...) -> None: ...

class MessageInfo(_message.Message):
    __slots__ = ("name", "full_name", "fields", "oneofs", "comment", "is_map_entry")
    NAME_FIELD_NUMBER: _ClassVar[int]
    FULL_NAME_FIELD_NUMBER: _ClassVar[int]
    FIELDS_FIELD_NUMBER: _ClassVar[int]
    ONEOFS_FIELD_NUMBER: _ClassVar[int]
    COMMENT_FIELD_NUMBER: _ClassVar[int]
    IS_MAP_ENTRY_FIELD_NUMBER: _ClassVar[int]
    name: str
    full_name: str
    fields: _containers.RepeatedCompositeFieldContainer[FieldInfo]
    oneofs: _containers.RepeatedCompositeFieldContainer[OneofInfo]
    comment: str
    is_map_entry: bool
    def __init__(self, name: _Optional[str] = ..., full_name: _Optional[str] = ..., fields: _Optional[_Iterable[_Union[FieldInfo, _Mapping]]] = ..., oneofs: _Optional[_Iterable[_Union[OneofInfo, _Mapping]]] = ..., comment: _Optional[str] = ..., is_map_entry: _Optional[bool] = ...) -> None: ...

class FieldInfo(_message.Message):
    __slots__ = ("name", "number", "type", "type_name", "label", "comment", "oneof_index", "optional", "json_name")
    NAME_FIELD_NUMBER: _ClassVar[int]
    NUMBER_FIELD_NUMBER: _ClassVar[int]
    TYPE_FIELD_NUMBER: _ClassVar[int]
    TYPE_NAME_FIELD_NUMBER: _ClassVar[int]
    LABEL_FIELD_NUMBER: _ClassVar[int]
    COMMENT_FIELD_NUMBER: _ClassVar[int]
    ONEOF_INDEX_FIELD_NUMBER: _ClassVar[int]
    OPTIONAL_FIELD_NUMBER: _ClassVar[int]
    JSON_NAME_FIELD_NUMBER: _ClassVar[int]
    name: str
    number: int
    type: int
    type_name: str
    label: int
    comment: str
    oneof_index: int
    optional: bool
    json_name: str
    def __init__(self, name: _Optional[str] = ..., number: _Optional[int] = ..., type: _Optional[int] = ..., type_name: _Optional[str] = ..., label: _Optional[int] = ..., comment: _Optional[str] = ..., oneof_index: _Optional[int] = ..., optional: _Optional[bool] = ..., json_name: _Optional[str] = ...) -> None: ...

class OneofInfo(_message.Message):
    __slots__ = ("name", "comment", "field_names")
    NAME_FIELD_NUMBER: _ClassVar[int]
    COMMENT_FIELD_NUMBER: _ClassVar[int]
    FIELD_NAMES_FIELD_NUMBER: _ClassVar[int]
    name: str
    comment: str
    field_names: _containers.RepeatedScalarFieldContainer[str]
    def __init__(self, name: _Optional[str] = ..., comment: _Optional[str] = ..., field_names: _Optional[_Iterable[str]] = ...) -> None: ...

class EnumInfo(_message.Message):
    __slots__ = ("name", "full_name", "values", "comment")
    NAME_FIELD_NUMBER: _ClassVar[int]
    FULL_NAME_FIELD_NUMBER: _ClassVar[int]
    VALUES_FIELD_NUMBER: _ClassVar[int]
    COMMENT_FIELD_NUMBER: _ClassVar[int]
    name: str
    full_name: str
    values: _containers.RepeatedCompositeFieldContainer[EnumValueInfo]
    comment: str
    def __init__(self, name: _Optional[str] = ..., full_name: _Optional[str] = ..., values: _Optional[_Iterable[_Union[EnumValueInfo, _Mapping]]] = ..., comment: _Optional[str] = ...) -> None: ...

class EnumValueInfo(_message.Message):
    __slots__ = ("name", "number", "comment")
    NAME_FIELD_NUMBER: _ClassVar[int]
    NUMBER_FIELD_NUMBER: _ClassVar[int]
    COMMENT_FIELD_NUMBER: _ClassVar[int]
    name: str
    number: int
    comment: str
    def __init__(self, name: _Optional[str] = ..., number: _Optional[int] = ..., comment: _Optional[str] = ...) -> None: ...
