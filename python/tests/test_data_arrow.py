from __future__ import annotations

import json
import subprocess
import sys
from decimal import Decimal
from pathlib import Path
from uuid import UUID

import data_pb2
import data_proto2_pb2
import pyarrow as pa
import pyarrow.parquet as pq
import pytest
from google.protobuf import any_pb2, descriptor_pb2, descriptor_pool, message_factory, struct_pb2, wrappers_pb2

from invariant import arrow_record_batch_reader, arrow_schema, arrow_table, find_dataset, parse_schema_bundle
from invariant.gen.invariant.data.v1 import annotations_pb2, schema_pb2

GOLDEN_BUNDLE = Path(__file__).resolve().parents[2] / "testdata" / "data.schema.binpb"
FIXED_LIST_BUNDLE = Path(__file__).resolve().parents[2] / "testdata" / "schema" / "schema.binpb"
FIXED_LIST_DESCRIPTOR = Path(__file__).resolve().parents[2] / "testdata" / "schema" / "descriptor.binpb"


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

    primitive_types = {
        "double_value": pa.float64(),
        "float_value": pa.float32(),
        "int64_value": pa.int64(),
        "uint64_value": pa.uint64(),
        "int32_value": pa.int32(),
        "fixed64_value": pa.uint64(),
        "fixed32_value": pa.uint32(),
        "bool_value": pa.bool_(),
        "string_value": pa.string(),
        "bytes_value": pa.binary(),
        "uint32_value": pa.uint32(),
        "sfixed32_value": pa.int32(),
        "sfixed64_value": pa.int64(),
        "sint32_value": pa.int32(),
        "sint64_value": pa.int64(),
        "state": pa.int32(),
    }
    for name, expected_type in primitive_types.items():
        assert schema.field(name).type == expected_type
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
        state=123,
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
    expected_scalars = {
        "double_value": 1.25,
        "float_value": 2.5,
        "int64_value": -3,
        "uint64_value": 4,
        "int32_value": -5,
        "fixed64_value": 6,
        "fixed32_value": 7,
        "bool_value": True,
        "string_value": "eight",
        "bytes_value": b"nine",
        "uint32_value": 10,
        "sfixed32_value": -11,
        "sfixed64_value": -12,
        "sint32_value": -13,
        "sint64_value": -14,
        "state": 123,
    }
    for name, value in expected_scalars.items():
        assert table.column(name).to_pylist() == [value]
    assert table.column("choice_count").to_pylist() == [0]
    assert table.column("choice_name").to_pylist() == [None]
    assert table.column("optional_note").to_pylist() == [""]
    assert table.column("nested").to_pylist() == [{"id": 15, "label": "nested"}]
    assert table.column("children").to_pylist() == [[{"id": 16, "label": None}]]
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
    assert table.column("choice_count").to_pylist() == [None]
    assert table.column("choice_name").to_pylist() == [None]

    choices, _ = arrow_table(
        canonical,
        [
            data_pb2.CanonicalRecord(choice_count=0),
            data_pb2.CanonicalRecord(choice_name=""),
            data_pb2.CanonicalRecord(),
        ],
    )
    assert choices.column("choice_count").to_pylist() == [0, None, None]
    assert choices.column("choice_name").to_pylist() == [None, "", None]

    with pytest.raises(TypeError, match="cannot convert protobuf message"):
        arrow_table(canonical, [data_proto2_pb2.Proto2Record(id=1)])

    with pytest.raises(ValueError, match="missing required fields: id"):
        arrow_table(proto2, [data_proto2_pb2.Proto2Record()])

    proto2_table, _ = arrow_table(proto2, [data_proto2_pb2.Proto2Record(id=1)])
    assert proto2_table.column("label").to_pylist() == [None]


def test_zero_field_arrow_table_and_ipc_preserve_rows_while_pyarrow_25_parquet_cannot(
    tmp_path: Path,
) -> None:
    file_proto = descriptor_pb2.FileDescriptorProto(
        name="empty_record.proto",
        package="data.empty",
        syntax="proto3",
    )
    file_proto.message_type.add(name="EmptyRecord")
    pool = descriptor_pool.DescriptorPool()  # ty: ignore[possibly-missing-implicit-call]
    pool.Add(file_proto)
    descriptor = pool.FindMessageTypeByName("data.empty.EmptyRecord")
    record = message_factory.GetMessageClass(descriptor)
    dataset = schema_pb2.DatasetSchema(
        source_message=descriptor.full_name,
        name="empty_records",
    )

    table, diagnostics = arrow_table(dataset, [record(), record()])

    assert table.num_rows == 2
    assert table.num_columns == 0
    assert _metadata(table.schema, "invariant.dataset") == "empty_records"
    assert diagnostics == []

    bounded, bounded_diagnostics = arrow_record_batch_reader(
        dataset,
        (record() for _ in range(257)),
    )
    bounded_schema = bounded.schema
    batches = list(bounded)
    assert bounded_diagnostics == []
    assert [batch.num_rows for batch in batches] == [256, 1]
    assert all(batch.num_columns == 0 for batch in batches)
    assert all(batch.schema.equals(bounded_schema, check_metadata=True) for batch in batches)
    assert pa.Table.from_batches(batches, schema=bounded_schema).num_rows == 257

    empty, _ = arrow_record_batch_reader(dataset, [], batch_size=2)
    empty_table = empty.read_all()
    assert empty_table.num_rows == 0
    assert empty_table.num_columns == 0
    assert empty_table.schema.equals(table.schema, check_metadata=True)
    assert empty.schema.equals(table.schema, check_metadata=True)

    arrow_path = tmp_path / "empty.arrow"
    with pa.ipc.new_file(arrow_path, bounded_schema) as writer:
        for batch in batches:
            writer.write_batch(batch)
    with pa.ipc.open_file(arrow_path) as reader:
        assert reader.read_all().num_rows == 257

    parquet_path = tmp_path / "empty.parquet"
    pq.write_table(table, parquet_path)
    assert pq.read_table(parquet_path).num_rows == 0


