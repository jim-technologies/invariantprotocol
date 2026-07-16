from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path

import data_pb2
import data_proto2_pb2
import pyarrow as pa
import pyarrow.parquet as pq
import pytest
from google.protobuf import descriptor_pb2, descriptor_pool, message_factory

from invariant import arrow_schema, arrow_table, find_dataset, parse_schema_bundle
from invariant.gen.invariant.data.v1 import schema_pb2

GOLDEN_BUNDLE = Path(__file__).resolve().parents[2] / "testdata" / "data.schema.binpb"


def test_core_import_does_not_import_optional_pyarrow() -> None:
    result = subprocess.run(
        [sys.executable, "-c", "import invariant, sys; assert 'pyarrow' not in sys.modules"],
        check=False,
        capture_output=True,
        text=True,
    )
    assert result.returncode == 0, result.stderr


def test_maps_shared_bundle_to_arrow_with_stable_metadata() -> None:
    bundle = parse_schema_bundle(GOLDEN_BUNDLE.read_bytes())
    dataset = find_dataset(bundle, "data.v1.CanonicalRecord")
    assert dataset is not None

    schema, diagnostics = arrow_schema(dataset)

    assert isinstance(schema, pa.Schema)
    assert len(schema) == len(dataset.fields)
    assert _metadata(schema, "invariant.source_message") == dataset.source_message
    assert _metadata(schema, "invariant.dataset") == dataset.name

    assert schema.field("double_value").type == pa.float64()
    assert schema.field("float_value").type == pa.float32()
    assert schema.field("uint64_value").type == pa.uint64()
    assert schema.field("uint32_value").type == pa.uint32()
    assert schema.field("bytes_value").type == pa.binary()
    assert _metadata(schema.field("uint64_value"), "PARQUET:field_id") == "4"
    assert _metadata(schema.field("uint64_value"), "invariant.proto.json_name") == "uint64Value"

    enum_field = schema.field("state")
    assert enum_field.type == pa.int32()
    assert "DATA_STATE_ACTIVE" in _metadata(enum_field, "invariant.enum.values")

    optional = schema.field("optional_note")
    assert optional.nullable
    assert _metadata(optional, "invariant.presence") == "PRESENCE_EXPLICIT"

    nested = schema.field("nested")
    assert pa.types.is_struct(nested.type)
    assert nested.type.field("id").type == pa.int64()
    assert _metadata(nested.type.field("id"), "PARQUET:field_id") != _metadata(nested, "PARQUET:field_id")

    labels = schema.field("labels")
    assert pa.types.is_list(labels.type)
    assert labels.type.value_field.name == "item"
    assert not labels.type.value_field.nullable
    assert _metadata(labels.type.value_field, "PARQUET:field_id") != _metadata(labels, "PARQUET:field_id")

    counters = schema.field("counters")
    assert pa.types.is_map(counters.type)
    assert counters.type.key_type == pa.string()
    assert counters.type.item_type == pa.uint64()
    assert not counters.type.key_field.nullable
    assert _metadata(counters.type.key_field, "PARQUET:field_id") != _metadata(
        counters.type.item_field, "PARQUET:field_id"
    )

    created_at = schema.field("created_at")
    assert created_at.type == pa.timestamp("ns", tz="UTC")
    elapsed = schema.field("elapsed")
    assert elapsed.type == pa.duration("ns")
    assert isinstance(schema.field("attributes").type, pa.JsonType)

    by_path = {diagnostic.field_path: diagnostic for diagnostic in diagnostics}
    assert len(diagnostics) == dataset.last_field_id == 36
    assert len(by_path) == len(diagnostics)
    assert by_path["state"].compatibility == schema_pb2.MAPPING_COMPATIBILITY_LOSSLESS
    assert by_path["choice_count"].compatibility == schema_pb2.MAPPING_COMPATIBILITY_RANGE_WIDENED
    assert "mutual exclusivity" in by_path["choice_count"].message
    assert by_path["created_at"].compatibility == schema_pb2.MAPPING_COMPATIBILITY_RANGE_REDUCED
    assert by_path["elapsed"].compatibility == schema_pb2.MAPPING_COMPATIBILITY_RANGE_REDUCED
    assert by_path["attributes"].compatibility == schema_pb2.MAPPING_COMPATIBILITY_RANGE_REDUCED
    assert "numbers to be finite" in by_path["attributes"].message
    assert by_path["opaque"].compatibility == schema_pb2.MAPPING_COMPATIBILITY_RANGE_REDUCED
    assert "type URL to resolve" in by_path["opaque"].message
    assert {"nested.id", "labels[]", "counters.key", "counters.value"} <= by_path.keys()

    closed = schema_pb2.DatasetSchema()
    closed.CopyFrom(dataset)
    next(field for field in closed.fields if field.name == "state").type.enum.closed = True
    _, closed_diagnostics = arrow_schema(closed)
    closed_state = next(item for item in closed_diagnostics if item.field_path == "state")
    assert closed_state.compatibility == schema_pb2.MAPPING_COMPATIBILITY_RANGE_WIDENED
    assert "closed value set" in closed_state.message


