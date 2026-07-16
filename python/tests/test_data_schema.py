from pathlib import Path

import pytest

from invariant import find_dataset, parse_schema_bundle, serialize_schema_bundle
from invariant.gen.invariant.data.v1 import schema_pb2

GOLDEN_BUNDLE = Path(__file__).resolve().parents[2] / "testdata" / "data.schema.binpb"


def test_reads_shared_canonical_schema_bundle() -> None:
    encoded = GOLDEN_BUNDLE.read_bytes()
    bundle = parse_schema_bundle(encoded)

    assert bundle.ir_version == 1
    assert bundle.mapping_version == 1
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

    labels = fields["labels"]
    assert (labels.stable_id, labels.presence) == (19, schema_pb2.PRESENCE_REPEATED)
    assert labels.type.list.element.stable_id == 31
    assert labels.type.list.element.presence == schema_pb2.PRESENCE_NOT_APPLICABLE
    assert labels.type.list.element.synthetic_role == schema_pb2.SYNTHETIC_ROLE_LIST_ELEMENT

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
    bundle = schema_pb2.SchemaBundle(ir_version=1, mapping_version=1)
    setattr(bundle, field, 2)

    with pytest.raises(ValueError, match=message):
        parse_schema_bundle(bundle.SerializeToString())