def test_arrow_record_batch_reader_is_lazy_row_bounded_and_writes_standard_arrow_sinks(
    tmp_path: Path,
) -> None:
    dataset, record = _fixed_list_record_fixture()
    consumed: list[int] = []

    def message(identifier: int):
        return record(
            id=str(identifier),
            label=f"row-{identifier}",
            vector=[float(identifier + offset) for offset in range(8)],
            vector64=[float(identifier + offset) for offset in range(4)],
        )

    def messages():
        for identifier in range(5):
            consumed.append(identifier)
            yield message(identifier)

    reader, diagnostics = arrow_record_batch_reader(dataset, messages(), batch_size=2)

    assert isinstance(reader, pa.RecordBatchReader)
    assert consumed == []
    vector_type = reader.schema.field("vector").type
    assert pa.types.is_fixed_size_list(vector_type)
    assert vector_type.value_type == pa.float32()
    assert vector_type.list_size == 8
    assert diagnostics

    ipc_path = tmp_path / "bounded.arrow"
    parquet_path = tmp_path / "bounded.parquet"
    batch_sizes = []
    with (
        pa.ipc.new_file(ipc_path, reader.schema) as ipc_writer,
        pq.ParquetWriter(parquet_path, reader.schema) as parquet_writer,
    ):
        for batch in reader:
            batch_sizes.append(batch.num_rows)
            assert len(consumed) == sum(batch_sizes)
            assert batch.schema.equals(reader.schema, check_metadata=True)
            ipc_writer.write_batch(batch)
            parquet_writer.write_batch(batch)

    assert batch_sizes == [2, 2, 1]
    assert consumed == [0, 1, 2, 3, 4]
    assert list(reader) == []

    expected, _ = arrow_table(dataset, [message(identifier) for identifier in range(5)])
    with pa.ipc.open_file(ipc_path) as ipc_reader:
        assert ipc_reader.read_all().equals(expected)
    assert pq.read_table(parquet_path).equals(expected)


def test_arrow_record_batch_reader_validates_batches_when_consumed() -> None:
    dataset, record = _fixed_list_record_fixture()
    valid_vector = [float(index) for index in range(8)]
    valid_vector64 = [float(index) for index in range(4)]

    for batch_size in (0, -1):
        with pytest.raises(ValueError, match="batch_size must be positive"):
            arrow_record_batch_reader(dataset, [], batch_size=batch_size)
    for batch_size in (True, 1.5):
        with pytest.raises(TypeError, match="batch_size must be an integer"):
            arrow_record_batch_reader(dataset, [], batch_size=batch_size)  # ty: ignore[invalid-argument-type]

    consumed = []

    def messages():
        values = [
            record(id="first", label="First", vector=valid_vector, vector64=valid_vector64),
            record(id="second", label="Second", vector=valid_vector, vector64=valid_vector64),
            record(id="invalid", label="Invalid", vector=[], vector64=valid_vector64),
            record(id="unconsumed", label="Unconsumed", vector=valid_vector, vector64=valid_vector64),
        ]
        for value in values:
            consumed.append(value.id)
            yield value

    reader, _ = arrow_record_batch_reader(
        dataset,
        messages(),
        batch_size=2,
    )
    assert reader.read_next_batch().column("id").to_pylist() == ["first", "second"]
    assert consumed == ["first", "second"]
    with pytest.raises(
        ValueError,
        match=r"fixed-list field 'schema_test_v1_lance_record\.vector' has 0 elements; expected exactly 8",
    ):
        reader.read_next_batch()
    assert consumed == ["first", "second", "invalid"]

    def failing_source():
        yield record(id="valid", label="Valid", vector=valid_vector, vector64=valid_vector64)
        raise RuntimeError("source failed")

    source_reader, _ = arrow_record_batch_reader(dataset, failing_source(), batch_size=1)
    assert source_reader.read_next_batch().column("id").to_pylist() == ["valid"]
    with pytest.raises(RuntimeError, match="source failed"):
        source_reader.read_next_batch()

    snapshot_reader, _ = arrow_record_batch_reader(
        dataset,
        [record(id="snapshot", label="Snapshot", vector=valid_vector, vector64=valid_vector64)],
        batch_size=1,
    )
    dataset.fields[0].name = "mutated_after_reader_creation"
    snapshot_batch = snapshot_reader.read_next_batch()
    assert snapshot_batch.schema.get_field_index("id") >= 0
    assert snapshot_batch.column("id").to_pylist() == ["snapshot"]


def test_fixed_lists_use_native_arrow_schema_and_arrays(tmp_path: Path) -> None:
    dataset, record = _fixed_list_record_fixture()

    schema, diagnostics = arrow_schema(dataset)

    vector = schema.field("vector")
    assert pa.types.is_fixed_size_list(vector.type)
    assert vector.type == pa.list_(pa.field("item", pa.float32(), nullable=False), list_size=8)
    assert vector.type.list_size == 8
    assert vector.type.value_type == pa.float32()
    assert _metadata(vector, "invariant.logical_type") == "fixed_list"

    vector64 = schema.field("vector64")
    assert pa.types.is_fixed_size_list(vector64.type)
    assert vector64.type == pa.list_(pa.field("item", pa.float64(), nullable=False), list_size=4)
    assert vector64.type.list_size == 4
    assert vector64.type.value_type == pa.float64()

    compatibility = {item.field_path: item.compatibility for item in diagnostics}
    assert compatibility["vector"] == schema_pb2.MAPPING_COMPATIBILITY_LOSSLESS
    assert compatibility["vector64"] == schema_pb2.MAPPING_COMPATIBILITY_LOSSLESS

    table, _ = arrow_table(
        dataset,
        [
            record(
                id="first",
                label="First",
                vector=[0.0, 1.0, 2.0, 3.0, 4.0, 5.0, 6.0, 7.0],
                vector64=[0.5, 1.5, 2.5, 3.5],
            ),
            record(
                id="second",
                label="Second",
                vector=[8.0, 9.0, 10.0, 11.0, 12.0, 13.0, 14.0, 15.0],
                vector64=[4.5, 5.5, 6.5, 7.5],
            ),
        ],
    )

    assert pa.types.is_fixed_size_list(table.column("vector").type)
    assert pa.types.is_fixed_size_list(table.column("vector64").type)
    assert table.column("vector").to_pylist() == [
        [0.0, 1.0, 2.0, 3.0, 4.0, 5.0, 6.0, 7.0],
        [8.0, 9.0, 10.0, 11.0, 12.0, 13.0, 14.0, 15.0],
    ]
    assert table.column("vector64").to_pylist() == [
        [0.5, 1.5, 2.5, 3.5],
        [4.5, 5.5, 6.5, 7.5],
    ]

    parquet_path = tmp_path / "fixed-lists.parquet"
    pq.write_table(table, parquet_path)
    restored = pq.read_table(parquet_path)
    assert pa.types.is_fixed_size_list(restored.schema.field("vector").type)
    assert restored.schema.field("vector").type.list_size == 8
    assert restored.column("vector").to_pylist() == table.column("vector").to_pylist()