def test_preserves_proto2_required_presence_and_declared_default() -> None:
    bundle = parse_schema_bundle(GOLDEN_BUNDLE.read_bytes())
    dataset = find_dataset(bundle, "data.v1.Proto2Record")
    assert dataset is not None

    schema, diagnostics = arrow_schema(dataset)

    identifier = schema.field("id")
    assert not identifier.nullable
    assert _metadata(identifier, "invariant.presence") == "PRESENCE_REQUIRED"

    label = schema.field("label")
    assert label.nullable
    assert _metadata(label, "invariant.proto.has_default") == "true"
    assert _metadata(label, "invariant.proto.default") == "unknown"
    assert all(item.compatibility == schema_pb2.MAPPING_COMPATIBILITY_LOSSLESS for item in diagnostics)


def test_arrow_schema_writes_and_reads_real_parquet(tmp_path: Path) -> None:
    bundle = parse_schema_bundle(GOLDEN_BUNDLE.read_bytes())
    dataset = find_dataset(bundle, "data.v1.CanonicalRecord")
    assert dataset is not None
    record = data_pb2.CanonicalRecord(
        double_value=1.25,
        float_value=2.5,
        int64_value=-3,
        uint64_value=4,
        int32_value=-5,
        fixed64_value=6,
        fixed32_value=7,
        bool_value=True,
        string_value="eight",
        bytes_value=b"nine",
        uint32_value=10,
        sfixed32_value=-11,
        sfixed64_value=-12,
        sint32_value=-13,
        sint64_value=-14,
        state=data_pb2.DATA_STATE_READY,
        optional_note="",
        nested=data_pb2.NestedRecord(id=15, label="nested"),
        labels=["two", "one"],
        children=[data_pb2.NestedRecord(id=16)],
        counters={"two": 2, "one": 1},
        choice_count=0,
    )
    record.created_at.seconds = 1_768_478_400
    record.created_at.nanos = 123
    record.elapsed.seconds = 18
    record.elapsed.nanos = 19
    record.wrapped_count.value = 0
    record.attributes.update({"active": True})
    record.opaque.Pack(data_pb2.NestedRecord(id=21))

    table, diagnostics = arrow_table(dataset, [record])
    schema = table.schema
    assert diagnostics
    assert table.column("choice_count").to_pylist() == [0]
    assert table.column("choice_name").to_pylist() == [None]
    assert table.column("optional_note").to_pylist() == [""]
    assert table.column("wrapped_count").to_pylist() == [0]
    assert table.column("created_at").cast(pa.int64()).to_pylist() == [1_768_478_400_000_000_123]
    assert table.column("elapsed").cast(pa.int64()).to_pylist() == [18_000_000_019]
    assert json.loads(table.column("attributes").to_pylist()[0]) == {"active": True}
    assert json.loads(table.column("opaque").to_pylist()[0]) == {
        "@type": "type.googleapis.com/data.v1.NestedRecord",
        "id": "21",
    }

    path = tmp_path / "canonical.parquet"
    pq.write_table(table, path)

    restored = pq.read_table(path)
    assert restored.num_rows == 1
    assert restored.schema.equals(schema)
    assert _metadata(restored.schema, "invariant.source_message") == "data.v1.CanonicalRecord"
    assert _metadata(restored.schema.field("uint64_value"), "PARQUET:field_id") == "4"
    assert _metadata(restored.schema.field("labels").type.value_field, "PARQUET:field_id") == "31"
    assert _metadata(restored.schema.field("counters").type.key_field, "PARQUET:field_id") == "35"
    assert _metadata(restored.schema.field("counters").type.item_field, "PARQUET:field_id") == "36"
    assert restored.column("labels").to_pylist() == [["two", "one"]]
    assert restored.column("counters").to_pylist() == [[("one", 1), ("two", 2)]]
    assert json.loads(restored.column("attributes").to_pylist()[0]) == {"active": True}


def test_arrow_table_preserves_absence_and_rejects_wrong_or_uninitialized_messages() -> None:
    bundle = parse_schema_bundle(GOLDEN_BUNDLE.read_bytes())
    canonical = find_dataset(bundle, "data.v1.CanonicalRecord")
    proto2 = find_dataset(bundle, "data.v1.Proto2Record")
    assert canonical is not None
    assert proto2 is not None

    table, _ = arrow_table(canonical, [data_pb2.CanonicalRecord()])
    assert table.column("optional_note").to_pylist() == [None]
    assert table.column("nested").to_pylist() == [None]
    assert table.column("labels").to_pylist() == [[]]
    assert table.column("counters").to_pylist() == [[]]

    with pytest.raises(TypeError, match="cannot convert protobuf message"):
        arrow_table(canonical, [data_proto2_pb2.Proto2Record(id=1)])

    with pytest.raises(ValueError, match="missing required fields: id"):
        arrow_table(proto2, [data_proto2_pb2.Proto2Record()])

    proto2_table, _ = arrow_table(proto2, [data_proto2_pb2.Proto2Record(id=1)])
    assert proto2_table.column("label").to_pylist() == [None]


