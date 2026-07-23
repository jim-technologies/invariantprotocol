from buf.validate import validate_pb2 as _validate_pb2
from google.api import annotations_pb2 as _annotations_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class Mood(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    MOOD_UNSPECIFIED: _ClassVar[Mood]
    MOOD_HAPPY: _ClassVar[Mood]
    MOOD_SAD: _ClassVar[Mood]
MOOD_UNSPECIFIED: Mood
MOOD_HAPPY: Mood
MOOD_SAD: Mood

class GreetRequest(_message.Message):
    __slots__ = ("name", "mood", "tags", "account_sequence")
    class TagsEntry(_message.Message):
        __slots__ = ("key", "value")
        KEY_FIELD_NUMBER: _ClassVar[int]
        VALUE_FIELD_NUMBER: _ClassVar[int]
        key: str
        value: str
        def __init__(self, key: _Optional[str] = ..., value: _Optional[str] = ...) -> None: ...
    NAME_FIELD_NUMBER: _ClassVar[int]
    MOOD_FIELD_NUMBER: _ClassVar[int]
    TAGS_FIELD_NUMBER: _ClassVar[int]
    ACCOUNT_SEQUENCE_FIELD_NUMBER: _ClassVar[int]
    name: str
    mood: Mood
    tags: _containers.ScalarMap[str, str]
    account_sequence: int
    def __init__(self, name: _Optional[str] = ..., mood: _Optional[_Union[Mood, str]] = ..., tags: _Optional[_Mapping[str, str]] = ..., account_sequence: _Optional[int] = ...) -> None: ...

class GreetResponse(_message.Message):
    __slots__ = ("message", "mood", "tags", "response_label", "response_count")
    class TagsEntry(_message.Message):
        __slots__ = ("key", "value")
        KEY_FIELD_NUMBER: _ClassVar[int]
        VALUE_FIELD_NUMBER: _ClassVar[int]
        key: str
        value: str
        def __init__(self, key: _Optional[str] = ..., value: _Optional[str] = ...) -> None: ...
    MESSAGE_FIELD_NUMBER: _ClassVar[int]
    MOOD_FIELD_NUMBER: _ClassVar[int]
    TAGS_FIELD_NUMBER: _ClassVar[int]
    RESPONSE_LABEL_FIELD_NUMBER: _ClassVar[int]
    RESPONSE_COUNT_FIELD_NUMBER: _ClassVar[int]
    message: str
    mood: Mood
    tags: _containers.ScalarMap[str, str]
    response_label: str
    response_count: int
    def __init__(self, message: _Optional[str] = ..., mood: _Optional[_Union[Mood, str]] = ..., tags: _Optional[_Mapping[str, str]] = ..., response_label: _Optional[str] = ..., response_count: _Optional[int] = ...) -> None: ...

class Person(_message.Message):
    __slots__ = ("name", "mood")
    NAME_FIELD_NUMBER: _ClassVar[int]
    MOOD_FIELD_NUMBER: _ClassVar[int]
    name: str
    mood: Mood
    def __init__(self, name: _Optional[str] = ..., mood: _Optional[_Union[Mood, str]] = ...) -> None: ...

class GreetGroupRequest(_message.Message):
    __slots__ = ("people",)
    PEOPLE_FIELD_NUMBER: _ClassVar[int]
    people: _containers.RepeatedCompositeFieldContainer[Person]
    def __init__(self, people: _Optional[_Iterable[_Union[Person, _Mapping]]] = ...) -> None: ...

class GreetGroupResponse(_message.Message):
    __slots__ = ("messages", "count")
    MESSAGES_FIELD_NUMBER: _ClassVar[int]
    COUNT_FIELD_NUMBER: _ClassVar[int]
    messages: _containers.RepeatedScalarFieldContainer[str]
    count: int
    def __init__(self, messages: _Optional[_Iterable[str]] = ..., count: _Optional[int] = ...) -> None: ...

class StreamGreetRequest(_message.Message):
    __slots__ = ("name", "count")
    NAME_FIELD_NUMBER: _ClassVar[int]
    COUNT_FIELD_NUMBER: _ClassVar[int]
    name: str
    count: int
    def __init__(self, name: _Optional[str] = ..., count: _Optional[int] = ...) -> None: ...