def test_fixed_lists_reject_wrong_length_and_omitted_values_with_canonical_paths() -> None:
    dataset, record = _fixed_list_record_fixture()
    valid_vector = [float(index) for index in range(8)]
    valid_vector64 = [float(index) for index in range(4)]

    for vector in ([], [0.0] * 7, [0.0] * 9):
        with pytest.raises(
            ValueError,
            match=rf"fixed-list field 'schema_test_v1_lance_record\.vector' has {len(vector)} elements; "
            r"expected exactly 8",
        ):
            arrow_table(
                dataset,
                [record(id="invalid", label="Invalid", vector=vector, vector64=valid_vector64)],
            )

    for vector64 in ([], [0.0] * 3, [0.0] * 5):
        with pytest.raises(
            ValueError,
            match=rf"fixed-list field 'schema_test_v1_lance_record\.vector64' has {len(vector64)} elements; "
            r"expected exactly 4",
        ):
            arrow_table(
                dataset,
                [record(id="invalid", label="Invalid", vector=valid_vector, vector64=vector64)],
            )


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

    reader, _ = arrow_record_batch_reader(dataset, [current, stale], batch_size=1)
    assert reader.read_next_batch().column("id").to_pylist() == [1]
    with pytest.raises(ValueError, match=r"descriptor does not match.*protobuf default"):
        reader.read_next_batch()


def test_arrow_table_rejects_removed_or_changed_refinement_annotations() -> None:
    refined, refined_record = _refined_record_fixture()
    refined_value = refined_record(
        amount="1.00",
        record_id="550e8400-e29b-41d4-a716-446655440000",
        checksum=b"four",
    )
    refined_mismatches = []

    changed_decimal = schema_pb2.DatasetSchema()
    changed_decimal.CopyFrom(refined)
    changed_decimal.fields[0].type.decimal.precision = 5
    refined_mismatches.append(changed_decimal)

    removed_decimal = schema_pb2.DatasetSchema()
    removed_decimal.CopyFrom(refined)
    removed_decimal.fields[0].type.ClearField("decimal")
    removed_decimal.fields[0].type.primitive.kind = schema_pb2.PRIMITIVE_KIND_STRING
    refined_mismatches.append(removed_decimal)

    removed_uuid = schema_pb2.DatasetSchema()
    removed_uuid.CopyFrom(refined)
    removed_uuid.fields[1].type.ClearField("uuid")
    removed_uuid.fields[1].type.primitive.kind = schema_pb2.PRIMITIVE_KIND_STRING
    refined_mismatches.append(removed_uuid)

    changed_fixed_bytes = schema_pb2.DatasetSchema()
    changed_fixed_bytes.CopyFrom(refined)
    changed_fixed_bytes.fields[2].type.fixed_bytes.byte_length = 3
    refined_mismatches.append(changed_fixed_bytes)

    removed_fixed_bytes = schema_pb2.DatasetSchema()
    removed_fixed_bytes.CopyFrom(refined)
    removed_fixed_bytes.fields[2].type.ClearField("fixed_bytes")
    removed_fixed_bytes.fields[2].type.primitive.kind = schema_pb2.PRIMITIVE_KIND_BYTES
    refined_mismatches.append(removed_fixed_bytes)

    for mismatch in refined_mismatches:
        with pytest.raises(ValueError, match=r"descriptor does not match"):
            arrow_table(mismatch, [refined_value])

    fixed, fixed_record = _fixed_list_record_fixture()
    fixed_value = fixed_record(
        id="id",
        label="label",
        vector=[float(value) for value in range(8)],
        vector64=[float(value) for value in range(4)],
    )

    changed_fixed_list = schema_pb2.DatasetSchema()
    changed_fixed_list.CopyFrom(fixed)
    next(field for field in changed_fixed_list.fields if field.name == "vector").type.list.fixed_length = 4

    removed_fixed_list = schema_pb2.DatasetSchema()
    removed_fixed_list.CopyFrom(fixed)
    next(field for field in removed_fixed_list.fields if field.name == "vector").type.list.fixed_length = 0

    for mismatch in (changed_fixed_list, removed_fixed_list):
        with pytest.raises(ValueError, match=r"descriptor does not match"):
            arrow_table(mismatch, [fixed_value])


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

    isolated_dataset, isolated_record, isolated_nested = _isolated_any_fixture()
    resolvable = isolated_record()
    resolvable.payload.type_url = "type.googleapis.com/data.isolated.Inner"
    resolvable.payload.value = isolated_nested(id=21).SerializeToString()
    isolated_table, _ = arrow_table(isolated_dataset, [resolvable])
    assert json.loads(isolated_table.column("payload").to_pylist()[0]) == {
        "@type": "type.googleapis.com/data.isolated.Inner",
        "id": "21",
    }

    malformed = isolated_record()
    malformed.payload.type_url = "type.googleapis.com/data.isolated.Inner"
    malformed.payload.value = b"\xff"
    with pytest.raises(
        ValueError,
        match=r"protobuf JSON field 'isolated_any\.payload'.*"
        r"data\.isolated\.Outer\.payload.*canonical ProtoJSON domain",
    ):
        arrow_table(isolated_dataset, [malformed])

    non_finite = data_pb2.CanonicalRecord()
    non_finite.attributes.fields["bad"].number_value = float("inf")
    with pytest.raises(
        ValueError,
        match=r"protobuf JSON field 'data_v1_canonical_record\.attributes'.*"
        r"data\.v1\.CanonicalRecord\.attributes.*numbers to be finite",
    ):
        arrow_table(dataset, [non_finite])

    unresolved = data_pb2.CanonicalRecord()
    unresolved.opaque.type_url = "type.googleapis.com/example.v1.Missing"
    unresolved.opaque.value = b"\x08\x01"
    with pytest.raises(
        ValueError,
        match=r"protobuf JSON field 'data_v1_canonical_record\.opaque'.*"
        r"data\.v1\.CanonicalRecord\.opaque.*type URL to resolve",
    ):
        arrow_table(dataset, [unresolved])


def test_refined_types_map_convert_and_round_trip_through_parquet(tmp_path: Path) -> None:
    dataset, record = _refined_record_fixture()

    schema, diagnostics = arrow_schema(dataset)

    assert schema.field("amount").type == pa.decimal128(6, 2)
    assert schema.field("record_id").type == pa.uuid()
    assert schema.field("checksum").type == pa.binary(4)
    assert pa.types.is_fixed_size_binary(schema.field("checksum").type)
    assert all(field.nullable for field in schema)
    compatibility = {item.field_path: item.compatibility for item in diagnostics}
    assert compatibility == {
        "amount": schema_pb2.MAPPING_COMPATIBILITY_REPRESENTATION_CHANGED,
        "record_id": schema_pb2.MAPPING_COMPATIBILITY_REPRESENTATION_CHANGED,
        "checksum": schema_pb2.MAPPING_COMPATIBILITY_LOSSLESS,
    }

    identifier = UUID("550e8400-e29b-41d4-a716-446655440000")
    table, _ = arrow_table(
        dataset,
        [
            record(amount="1234.50", record_id=str(identifier), checksum=b"\x00\x01\x02\x03"),
            record(amount="-0.01", record_id="00000000-0000-0000-0000-000000000000", checksum=b"four"),
            record(),
        ],
    )

    assert table.column("amount").to_pylist() == [Decimal("1234.50"), Decimal("-0.01"), None]
    assert table.column("record_id").to_pylist() == [identifier, UUID(int=0), None]
    assert table.column("checksum").to_pylist() == [b"\x00\x01\x02\x03", b"four", None]

    path = tmp_path / "refined.parquet"
    pq.write_table(table, path)
    restored = pq.read_table(path)
    assert restored.schema.equals(table.schema, check_metadata=True)
    assert restored.to_pydict() == table.to_pydict()


