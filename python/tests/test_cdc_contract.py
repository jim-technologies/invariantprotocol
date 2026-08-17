import json
from pathlib import Path
from typing import Any

import pytest
from google.protobuf import any_pb2, timestamp_pb2

from invariant.gen.invariant.cdc.v1 import change_pb2
from invariant.gen.io.cloudevents.v1 import cloudevents_pb2

EVENT_TYPE = "io.invariantprotocol.cdc.v1.change"
CHANGE_RECORD_TYPE_URL = "type.googleapis.com/invariant.cdc.v1.ChangeRecord"
FIXTURE_VERSION = "3.6.1.Final"
FIXTURE_ROOT = Path(__file__).resolve().parents[2] / "testdata" / "cdc" / "debezium" / FIXTURE_VERSION


def _timestamp(seconds: int, nanos: int = 0) -> timestamp_pb2.Timestamp:
    return timestamp_pb2.Timestamp(seconds=seconds, nanos=nanos)


def _field(name: str, value: change_pb2.Value) -> change_pb2.RecordField:
    return change_pb2.RecordField(name=name, value=value)


def _rich_change_record(operation: int = change_pb2.OPERATION_UPDATE) -> change_pb2.ChangeRecord:
    return change_pb2.ChangeRecord(
        operation=operation,
        key=change_pb2.Record(fields=[_field("id", change_pb2.Value(uint64_value=18_446_744_073_709_551_615))]),
        after=change_pb2.Record(
            fields=[
                _field("explicit_null", change_pb2.Value(null_value=change_pb2.NullValue())),
                _field("unsigned", change_pb2.Value(uint64_value=18_446_744_073_709_551_615)),
                _field(
                    "amount",
                    change_pb2.Value(
                        type_name="org.apache.kafka.connect.data.Decimal",
                        decimal_value=change_pb2.DecimalValue(
                            value="12345678901234567890.123456789",
                            scale=9,
                            precision=29,
                        ),
                    ),
                ),
                _field("binary", change_pb2.Value(bytes_value=b"\x00\xff\x10binary")),
                _field(
                    "occurred_at",
                    change_pb2.Value(
                        type_name="io.debezium.time.NanoTimestamp",
                        timestamp_value=_timestamp(1_721_234_567, 987_654_321),
                    ),
                ),
                _field(
                    "items",
                    change_pb2.Value(
                        list_value=change_pb2.ListValue(
                            values=[
                                change_pb2.Value(string_value="first"),
                                change_pb2.Value(null_value=change_pb2.NullValue()),
                                change_pb2.Value(uint64_value=9_007_199_254_740_993),
                            ]
                        )
                    ),
                ),
                _field(
                    "address",
                    change_pb2.Value(
                        record_value=change_pb2.Record(
                            fields=[
                                _field("city", change_pb2.Value(string_value="Oakland")),
                                _field("zip", change_pb2.Value(uint32_value=94_607)),
                            ]
                        )
                    ),
                ),
                # "omitted" is deliberately absent: absence is not a null Value.
            ]
        ),
        data_collection=change_pb2.DataCollection(id="inventory.public.customers"),
        schema_reference=change_pb2.SchemaReference(
            uri="urn:example:schema:customers",
            version="42",
            fingerprint=b"\x12\x34\x56\x78",
        ),
        source_position=change_pb2.SourcePosition(
            stream="source-stream-7",
            format="application/vnd.debezium.source-position+json",
            value=b'{"opaque":"position","lsn":24023128}',
        ),
        transaction=change_pb2.TransactionContext(
            id="tx-123",
            total_order=9_007_199_254_740_993,
            data_collection_order=7,
        ),
        source_time=_timestamp(1_721_234_567, 123_456_789),
        capture_time=_timestamp(1_721_234_568, 1),
        changed_fields=change_pb2.ChangedFieldMask(
            paths=[
                change_pb2.FieldPath(segments=["amount"]),
                change_pb2.FieldPath(segments=["address", "city"]),
            ]
        ),
        source_extension=change_pb2.SourceExtension(
            opaque_data=change_pb2.OpaqueData(
                media_type="application/json",
                schema="https://debezium.io/schemas/3.6/source/postgresql",
                data=b'{"connector":"postgresql","future_source_field":{"x":1}}',
            )
        ),
    )


