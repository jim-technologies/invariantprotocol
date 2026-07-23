"""Test JSON schema generation from proto descriptors."""

import re

from conftest import DESCRIPTOR_PATH
from google.protobuf import descriptor_pb2

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
    assert "wireSequenceId" in s["properties"]
    assert "account_sequence" not in s["properties"]


def test_schema_required_fields():
    sg = _schema_gen()

    s = sg.message_to_schema("greet.v1.GreetRequest")
    assert "name" in s["required"]
    assert "mood" not in s.get("required", [])  # optional
    assert "tags" not in s.get("required", [])  # map
    assert "wireSequenceId" not in s.get("required", [])  # optional int64

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

    # ProtoJSON renders 64-bit integers as decimal strings, including fields
    # with an explicit json_name.
    sequence = p["wireSequenceId"]
    assert sequence["type"] == "string"
    assert re.fullmatch(sequence["pattern"], "9007199254740993")
    assert re.fullmatch(sequence["pattern"], "-9007199254740993")
    assert not re.fullmatch(sequence["pattern"], "01")

    response_count = sg.message_to_schema("greet.v1.GreetResponse")["properties"]["wireResponseCount"]
    assert response_count["type"] == "string"
    assert re.fullmatch(response_count["pattern"], "42")

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


def test_proto2_optional_field_is_not_required():
    field = descriptor_pb2.FieldDescriptorProto
    file_proto = descriptor_pb2.FileDescriptorProto(
        name="invariant/tests/proto2_presence.proto",
        package="invariant.tests.proto2",
        syntax="proto2",
    )
    message = file_proto.message_type.add(name="Presence")
    message.field.add(
        name="optional_value",
        json_name="optionalValue",
        number=1,
        label=field.LABEL_OPTIONAL,
        type=field.TYPE_STRING,
    )
    message.field.add(
        name="required_value",
        json_name="requiredValue",
        number=2,
        label=field.LABEL_REQUIRED,
        type=field.TYPE_STRING,
    )
    message.field.add(
        name="repeated_value",
        json_name="repeatedValue",
        number=3,
        label=field.LABEL_REPEATED,
        type=field.TYPE_STRING,
    )

    parsed = ParsedDescriptor(descriptor_pb2.FileDescriptorSet(file=[file_proto]))
    schema = SchemaGenerator(parsed).message_to_schema("invariant.tests.proto2.Presence")

    assert set(schema["properties"]) == {"optionalValue", "requiredValue", "repeatedValue"}
    assert schema["required"] == ["requiredValue"]


def test_all_protojson_64_bit_integer_schemas_use_decimal_strings():
    field = descriptor_pb2.FieldDescriptorProto
    file_proto = descriptor_pb2.FileDescriptorProto(
        name="invariant/tests/protojson_int64.proto",
        package="invariant.tests.protojson",
        syntax="proto3",
    )
    message = file_proto.message_type.add(name="Integers")
    for number, (name, field_type) in enumerate(
        (
            ("int64_value", field.TYPE_INT64),
            ("sint64_value", field.TYPE_SINT64),
            ("sfixed64_value", field.TYPE_SFIXED64),
            ("uint64_value", field.TYPE_UINT64),
            ("fixed64_value", field.TYPE_FIXED64),
        ),
        start=1,
    ):
        message.field.add(
            name=name,
            json_name=name,
            number=number,
            label=field.LABEL_OPTIONAL,
            type=field_type,
        )

    parsed = ParsedDescriptor(descriptor_pb2.FileDescriptorSet(file=[file_proto]))
    properties = SchemaGenerator(parsed).message_to_schema("invariant.tests.protojson.Integers")["properties"]

    for name in ("int64_value", "sint64_value", "sfixed64_value"):
        assert properties[name]["type"] == "string"
        assert re.fullmatch(properties[name]["pattern"], "-42")
    for name in ("uint64_value", "fixed64_value"):
        assert properties[name]["type"] == "string"
        assert re.fullmatch(properties[name]["pattern"], "42")
        assert not re.fullmatch(properties[name]["pattern"], "-42")


