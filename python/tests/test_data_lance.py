from __future__ import annotations

import json
import math
import subprocess
import sys
from pathlib import Path

import data_pb2
import lancedb
import pyarrow as pa
import pytest
from google.protobuf import descriptor_pb2, descriptor_pool, message_factory
from lancedb.index import HnswSq

from invariant import arrow_record_batch_reader, arrow_table, find_dataset, parse_schema_bundle
from invariant.gen.invariant.data.v1 import schema_pb2

ROOT = Path(__file__).resolve().parents[2]
DESCRIPTOR = ROOT / "testdata" / "schema" / "descriptor.binpb"
BUNDLE = ROOT / "testdata" / "schema" / "schema.binpb"
CANONICAL_BUNDLE = ROOT / "testdata" / "data.schema.binpb"


def _fixture(full_name: str):
    descriptor_set = descriptor_pb2.FileDescriptorSet.FromString(DESCRIPTOR.read_bytes())
    pool = descriptor_pool.DescriptorPool()  # ty: ignore[possibly-missing-implicit-call]
    for file_descriptor in descriptor_set.file:
        pool.Add(file_descriptor)

    record = message_factory.GetMessageClass(pool.FindMessageTypeByName(full_name))
    dataset = find_dataset(
        parse_schema_bundle(BUNDLE.read_bytes()),
        full_name,
    )
    assert dataset is not None
    return dataset, record


def _record(record, identifier: int, *, label: str | None = None):
    return record(
        id=str(identifier),
        label=label or f"row-{identifier}",
        vector=[float(identifier) + offset / 10 for offset in range(8)],
        vector64=[float(identifier) + offset / 10 for offset in range(4)],
    )


def _assert_process_reopen(path: Path) -> None:
    script = """
import asyncio
import sys

import lancedb
import pyarrow as pa


async def main():
    database = await lancedb.connect_async(sys.argv[1])
    table = await database.open_table("vectors")
    assert await table.count_rows() == 130
    schema = await table.schema()
    vector = schema.field("vector")
    assert pa.types.is_fixed_size_list(vector.type)
    assert vector.type.value_type == pa.float32()
    assert vector.type.list_size == 8
    assert vector.metadata[b"invariant.stable_id"] == b"3"
    assert len(list(await table.list_indices())) == 1

    for identifier, label in ((0, "updated"), (129, "inserted")):
        query = [float(identifier) + offset / 10 for offset in range(8)]
        result = await table.vector_search(query).limit(1).to_arrow()
        assert result.column("id").to_pylist() == [str(identifier)]
        assert result.column("label").to_pylist() == [label]
        expected_vector = pa.array(query, type=pa.float32()).to_pylist()
        assert result.column("vector").to_pylist() == [expected_vector]
        assert result.column("vector64").to_pylist() == [
            [float(identifier) + offset / 10 for offset in range(4)]
        ]

    table.close()
    database.close()


asyncio.run(main())
"""
    result = subprocess.run(
        [sys.executable, "-c", script, str(path)],
        check=False,
        capture_output=True,
        text=True,
    )
    assert result.returncode == 0, result.stderr


