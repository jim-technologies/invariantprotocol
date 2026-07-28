from __future__ import annotations

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

from invariant import arrow_table, find_dataset, parse_schema_bundle
from invariant.gen.invariant.data.v1 import schema_pb2

ROOT = Path(__file__).resolve().parents[2]
DESCRIPTOR = ROOT / "testdata" / "schema" / "descriptor.binpb"
BUNDLE = ROOT / "testdata" / "schema" / "schema.binpb"
CANONICAL_BUNDLE = ROOT / "testdata" / "data.schema.binpb"


def _fixture():
    descriptor_set = descriptor_pb2.FileDescriptorSet.FromString(DESCRIPTOR.read_bytes())
    pool = descriptor_pool.DescriptorPool()  # ty: ignore[possibly-missing-implicit-call]
    for file_descriptor in descriptor_set.file:
        pool.Add(file_descriptor)

    record = message_factory.GetMessageClass(pool.FindMessageTypeByName("schema.test.v1.LanceRecord"))
    dataset = find_dataset(
        parse_schema_bundle(BUNDLE.read_bytes()),
        "schema.test.v1.LanceRecord",
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
    dataset, record = _fixture()
    initial, diagnostics = arrow_table(dataset, [_record(record, value) for value in range(128)])

    vector_type = initial.schema.field("vector").type
    vector64_type = initial.schema.field("vector64").type
    assert pa.types.is_fixed_size_list(vector_type)
    assert vector_type.value_type == pa.float32()
    assert vector_type.list_size == 8
    assert pa.types.is_fixed_size_list(vector64_type)
    assert vector64_type.value_type == pa.float64()
    assert vector64_type.list_size == 4
    assert (
        vector_type.value_field.metadata[b"invariant.stable_id"]
        == str(dataset.fields[2].type.list.element.stable_id).encode()
    )
    compatibility = {item.field_path: item.compatibility for item in diagnostics}
    assert compatibility["vector"] == schema_pb2.MAPPING_COMPATIBILITY_LOSSLESS
    assert compatibility["vector64"] == schema_pb2.MAPPING_COMPATIBILITY_LOSSLESS

    database = await lancedb.connect_async(tmp_path)
    table = await database.create_table("vectors", data=initial)

    appended, _ = arrow_table(dataset, [_record(record, 128)])
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
    assert pa.types.is_fixed_size_list(persisted_vector64)
    assert persisted_vector64.value_type == pa.float64()
    assert persisted_vector64.list_size == 4
    assert persisted_schema.field("vector").metadata[b"invariant.stable_id"] == b"3"
    # LanceDB preserves the logical fixed-size shape and top-level identity, but
    # 0.34.0 normalizes away custom metadata on the list's synthetic child.
    assert not persisted_vector.value_field.metadata

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
    dataset, record = _fixture()
    initial, _ = arrow_table(dataset, [_record(record, 1)])

    database = await lancedb.connect_async(tmp_path)
    table = await database.create_table("policy", data=initial)
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
    data, _ = arrow_table(dataset, [data_pb2.CanonicalRecord()])
    assert pa.types.is_map(data.schema.field("counters").type)

    database = await lancedb.connect_async(
        tmp_path,
        storage_options={"new_table_data_storage_version": "2.2"},
    )
    table = await database.create_table("canonical", data=data)
    assert await table.count_rows() == 1
    assert pa.types.is_map((await table.schema()).field("counters").type)
    table.close()
    database.close()

    database = await lancedb.connect_async(tmp_path)
    table = await database.open_table("canonical")
    assert await table.count_rows() == 1
    assert pa.types.is_map((await table.schema()).field("counters").type)
    table.close()
    database.close()


@pytest.mark.asyncio
async def test_lancedb_default_vector_policy_rejects_nan_without_coercion(tmp_path: Path) -> None:
    dataset, record = _fixture()
    message = _record(record, 0)
    message.vector[0] = float("nan")
    data, _ = arrow_table(dataset, [message])
    assert math.isnan(data.column("vector").to_pylist()[0][0])

    database = await lancedb.connect_async(tmp_path)
    with pytest.raises(RuntimeError, match=r"Vector column 'vector' has NaNs"):
        await database.create_table("invalid", data=data)
    database.close()