def test_arrow_table_converts_wrappers_dynamic_json_repeated_refinements_and_stable_names() -> None:
    dataset, record_type = _portable_values_fixture()
    record = record_type(renamed_value="stable")
    record.wrapped_double.value = 1.5
    record.wrapped_float.value = 2.5
    record.wrapped_int64.value = -(1 << 63)
    record.wrapped_uint64.value = (1 << 64) - 1
    record.wrapped_int32.value = -(1 << 31)
    record.wrapped_uint32.value = (1 << 32) - 1
    record.wrapped_bool.value = True
    record.wrapped_string.value = "text"
    record.wrapped_bytes.value = b"bytes"
    record.dynamic_value.struct_value.fields["answer"].number_value = 42
    record.dynamic_value.struct_value.fields["ready"].bool_value = True
    record.dynamic_list.values.add().string_value = "first"
    record.dynamic_list.values.add().number_value = 2
    record.dynamic_list.values.add().null_value = 0
    record.amounts.extend(["12.30", "-0.01"])
    record.record_ids.extend(
        [
            "550e8400-e29b-41d4-a716-446655440000",
            "00000000-0000-0000-0000-000000000000",
        ]
    )
    record.checksums.extend([b"four", b"\x00\x01\x02\x03"])

    table, diagnostics = arrow_table(dataset, [record, record_type()])
    empty, _ = arrow_table(dataset, [])

    expected_wrappers = {
        "wrapped_double": 1.5,
        "wrapped_float": 2.5,
        "wrapped_int64": -(1 << 63),
        "wrapped_uint64": (1 << 64) - 1,
        "wrapped_int32": -(1 << 31),
        "wrapped_uint32": (1 << 32) - 1,
        "wrapped_bool": True,
        "wrapped_string": "text",
        "wrapped_bytes": b"bytes",
    }
    for name, value in expected_wrappers.items():
        assert table.column(name).to_pylist() == [value, None]
    assert json.loads(table.column("dynamic_value").to_pylist()[0]) == {"answer": 42, "ready": True}
    assert json.loads(table.column("dynamic_list").to_pylist()[0]) == ["first", 2, None]
    assert table.column("dynamic_value").to_pylist()[1] is None
    assert table.column("dynamic_list").to_pylist()[1] is None
    assert table.column("amounts").to_pylist() == [[Decimal("12.30"), Decimal("-0.01")], []]
    assert table.column("record_ids").to_pylist() == [
        [
            UUID("550e8400-e29b-41d4-a716-446655440000"),
            UUID("00000000-0000-0000-0000-000000000000"),
        ],
        [],
    ]
    assert table.column("checksums").to_pylist() == [[b"four", b"\x00\x01\x02\x03"], []]
    assert "original_value" in table.column_names
    assert "renamed_value" not in table.column_names
    assert table.column("original_value").to_pylist() == ["stable", ""]
    assert _metadata(table.schema.field("original_value"), "invariant.proto.json_name") == "wire-value"
    assert len(diagnostics) == dataset.last_field_id
    assert empty.num_rows == 0
    assert empty.schema.equals(table.schema, check_metadata=True)

    invalid_values = [
        record_type(amounts=["1.0"]),
        record_type(record_ids=["not-a-uuid"]),
        record_type(checksums=[b"bad"]),
    ]
    for invalid in invalid_values:
        with pytest.raises(ValueError, match=r"portable_values\.(amounts|record_ids|checksums)\[\]"):
            arrow_table(dataset, [invalid])


def test_arrow_table_composes_extension_leaves_through_nested_structs_maps_and_parquet(tmp_path: Path) -> None:
    dataset, record_type = _nested_extension_fixture()
    record = record_type()
    record.nested.record_id = "550e8400-e29b-41d4-a716-446655440000"
    record.nested.dynamic_value.struct_value.fields["kind"].string_value = "singular"
    record.nested.attributes.fields["active"].bool_value = True

    mapped = record.entries["mapped"]
    mapped.record_id = "00000000-0000-0000-0000-000000000000"
    mapped.dynamic_value.list_value.values.add().string_value = "map"
    mapped.attributes.fields["count"].number_value = 2
    record.entries["partial"].SetInParent()

    table, _ = arrow_table(dataset, [record, record_type()])

    nested_type = table.schema.field("nested").type
    assert nested_type.field("record_id").type == pa.uuid()
    assert isinstance(nested_type.field("dynamic_value").type, pa.JsonType)
    assert isinstance(nested_type.field("attributes").type, pa.JsonType)

    map_value_type = table.schema.field("entries").type.item_type
    assert map_value_type.field("record_id").type == pa.uuid()
    assert isinstance(map_value_type.field("dynamic_value").type, pa.JsonType)
    assert isinstance(map_value_type.field("attributes").type, pa.JsonType)

    nested = table.column("nested").to_pylist()
    assert nested[0]["record_id"] == UUID("550e8400-e29b-41d4-a716-446655440000")
    assert json.loads(nested[0]["dynamic_value"]) == {"kind": "singular"}
    assert json.loads(nested[0]["attributes"]) == {"active": True}
    assert nested[1] is None

    entries = dict(table.column("entries").to_pylist()[0])
    assert entries["mapped"]["record_id"] == UUID("00000000-0000-0000-0000-000000000000")
    assert json.loads(entries["mapped"]["dynamic_value"]) == ["map"]
    assert json.loads(entries["mapped"]["attributes"]) == {"count": 2}
    assert entries["partial"] == {
        "record_id": None,
        "dynamic_value": None,
        "attributes": None,
    }
    assert table.column("entries").to_pylist()[1] == []

    path = tmp_path / "nested-extensions.parquet"
    reader, _ = arrow_record_batch_reader(dataset, [record, record_type()], batch_size=1)
    batch_sizes = []
    with pq.ParquetWriter(path, reader.schema) as writer:
        for batch in reader:
            batch_sizes.append(batch.num_rows)
            writer.write_batch(batch)
    assert batch_sizes == [1, 1]
    restored = pq.read_table(path)
    assert restored.schema.equals(table.schema, check_metadata=True)
    assert restored.to_pydict() == table.to_pydict()