@pytest.mark.asyncio
async def test_lancedb_lifecycle_uses_only_invariant_generated_arrow(tmp_path: Path) -> None:
    dataset, record = _fixture("schema.test.v1.LanceRecord")
    initial_messages = [_record(record, value) for value in range(128)]
    initial, diagnostics = arrow_table(dataset, initial_messages)

    vector_type = initial.schema.field("vector").type
    vector64_type = initial.schema.field("vector64").type
    assert pa.types.is_fixed_size_list(vector_type)
    assert vector_type.value_type == pa.float32()
    assert vector_type.list_size == 8
    assert not vector_type.value_field.nullable
    assert pa.types.is_fixed_size_list(vector64_type)
    assert vector64_type.value_type == pa.float64()
    assert vector64_type.list_size == 4
    assert not vector64_type.value_field.nullable
    assert (
        vector_type.value_field.metadata[b"invariant.stable_id"]
        == str(dataset.fields[2].type.list.element.stable_id).encode()
    )
    compatibility = {item.field_path: item.compatibility for item in diagnostics}
    assert compatibility["vector"] == schema_pb2.MAPPING_COMPATIBILITY_LOSSLESS
    assert compatibility["vector64"] == schema_pb2.MAPPING_COMPATIBILITY_LOSSLESS

    database = await lancedb.connect_async(tmp_path)
    initial_reader, reader_diagnostics = arrow_record_batch_reader(dataset, iter(initial_messages), batch_size=31)
    assert reader_diagnostics == diagnostics
    table = await database.create_table("vectors", schema=initial_reader.schema)
    await table.add(initial_reader)

    appended, _ = arrow_record_batch_reader(dataset, [_record(record, 128)], batch_size=1)
    await table.add(appended)
    assert await table.count_rows() == 129
    table.close()
    database.close()

    database = await lancedb.connect_async(tmp_path)
    table = await database.open_table("vectors")
    persisted_schema = await table.schema()
    persisted_vector = persisted_schema.field("vector").type
    persisted_vector64 = persisted_schema.field("vector64").type
    assert pa.types.is_fixed_size_list(persisted_vector)
    assert persisted_vector.value_type == pa.float32()
    assert persisted_vector.list_size == 8
    assert persisted_vector.value_field.nullable
    assert pa.types.is_fixed_size_list(persisted_vector64)
    assert persisted_vector64.value_type == pa.float64()
    assert persisted_vector64.list_size == 4
    assert persisted_vector64.value_field.nullable
    assert persisted_schema.field("vector").nullable == initial.schema.field("vector").nullable
    assert persisted_schema.field("vector64").nullable == initial.schema.field("vector64").nullable
    assert persisted_schema.field("vector").metadata[b"invariant.stable_id"] == b"3"
    # LanceDB preserves the fixed-size shape and top-level identity, but 0.36.0
    # widens child nullability and drops the synthetic child's custom metadata.
    assert not persisted_vector.value_field.metadata
    assert not persisted_vector64.value_field.metadata

    await table.create_index("vector", config=HnswSq(distance_type="l2"))
    indexed = list(await table.list_indices())
    assert len(indexed) == 1
    assert indexed[0].columns == ["vector"]

    query = [offset / 10 for offset in range(8)]
    nearest = await table.vector_search(query).limit(2).to_arrow()
    nearest_ids = nearest.column("id").to_pylist()
    nearest_distances = nearest.column("_distance").to_pylist()
    assert nearest_ids[0] == "0"
    assert nearest_distances[0] == pytest.approx(0.0)
    assert nearest.column("vector").to_pylist()[0] == initial.column("vector")[0].as_py()
    assert nearest.column("vector64").to_pylist()[0] == initial.column("vector64")[0].as_py()
    assert len(nearest_ids) == len(set(nearest_ids)) == 2
    assert set(nearest_ids) <= {str(value) for value in range(129)}
    assert nearest_distances == sorted(nearest_distances)

    changes, _ = arrow_table(
        dataset,
        [
            _record(record, 0, label="updated"),
            _record(record, 129, label="inserted"),
        ],
    )
    await table.merge_insert("id").when_matched_update_all().when_not_matched_insert_all().execute(changes)
    assert await table.count_rows() == 130
    updated = await table.vector_search(query).limit(1).to_arrow()
    assert updated.column("id").to_pylist() == ["0"]
    assert updated.column("label").to_pylist() == ["updated"]
    inserted_query = [129.0 + offset / 10 for offset in range(8)]
    inserted = await table.vector_search(inserted_query).limit(1).to_arrow()
    assert inserted.column("id").to_pylist() == ["129"]
    assert inserted.column("label").to_pylist() == ["inserted"]

    stats = await table.optimize()
    assert stats.compaction is not None
    assert stats.compaction.fragments_removed > 0
    assert stats.compaction.fragments_added > 0
    table.close()
    database.close()

    _assert_process_reopen(tmp_path)


@pytest.mark.asyncio
async def test_lancedb_unenforced_primary_key_remains_table_policy(tmp_path: Path) -> None:
    dataset, record = _fixture("schema.test.v1.LanceRecord")
    initial, _ = arrow_record_batch_reader(dataset, [_record(record, 1)], batch_size=1)

    database = await lancedb.connect_async(tmp_path)
    table = await database.create_table("policy", data=initial, schema=initial.schema)
    await table.set_unenforced_primary_key("id")

    duplicate, _ = arrow_table(dataset, [_record(record, 1, label="duplicate")])
    await table.add(duplicate)
    assert await table.count_rows() == 2
    persisted_vector = (await table.schema()).field("vector").type
    assert pa.types.is_fixed_size_list(persisted_vector)
    assert persisted_vector.value_type == pa.float32()
    assert persisted_vector.list_size == 8
    table.close()
    database.close()


