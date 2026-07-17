"""Test JSON schema generation from proto descriptors."""

from conftest import DESCRIPTOR_PATH

from invariant.descriptor import ParsedDescriptor
from invariant.schema import SchemaGenerator


def _schema_gen():
    parsed = ParsedDescriptor.from_file(DESCRIPTOR_PATH)
    return SchemaGenerator(parsed)


def test_schema_basic_structure():
    s = _schema_gen().message_to_schema("greet.v1.GreetRequest")
    assert s["type"] == "object"
    assert s["additionalProperties"] is False
    assert "properties" in s
    assert "name" in s["properties"]


def test_schema_required_fields():
    sg = _schema_gen()

    s = sg.message_to_schema("greet.v1.GreetRequest")
    assert "name" in s["required"]
    assert "mood" not in s.get("required", [])  # optional
    assert "tags" not in s.get("required", [])  # map

    s = sg.message_to_schema("greet.v1.GreetGroupRequest")
    assert "people" not in s.get("required", [])  # repeated

    # Person.mood is not proto3 optional — should be required
    s = sg.message_to_schema("greet.v1.Person")
    assert "name" in s["required"]
    assert "mood" in s["required"]


def test_schema_field_types():
    sg = _schema_gen()
    s = sg.message_to_schema("greet.v1.GreetRequest")
    p = s["properties"]

    # String
    assert p["name"]["type"] == "string"

    # Enum
    assert p["mood"]["type"] == "string"
    assert set(p["mood"]["enum"]) == {"MOOD_UNSPECIFIED", "MOOD_HAPPY", "MOOD_SAD"}

    # Map
    assert p["tags"]["type"] == "object"
    assert p["tags"]["additionalProperties"] == {"type": "string"}

    # Integer
    s2 = sg.message_to_schema("greet.v1.GreetGroupResponse")
    assert s2["properties"]["count"]["type"] == "integer"

    # Repeated message
    s3 = sg.message_to_schema("greet.v1.GreetGroupRequest")
    people = s3["properties"]["people"]
    assert people["type"] == "array"
    assert people["items"]["type"] == "object"
    assert "name" in people["items"]["properties"]

    # Repeated scalar
    assert s2["properties"]["messages"]["type"] == "array"
    assert s2["properties"]["messages"]["items"]["type"] == "string"


def test_schema_nested_messages_and_descriptions():
    sg = _schema_gen()

    # Nested message schema
    s = sg.message_to_schema("greet.v1.GreetGroupRequest")
    person = s["properties"]["people"]["items"]
    assert person["type"] == "object"
    assert "name" in person["properties"]
    assert "mood" in person["properties"]
    assert person["properties"]["mood"]["type"] == "string"
    assert "enum" in person["properties"]["mood"]

    # Field descriptions
    s = sg.message_to_schema("greet.v1.GreetRequest")
    assert "name of the person" in s["properties"]["name"]["description"].lower()
    assert "optional mood" in s["properties"]["mood"]["description"].lower()


def test_unknown_message_returns_generic_object():
    s = _schema_gen().message_to_schema("does.not.Exist")
    assert s == {"type": "object"}


def test_recursive_message_stays_finite():
    schema = _schema_gen().message_to_schema("data.v1.RecursiveRecord")
    parent = schema["properties"]["parent"]

    assert parent["type"] == "object"
    recursive_link = parent["properties"]["parent"]
    assert recursive_link["type"] == "object"
    assert "properties" not in recursive_link