def test_refined_types_reject_noncanonical_or_out_of_domain_values() -> None:
    dataset, record = _refined_record_fixture()
    valid_uuid = "550e8400-e29b-41d4-a716-446655440000"

    for amount in [
        "+1.00",
        "01.00",
        ".50",
        "1",
        "1.0",
        "1.000",
        "1e0",
        " 1.00",
        "-0.00",
    ]:
        with pytest.raises(
            ValueError,
            match=r"decimal field 'refined_record\.amount'.*(canonical|negative-zero)",
        ):
            arrow_table(dataset, [record(amount=amount, record_id=valid_uuid, checksum=b"four")])

    with pytest.raises(ValueError, match=r"decimal field 'refined_record\.amount' exceeds precision 6"):
        arrow_table(dataset, [record(amount="10000.00", record_id=valid_uuid, checksum=b"four")])

    for identifier in [
        "550E8400-E29B-41D4-A716-446655440000",
        "550e8400e29b41d4a716446655440000",
        "{550e8400-e29b-41d4-a716-446655440000}",
        "not-a-uuid",
    ]:
        with pytest.raises(ValueError, match=r"UUID field 'refined_record\.record_id'.*canonical UUID"):
            arrow_table(dataset, [record(amount="1.00", record_id=identifier, checksum=b"four")])

    for checksum in [b"", b"abc", b"abcde"]:
        with pytest.raises(
            ValueError,
            match=r"fixed-bytes field 'refined_record\.checksum'.*expected exactly 4 bytes",
        ):
            arrow_table(dataset, [record(amount="1.00", record_id=valid_uuid, checksum=checksum)])


def test_refined_type_schemas_reject_invalid_parameters_and_carriers() -> None:
    dataset, record = _refined_record_fixture()

    invalid = schema_pb2.DatasetSchema()
    invalid.CopyFrom(dataset)
    invalid.fields[0].type.decimal.precision = 0
    with pytest.raises(ValueError, match="precision must be between 1 and 38"):
        arrow_schema(invalid)

    invalid.CopyFrom(dataset)
    invalid.fields[0].type.decimal.precision = 39
    with pytest.raises(ValueError, match="precision must be between 1 and 38"):
        arrow_schema(invalid)

    invalid.CopyFrom(dataset)
    invalid.fields[0].type.decimal.scale = 7
    with pytest.raises(ValueError, match="scale must not exceed precision 6"):
        arrow_schema(invalid)

    invalid.CopyFrom(dataset)
    invalid.fields[2].type.fixed_bytes.byte_length = 0
    with pytest.raises(ValueError, match=r"length must be between 1 and 2147483647"):
        arrow_schema(invalid)

    invalid.CopyFrom(dataset)
    invalid.fields[2].type.fixed_bytes.byte_length = 1 << 31
    with pytest.raises(ValueError, match=r"length must be between 1 and 2147483647"):
        arrow_schema(invalid)

    invalid.CopyFrom(dataset)
    invalid.fields[0].type.ClearField("decimal")
    invalid.fields[0].type.fixed_bytes.byte_length = 4
    with pytest.raises(ValueError, match="logical fixed_bytes requires a protobuf bytes carrier"):
        arrow_table(invalid, [record(amount="1.00")])


def _refined_record_fixture():
    file_proto = descriptor_pb2.FileDescriptorProto(
        name="refined_types.proto",
        package="data.refined",
        syntax="proto2",
        dependency=["invariant/data/v1/annotations.proto"],
    )
    message_proto = file_proto.message_type.add(name="RefinedRecord")
    descriptor_fields = []
    for name, number, field_type in [
        ("amount", 1, descriptor_pb2.FieldDescriptorProto.TYPE_STRING),
        ("record_id", 2, descriptor_pb2.FieldDescriptorProto.TYPE_STRING),
        ("checksum", 3, descriptor_pb2.FieldDescriptorProto.TYPE_BYTES),
    ]:
        descriptor_fields.append(
            message_proto.field.add(
                name=name,
                number=number,
                label=descriptor_pb2.FieldDescriptorProto.LABEL_OPTIONAL,
                type=field_type,
            )
        )
    descriptor_fields[0].options.Extensions[annotations_pb2.field].decimal.CopyFrom(
        annotations_pb2.DecimalOptions(precision=6, scale=2)
    )
    descriptor_fields[1].options.Extensions[annotations_pb2.field].uuid.SetInParent()
    descriptor_fields[2].options.Extensions[annotations_pb2.field].fixed_bytes.CopyFrom(
        annotations_pb2.FixedBytesOptions(byte_length=4)
    )

    pool = descriptor_pool.DescriptorPool()  # ty: ignore[possibly-missing-implicit-call]
    added: set[str] = set()
    _add_descriptor_file(pool, annotations_pb2.DESCRIPTOR, added)
    pool.Add(file_proto)
    record_descriptor = pool.FindMessageTypeByName("data.refined.RefinedRecord")
    record = message_factory.GetMessageClass(record_descriptor)
    fields = [
        schema_pb2.Field(
            proto_full_name="data.refined.RefinedRecord.amount",
            proto_number_path=[1],
            name="amount",
            stable_id=1,
            presence=schema_pb2.PRESENCE_EXPLICIT,
            nullable=True,
            type=schema_pb2.DataType(decimal=schema_pb2.DecimalType(precision=6, scale=2)),
            synthetic_role=schema_pb2.SYNTHETIC_ROLE_PROTO_FIELD,
            json_name="amount",
            storage_name_source="amount",
        ),
        schema_pb2.Field(
            proto_full_name="data.refined.RefinedRecord.record_id",
            proto_number_path=[2],
            name="record_id",
            stable_id=2,
            presence=schema_pb2.PRESENCE_EXPLICIT,
            nullable=True,
            type=schema_pb2.DataType(uuid=schema_pb2.UuidType()),
            synthetic_role=schema_pb2.SYNTHETIC_ROLE_PROTO_FIELD,
            json_name="recordId",
            storage_name_source="record_id",
        ),
        schema_pb2.Field(
            proto_full_name="data.refined.RefinedRecord.checksum",
            proto_number_path=[3],
            name="checksum",
            stable_id=3,
            presence=schema_pb2.PRESENCE_EXPLICIT,
            nullable=True,
            type=schema_pb2.DataType(fixed_bytes=schema_pb2.FixedBytesType(byte_length=4)),
            synthetic_role=schema_pb2.SYNTHETIC_ROLE_PROTO_FIELD,
            json_name="checksum",
            storage_name_source="checksum",
        ),
    ]
    return (
        schema_pb2.DatasetSchema(
            source_message="data.refined.RefinedRecord",
            name="refined_record",
            fields=fields,
            last_field_id=3,
        ),
        record,
    )