def _cloud_event(record: change_pb2.ChangeRecord, event_id: str = "server-1:24023128:7") -> cloudevents_pb2.CloudEvent:
    proto_data = any_pb2.Any()
    proto_data.Pack(record)
    return cloudevents_pb2.CloudEvent(
        id=event_id,
        source="urn:invariant:test:source:inventory",
        spec_version="1.0",
        type=EVENT_TYPE,
        attributes={
            "time": cloudevents_pb2.CloudEvent.CloudEventAttributeValue(
                ce_timestamp=_timestamp(1_721_234_567, 123_456_789)
            ),
            "datacontenttype": cloudevents_pb2.CloudEvent.CloudEventAttributeValue(ce_string="application/protobuf"),
            "dataschema": cloudevents_pb2.CloudEvent.CloudEventAttributeValue(ce_uri=CHANGE_RECORD_TYPE_URL),
            "correlationid": cloudevents_pb2.CloudEvent.CloudEventAttributeValue(ce_string="request-42"),
            "causationid": cloudevents_pb2.CloudEvent.CloudEventAttributeValue(ce_string="command-11"),
            "traceparent": cloudevents_pb2.CloudEvent.CloudEventAttributeValue(
                ce_string="00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
            ),
        },
        proto_data=proto_data,
    )


def _assert_rich_values(record: change_pb2.ChangeRecord) -> None:
    assert record.operation == change_pb2.OPERATION_UPDATE
    assert record.HasField("key")
    assert not record.HasField("before")
    assert record.HasField("after")
    assert record.data_collection.id == "inventory.public.customers"
    assert record.source_position.value == b'{"opaque":"position","lsn":24023128}'
    assert record.transaction.id == "tx-123"
    assert record.transaction.HasField("total_order")
    assert record.transaction.total_order == 9_007_199_254_740_993
    assert record.source_time == _timestamp(1_721_234_567, 123_456_789)

    fields = {field.name: field.value for field in record.after.fields}
    assert "omitted" not in fields
    assert fields["explicit_null"].WhichOneof("kind") == "null_value"
    assert fields["unsigned"].uint64_value == 18_446_744_073_709_551_615
    assert fields["amount"].WhichOneof("kind") == "decimal_value"
    assert (
        fields["amount"].decimal_value.value,
        fields["amount"].decimal_value.scale,
        fields["amount"].decimal_value.precision,
    ) == ("12345678901234567890.123456789", 9, 29)
    assert fields["amount"].decimal_value.HasField("precision")
    assert fields["binary"].bytes_value == b"\x00\xff\x10binary"
    assert fields["occurred_at"].timestamp_value == _timestamp(1_721_234_567, 987_654_321)

    items = fields["items"].list_value.values
    assert items[0].string_value == "first"
    assert items[1].WhichOneof("kind") == "null_value"
    assert items[2].uint64_value == 9_007_199_254_740_993
    nested = {field.name: field.value for field in fields["address"].record_value.fields}
    assert nested["city"].string_value == "Oakland"
    assert nested["zip"].uint32_value == 94_607

    assert [list(path.segments) for path in record.changed_fields.paths] == [
        ["amount"],
        ["address", "city"],
    ]
    assert record.source_extension.WhichOneof("representation") == "opaque_data"
    assert b"future_source_field" in record.source_extension.opaque_data.data


def test_cloud_event_wraps_typed_change_record_without_loss() -> None:
    event = _cloud_event(_rich_change_record())
    decoded = cloudevents_pb2.CloudEvent.FromString(event.SerializeToString())

    assert (decoded.source, decoded.id) == ("urn:invariant:test:source:inventory", "server-1:24023128:7")
    assert decoded.spec_version == "1.0"
    assert decoded.type == EVENT_TYPE
    assert decoded.WhichOneof("data") == "proto_data"
    assert decoded.proto_data.type_url == CHANGE_RECORD_TYPE_URL
    assert decoded.attributes["datacontenttype"].ce_string == "application/protobuf"
    assert decoded.attributes["dataschema"].ce_uri == CHANGE_RECORD_TYPE_URL
    assert decoded.attributes["time"].ce_timestamp == _timestamp(1_721_234_567, 123_456_789)
    assert decoded.attributes["correlationid"].ce_string == "request-42"
    assert decoded.attributes["causationid"].ce_string == "command-11"
    assert decoded.attributes["traceparent"].ce_string.startswith("00-4bf92f")

    record = change_pb2.ChangeRecord()
    assert decoded.proto_data.Unpack(record)
    _assert_rich_values(record)

    retry_record = _rich_change_record()
    retry_record.capture_time.CopyFrom(_timestamp(1_721_234_569, 2))
    retry = _cloud_event(retry_record)
    assert (retry.source, retry.id) == (decoded.source, decoded.id)


