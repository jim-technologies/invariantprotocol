"""Test descriptor parsing."""

from conftest import DESCRIPTOR_PATH

from invariant.descriptor import ParsedDescriptor


def _parsed():
    return ParsedDescriptor.from_file(DESCRIPTOR_PATH)


# -- Services --


def test_service_found():
    p = _parsed()
    assert "greet.v1.GreetService" in p.services


def test_service_name():
    svc = _parsed().services["greet.v1.GreetService"]
    assert svc.name == "GreetService"
    assert svc.full_name == "greet.v1.GreetService"


def test_service_comment():
    svc = _parsed().services["greet.v1.GreetService"]
    assert "simple greeting service" in svc.comment.lower()


def test_service_has_two_methods():
    svc = _parsed().services["greet.v1.GreetService"]
    assert set(svc.methods.keys()) == {"Greet", "GreetGroup"}


# -- Methods --


def test_method_greet():
    m = _parsed().services["greet.v1.GreetService"].methods["Greet"]
    assert m.input_type == "greet.v1.GreetRequest"
    assert m.output_type == "greet.v1.GreetResponse"
    assert not m.client_streaming
    assert not m.server_streaming


def test_method_greet_group():
    m = _parsed().services["greet.v1.GreetService"].methods["GreetGroup"]
    assert m.input_type == "greet.v1.GreetGroupRequest"
    assert m.output_type == "greet.v1.GreetGroupResponse"


def test_method_comment():
    m = _parsed().services["greet.v1.GreetService"].methods["Greet"]
    assert "greet a person" in m.comment.lower()


# -- Messages --


def test_messages_parsed():
    p = _parsed()
    expected = {
        "greet.v1.GreetRequest",
        "greet.v1.GreetResponse",
        "greet.v1.Person",
        "greet.v1.GreetGroupRequest",
        "greet.v1.GreetGroupResponse",
    }
    # May also contain map entry messages
    assert expected.issubset(set(p.messages.keys()))


def test_greet_request_fields():
    msg = _parsed().messages["greet.v1.GreetRequest"]
    field_names = [f.name for f in msg.fields]
    assert "name" in field_names
    assert "mood" in field_names
    assert "tags" in field_names


def test_field_comment():
    msg = _parsed().messages["greet.v1.GreetRequest"]
    name_field = next(f for f in msg.fields if f.name == "name")
    assert "name of the person" in name_field.comment.lower()


def test_optional_field():
    msg = _parsed().messages["greet.v1.GreetRequest"]
    mood_field = next(f for f in msg.fields if f.name == "mood")
    assert mood_field.optional is True


def test_non_optional_field():
    msg = _parsed().messages["greet.v1.GreetRequest"]
    name_field = next(f for f in msg.fields if f.name == "name")
    assert name_field.optional is False


def test_map_entry_message():
    p = _parsed()
    # Map fields generate a synthetic "XxxEntry" message with is_map_entry=True
    map_entries = [m for m in p.messages.values() if m.is_map_entry]
    assert len(map_entries) >= 1  # GreetRequest.tags and GreetResponse.tags


def test_repeated_field():
    msg = _parsed().messages["greet.v1.GreetGroupRequest"]
    people_field = next(f for f in msg.fields if f.name == "people")
    from google.protobuf.descriptor_pb2 import FieldDescriptorProto

    assert people_field.label == FieldDescriptorProto.LABEL_REPEATED


def test_nested_message_reference():
    msg = _parsed().messages["greet.v1.GreetGroupRequest"]
    people_field = next(f for f in msg.fields if f.name == "people")
    assert people_field.type_name == "greet.v1.Person"


def test_enum_field_type():
    msg = _parsed().messages["greet.v1.GreetRequest"]
    mood_field = next(f for f in msg.fields if f.name == "mood")
    from google.protobuf.descriptor_pb2 import FieldDescriptorProto

    assert mood_field.type == FieldDescriptorProto.TYPE_ENUM


# -- Enums --


def test_enum_parsed():
    p = _parsed()
    assert "greet.v1.Mood" in p.enums


def test_enum_values():
    e = _parsed().enums["greet.v1.Mood"]
    names = [v.name for v in e.values]
    assert names == ["MOOD_UNSPECIFIED", "MOOD_HAPPY", "MOOD_SAD"]


def test_enum_comment():
    e = _parsed().enums["greet.v1.Mood"]
    assert "mood" in e.comment.lower()


def test_enum_value_comments():
    e = _parsed().enums["greet.v1.Mood"]
    happy = next(v for v in e.values if v.name == "MOOD_HAPPY")
    assert "happy" in happy.comment.lower()


# -- Error cases --


def test_from_file_not_found():
    import pytest

    with pytest.raises(FileNotFoundError):
        ParsedDescriptor.from_file("/nonexistent/path.binpb")