def _portable_values_fixture():
    file_proto = descriptor_pb2.FileDescriptorProto(
        name="portable_values.proto",
        package="data.portable",
        syntax="proto3",
        dependency=[
            "google/protobuf/struct.proto",
            "google/protobuf/wrappers.proto",
            "invariant/data/v1/annotations.proto",
        ],
    )
    message_proto = file_proto.message_type.add(name="PortableValues")
    wrappers = [
        ("wrapped_double", ".google.protobuf.DoubleValue", schema_pb2.PRIMITIVE_KIND_DOUBLE),
        ("wrapped_float", ".google.protobuf.FloatValue", schema_pb2.PRIMITIVE_KIND_FLOAT),
        ("wrapped_int64", ".google.protobuf.Int64Value", schema_pb2.PRIMITIVE_KIND_INT64),
        ("wrapped_uint64", ".google.protobuf.UInt64Value", schema_pb2.PRIMITIVE_KIND_UINT64),
        ("wrapped_int32", ".google.protobuf.Int32Value", schema_pb2.PRIMITIVE_KIND_INT32),
        ("wrapped_uint32", ".google.protobuf.UInt32Value", schema_pb2.PRIMITIVE_KIND_UINT32),
        ("wrapped_bool", ".google.protobuf.BoolValue", schema_pb2.PRIMITIVE_KIND_BOOL),
        ("wrapped_string", ".google.protobuf.StringValue", schema_pb2.PRIMITIVE_KIND_STRING),
        ("wrapped_bytes", ".google.protobuf.BytesValue", schema_pb2.PRIMITIVE_KIND_BYTES),
    ]
    for number, (name, type_name, _) in enumerate(wrappers, start=1):
        message_proto.field.add(
            name=name,
            number=number,
            label=descriptor_pb2.FieldDescriptorProto.LABEL_OPTIONAL,
            type=descriptor_pb2.FieldDescriptorProto.TYPE_MESSAGE,
            type_name=type_name,
        )
    message_proto.field.add(
        name="dynamic_value",
        number=10,
        label=descriptor_pb2.FieldDescriptorProto.LABEL_OPTIONAL,
        type=descriptor_pb2.FieldDescriptorProto.TYPE_MESSAGE,
        type_name=".google.protobuf.Value",
    )
    message_proto.field.add(
        name="dynamic_list",
        number=11,
        label=descriptor_pb2.FieldDescriptorProto.LABEL_OPTIONAL,
        type=descriptor_pb2.FieldDescriptorProto.TYPE_MESSAGE,
        type_name=".google.protobuf.ListValue",
    )
    amounts = message_proto.field.add(
        name="amounts",
        number=12,
        label=descriptor_pb2.FieldDescriptorProto.LABEL_REPEATED,
        type=descriptor_pb2.FieldDescriptorProto.TYPE_STRING,
    )
    amounts.options.Extensions[annotations_pb2.field].decimal.CopyFrom(
        annotations_pb2.DecimalOptions(precision=6, scale=2)
    )
    record_ids = message_proto.field.add(
        name="record_ids",
        number=13,
        label=descriptor_pb2.FieldDescriptorProto.LABEL_REPEATED,
        type=descriptor_pb2.FieldDescriptorProto.TYPE_STRING,
    )
    record_ids.options.Extensions[annotations_pb2.field].uuid.SetInParent()
    checksums = message_proto.field.add(
        name="checksums",
        number=14,
        label=descriptor_pb2.FieldDescriptorProto.LABEL_REPEATED,
        type=descriptor_pb2.FieldDescriptorProto.TYPE_BYTES,
    )
    checksums.options.Extensions[annotations_pb2.field].fixed_bytes.CopyFrom(
        annotations_pb2.FixedBytesOptions(byte_length=4)
    )
    message_proto.field.add(
        name="renamed_value",
        json_name="wire-value",
        number=15,
        label=descriptor_pb2.FieldDescriptorProto.LABEL_OPTIONAL,
        type=descriptor_pb2.FieldDescriptorProto.TYPE_STRING,
    )
    message_proto.reserved_name.append("original_value")

    pool = descriptor_pool.DescriptorPool()  # ty: ignore[possibly-missing-implicit-call]
    added: set[str] = set()
    for dependency in (struct_pb2.DESCRIPTOR, wrappers_pb2.DESCRIPTOR, annotations_pb2.DESCRIPTOR):
        _add_descriptor_file(pool, dependency, added)
    pool.Add(file_proto)
    descriptor = pool.FindMessageTypeByName("data.portable.PortableValues")
    record = message_factory.GetMessageClass(descriptor)

    fields = []
    next_stable_id = 1
    for number, (name, type_name, kind) in enumerate(wrappers, start=1):
        actual = descriptor.fields_by_number[number]
        fields.append(
            schema_pb2.Field(
                proto_full_name=actual.full_name,
                proto_number_path=[number],
                name=name,
                stable_id=next_stable_id,
                presence=schema_pb2.PRESENCE_EXPLICIT,
                nullable=True,
                type=schema_pb2.DataType(
                    protobuf_type=type_name.removeprefix("."),
                    primitive=schema_pb2.PrimitiveType(kind=kind),
                ),
                synthetic_role=schema_pb2.SYNTHETIC_ROLE_PROTO_FIELD,
                json_name=actual.json_name,
                storage_name_source=name,
            )
        )
        next_stable_id += 1

    for number, name, protobuf_type, json_kind in [
        (10, "dynamic_value", "google.protobuf.Value", schema_pb2.JSON_KIND_VALUE),
        (11, "dynamic_list", "google.protobuf.ListValue", schema_pb2.JSON_KIND_LIST_VALUE),
    ]:
        actual = descriptor.fields_by_number[number]
        fields.append(
            schema_pb2.Field(
                proto_full_name=actual.full_name,
                proto_number_path=[number],
                name=name,
                stable_id=next_stable_id,
                presence=schema_pb2.PRESENCE_EXPLICIT,
                nullable=True,
                type=schema_pb2.DataType(
                    protobuf_type=protobuf_type,
                    json=schema_pb2.JsonType(kind=json_kind),
                ),
                synthetic_role=schema_pb2.SYNTHETIC_ROLE_PROTO_FIELD,
                json_name=actual.json_name,
                storage_name_source=name,
            )
        )
        next_stable_id += 1

    repeated_types = [
        schema_pb2.DataType(decimal=schema_pb2.DecimalType(precision=6, scale=2)),
        schema_pb2.DataType(uuid=schema_pb2.UuidType()),
        schema_pb2.DataType(fixed_bytes=schema_pb2.FixedBytesType(byte_length=4)),
    ]
    for number, name, element_type in zip(
        range(12, 15), ("amounts", "record_ids", "checksums"), repeated_types, strict=True
    ):
        actual = descriptor.fields_by_number[number]
        element = schema_pb2.Field(
            proto_full_name=f"{actual.full_name}[]",
            proto_number_path=[number],
            name="element",
            stable_id=next_stable_id + 1,
            presence=schema_pb2.PRESENCE_NOT_APPLICABLE,
            nullable=False,
            type=element_type,
            synthetic_role=schema_pb2.SYNTHETIC_ROLE_LIST_ELEMENT,
        )
        fields.append(
            schema_pb2.Field(
                proto_full_name=actual.full_name,
                proto_number_path=[number],
                name=name,
                stable_id=next_stable_id,
                presence=schema_pb2.PRESENCE_REPEATED,
                nullable=False,
                type=schema_pb2.DataType(list=schema_pb2.ListType(element=element)),
                synthetic_role=schema_pb2.SYNTHETIC_ROLE_PROTO_FIELD,
                json_name=actual.json_name,
                storage_name_source=name,
            )
        )
        next_stable_id += 2

    renamed = descriptor.fields_by_number[15]
    fields.append(
        schema_pb2.Field(
            proto_full_name=renamed.full_name,
            proto_number_path=[15],
            name="original_value",
            stable_id=next_stable_id,
            presence=schema_pb2.PRESENCE_IMPLICIT,
            nullable=False,
            type=schema_pb2.DataType(primitive=schema_pb2.PrimitiveType(kind=schema_pb2.PRIMITIVE_KIND_STRING)),
            synthetic_role=schema_pb2.SYNTHETIC_ROLE_PROTO_FIELD,
            json_name=renamed.json_name,
            storage_name_source="original_value",
        )
    )
    return (
        schema_pb2.DatasetSchema(
            source_message=descriptor.full_name,
            name="portable_values",
            fields=fields,
            last_field_id=next_stable_id,
        ),
        record,
    )