@pytest.mark.asyncio
async def test_lancedb_full_arrow_schema_uses_application_owned_format_policy(tmp_path: Path) -> None:
    dataset = find_dataset(
        parse_schema_bundle(CANONICAL_BUNDLE.read_bytes()),
        "data.v1.CanonicalRecord",
    )
    assert dataset is not None
    record = data_pb2.CanonicalRecord(
        double_value=1.25,
        float_value=2.5,
        int64_value=-3,
        uint64_value=(1 << 64) - 1,
        int32_value=-5,
        fixed64_value=(1 << 64) - 2,
        fixed32_value=(1 << 32) - 1,
        bool_value=True,
        string_value="eight",
        bytes_value=b"nine",
        uint32_value=(1 << 32) - 2,
        sfixed32_value=-11,
        sfixed64_value=-12,
        sint32_value=-13,
        sint64_value=-14,
        state=data_pb2.DATA_STATE_READY,
        optional_note="",
        nested=data_pb2.NestedRecord(id=15, label="nested"),
        labels=["two", "one"],
        children=[data_pb2.NestedRecord(id=16)],
        counters={"two": (1 << 64) - 1, "one": 1},
        choice_count=0,
    )
    record.created_at.seconds = 1_768_478_400
    record.created_at.nanos = 123
    record.elapsed.seconds = 18
    record.elapsed.nanos = 19
    record.wrapped_count.value = 0
    record.attributes.update({"active": True})
    record.opaque.Pack(data_pb2.NestedRecord(id=21))
    data, _ = arrow_table(dataset, [record])
    assert pa.types.is_map(data.schema.field("counters").type)

    database = await lancedb.connect_async(
        tmp_path,
        storage_options={"new_table_data_storage_version": "2.2"},
    )
    table = await database.create_table("canonical", data=data)
    assert await table.count_rows() == 1
    table.close()
    database.close()

    database = await lancedb.connect_async(tmp_path)
    table = await database.open_table("canonical")
    restored = await table.query().to_arrow()
    assert restored.schema.equals(data.schema, check_metadata=True)
    for field in data.schema:
        expected = data.column(field.name)
        actual = restored.column(field.name)
        if isinstance(field.type, pa.JsonType):
            assert [json.loads(value) if value is not None else None for value in actual.to_pylist()] == [
                json.loads(value) if value is not None else None for value in expected.to_pylist()
            ]
        else:
            assert actual.equals(expected), field.name
    table.close()
    database.close()


@pytest.mark.asyncio
async def test_lancedb_round_trips_refined_decimal_uuid_and_fixed_bytes(tmp_path: Path) -> None:
    dataset, record = _fixture("schema.test.v1.AnnotatedRecord")
    data, _ = arrow_table(
        dataset,
        [
            record(
                amount="12345678901234.5000",
                record_id="550e8400-e29b-41d4-a716-446655440000",
                digest=b"0123456789abcdefghijklmn",
                external_id="external-1",
            )
        ],
    )
    assert data.schema.field("amount").type == pa.decimal128(18, 4)
    assert data.schema.field("record_id").type == pa.uuid()
    assert data.schema.field("digest").type == pa.binary(24)

    database = await lancedb.connect_async(tmp_path)
    table = await database.create_table("refined", data=data)
    table.close()
    database.close()

    database = await lancedb.connect_async(tmp_path)
    table = await database.open_table("refined")
    restored = await table.query().to_arrow()
    assert restored.schema.equals(data.schema, check_metadata=True)
    assert restored.equals(data)
    table.close()
    database.close()


@pytest.mark.asyncio
async def test_lancedb_default_vector_policy_rejects_nan_without_coercion(tmp_path: Path) -> None:
    dataset, record = _fixture("schema.test.v1.LanceRecord")
    message = _record(record, 0)
    message.vector[0] = float("nan")
    data, _ = arrow_table(dataset, [message])
    assert math.isnan(data.column("vector").to_pylist()[0][0])

    database = await lancedb.connect_async(tmp_path)
    with pytest.raises(RuntimeError, match=r"Vector column 'vector' has NaNs"):
        await database.create_table("invalid", data=data)
    database.close()
