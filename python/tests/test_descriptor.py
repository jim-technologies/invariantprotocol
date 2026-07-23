"""Test descriptor parsing."""

import pytest
from conftest import DESCRIPTOR_PATH

from invariant.descriptor import ParsedDescriptor


def _parsed():
    return ParsedDescriptor.from_file(DESCRIPTOR_PATH)


def test_parse_services():
    p = _parsed()
    svc = p.services["greet.v1.GreetService"]
    assert svc.name == "GreetService"
    assert svc.full_name == "greet.v1.GreetService"
    assert "simple greeting service" in svc.comment.lower()
    assert set(svc.methods.keys()) == {"Greet", "GreetGroup", "StreamGreet"}

    greet = svc.methods["Greet"]
    assert greet.input_type == "greet.v1.GreetRequest"
    assert greet.output_type == "greet.v1.GreetResponse"
    assert not greet.client_streaming
    assert not greet.server_streaming
    assert "greet a person" in greet.comment.lower()

    group = svc.methods["GreetGroup"]
    assert group.input_type == "greet.v1.GreetGroupRequest"
    assert group.output_type == "greet.v1.GreetGroupResponse"

    stream = svc.methods["StreamGreet"]
    assert stream.input_type == "greet.v1.StreamGreetRequest"
    assert stream.output_type == "greet.v1.GreetResponse"
    assert stream.server_streaming
    assert not stream.client_streaming


def test_parse_messages():
    p = _parsed()
    expected = {
        "greet.v1.GreetRequest",
        "greet.v1.GreetResponse",
        "greet.v1.Person",
        "greet.v1.GreetGroupRequest",
        "greet.v1.GreetGroupResponse",
    }
    assert expected.issubset(set(p.messages.keys()))

    msg = p.messages["greet.v1.GreetRequest"]
    field_names = [f.name for f in msg.fields]
    assert "name" in field_names
    assert "mood" in field_names
    assert "tags" in field_names

    name_field = next(f for f in msg.fields if f.name == "name")
    assert "name of the person" in name_field.comment.lower()
    assert name_field.optional is False

    mood_field = next(f for f in msg.fields if f.name == "mood")
    assert mood_field.optional is True
    sequence_field = next(f for f in msg.fields if f.name == "account_sequence")
    assert sequence_field.optional is True
    assert sequence_field.json_name == "wireSequenceId"

    from google.protobuf.descriptor_pb2 import FieldDescriptorProto

    assert mood_field.type == FieldDescriptorProto.TYPE_ENUM

    # Map entry messages
    map_entries = [m for m in p.messages.values() if m.is_map_entry]
    assert len(map_entries) >= 1

    # Repeated and nested message references
    people_field = next(f for f in p.messages["greet.v1.GreetGroupRequest"].fields if f.name == "people")
    assert people_field.label == FieldDescriptorProto.LABEL_REPEATED
    assert people_field.type_name == "greet.v1.Person"


def test_parse_enums():
    p = _parsed()
    e = p.enums["greet.v1.Mood"]
    assert "mood" in e.comment.lower()

    names = [v.name for v in e.values]
    assert names == ["MOOD_UNSPECIFIED", "MOOD_HAPPY", "MOOD_SAD"]

    happy = next(v for v in e.values if v.name == "MOOD_HAPPY")
    assert "happy" in happy.comment.lower()


def test_from_file_not_found():
    with pytest.raises(FileNotFoundError):
        ParsedDescriptor.from_file("/nonexistent/path.binpb")