def _nested_extension_fixture():
    file_proto = descriptor_pb2.FileDescriptorProto(
        name="nested_extensions.proto",
        package="data.nested_extensions",
        syntax="proto2",
        dependency=[
            "google/protobuf/struct.proto",
            "invariant/data/v1/annotations.proto",
        ],
    )
    nested_proto = file_proto.message_type.add(name="Nested")
    record_id = nested_proto.field.add(
        name="record_id",
        number=1,
        label=descriptor_pb2.FieldDescriptorProto.LABEL_OPTIONAL,
        type=descriptor_pb2.FieldDescriptorProto.TYPE_STRING,
    )
    record_id.options.Extensions[annotations_pb2.field].uuid.SetInParent()
    nested_proto.field.add(
        name="dynamic_value",
        number=2,
        label=descriptor_pb2.FieldDescriptorProto.LABEL_OPTIONAL,
        type=descriptor_pb2.FieldDescriptorProto.TYPE_MESSAGE,
        type_name=".google.protobuf.Value",
    )
    nested_proto.field.add(
        name="attributes",
        number=3,
        label=descriptor_pb2.FieldDescriptorProto.LABEL_OPTIONAL,
        type=descriptor_pb2.FieldDescriptorProto.TYPE_MESSAGE,
        type_name=".google.protobuf.Struct",
    )

    root_proto = file_proto.message_type.add(name="Root")
    root_proto.field.add(
        name="nested",
        number=1,
        label=descriptor_pb2.FieldDescriptorProto.LABEL_OPTIONAL,
        type=descriptor_pb2.FieldDescriptorProto.TYPE_MESSAGE,
        type_name=".data.nested_extensions.Nested",
    )
    map_entry = root_proto.nested_type.add(name="EntriesEntry")
    map_entry.options.map_entry = True
    map_entry.field.add(
        name="key",
        number=1,
        label=descriptor_pb2.FieldDescriptorProto.LABEL_OPTIONAL,
        type=descriptor_pb2.FieldDescriptorProto.TYPE_STRING,
    )
    map_entry.field.add(
        name="value",
        number=2,
        label=descriptor_pb2.FieldDescriptorProto.LABEL_OPTIONAL,
        type=descriptor_pb2.FieldDescriptorProto.TYPE_MESSAGE,
        type_name=".data.nested_extensions.Nested",
    )
    root_proto.field.add(
        name="entries",
        number=2,
        label=descriptor_pb2.FieldDescriptorProto.LABEL_REPEATED,
        type=descriptor_pb2.FieldDescriptorProto.TYPE_MESSAGE,
        type_name=".data.nested_extensions.Root.EntriesEntry",
    )

    pool = descriptor_pool.DescriptorPool()  # ty: ignore[possibly-missing-implicit-call]
    added: set[str] = set()
    for dependency in (struct_pb2.DESCRIPTOR, annotations_pb2.DESCRIPTOR):
        _add_descriptor_file(pool, dependency, added)
    pool.Add(file_proto)
    root_descriptor = pool.FindMessageTypeByName("data.nested_extensions.Root")
    nested_descriptor = pool.FindMessageTypeByName("data.nested_extensions.Nested")
    nested_fields = nested_descriptor.fields_by_number

    def nested_type(parent_path: list[int], stable_ids: tuple[int, int, int]) -> schema_pb2.DataType:
        return schema_pb2.DataType(
            protobuf_type=nested_descriptor.full_name,
            struct=schema_pb2.StructType(
                fields=[
                    schema_pb2.Field(
                        proto_full_name=nested_fields[1].full_name,
                        proto_number_path=[*parent_path, 1],
                        name="record_id",
                        stable_id=stable_ids[0],
                        presence=schema_pb2.PRESENCE_EXPLICIT,
                        nullable=True,
                        type=schema_pb2.DataType(uuid=schema_pb2.UuidType()),
                        synthetic_role=schema_pb2.SYNTHETIC_ROLE_PROTO_FIELD,
                        json_name=nested_fields[1].json_name,
                        storage_name_source="record_id",
                    ),
                    schema_pb2.Field(
                        proto_full_name=nested_fields[2].full_name,
                        proto_number_path=[*parent_path, 2],
                        name="dynamic_value",
                        stable_id=stable_ids[1],
                        presence=schema_pb2.PRESENCE_EXPLICIT,
                        nullable=True,
                        type=schema_pb2.DataType(
                            protobuf_type="google.protobuf.Value",
                            json=schema_pb2.JsonType(kind=schema_pb2.JSON_KIND_VALUE),
                        ),
                        synthetic_role=schema_pb2.SYNTHETIC_ROLE_PROTO_FIELD,
                        json_name=nested_fields[2].json_name,
                        storage_name_source="dynamic_value",
                    ),
                    schema_pb2.Field(
                        proto_full_name=nested_fields[3].full_name,
                        proto_number_path=[*parent_path, 3],
                        name="attributes",
                        stable_id=stable_ids[2],
                        presence=schema_pb2.PRESENCE_EXPLICIT,
                        nullable=True,
                        type=schema_pb2.DataType(
                            protobuf_type="google.protobuf.Struct",
                            json=schema_pb2.JsonType(kind=schema_pb2.JSON_KIND_STRUCT),
                        ),
                        synthetic_role=schema_pb2.SYNTHETIC_ROLE_PROTO_FIELD,
                        json_name=nested_fields[3].json_name,
                        storage_name_source="attributes",
                    ),
                ]
            ),
        )

    root_fields = root_descriptor.fields_by_number
    return (
        schema_pb2.DatasetSchema(
            source_message=root_descriptor.full_name,
            name="nested_extensions",
            fields=[
                schema_pb2.Field(
                    proto_full_name=root_fields[1].full_name,
                    proto_number_path=[1],
                    name="nested",
                    stable_id=1,
                    presence=schema_pb2.PRESENCE_EXPLICIT,
                    nullable=True,
                    type=nested_type([1], (2, 3, 4)),
                    synthetic_role=schema_pb2.SYNTHETIC_ROLE_PROTO_FIELD,
                    json_name=root_fields[1].json_name,
                    storage_name_source="nested",
                ),
                schema_pb2.Field(
                    proto_full_name=root_fields[2].full_name,
                    proto_number_path=[2],
                    name="entries",
                    stable_id=5,
                    presence=schema_pb2.PRESENCE_MAP,
                    nullable=False,
                    type=schema_pb2.DataType(
                        map=schema_pb2.MapType(
                            key=schema_pb2.Field(
                                proto_full_name=f"{root_fields[2].full_name}.key",
                                proto_number_path=[2],
                                name="key",
                                stable_id=6,
                                presence=schema_pb2.PRESENCE_NOT_APPLICABLE,
                                nullable=False,
                                type=schema_pb2.DataType(
                                    primitive=schema_pb2.PrimitiveType(kind=schema_pb2.PRIMITIVE_KIND_STRING)
                                ),
                                synthetic_role=schema_pb2.SYNTHETIC_ROLE_MAP_KEY,
                            ),
                            value=schema_pb2.Field(
                                proto_full_name=f"{root_fields[2].full_name}.value",
                                proto_number_path=[2],
                                name="value",
                                stable_id=7,
                                presence=schema_pb2.PRESENCE_NOT_APPLICABLE,
                                nullable=False,
                                type=nested_type([2], (8, 9, 10)),
                                synthetic_role=schema_pb2.SYNTHETIC_ROLE_MAP_VALUE,
                            ),
                        )
                    ),
                    synthetic_role=schema_pb2.SYNTHETIC_ROLE_PROTO_FIELD,
                    json_name=root_fields[2].json_name,
                    storage_name_source="entries",
                ),
            ],
            last_field_id=10,
        ),
        message_factory.GetMessageClass(root_descriptor),
    )


