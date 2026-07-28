"""Read and write the derived, language-neutral protobuf data contract."""

from invariant.gen.invariant.data.v1.schema_pb2 import (
    DatasetSchema,
    SchemaBundle,
)

SCHEMA_IR_VERSION = 4
SCHEMA_MAPPING_VERSION = 3


def parse_schema_bundle(data: bytes | bytearray | memoryview) -> SchemaBundle:
    """Parse a supported ``invariant.data.v1.SchemaBundle``."""
    bundle = SchemaBundle()
    bundle.ParseFromString(bytes(data))
    return migrate_schema_bundle(bundle)


def migrate_schema_bundle(bundle: SchemaBundle) -> SchemaBundle:
    """Upgrade the one supported historical SchemaBundle version in memory."""
    if (bundle.ir_version, bundle.mapping_version) == (SCHEMA_IR_VERSION, SCHEMA_MAPPING_VERSION):
        return bundle
    if (bundle.ir_version, bundle.mapping_version) != (3, 2):
        raise ValueError(
            "unsupported SchemaBundle version pair "
            f"ir_version={bundle.ir_version} mapping_version={bundle.mapping_version}; "
            f"expected 3/2 or {SCHEMA_IR_VERSION}/{SCHEMA_MAPPING_VERSION}"
        )

    known = SchemaBundle()
    known.CopyFrom(bundle)
    known.DiscardUnknownFields()
    if known.SerializeToString(deterministic=True) != bundle.SerializeToString(deterministic=True):
        raise ValueError("migrate SchemaBundle: legacy artifact contains fields unknown to this migrator")

    def validate_fields(fields, path: str) -> None:
        for field in fields:
            field_path = f"{path}.{field.name}" if path else field.name
            kind = field.type.WhichOneof("kind") if field.HasField("type") else None
            if kind == "struct":
                validate_fields(field.type.struct.fields, field_path)
            elif kind == "list":
                if field.type.list.fixed_length:
                    raise ValueError(
                        "SchemaBundle mapping_version 2 field "
                        f"{field_path!r} contains fixed_length {field.type.list.fixed_length}, "
                        f"which was introduced in mapping_version {SCHEMA_MAPPING_VERSION}"
                    )
                validate_fields([field.type.list.element], f"{field_path}[]")
            elif kind == "map":
                validate_fields([field.type.map.key], f"{field_path}.key")
                validate_fields([field.type.map.value], f"{field_path}.value")

    for dataset in bundle.datasets:
        validate_fields(dataset.fields, dataset.name)

    migrated = SchemaBundle()
    migrated.CopyFrom(bundle)
    migrated.ir_version = SCHEMA_IR_VERSION
    migrated.mapping_version = SCHEMA_MAPPING_VERSION
    return migrated


def validate_schema_bundle(bundle: SchemaBundle) -> None:
    """Reject a bundle whose IR or mapping rules this package cannot interpret."""
    if bundle.ir_version != SCHEMA_IR_VERSION:
        raise ValueError(f"unsupported SchemaBundle ir_version {bundle.ir_version}; expected {SCHEMA_IR_VERSION}")
    if bundle.mapping_version != SCHEMA_MAPPING_VERSION:
        raise ValueError(
            f"unsupported SchemaBundle mapping_version {bundle.mapping_version}; expected {SCHEMA_MAPPING_VERSION}"
        )


def serialize_schema_bundle(bundle: SchemaBundle) -> bytes:
    """Serialize a schema bundle deterministically for storage or transport."""
    validate_schema_bundle(bundle)
    return bundle.SerializeToString(deterministic=True)


def find_dataset(bundle: SchemaBundle, source_message: str) -> DatasetSchema | None:
    """Find a dataset by its fully-qualified protobuf source message name."""
    return next((dataset for dataset in bundle.datasets if dataset.source_message == source_message), None)


__all__ = [
    "SCHEMA_IR_VERSION",
    "SCHEMA_MAPPING_VERSION",
    "DatasetSchema",
    "SchemaBundle",
    "find_dataset",
    "migrate_schema_bundle",
    "parse_schema_bundle",
    "serialize_schema_bundle",
    "validate_schema_bundle",
]