def test_arrow_table_rejects_stale_same_name_generated_message() -> None:
    pool = descriptor_pool.DescriptorPool()  # ty: ignore[possibly-missing-implicit-call]
    added: set[str] = set()

    def add_file(file_descriptor) -> None:
        if file_descriptor.name in added:
            return
        for dependency in file_descriptor.dependencies:
            add_file(dependency)
        file_proto = descriptor_pb2.FileDescriptorProto.FromString(file_descriptor.serialized_pb)
        if file_descriptor is data_pb2.DESCRIPTOR:
            record = next(message for message in file_proto.message_type if message.name == "CanonicalRecord")
            double_value = next(field for field in record.field if field.number == 1)
            double_value.type = descriptor_pb2.FieldDescriptorProto.TYPE_INT64
        pool.Add(file_proto)
        added.add(file_descriptor.name)

    add_file(data_pb2.DESCRIPTOR)
    stale_class = message_factory.GetMessageClass(pool.FindMessageTypeByName("data.v1.CanonicalRecord"))
    stale = stale_class(double_value=7)

    bundle = parse_schema_bundle(GOLDEN_BUNDLE.read_bytes())
    dataset = find_dataset(bundle, "data.v1.CanonicalRecord")
    assert dataset is not None

    with pytest.raises(ValueError, match=r"descriptor does not match.*primitive kind"):
        arrow_table(dataset, [stale])


def test_arrow_table_validates_each_same_name_descriptor_pool() -> None:
    file_proto = descriptor_pb2.FileDescriptorProto.FromString(data_proto2_pb2.DESCRIPTOR.serialized_pb)
    record = next(message for message in file_proto.message_type if message.name == "Proto2Record")
    label = next(field for field in record.field if field.name == "label")
    label.default_value = "stale"

    pool = descriptor_pool.DescriptorPool()  # ty: ignore[possibly-missing-implicit-call]
    pool.Add(file_proto)
    stale_class = message_factory.GetMessageClass(pool.FindMessageTypeByName("data.v1.Proto2Record"))

    bundle = parse_schema_bundle(GOLDEN_BUNDLE.read_bytes())
    dataset = find_dataset(bundle, "data.v1.Proto2Record")
    assert dataset is not None

    current = data_proto2_pb2.Proto2Record(id=1)
    stale = stale_class(id=2)
    with pytest.raises(ValueError, match=r"descriptor does not match.*protobuf default"):
        arrow_table(dataset, [current, stale])


@pytest.mark.parametrize(
    ("field", "seconds", "nanos", "message"),
    [
        ("created_at", 0, 1_000_000_000, "Timestamp.*nanos"),
        ("created_at", 253_402_300_800, 0, "Timestamp.*seconds"),
        ("elapsed", 1, -1, "Duration.*signs disagree"),
        ("elapsed", 0, -1_000_000_000, "Duration.*nanos"),
    ],
)
def test_arrow_table_rejects_invalid_protobuf_temporal_values(
    field: str,
    seconds: int,
    nanos: int,
    message: str,
) -> None:
    bundle = parse_schema_bundle(GOLDEN_BUNDLE.read_bytes())
    dataset = find_dataset(bundle, "data.v1.CanonicalRecord")
    assert dataset is not None
    record = data_pb2.CanonicalRecord()
    temporal = getattr(record, field)
    temporal.seconds = seconds
    temporal.nanos = nanos

    with pytest.raises(ValueError, match=message):
        arrow_table(dataset, [record])


def test_arrow_table_reports_protojson_range_failures_with_source_context() -> None:
    bundle = parse_schema_bundle(GOLDEN_BUNDLE.read_bytes())
    dataset = find_dataset(bundle, "data.v1.CanonicalRecord")
    assert dataset is not None

    non_finite = data_pb2.CanonicalRecord()
    non_finite.attributes.fields["bad"].number_value = float("inf")
    with pytest.raises(
        ValueError,
        match=r"protobuf JSON field 'attributes'.*data\.v1\.CanonicalRecord\.attributes.*numbers to be finite",
    ):
        arrow_table(dataset, [non_finite])

    unresolved = data_pb2.CanonicalRecord()
    unresolved.opaque.type_url = "type.googleapis.com/example.v1.Missing"
    unresolved.opaque.value = b"\x08\x01"
    with pytest.raises(
        ValueError,
        match=r"protobuf JSON field 'opaque'.*data\.v1\.CanonicalRecord\.opaque.*type URL to resolve",
    ):
        arrow_table(dataset, [unresolved])


def _metadata(owner: pa.Schema | pa.Field, key: str) -> str:
    metadata = owner.metadata
    assert metadata is not None
    return metadata[key.encode()].decode()