def _isolated_any_fixture():
    file_proto = descriptor_pb2.FileDescriptorProto(
        name="isolated_any.proto",
        package="data.isolated",
        syntax="proto3",
        dependency=["google/protobuf/any.proto"],
    )
    inner = file_proto.message_type.add(name="Inner")
    inner.field.add(
        name="id",
        number=1,
        label=descriptor_pb2.FieldDescriptorProto.LABEL_OPTIONAL,
        type=descriptor_pb2.FieldDescriptorProto.TYPE_INT64,
    )
    outer = file_proto.message_type.add(name="Outer")
    outer.field.add(
        name="payload",
        number=1,
        label=descriptor_pb2.FieldDescriptorProto.LABEL_OPTIONAL,
        type=descriptor_pb2.FieldDescriptorProto.TYPE_MESSAGE,
        type_name=".google.protobuf.Any",
    )

    pool = descriptor_pool.DescriptorPool()  # ty: ignore[possibly-missing-implicit-call]
    _add_descriptor_file(pool, any_pb2.DESCRIPTOR, set())
    pool.Add(file_proto)
    outer_descriptor = pool.FindMessageTypeByName("data.isolated.Outer")
    payload = outer_descriptor.fields_by_name["payload"]
    return (
        schema_pb2.DatasetSchema(
            source_message=outer_descriptor.full_name,
            name="isolated_any",
            fields=[
                schema_pb2.Field(
                    proto_full_name=payload.full_name,
                    proto_number_path=[1],
                    name="payload",
                    stable_id=1,
                    presence=schema_pb2.PRESENCE_EXPLICIT,
                    nullable=True,
                    type=schema_pb2.DataType(
                        protobuf_type="google.protobuf.Any",
                        json=schema_pb2.JsonType(kind=schema_pb2.JSON_KIND_ANY),
                    ),
                    synthetic_role=schema_pb2.SYNTHETIC_ROLE_PROTO_FIELD,
                    json_name=payload.json_name,
                    storage_name_source="payload",
                )
            ],
            last_field_id=1,
        ),
        message_factory.GetMessageClass(outer_descriptor),
        message_factory.GetMessageClass(pool.FindMessageTypeByName("data.isolated.Inner")),
    )


def _add_descriptor_file(pool, file_descriptor, added: set[str]) -> None:
    if file_descriptor.name in added:
        return
    for dependency in file_descriptor.dependencies:
        _add_descriptor_file(pool, dependency, added)
    pool.AddSerializedFile(file_descriptor.serialized_pb)
    added.add(file_descriptor.name)


def _fixed_list_record_fixture():
    descriptor_set = descriptor_pb2.FileDescriptorSet.FromString(FIXED_LIST_DESCRIPTOR.read_bytes())
    pool = descriptor_pool.DescriptorPool()  # ty: ignore[possibly-missing-implicit-call]
    for file_descriptor in descriptor_set.file:
        pool.Add(file_descriptor)

    record_descriptor = pool.FindMessageTypeByName("schema.test.v1.LanceRecord")
    record = message_factory.GetMessageClass(record_descriptor)
    bundle = parse_schema_bundle(FIXED_LIST_BUNDLE.read_bytes())
    dataset = find_dataset(bundle, record_descriptor.full_name)
    assert dataset is not None
    return dataset, record


def _metadata(owner: pa.Schema | pa.Field, key: str) -> str:
    metadata = owner.metadata
    assert metadata is not None
    return metadata[key.encode()].decode()
