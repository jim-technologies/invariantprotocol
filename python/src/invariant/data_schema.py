"""Read and write the derived, language-neutral protobuf data contract."""

from invariant.gen.invariant.data.v1.schema_pb2 import (
    DatasetSchema,  # ty: ignore[unresolved-import] — generated member
    SchemaBundle,  # ty: ignore[unresolved-import] — generated member
)

SCHEMA_IR_VERSION = 2
SCHEMA_MAPPING_VERSION = 2


def parse_schema_bundle(data: bytes | bytearray | memoryview) -> SchemaBundle:
    """Parse a supported ``invariant.data.v1.SchemaBundle``."""
    bundle = SchemaBundle()
    bundle.ParseFromString(bytes(data))
    validate_schema_bundle(bundle)
    return bundle


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
    "parse_schema_bundle",
    "serialize_schema_bundle",
    "validate_schema_bundle",
]
