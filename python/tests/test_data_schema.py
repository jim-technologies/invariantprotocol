from pathlib import Path

import pytest

from invariant import (
    SCHEMA_IR_VERSION,
    SCHEMA_MAPPING_VERSION,
    find_dataset,
    migrate_schema_bundle,
    parse_schema_bundle,
    serialize_schema_bundle,
)
from invariant.gen.invariant.data.v1 import annotations_pb2, schema_pb2

GOLDEN_BUNDLE = Path(__file__).resolve().parents[2] / "testdata" / "data.schema.binpb"


def test_generated_annotation_bindings_use_the_assigned_option_number() -> None:
    assert annotations_pb2.dataset.number == 51974
    assert annotations_pb2.field.number == 51974


def test_reads_shared_canonical_schema_bundle() -> None:
    encoded = GOLDEN_BUNDLE.read_bytes()
    bundle = parse_schema_bundle(encoded)

    assert bundle.ir_version == SCHEMA_IR_VERSION == 4
    assert bundle.mapping_version == SCHEMA_MAPPING_VERSION == 3
    assert [dataset.source_message for dataset in bundle.datasets] == [
        "data.v1.CanonicalRecord",
        "data.v1.Proto2Record",
    ]

    canonical = find_dataset(bundle, "data.v1.CanonicalRecord")
    assert canonical is not None
    fields = {field.name: field for field in canonical.fields}

    optional_note = fields["optional_note"]
    assert (optional_note.stable_id, optional_note.presence, optional_note.nullable) == (
        17,
        schema_pb2.PRESENCE_EXPLICIT,
        True,
    )
    assert optional_note.storage_name_source == "optional_note"

    labels = fields["labels"]
    assert (labels.stable_id, labels.presence) == (19, schema_pb2.PRESENCE_REPEATED)
    assert labels.type.list.element.stable_id == 31
    assert labels.type.list.element.presence == schema_pb2.PRESENCE_NOT_APPLICABLE
    assert labels.type.list.element.synthetic_role == schema_pb2.SYNTHETIC_ROLE_LIST_ELEMENT
    assert labels.type.list.element.storage_name_source == ""

    choice_count = fields["choice_count"]
    assert (choice_count.stable_id, choice_count.presence, choice_count.oneof) == (
        22,
        schema_pb2.PRESENCE_ONEOF,
        "choice",
    )

    proto2 = find_dataset(bundle, "data.v1.Proto2Record")
    assert proto2 is not None
    proto2_fields = {field.name: field for field in proto2.fields}
    assert (proto2_fields["id"].stable_id, proto2_fields["id"].presence) == (
        1,
        schema_pb2.PRESENCE_REQUIRED,
    )
    assert (
        proto2_fields["label"].stable_id,
        proto2_fields["label"].presence,
        proto2_fields["label"].has_default,
        proto2_fields["label"].protobuf_default,
    ) == (2, schema_pb2.PRESENCE_EXPLICIT, True, "unknown")

    assert serialize_schema_bundle(bundle) == encoded
    assert find_dataset(bundle, "data.v1.Missing") is None


@pytest.mark.parametrize(("field", "message"), [("ir_version", "ir_version"), ("mapping_version", "mapping_version")])
def test_rejects_unsupported_bundle_versions(field: str, message: str) -> None:
    bundle = schema_pb2.SchemaBundle(ir_version=3, mapping_version=2)
    setattr(bundle, field, 1)

    with pytest.raises(ValueError, match=message):
        parse_schema_bundle(bundle.SerializeToString())


def test_migrates_exact_legacy_version_without_losing_schema_state() -> None:
    legacy = schema_pb2.SchemaBundle(
        ir_version=3,
        mapping_version=2,
        source_descriptor_sha256=b"digest",
        datasets=[
            schema_pb2.DatasetSchema(
                source_message="example.v1.Record",
                name="example_v1_record",
                last_field_id=8,
                fields=[
                    schema_pb2.Field(
                        name="values",
                        stable_id=7,
                        type=schema_pb2.DataType(
                            list=schema_pb2.ListType(
                                element=schema_pb2.Field(
                                    name="element",
                                    stable_id=8,
                                    type=schema_pb2.DataType(
                                        primitive=schema_pb2.PrimitiveType(kind=schema_pb2.PRIMITIVE_KIND_FLOAT)
                                    ),
                                )
                            )
                        ),
                    )
                ],
                retired_fields=[
                    schema_pb2.RetiredField(
                        identity="f:6",
                        stable_id=6,
                        proto_full_name="example.v1.Record.old_value",
                        name="old_value",
                        storage_name_source="old_value",
                    )
                ],
            )
        ],
    )

    migrated = migrate_schema_bundle(legacy)

    assert (migrated.ir_version, migrated.mapping_version) == (
        SCHEMA_IR_VERSION,
        SCHEMA_MAPPING_VERSION,
    )
    assert migrated.source_descriptor_sha256 == b"digest"
    assert migrated.datasets[0].last_field_id == 8
    assert migrated.datasets[0].fields[0].stable_id == 7
    assert migrated.datasets[0].fields[0].type.list.element.stable_id == 8
    assert migrated.datasets[0].retired_fields[0] == schema_pb2.RetiredField(
        identity="f:6",
        stable_id=6,
        proto_full_name="example.v1.Record.old_value",
        name="old_value",
        storage_name_source="old_value",
    )
    assert parse_schema_bundle(legacy.SerializeToString()) == migrated
    assert migrate_schema_bundle(migrated) is migrated