def test_protojson_float_special_values_and_well_known_types():
    schema_gen = _schema_gen()
    canonical = schema_gen.message_to_schema("data.v1.CanonicalRecord")["properties"]
    float_schema = {
        "oneOf": [
            {"type": "number"},
            {"type": "string", "enum": ["NaN", "Infinity", "-Infinity"]},
        ]
    }
    assert canonical["doubleValue"]["oneOf"] == float_schema["oneOf"]
    assert canonical["floatValue"]["oneOf"] == float_schema["oneOf"]

    field = descriptor_pb2.FieldDescriptorProto
    file_proto = descriptor_pb2.FileDescriptorProto(
        name="invariant/tests/protojson_wkt.proto",
        package="invariant.tests.protojson",
        syntax="proto3",
    )
    message = file_proto.message_type.add(name="WellKnownTypes")
    fields = (
        ("double_wrapper", field.TYPE_MESSAGE, ".google.protobuf.DoubleValue"),
        ("float_wrapper", field.TYPE_MESSAGE, ".google.protobuf.FloatValue"),
        ("int64_wrapper", field.TYPE_MESSAGE, ".google.protobuf.Int64Value"),
        ("uint64_wrapper", field.TYPE_MESSAGE, ".google.protobuf.UInt64Value"),
        ("field_mask", field.TYPE_MESSAGE, ".google.protobuf.FieldMask"),
        ("list_value", field.TYPE_MESSAGE, ".google.protobuf.ListValue"),
        ("empty", field.TYPE_MESSAGE, ".google.protobuf.Empty"),
        ("duration", field.TYPE_MESSAGE, ".google.protobuf.Duration"),
        ("null_value", field.TYPE_ENUM, ".google.protobuf.NullValue"),
    )
    for number, (name, field_type, type_name) in enumerate(fields, start=1):
        message.field.add(
            name=name,
            json_name=name,
            number=number,
            label=field.LABEL_OPTIONAL,
            type=field_type,
            type_name=type_name,
        )

    parsed = ParsedDescriptor(descriptor_pb2.FileDescriptorSet(file=[file_proto]))
    properties = SchemaGenerator(parsed).message_to_schema("invariant.tests.protojson.WellKnownTypes")["properties"]

    assert properties["double_wrapper"] == float_schema
    assert properties["float_wrapper"] == float_schema
    assert properties["int64_wrapper"]["type"] == "string"
    assert re.fullmatch(properties["int64_wrapper"]["pattern"], "-42")
    assert properties["uint64_wrapper"]["type"] == "string"
    assert not re.fullmatch(properties["uint64_wrapper"]["pattern"], "-42")
    assert properties["field_mask"] == {"type": "string"}
    assert properties["list_value"] == {"type": "array", "items": {}}
    assert properties["empty"] == {"type": "object", "additionalProperties": False}
    assert properties["duration"] == {
        "type": "string",
        "pattern": r"^-?(?:0|[1-9][0-9]*)(?:\.[0-9]{1,9})?s$",
    }
    assert re.fullmatch(properties["duration"]["pattern"], "300s")
    assert re.fullmatch(properties["duration"]["pattern"], "1.500s")
    assert not re.fullmatch(properties["duration"]["pattern"], "1.5h")
    assert properties["null_value"] == {"type": "null"}

    properties["list_value"]["items"]["mutated"] = True
    fresh_properties = SchemaGenerator(parsed).message_to_schema("invariant.tests.protojson.WellKnownTypes")[
        "properties"
    ]
    assert fresh_properties["list_value"] == {"type": "array", "items": {}}


def test_map_key_schemas_constrain_canonical_json_property_names():
    field = descriptor_pb2.FieldDescriptorProto
    file_proto = descriptor_pb2.FileDescriptorProto(
        name="invariant/tests/map_keys.proto",
        package="invariant.tests.maps",
        syntax="proto3",
    )
    message = file_proto.message_type.add(name="Maps")
    key_types = (
        ("bool_values", "BoolValuesEntry", field.TYPE_BOOL),
        ("signed_values", "SignedValuesEntry", field.TYPE_SINT64),
        ("unsigned_values", "UnsignedValuesEntry", field.TYPE_FIXED64),
        ("string_values", "StringValuesEntry", field.TYPE_STRING),
    )
    for number, (name, entry_name, key_type) in enumerate(key_types, start=1):
        entry = message.nested_type.add(name=entry_name)
        entry.options.map_entry = True
        entry.field.add(name="key", number=1, label=field.LABEL_OPTIONAL, type=key_type)
        entry.field.add(name="value", number=2, label=field.LABEL_OPTIONAL, type=field.TYPE_STRING)
        message.field.add(
            name=name,
            json_name=name,
            number=number,
            label=field.LABEL_REPEATED,
            type=field.TYPE_MESSAGE,
            type_name=f".invariant.tests.maps.Maps.{entry_name}",
        )

    parsed = ParsedDescriptor(descriptor_pb2.FileDescriptorSet(file=[file_proto]))
    properties = SchemaGenerator(parsed).message_to_schema("invariant.tests.maps.Maps")["properties"]

    assert properties["bool_values"]["propertyNames"] == {"enum": ["false", "true"]}

    signed_pattern = properties["signed_values"]["propertyNames"]["pattern"]
    assert all(re.fullmatch(signed_pattern, value) for value in ("0", "42", "-42"))
    assert not re.fullmatch(signed_pattern, "01")

    unsigned_pattern = properties["unsigned_values"]["propertyNames"]["pattern"]
    assert all(re.fullmatch(unsigned_pattern, value) for value in ("0", "42"))
    assert not re.fullmatch(unsigned_pattern, "-42")
    assert "propertyNames" not in properties["string_values"]