@pytest.mark.parametrize(
    ("operation", "has_key", "has_before", "has_after", "has_collection", "has_message"),
    [
        (change_pb2.OPERATION_CREATE, True, False, True, True, False),
        (change_pb2.OPERATION_UPDATE, True, False, True, True, False),
        (change_pb2.OPERATION_DELETE, True, True, False, True, False),
        (change_pb2.OPERATION_SNAPSHOT_READ, True, False, True, True, False),
        (change_pb2.OPERATION_TRUNCATE, False, False, False, True, False),
        (change_pb2.OPERATION_SOURCE_MESSAGE, False, False, False, False, True),
    ],
)
def test_operation_presence_shapes(
    operation: int,
    has_key: bool,
    has_before: bool,
    has_after: bool,
    has_collection: bool,
    has_message: bool,
) -> None:
    image = change_pb2.Record(fields=[_field("id", change_pb2.Value(uint64_value=1))])
    record = change_pb2.ChangeRecord(operation=operation)
    if has_key:
        record.key.CopyFrom(image)
    if has_before:
        record.before.CopyFrom(image)
    if has_after:
        record.after.CopyFrom(image)
    if has_collection:
        record.data_collection.id = "inventory.public.customers"
    if has_message:
        record.source_message.bytes_value = b"source-native-message"

    reparsed = change_pb2.ChangeRecord.FromString(record.SerializeToString())
    assert reparsed.HasField("key") is has_key
    assert reparsed.HasField("before") is has_before
    assert reparsed.HasField("after") is has_after
    assert reparsed.HasField("data_collection") is has_collection
    assert reparsed.HasField("source_message") is has_message
    if operation == change_pb2.OPERATION_SOURCE_MESSAGE:
        assert not reparsed.HasField("key")
        assert not reparsed.HasField("before")
        assert not reparsed.HasField("after")


def test_future_change_record_fields_are_accepted_and_preserved() -> None:
    record = _rich_change_record()
    # Field 100, varint value 1, stands in for a field added by a future writer.
    future_wire = record.SerializeToString() + b"\xa0\x06\x01"

    decoded = change_pb2.ChangeRecord.FromString(future_wire)
    _assert_rich_values(decoded)
    assert decoded.SerializeToString() == future_wire


def _data_change_operation(document: dict[str, Any]) -> str:
    if isinstance(document.get("iodebeziumop"), str):
        return document["iodebeziumop"]
    value: Any = document.get("data", document.get("value"))
    if isinstance(value, dict) and isinstance(value.get("payload"), dict):
        value = value["payload"]
    assert isinstance(value, dict)
    operation = value.get("op")
    assert isinstance(operation, str)
    return operation


def test_pinned_debezium_fixtures_parse_and_classify() -> None:
    manifest = json.loads((FIXTURE_ROOT / "manifest.json").read_text())
    assert manifest["debezium_version"] == FIXTURE_VERSION
    assert manifest["cloudevents_specification_version"] == "1.0.2"
    assert manifest["cloudevents_event_specversion"] == "1.0"

    expected_operations = {
        "native-create-schemaful.json": "c",
        "native-update-schemaless.json": "u",
        "native-delete-schemaless.json": "d",
        "native-snapshot-schemaless.json": "r",
        "native-truncate-schemaless.json": "t",
        "native-logical-message-schemaless.json": "m",
        "structured-cloudevent-snapshot.json": "r",
        "structured-cloudevent-snapshot-retry.json": "r",
    }
    entries = {entry["path"]: entry for entry in manifest["fixtures"]}
    assert expected_operations.keys() <= entries.keys()

    for name, operation in expected_operations.items():
        entry = entries[name]
        assert entry["category"] == "data_change"
        document = json.loads((FIXTURE_ROOT / name).read_text())
        assert _data_change_operation(document) == operation

    schemaful = json.loads((FIXTURE_ROOT / "native-create-schemaful.json").read_text())
    assert set(schemaful["value"]) >= {"schema", "payload"}
    schemaless = json.loads((FIXTURE_ROOT / "native-update-schemaless.json").read_text())
    assert "schema" not in schemaless["value"]

    structured = json.loads((FIXTURE_ROOT / "structured-cloudevent-snapshot.json").read_text())
    retry = json.loads((FIXTURE_ROOT / "structured-cloudevent-snapshot-retry.json").read_text())
    assert structured["specversion"] == "1.0"
    assert (structured["source"], structured["id"]) == (retry["source"], retry["id"])
    assert (FIXTURE_ROOT / "structured-cloudevent-snapshot.json").read_bytes() == (
        FIXTURE_ROOT / "structured-cloudevent-snapshot-retry.json"
    ).read_bytes()

    auxiliary = {
        "auxiliary-kafka-tombstone.json": "kafka_tombstone",
        "auxiliary-heartbeat.json": "heartbeat",
        "auxiliary-schema-change.json": "schema_change",
        "auxiliary-transaction-begin.json": "transaction_boundary",
        "auxiliary-transaction-end.json": "transaction_boundary",
    }
    for name, category in auxiliary.items():
        assert entries[name]["category"] == category
        assert entries[name]["row_change"] is False
        assert entries[name]["operation"] is None
        json.loads((FIXTURE_ROOT / name).read_text())