@pytest.mark.parametrize(("ir_version", "mapping_version"), [(3, 3), (4, 2)])
def test_migration_rejects_mixed_version_pairs(ir_version: int, mapping_version: int) -> None:
    bundle = schema_pb2.SchemaBundle(ir_version=ir_version, mapping_version=mapping_version)

    with pytest.raises(ValueError, match=rf"version pair ir_version={ir_version} mapping_version={mapping_version}"):
        migrate_schema_bundle(bundle)


def test_migration_rejects_fixed_lists_that_could_not_exist_in_mapping_v2() -> None:
    legacy = schema_pb2.SchemaBundle(
        ir_version=3,
        mapping_version=2,
        datasets=[
            schema_pb2.DatasetSchema(
                name="example_v1_record",
                fields=[
                    schema_pb2.Field(
                        name="embedding",
                        type=schema_pb2.DataType(
                            list=schema_pb2.ListType(
                                element=schema_pb2.Field(name="element"),
                                fixed_length=8,
                            )
                        ),
                    )
                ],
            )
        ],
    )

    with pytest.raises(
        ValueError,
        match=r"mapping_version 2 field 'example_v1_record\.embedding' contains fixed_length 8",
    ):
        migrate_schema_bundle(legacy)


def test_migration_rejects_unknown_legacy_wire_fields() -> None:
    legacy = schema_pb2.SchemaBundle(ir_version=3, mapping_version=2)
    encoded = legacy.SerializeToString(deterministic=True) + bytes([0xF8, 0x07, 0x01])

    with pytest.raises(ValueError, match="fields unknown to this migrator"):
        parse_schema_bundle(encoded)


def test_round_trips_portable_refined_types_and_fixed_list_shape() -> None:
    bundle = schema_pb2.SchemaBundle(
        ir_version=SCHEMA_IR_VERSION,
        mapping_version=SCHEMA_MAPPING_VERSION,
        datasets=[
            schema_pb2.DatasetSchema(
                source_message="example.v1.Record",
                name="example_v1_record",
                fields=[
                    schema_pb2.Field(
                        name="amount",
                        type=schema_pb2.DataType(decimal=schema_pb2.DecimalType(precision=18, scale=4)),
                    ),
                    schema_pb2.Field(name="id", type=schema_pb2.DataType(uuid=schema_pb2.UuidType())),
                    schema_pb2.Field(
                        name="digest",
                        type=schema_pb2.DataType(fixed_bytes=schema_pb2.FixedBytesType(byte_length=32)),
                    ),
                    schema_pb2.Field(
                        name="embedding",
                        type=schema_pb2.DataType(
                            list=schema_pb2.ListType(
                                element=schema_pb2.Field(
                                    name="element",
                                    type=schema_pb2.DataType(
                                        primitive=schema_pb2.PrimitiveType(kind=schema_pb2.PRIMITIVE_KIND_FLOAT)
                                    ),
                                ),
                                fixed_length=1536,
                            )
                        ),
                    ),
                ],
            )
        ],
    )

    decoded = parse_schema_bundle(serialize_schema_bundle(bundle))
    fields = decoded.datasets[0].fields
    assert fields[0].type.WhichOneof("kind") == "decimal"
    assert (fields[0].type.decimal.precision, fields[0].type.decimal.scale) == (18, 4)
    assert fields[1].type.WhichOneof("kind") == "uuid"
    assert fields[2].type.WhichOneof("kind") == "fixed_bytes"
    assert fields[2].type.fixed_bytes.byte_length == 32
    assert fields[3].type.WhichOneof("kind") == "list"
    assert fields[3].type.list.fixed_length == 1536
    assert fields[3].type.list.element.type.primitive.kind == schema_pb2.PRIMITIVE_KIND_FLOAT
