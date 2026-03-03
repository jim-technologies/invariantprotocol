"""Test JSON schema generation from proto descriptors."""

from conftest import DESCRIPTOR_PATH

from invariant.descriptor import ParsedDescriptor
from invariant.schema import SchemaGenerator


def _schema_gen():
    parsed = ParsedDescriptor.from_file(DESCRIPTOR_PATH)
    return SchemaGenerator(parsed)


# -- Basic structure --


def test_schema_type_is_object():
    s = _schema_gen().message_to_schema("greet.v1.GreetRequest")
    assert s["type"] == "object"


def test_schema_has_properties():
    s = _schema_gen().message_to_schema("greet.v1.GreetRequest")
    assert "properties" in s
    assert "name" in s["properties"]


def test_schema_additional_properties_false():
    s = _schema_gen().message_to_schema("greet.v1.GreetRequest")
    assert s["additionalProperties"] is False


# -- Required fields --


def test_required_includes_non_optional():
    s = _schema_gen().message_to_schema("greet.v1.GreetRequest")
    assert "name" in s["required"]


def test_required_excludes_optional():
    s = _schema_gen().message_to_schema("greet.v1.GreetRequest")
    assert "mood" not in s.get("required", [])


def test_required_excludes_map():
    s = _schema_gen().message_to_schema("greet.v1.GreetRequest")
    # Map fields are repeated, so they should not be in required
    assert "tags" not in s.get("required", [])


def test_required_excludes_repeated():
    s = _schema_gen().message_to_schema("greet.v1.GreetGroupRequest")
    # repeated fields are not required
    assert "people" not in s.get("required", [])


# -- String fields --


def test_string_field_schema():
    s = _schema_gen().message_to_schema("greet.v1.GreetRequest")
    assert s["properties"]["name"]["type"] == "string"


# -- Enum fields --


def test_enum_field_schema():
    s = _schema_gen().message_to_schema("greet.v1.GreetRequest")
    mood_schema = s["properties"]["mood"]
    assert mood_schema["type"] == "string"
    assert set(mood_schema["enum"]) == {"MOOD_UNSPECIFIED", "MOOD_HAPPY", "MOOD_SAD"}


# -- Map fields --


def test_map_field_schema():
    s = _schema_gen().message_to_schema("greet.v1.GreetRequest")
    tags_schema = s["properties"]["tags"]
    assert tags_schema["type"] == "object"
    assert tags_schema["additionalProperties"] == {"type": "string"}


# -- Repeated fields --


def test_repeated_message_field_schema():
    s = _schema_gen().message_to_schema("greet.v1.GreetGroupRequest")
    people_schema = s["properties"]["people"]
    assert people_schema["type"] == "array"
    assert people_schema["items"]["type"] == "object"
    assert "name" in people_schema["items"]["properties"]


def test_repeated_scalar_field_schema():
    s = _schema_gen().message_to_schema("greet.v1.GreetGroupResponse")
    messages_schema = s["properties"]["messages"]
    assert messages_schema["type"] == "array"
    assert messages_schema["items"]["type"] == "string"


# -- Integer fields --


def test_integer_field_schema():
    s = _schema_gen().message_to_schema("greet.v1.GreetGroupResponse")
    count_schema = s["properties"]["count"]
    assert count_schema["type"] == "integer"


# -- Nested message fields --


def test_nested_message_schema():
    s = _schema_gen().message_to_schema("greet.v1.GreetGroupRequest")
    person_schema = s["properties"]["people"]["items"]
    assert person_schema["type"] == "object"
    assert "name" in person_schema["properties"]
    assert "mood" in person_schema["properties"]
    assert person_schema["properties"]["mood"]["type"] == "string"
    assert "enum" in person_schema["properties"]["mood"]


# -- Field descriptions --


def test_field_description_in_schema():
    s = _schema_gen().message_to_schema("greet.v1.GreetRequest")
    assert "description" in s["properties"]["name"]
    assert "name of the person" in s["properties"]["name"]["description"].lower()


def test_enum_field_description_in_schema():
    s = _schema_gen().message_to_schema("greet.v1.GreetRequest")
    assert "description" in s["properties"]["mood"]
    assert "optional mood" in s["properties"]["mood"]["description"].lower()


# -- Unknown message --


def test_unknown_message_returns_generic_object():
    s = _schema_gen().message_to_schema("does.not.Exist")
    assert s == {"type": "object"}


# -- Person message (non-optional enum) --


def test_person_mood_required():
    s = _schema_gen().message_to_schema("greet.v1.Person")
    # Person.mood is NOT proto3 optional, but it's an enum with default value
    # In proto3, all fields have defaults so they should be "required"
    assert "name" in s["required"]
    assert "mood" in s["required"]
