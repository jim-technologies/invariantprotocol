import math
from collections.abc import Iterable, Iterator
from pathlib import Path

import pytest
from google.protobuf.timestamp_pb2 import Timestamp

from invariant.gen.invariant.cdc.v2 import change_pb2
from invariant.gen.io.cloudevents.v1 import cloudevents_pb2

EVENT_TYPE = "io.invariantprotocol.cdc.v2.change"
CHANGE_RECORD_TYPE_URL = "type.googleapis.com/invariant.cdc.v2.ChangeRecord"
FIXTURE_ROOT = Path(__file__).resolve().parents[2] / "testdata" / "cdc" / "v2"
FULL_FIXTURE = FIXTURE_ROOT / "full.binpb"
DELTA_FIXTURE = FIXTURE_ROOT / "delta.binpb"

type Semantic = tuple[object, ...]
type StateKey = tuple[str, str, Semantic]
type State = dict[StateKey, change_pb2.Record]
type SourceFrontier = tuple[str, str, str, bytes]


class ReplayError(ValueError):
    """A CDC history cannot be replayed under the strict v2 profile."""


def _timestamp_semantic(timestamp: Timestamp) -> tuple[int, int]:
    seconds = timestamp.seconds
    nanos = timestamp.nanos
    if not -62_135_596_800 <= seconds <= 253_402_300_799 or not 0 <= nanos <= 999_999_999:
        raise ReplayError("timestamp is outside the protobuf Timestamp domain")
    return (seconds, nanos)


def _record_semantic(record: change_pb2.Record) -> Semantic:
    fields: list[tuple[str, Semantic]] = []
    names: set[str] = set()
    for field in record.fields:
        if field.name in names:
            raise ReplayError(f"duplicate record field {field.name!r}")
        names.add(field.name)
        if not field.HasField("value") or field.value.WhichOneof("kind") is None:
            raise ReplayError(f"record field {field.name!r} has no value")
        fields.append((field.name, _value_semantic(field.value)))
    return tuple(sorted(fields))


def _value_semantic(value: change_pb2.Value) -> Semantic:
    kind = value.WhichOneof("kind")
    if kind is None:
        raise ReplayError("value kind is required")
    if kind == "null_value":
        payload: object = None
    elif kind in {
        "bool_value",
        "int32_value",
        "int64_value",
        "uint32_value",
        "uint64_value",
        "string_value",
        "bytes_value",
    }:
        payload = getattr(value, kind)
    elif kind in {"float32_value", "float64_value"}:
        number = getattr(value, kind)
        if math.isnan(number):
            payload = "nan"
        elif number == 0.0:
            payload = "-0" if math.copysign(1.0, number) < 0 else "+0"
        elif math.isinf(number):
            payload = "-infinity" if number < 0 else "+infinity"
        else:
            payload = number.hex()
    elif kind == "decimal_value":
        decimal = value.decimal_value
        payload = (
            decimal.value,
            decimal.scale,
            decimal.precision if decimal.HasField("precision") else None,
        )
    elif kind == "timestamp_value":
        payload = _timestamp_semantic(value.timestamp_value)
    elif kind == "record_value":
        payload = _record_semantic(value.record_value)
    elif kind == "list_value":
        payload = tuple(_value_semantic(item) for item in value.list_value.values)
    elif kind == "map_value":
        entries: list[tuple[Semantic, Semantic]] = []
        keys: set[Semantic] = set()
        for entry in value.map_value.entries:
            if not entry.HasField("key") or not entry.HasField("value"):
                raise ReplayError("map entry requires key and value")
            key = _value_semantic(entry.key)
            if key in keys:
                raise ReplayError("duplicate canonical map key")
            keys.add(key)
            entries.append((key, _value_semantic(entry.value)))
        payload = tuple(sorted(entries, key=repr))
    else:  # pragma: no cover - exhaustive over the generated v2 oneof
        raise AssertionError(f"unhandled value kind {kind}")
    return (value.type_name, kind, payload)


def _state_semantic(state: State) -> tuple[tuple[str, str, Semantic, Semantic], ...]:
    return tuple(
        sorted(
            ((source, collection, key, _record_semantic(value)) for (source, collection, key), value in state.items()),
            key=repr,
        )
    )


def _clone_record(record: change_pb2.Record) -> change_pb2.Record:
    return change_pb2.Record.FromString(record.SerializeToString())


def _clone_change_record(record: change_pb2.ChangeRecord) -> change_pb2.ChangeRecord:
    return change_pb2.ChangeRecord.FromString(record.SerializeToString())


def _clone_state(state: State) -> State:
    return {key: _clone_record(value) for key, value in state.items()}


def _fields(record: change_pb2.Record) -> dict[str, change_pb2.Value]:
    _record_semantic(record)
    return {field.name: field.value for field in record.fields}


def _validate_shape(record: change_pb2.ChangeRecord) -> None:
    operation = record.operation
    representation = record.WhichOneof("representation")
    has_message = record.HasField("source_message")

    if operation != change_pb2.OPERATION_SOURCE_MESSAGE and not record.HasField("data_collection"):
        raise ReplayError("data_collection is required")
    if not record.HasField("capture_time"):
        raise ReplayError("capture_time is required")

    if operation in {change_pb2.OPERATION_CREATE, change_pb2.OPERATION_SNAPSHOT_READ}:
        if representation == "full":
            if record.full.HasField("before") or not record.full.HasField("after"):
                raise ReplayError("CREATE and SNAPSHOT_READ require only full.after")
            if record.full.HasField("changed_fields"):
                raise ReplayError("changed_fields is permitted only for UPDATE")
        elif representation == "delta":
            if record.delta.WhichOneof("change") != "result":
                raise ReplayError("CREATE and SNAPSHOT_READ require delta.result")
        else:
            raise ReplayError("CREATE and SNAPSHOT_READ require a representation")
        if has_message:
            raise ReplayError("row changes prohibit source_message")
    elif operation == change_pb2.OPERATION_UPDATE:
        if representation == "full":
            if not record.full.HasField("after"):
                raise ReplayError("UPDATE full representation requires after")
        elif representation == "delta":
            if record.delta.WhichOneof("change") != "patch":
                raise ReplayError("UPDATE delta representation requires patch")
        else:
            raise ReplayError("UPDATE requires a representation")
        if has_message:
            raise ReplayError("row changes prohibit source_message")
    elif operation == change_pb2.OPERATION_DELETE:
        if representation == "full":
            if record.full.HasField("after"):
                raise ReplayError("DELETE prohibits full.after")
            if record.full.HasField("changed_fields"):
                raise ReplayError("changed_fields is permitted only for UPDATE")
        elif representation == "delta":
            if record.delta.WhichOneof("change") != "delete":
                raise ReplayError("DELETE delta representation requires delete")
        else:
            raise ReplayError("DELETE requires a representation")
        if has_message:
            raise ReplayError("row changes prohibit source_message")
    elif operation == change_pb2.OPERATION_TRUNCATE:
        if representation is not None or record.HasField("key") or has_message:
            raise ReplayError("TRUNCATE prohibits row data")
    elif operation == change_pb2.OPERATION_SOURCE_MESSAGE:
        if representation is not None or record.HasField("key") or not has_message:
            raise ReplayError("SOURCE_MESSAGE requires only source_message")
    else:
        raise ReplayError("operation must be specified")

    _validate_canonical_payload(record)


def _validate_canonical_payload(record: change_pb2.ChangeRecord) -> None:
    if record.HasField("key"):
        _record_semantic(record.key)
    if record.HasField("source_time"):
        _timestamp_semantic(record.source_time)
    if record.HasField("capture_time"):
        _timestamp_semantic(record.capture_time)
    if record.HasField("source_message"):
        _value_semantic(record.source_message)
    representation = record.WhichOneof("representation")
    if representation == "full":
        if record.full.HasField("before"):
            _record_semantic(record.full.before)
        if record.full.HasField("after"):
            _record_semantic(record.full.after)
    elif representation == "delta":
        change = record.delta.WhichOneof("change")
        if change == "result":
            _record_semantic(record.delta.result)
        elif change == "patch":
            _validate_patch(record.delta.patch)


def _validate_event(event: cloudevents_pb2.CloudEvent) -> change_pb2.ChangeRecord:
    if not event.source or not event.id:
        raise ReplayError("CloudEvent source and id are required")
    if event.spec_version != "1.0" or event.type != EVENT_TYPE:
        raise ReplayError("unexpected CloudEvents contract")
    if event.WhichOneof("data") != "proto_data" or event.proto_data.type_url != CHANGE_RECORD_TYPE_URL:
        raise ReplayError("CloudEvent must contain Any<invariant.cdc.v2.ChangeRecord>")
    if event.attributes["datacontenttype"].ce_string != "application/protobuf":
        raise ReplayError("unexpected datacontenttype")
    if event.attributes["dataschema"].ce_uri != CHANGE_RECORD_TYPE_URL:
        raise ReplayError("unexpected dataschema")

    record = change_pb2.ChangeRecord()
    if not event.proto_data.Unpack(record):
        raise ReplayError("ChangeRecord Any could not be unpacked")
    _validate_shape(record)
    expected_time = record.source_time if record.HasField("source_time") else record.capture_time
    if event.attributes["time"].ce_timestamp != expected_time:
        raise ReplayError("CloudEvent time must be source_time or capture_time fallback")
    return record


def _read_history(path: Path) -> list[tuple[cloudevents_pb2.CloudEvent, change_pb2.ChangeRecord]]:
    batch = cloudevents_pb2.CloudEventBatch.FromString(path.read_bytes())
    if not batch.events:
        raise ReplayError(f"empty fixture {path}")
    return [(event, _validate_event(event)) for event in batch.events]


_MISSING = object()


def _find_field(record: change_pb2.Record, name: str) -> tuple[int, change_pb2.RecordField] | None:
    found: tuple[int, change_pb2.RecordField] | None = None
    for index, field in enumerate(record.fields):
        if field.name == name:
            if found is not None:
                raise ReplayError(f"duplicate record field {name!r}")
            found = (index, field)
    return found


def _lookup(record: change_pb2.Record, path: tuple[str, ...]) -> object:
    current = record
    for index, segment in enumerate(path):
        found = _find_field(current, segment)
        if found is None:
            if index == len(path) - 1:
                return _MISSING
            raise ReplayError(f"missing ancestor at {'.'.join(path[: index + 1])}")
        value = found[1].value
        if index == len(path) - 1:
            return value
        if value.WhichOneof("kind") != "record_value":
            raise ReplayError(f"non-record ancestor at {'.'.join(path[: index + 1])}")
        current = value.record_value
    raise ReplayError("empty patch path")


def _field_state_semantic(state: change_pb2.FieldState) -> Semantic:
    kind = state.WhichOneof("state")
    if kind == "absent":
        return ("absent",)
    if kind == "value":
        return ("value", _value_semantic(state.value))
    raise ReplayError("field state is required")


def _observed_state(value: object) -> Semantic:
    if value is _MISSING:
        return ("absent",)
    assert isinstance(value, change_pb2.Value)
    return ("value", _value_semantic(value))


def _validate_patch(patch: change_pb2.RecordPatch) -> list[tuple[str, ...]]:
    paths: list[tuple[str, ...]] = []
    for change in patch.changes:
        path = tuple(change.path.segments)
        if not path:
            raise ReplayError("empty patch path")
        if not change.HasField("before") or not change.HasField("after"):
            raise ReplayError("patch before and after are required")
        before = _field_state_semantic(change.before)
        after = _field_state_semantic(change.after)
        if before == after:
            raise ReplayError("patch change must describe a real transition")
        for existing in paths:
            if path == existing:
                raise ReplayError("duplicate patch path")
            common = min(len(path), len(existing))
            if path[:common] == existing[:common]:
                raise ReplayError("overlapping patch paths")
        paths.append(path)
    return paths


def _set_path(record: change_pb2.Record, path: tuple[str, ...], after: change_pb2.FieldState) -> None:
    parent = record
    for index, segment in enumerate(path[:-1]):
        found = _find_field(parent, segment)
        if found is None:
            raise ReplayError(f"missing ancestor at {'.'.join(path[: index + 1])}")
        if found[1].value.WhichOneof("kind") != "record_value":
            raise ReplayError(f"non-record ancestor at {'.'.join(path[: index + 1])}")
        parent = found[1].value.record_value

    found = _find_field(parent, path[-1])
    if after.WhichOneof("state") == "absent":
        if found is None:
            raise ReplayError(f"cannot remove absent field {'.'.join(path)}")
        del parent.fields[found[0]]
        return
    if after.WhichOneof("state") != "value":
        raise ReplayError("field state is required")
    if found is None:
        field = parent.fields.add()
        field.name = path[-1]
        field.value.CopyFrom(after.value)
    else:
        found[1].value.CopyFrom(after.value)


def _apply_patch(base: change_pb2.Record, patch: change_pb2.RecordPatch) -> change_pb2.Record:
    paths = _validate_patch(patch)

    for change, path in zip(patch.changes, paths, strict=True):
        if _observed_state(_lookup(base, path)) != _field_state_semantic(change.before):
            raise ReplayError(f"before mismatch at {'.'.join(path)}")

    result = _clone_record(base)
    for change, path in zip(patch.changes, paths, strict=True):
        _set_path(result, path, change.after)
    return result


def _state_key(event: cloudevents_pb2.CloudEvent, record: change_pb2.ChangeRecord) -> StateKey:
    if not record.HasField("key"):
        raise ReplayError("keyless record cannot be replayed as keyed state")
    return (event.source, record.data_collection.id, _record_semantic(record.key))


def _apply_event(state: State, event: cloudevents_pb2.CloudEvent, record: change_pb2.ChangeRecord) -> None:
    operation = record.operation
    if operation == change_pb2.OPERATION_SOURCE_MESSAGE:
        return
    if operation == change_pb2.OPERATION_TRUNCATE:
        prefix = (event.source, record.data_collection.id)
        for key in [candidate for candidate in state if candidate[:2] == prefix]:
            del state[key]
        return

    key = _state_key(event, record)
    if operation in {change_pb2.OPERATION_CREATE, change_pb2.OPERATION_SNAPSHOT_READ}:
        if operation == change_pb2.OPERATION_CREATE and key in state:
            raise ReplayError("CREATE base already exists")
        result = record.full.after if record.WhichOneof("representation") == "full" else record.delta.result
        state[key] = _clone_record(result)
        return
    base = state.get(key)
    if operation == change_pb2.OPERATION_UPDATE:
        if record.WhichOneof("representation") == "full":
            if (
                base is not None
                and record.full.HasField("before")
                and _record_semantic(record.full.before) != _record_semantic(base)
            ):
                raise ReplayError("full before mismatch")
            state[key] = _clone_record(record.full.after)
        else:
            if base is None:
                raise ReplayError("row base is missing")
            state[key] = _apply_patch(base, record.delta.patch)
        return
    if operation == change_pb2.OPERATION_DELETE:
        if (
            base is not None
            and record.WhichOneof("representation") == "full"
            and record.full.HasField("before")
            and _record_semantic(record.full.before) != _record_semantic(base)
        ):
            raise ReplayError("full before mismatch")
        state.pop(key, None)
        return
    raise ReplayError("unsupported operation")


def _replay(
    history: Iterable[tuple[cloudevents_pb2.CloudEvent, change_pb2.ChangeRecord]],
    *,
    stop_position: SourceFrontier | None = None,
) -> tuple[State, list[tuple[tuple[str, str], State]]]:
    state: State = {}
    seen: set[tuple[str, str]] = set()
    snapshots: list[tuple[tuple[str, str], State]] = []
    found_position = stop_position is None
    for event, record in history:
        identity = (event.source, event.id)
        if identity in seen:
            continue
        seen.add(identity)
        _apply_event(state, event, record)
        snapshots.append((identity, _clone_state(state)))
        position = (
            event.source,
            record.source_position.stream,
            record.source_position.format,
            record.source_position.value,
        )
        if stop_position is not None and position == stop_position:
            found_position = True
            break
    if not found_position:
        raise ReplayError("source position was not found")
    return state, snapshots


def _walk_value(value: change_pb2.Value) -> Iterator[change_pb2.Value]:
    yield value
    kind = value.WhichOneof("kind")
    if kind == "record_value":
        for field in value.record_value.fields:
            yield from _walk_value(field.value)
    elif kind == "list_value":
        for item in value.list_value.values:
            yield from _walk_value(item)
    elif kind == "map_value":
        for entry in value.map_value.entries:
            yield from _walk_value(entry.key)
            yield from _walk_value(entry.value)


def _history_values(
    history: Iterable[tuple[cloudevents_pb2.CloudEvent, change_pb2.ChangeRecord]],
) -> Iterator[change_pb2.Value]:
    for _, record in history:
        records: list[change_pb2.Record] = []
        if record.HasField("key"):
            records.append(record.key)
        if record.WhichOneof("representation") == "full":
            if record.full.HasField("before"):
                records.append(record.full.before)
            if record.full.HasField("after"):
                records.append(record.full.after)
        elif record.delta.WhichOneof("change") == "result":
            records.append(record.delta.result)
        elif record.delta.WhichOneof("change") == "patch":
            for change in record.delta.patch.changes:
                if change.before.WhichOneof("state") == "value":
                    yield from _walk_value(change.before.value)
                if change.after.WhichOneof("state") == "value":
                    yield from _walk_value(change.after.value)
        if record.HasField("source_message"):
            yield from _walk_value(record.source_message)
        for image in records:
            for field in image.fields:
                yield from _walk_value(field.value)


def _value(text: str) -> change_pb2.Value:
    return change_pb2.Value(string_value=text)


def _record(**fields: change_pb2.Value) -> change_pb2.Record:
    return change_pb2.Record(fields=[change_pb2.RecordField(name=name, value=value) for name, value in fields.items()])


def _state_value(value: change_pb2.Value) -> change_pb2.FieldState:
    return change_pb2.FieldState(value=value)


def _absent() -> change_pb2.FieldState:
    return change_pb2.FieldState(absent=change_pb2.Absent())


def _change(path: list[str], before: change_pb2.FieldState, after: change_pb2.FieldState) -> change_pb2.FieldChange:
    return change_pb2.FieldChange(path=change_pb2.FieldPath(segments=path), before=before, after=after)


def _minimal_record(
    operation: change_pb2.Operation, *, delta: change_pb2.DeltaChange | None = None
) -> change_pb2.ChangeRecord:
    record = change_pb2.ChangeRecord(
        operation=operation,
        key=_record(id=_value("1")),
        data_collection=change_pb2.DataCollection(id="inventory.records"),
        capture_time={"seconds": 1},
    )
    if delta is not None:
        record.delta.CopyFrom(delta)
    return record


def _diff_records(
    before: change_pb2.Record,
    after: change_pb2.Record,
    prefix: tuple[str, ...] = (),
) -> change_pb2.RecordPatch:
    before_fields = _fields(before)
    after_fields = _fields(after)
    patch = change_pb2.RecordPatch()
    for name in sorted(before_fields.keys() | after_fields.keys()):
        before_value = before_fields.get(name)
        after_value = after_fields.get(name)
        if (
            before_value is not None
            and after_value is not None
            and _value_semantic(before_value) == _value_semantic(after_value)
        ):
            continue
        if (
            before_value is not None
            and after_value is not None
            and before_value.type_name == after_value.type_name
            and before_value.WhichOneof("kind") == "record_value"
            and after_value.WhichOneof("kind") == "record_value"
        ):
            nested = _diff_records(before_value.record_value, after_value.record_value, (*prefix, name))
            patch.changes.extend(nested.changes)
            continue
        patch.changes.append(
            _change(
                [*prefix, name],
                _absent() if before_value is None else _state_value(before_value),
                _absent() if after_value is None else _state_value(after_value),
            )
        )
    return patch


def _full_to_delta(
    record: change_pb2.ChangeRecord,
    base: change_pb2.Record | None = None,
) -> change_pb2.ChangeRecord:
    converted = _clone_change_record(record)
    operation = converted.operation
    if operation in {change_pb2.OPERATION_TRUNCATE, change_pb2.OPERATION_SOURCE_MESSAGE}:
        return converted
    if converted.WhichOneof("representation") != "full":
        raise ReplayError("FullChange is required")
    full = converted.full
    delta = change_pb2.DeltaChange()
    if operation in {change_pb2.OPERATION_CREATE, change_pb2.OPERATION_SNAPSHOT_READ}:
        if not full.HasField("after"):
            raise ReplayError("full anchor requires after")
        delta.result.CopyFrom(full.after)
    elif operation == change_pb2.OPERATION_UPDATE:
        if not full.HasField("after"):
            raise ReplayError("full UPDATE requires after")
        if base is not None and full.HasField("before") and _record_semantic(base) != _record_semantic(full.before):
            raise ReplayError("full before mismatch")
        prior = base if base is not None else full.before if full.HasField("before") else None
        if prior is None:
            raise ReplayError("full-to-delta base is missing")
        delta.patch.CopyFrom(_diff_records(prior, full.after))
    elif operation == change_pb2.OPERATION_DELETE:
        if base is not None and full.HasField("before") and _record_semantic(base) != _record_semantic(full.before):
            raise ReplayError("full before mismatch")
        delta.delete.SetInParent()
    else:
        raise ReplayError("unsupported operation")
    converted.delta.CopyFrom(delta)
    return converted


def _delta_to_full(
    record: change_pb2.ChangeRecord,
    base: change_pb2.Record | None = None,
) -> change_pb2.ChangeRecord:
    converted = _clone_change_record(record)
    operation = converted.operation
    if operation in {change_pb2.OPERATION_TRUNCATE, change_pb2.OPERATION_SOURCE_MESSAGE}:
        return converted
    if converted.WhichOneof("representation") != "delta":
        raise ReplayError("DeltaChange is required")
    change = converted.delta.WhichOneof("change")
    full = change_pb2.FullChange()
    if operation in {change_pb2.OPERATION_CREATE, change_pb2.OPERATION_SNAPSHOT_READ} and change == "result":
        full.after.CopyFrom(converted.delta.result)
    elif operation == change_pb2.OPERATION_UPDATE and change == "patch":
        if base is None:
            raise ReplayError("delta-to-full base is missing")
        full.before.CopyFrom(base)
        full.after.CopyFrom(_apply_patch(base, converted.delta.patch))
        full.changed_fields.paths.extend(change.path for change in converted.delta.patch.changes)
    elif operation == change_pb2.OPERATION_DELETE and change == "delete":
        if base is not None:
            full.before.CopyFrom(base)
        else:
            full.SetInParent()
    else:
        raise ReplayError("invalid delta operation")
    converted.full.CopyFrom(full)
    return converted


def _delta_semantic(record: change_pb2.ChangeRecord) -> Semantic:
    if record.WhichOneof("representation") is None:
        return ("none",)
    if record.WhichOneof("representation") != "delta":
        raise ReplayError("DeltaChange is required")
    change = record.delta.WhichOneof("change")
    if change == "result":
        return ("result", _record_semantic(record.delta.result))
    if change == "delete":
        return ("delete",)
    if change == "patch":
        transitions = [
            (
                tuple(item.path.segments),
                _field_state_semantic(item.before),
                _field_state_semantic(item.after),
            )
            for item in record.delta.patch.changes
        ]
        return ("patch", *sorted(transitions, key=repr))
    raise ReplayError("unknown delta effect")


def _full_semantic(record: change_pb2.ChangeRecord) -> Semantic:
    if record.WhichOneof("representation") is None:
        return ("none",)
    if record.WhichOneof("representation") != "full":
        raise ReplayError("FullChange is required")
    return (
        "full",
        _record_semantic(record.full.before) if record.full.HasField("before") else None,
        _record_semantic(record.full.after) if record.full.HasField("after") else None,
        tuple(sorted(tuple(path.segments) for path in record.full.changed_fields.paths))
        if record.full.HasField("changed_fields")
        else None,
    )


def _metadata_wire(record: change_pb2.ChangeRecord) -> bytes:
    clone = _clone_change_record(record)
    representation = clone.WhichOneof("representation")
    if representation is not None:
        clone.ClearField(representation)
    return clone.SerializeToString(deterministic=True)


def test_full_and_delta_golden_histories_replay_identically() -> None:
    full = _read_history(FULL_FIXTURE)
    delta = _read_history(DELTA_FIXTURE)

    full_state, full_snapshots = _replay(full)
    delta_state, delta_snapshots = _replay(delta)

    assert [identity for identity, _ in full_snapshots] == [identity for identity, _ in delta_snapshots]
    assert len(full_snapshots) < len(full), "full fixture must contain a retry with stable source + id"
    assert len(delta_snapshots) < len(delta), "delta fixture must contain a retry with stable source + id"
    assert full[1][0].SerializeToString() == full[2][0].SerializeToString()
    assert delta[1][0].SerializeToString() == delta[2][0].SerializeToString()
    for (_, full_at_event), (_, delta_at_event) in zip(full_snapshots, delta_snapshots, strict=True):
        assert _state_semantic(full_at_event) == _state_semantic(delta_at_event)
    assert _state_semantic(full_state) == _state_semantic(delta_state)

    operations = {record.operation for _, record in full}
    assert operations == {
        change_pb2.OPERATION_CREATE,
        change_pb2.OPERATION_UPDATE,
        change_pb2.OPERATION_DELETE,
        change_pb2.OPERATION_SNAPSHOT_READ,
        change_pb2.OPERATION_TRUNCATE,
        change_pb2.OPERATION_SOURCE_MESSAGE,
    }

    anchor = _fields(full[0][1].full.after)
    assert (anchor["account_balance"].decimal_value.value, anchor["account_balance"].decimal_value.scale) == (
        "12345678901234567890.123400",
        6,
    )
    assert anchor["account_balance"].decimal_value.precision == 38
    assert anchor["avatar"].bytes_value == b"\x00\x7f\x80\xff"
    assert (anchor["created_at"].timestamp_value.seconds, anchor["created_at"].timestamp_value.nanos) == (
        1_723_912_200,
        987_654_321,
    )
    assert anchor["revision"].uint64_value == 18_446_744_073_709_551_615
    assert [value.WhichOneof("kind") for value in anchor["tags"].list_value.values] == [
        "string_value",
        "null_value",
        "string_value",
    ]
    assert anchor["attributes"].map_value.entries[1].key.int32_value == 7
    assert _fields(anchor["profile"].record_value)["display_name"].string_value == "Ada"

    kinds = {value.WhichOneof("kind") for value in _history_values(full)}
    assert {
        "null_value",
        "uint64_value",
        "bytes_value",
        "decimal_value",
        "timestamp_value",
        "record_value",
        "list_value",
        "map_value",
    } <= kinds
    decimal_text = {
        value.decimal_value.value for value in _history_values(full) if value.WhichOneof("kind") == "decimal_value"
    }
    assert decimal_text
    assert (
        max(value.uint64_value for value in _history_values(full) if value.WhichOneof("kind") == "uint64_value") > 2**53
    )

    transitions = {
        (_field_state_semantic(change.before)[0], _field_state_semantic(change.after)[0])
        for _, record in delta
        if record.delta.WhichOneof("change") == "patch"
        for change in record.delta.patch.changes
    }
    assert ("absent", "value") in transitions
    assert ("value", "absent") in transitions
    assert any(
        change.after.WhichOneof("state") == "value" and change.after.value.WhichOneof("kind") == "null_value"
        for _, record in delta
        if record.delta.WhichOneof("change") == "patch"
        for change in record.delta.patch.changes
    )

    update = next(record for _, record in delta if record.operation == change_pb2.OPERATION_UPDATE)
    position = (
        delta[1][0].source,
        update.source_position.stream,
        update.source_position.format,
        update.source_position.value,
    )
    full_at_position, _ = _replay(full, stop_position=position)
    delta_at_position, _ = _replay(delta, stop_position=position)
    assert _state_semantic(full_at_position) == _state_semantic(delta_at_position)
    assert [_fields(record)["id"].int64_value for record in full_at_position.values()] == [42]

    assert DELTA_FIXTURE.stat().st_size < FULL_FIXTURE.stat().st_size


def test_mixed_representations_reanchor_and_scope_collection_outcomes() -> None:
    full = _read_history(FULL_FIXTURE)
    delta = _read_history(DELTA_FIXTURE)
    mixed = [delta[0], delta[1], delta[2], full[3], delta[4], full[5], delta[6], full[7]]

    _, full_snapshots = _replay(full)
    mixed_state, mixed_snapshots = _replay(mixed)
    assert [identity for identity, _ in mixed_snapshots] == [identity for identity, _ in full_snapshots]
    assert [_state_semantic(state) for _, state in mixed_snapshots] == [
        _state_semantic(state) for _, state in full_snapshots
    ]
    assert _state_semantic(mixed_state) == _state_semantic(_replay(delta)[0])

    # A complete full UPDATE is an outcome anchor even when earlier history is
    # unavailable; the following delta UPDATE then has an exact base.
    reanchored, _ = _replay([full[1], delta[3]])
    expected, _ = _replay([full[0], full[1], full[3]])
    assert _state_semantic(reanchored) == _state_semantic(expected)

    validated_base, _ = _replay([full[0]])
    base_before_failure = _state_semantic(validated_base)
    stale_full = _clone_change_record(full[1][1])
    _fields(stale_full.full.before)["profile"].type_name = "urn:stale-before"
    with pytest.raises(ReplayError, match="full before mismatch"):
        _apply_event(validated_base, full[1][0], stale_full)
    assert _state_semantic(validated_base) == base_before_failure

    # DELETE is the self-contained absent outcome. CREATE retains its stronger
    # continuity check, while SNAPSHOT_READ may replace a checkpointed row.
    missing: State = {}
    _apply_event(missing, delta[4][0], delta[4][1])
    assert not missing
    snapshot_state, _ = _replay([delta[0]])
    create_at_existing_key = _clone_change_record(delta[0][1])
    create_at_existing_key.operation = change_pb2.OPERATION_CREATE
    with pytest.raises(ReplayError, match="CREATE base already exists"):
        _apply_event(snapshot_state, delta[0][0], create_at_existing_key)
    replacement = _clone_change_record(delta[0][1])
    _fields(replacement.delta.result)["profile"].type_name = "urn:profile:replacement"
    _apply_event(snapshot_state, delta[0][0], replacement)
    assert _fields(next(iter(snapshot_state.values())))["profile"].type_name == "urn:profile:replacement"

    # State and collection-wide outcomes are scoped by CloudEvent source.
    source_a = cloudevents_pb2.CloudEvent(source="urn:test:a", id="a")
    source_b = cloudevents_pb2.CloudEvent(source="urn:test:b", id="b")
    scoped: State = {}
    _apply_event(scoped, source_a, delta[0][1])
    _apply_event(scoped, source_b, delta[0][1])
    _apply_event(scoped, source_a, full[6][1])
    assert {key[0] for key in scoped} == {"urn:test:b"}

    # The DELETE+CREATE pair used for a key change has an observable absent
    # frontier, then establishes the new key. Transactional visibility remains
    # a service-owned frontier decision.
    assert not full_snapshots[3][1]
    assert [_fields(row)["id"].int64_value for row in full_snapshots[4][1].values()] == [84]

    # Equal opaque bytes in another source/stream/format are not the same
    # frontier identity.
    wrong_frontier = (
        full[1][0].source,
        full[1][1].source_position.stream,
        "application/x-wrong-position",
        full[1][1].source_position.value,
    )
    with pytest.raises(ReplayError, match="source position was not found"):
        _replay(full, stop_position=wrong_frontier)

    shared_position = delta[0][1].source_position
    collision_history = [(source_a, delta[0][1]), (source_b, delta[0][1])]
    source_a_frontier = (
        source_a.source,
        shared_position.stream,
        shared_position.format,
        shared_position.value,
    )
    source_b_frontier = (
        source_b.source,
        shared_position.stream,
        shared_position.format,
        shared_position.value,
    )
    assert len(_replay(collision_history, stop_position=source_a_frontier)[0]) == 1
    assert len(_replay(collision_history, stop_position=source_b_frontier)[0]) == 2


def test_full_delta_construction_is_state_semantic_and_base_aware() -> None:
    full = _read_history(FULL_FIXTURE)
    delta = _read_history(DELTA_FIXTURE)
    state: State = {}
    seen: set[tuple[str, str]] = set()

    for (full_event, full_record), (_, delta_record) in zip(full, delta, strict=True):
        identity = (full_event.source, full_event.id)
        if identity in seen:
            continue
        seen.add(identity)
        base = None
        if full_record.HasField("key"):
            base = state.get(_state_key(full_event, full_record))

        constructed_delta = _full_to_delta(full_record, base)
        constructed_full = _delta_to_full(delta_record, base)
        assert constructed_delta.operation == delta_record.operation
        assert constructed_full.operation == full_record.operation
        assert _delta_semantic(constructed_delta) == _delta_semantic(delta_record)
        assert _full_semantic(constructed_full) == _full_semantic(full_record)
        assert _metadata_wire(constructed_delta) == _metadata_wire(full_record)
        assert _metadata_wire(constructed_full) == _metadata_wire(delta_record)
        _apply_event(state, full_event, full_record)

    # A complete FullChange.before is sufficient for construction even without
    # an external materialization; replay itself only needs complete after.
    assert _delta_semantic(_full_to_delta(full[1][1])) == _delta_semantic(delta[1][1])
    reanchored: State = {}
    _apply_event(reanchored, full[1][0], full[1][1])
    assert _record_semantic(next(iter(reanchored.values()))) == _record_semantic(full[1][1].full.after)

    # DELETE projections carry an absent outcome without requiring old state.
    assert _delta_semantic(_full_to_delta(full[4][1])) == ("delete",)
    base_free_full_delete = _delta_to_full(delta[4][1])
    assert base_free_full_delete.WhichOneof("representation") == "full"
    assert not base_free_full_delete.full.HasField("before")
    with pytest.raises(ReplayError, match="base is missing"):
        _delta_to_full(delta[1][1])


def test_canonical_value_equality_and_atomic_collection_changes() -> None:
    exact_values = [
        change_pb2.Value(bool_value=True),
        change_pb2.Value(uint32_value=2**32 - 1),
        change_pb2.Value(int32_value=-(2**31)),
        change_pb2.Value(int64_value=-(2**63)),
        change_pb2.Value(float32_value=float("nan")),
        change_pb2.Value(float32_value=float("inf")),
        change_pb2.Value(float32_value=float("-inf")),
        change_pb2.Value(float32_value=0.0),
        change_pb2.Value(float32_value=-0.0),
        change_pb2.Value(float64_value=float("nan")),
        change_pb2.Value(float64_value=float("inf")),
        change_pb2.Value(float64_value=float("-inf")),
        change_pb2.Value(float64_value=0.0),
        change_pb2.Value(float64_value=-0.0),
        change_pb2.Value(bytes_value=b"\x00\x80\xff"),
        change_pb2.Value(uint64_value=2**64 - 1),
        change_pb2.Value(decimal_value=change_pb2.DecimalValue(value="123", scale=-2)),
        change_pb2.Value(timestamp_value=Timestamp(seconds=1_723_912_200, nanos=987_654_321)),
        change_pb2.Value(record_value=_record(name=_value("nested"))),
        change_pb2.Value(list_value=change_pb2.ListValue(values=[_value("a"), _value("b")])),
        change_pb2.Value(
            map_value=change_pb2.MapValue(entries=[change_pb2.MapEntry(key=_value("key"), value=_value("value"))])
        ),
    ]
    for value in exact_values:
        decoded = change_pb2.Value.FromString(value.SerializeToString())
        assert _value_semantic(decoded) == _value_semantic(value)

    assert _value_semantic(change_pb2.Value(float32_value=float("nan"))) == _value_semantic(
        change_pb2.Value(float32_value=float("nan"))
    )
    assert _value_semantic(change_pb2.Value(float64_value=float("nan"))) == _value_semantic(
        change_pb2.Value(float64_value=float("nan"))
    )
    assert _value_semantic(change_pb2.Value(float32_value=0.0)) != _value_semantic(change_pb2.Value(float32_value=-0.0))
    assert _value_semantic(change_pb2.Value(float64_value=0.0)) != _value_semantic(change_pb2.Value(float64_value=-0.0))

    precision_absent = change_pb2.Value(decimal_value=change_pb2.DecimalValue(value="1", scale=-2))
    precision_zero = change_pb2.Value(decimal_value=change_pb2.DecimalValue(value="1", scale=-2, precision=0))
    assert _value_semantic(precision_absent) != _value_semantic(precision_zero)
    assert _value_semantic(change_pb2.Value(string_value="1")) != _value_semantic(change_pb2.Value(int64_value=1))
    assert _value_semantic(change_pb2.Value(type_name="urn:a", string_value="1")) != _value_semantic(
        change_pb2.Value(type_name="urn:b", string_value="1")
    )

    old_map = change_pb2.Value(
        map_value=change_pb2.MapValue(
            entries=[
                change_pb2.MapEntry(key=_value("a"), value=_value("1")),
                change_pb2.MapEntry(key=_value("b"), value=_value("2")),
            ]
        )
    )
    reordered_map = change_pb2.Value(map_value=change_pb2.MapValue(entries=list(reversed(old_map.map_value.entries))))
    old_record = change_pb2.Record(
        fields=[
            change_pb2.RecordField(name="map", value=old_map),
            change_pb2.RecordField(name="name", value=_value("x")),
        ]
    )
    reordered_record = change_pb2.Record(
        fields=[
            change_pb2.RecordField(name="name", value=_value("x")),
            change_pb2.RecordField(name="map", value=reordered_map),
        ]
    )
    assert _record_semantic(old_record) == _record_semantic(reordered_record)
    assert not _diff_records(old_record, reordered_record).changes

    duplicate_map = change_pb2.Value(
        map_value=change_pb2.MapValue(
            entries=[
                change_pb2.MapEntry(key=_value("same"), value=_value("1")),
                change_pb2.MapEntry(key=_value("same"), value=_value("2")),
            ]
        )
    )
    with pytest.raises(ReplayError, match="duplicate canonical map key"):
        _value_semantic(duplicate_map)

    old_list = change_pb2.Value(list_value=change_pb2.ListValue(values=[_value("a"), _value("b")]))
    new_list = change_pb2.Value(list_value=change_pb2.ListValue(values=[_value("b"), _value("a")]))
    new_map = change_pb2.Value(
        map_value=change_pb2.MapValue(
            entries=[
                change_pb2.MapEntry(key=_value("a"), value=_value("changed")),
                change_pb2.MapEntry(key=_value("b"), value=_value("2")),
            ]
        )
    )
    base = _record(items=old_list, attributes=old_map)
    collections = change_pb2.RecordPatch(
        changes=[
            _change(["items"], _state_value(old_list), _state_value(new_list)),
            _change(["attributes"], _state_value(old_map), _state_value(new_map)),
        ]
    )
    changed = _apply_patch(base, collections)
    assert _value_semantic(_fields(changed)["items"]) == _value_semantic(new_list)
    assert _value_semantic(_fields(changed)["attributes"]) == _value_semantic(new_map)

    base_before_failure = base.SerializeToString(deterministic=True)
    stale = change_pb2.RecordPatch(
        changes=[
            _change(["items"], _state_value(old_list), _state_value(new_list)),
            _change(["attributes"], _state_value(new_map), _state_value(old_map)),
        ]
    )
    with pytest.raises(ReplayError, match="before mismatch"):
        _apply_patch(base, stale)
    assert base.SerializeToString(deterministic=True) == base_before_failure

    nested_before = _record(
        profile=change_pb2.Value(type_name="urn:profile:v1", record_value=_record(name=_value("same")))
    )
    nested_after = _record(
        profile=change_pb2.Value(type_name="urn:profile:v2", record_value=_record(name=_value("same")))
    )
    nested_patch = _diff_records(nested_before, nested_after)
    assert [list(change.path.segments) for change in nested_patch.changes] == [["profile"]]

    precision_patch = _diff_records(_record(amount=precision_absent), _record(amount=precision_zero))
    assert [list(change.path.segments) for change in precision_patch.changes] == [["amount"]]
    signed_zero_patch = _diff_records(
        _record(number=change_pb2.Value(float64_value=0.0)),
        _record(number=change_pb2.Value(float64_value=-0.0)),
    )
    assert [list(change.path.segments) for change in signed_zero_patch.changes] == [["number"]]
    assert not _diff_records(
        _record(number=change_pb2.Value(float64_value=float("nan"))),
        _record(number=change_pb2.Value(float64_value=float("nan"))),
    ).changes


def test_delta_patch_rejects_invalid_or_unreplayable_histories() -> None:
    base = _record(name=_value("before"), profile=change_pb2.Value(record_value=_record(city=_value("Oslo"))))

    missing_base = _minimal_record(
        change_pb2.OPERATION_UPDATE,
        delta=change_pb2.DeltaChange(
            patch=change_pb2.RecordPatch(
                changes=[_change(["name"], _state_value(_value("before")), _state_value(_value("after")))]
            )
        ),
    )
    event = cloudevents_pb2.CloudEvent(source="urn:test")
    with pytest.raises(ReplayError, match="base is missing"):
        _apply_event({}, event, missing_base)

    mismatch = change_pb2.RecordPatch(
        changes=[_change(["name"], _state_value(_value("wrong")), _state_value(_value("after")))]
    )
    base_before_failure = base.SerializeToString(deterministic=True)
    with pytest.raises(ReplayError, match="before mismatch"):
        _apply_patch(base, mismatch)
    assert base.SerializeToString(deterministic=True) == base_before_failure

    duplicate = change_pb2.RecordPatch(
        changes=[
            _change(["name"], _state_value(_value("before")), _state_value(_value("one"))),
            _change(["name"], _state_value(_value("before")), _state_value(_value("two"))),
        ]
    )
    with pytest.raises(ReplayError, match="duplicate patch path"):
        _apply_patch(base, duplicate)

    overlap = change_pb2.RecordPatch(
        changes=[
            _change(
                ["profile"],
                _state_value(base.fields[1].value),
                _state_value(change_pb2.Value(record_value=_record(city=_value("Bergen")))),
            ),
            _change(["profile", "city"], _state_value(_value("Oslo")), _state_value(_value("Bergen"))),
        ]
    )
    with pytest.raises(ReplayError, match="overlapping patch paths"):
        _apply_patch(base, overlap)

    with pytest.raises(ReplayError, match="empty patch path"):
        _apply_patch(base, change_pb2.RecordPatch(changes=[_change([], _absent(), _state_value(_value("new")))]))

    with pytest.raises(ReplayError, match="missing ancestor"):
        _apply_patch(
            base,
            change_pb2.RecordPatch(changes=[_change(["missing", "leaf"], _absent(), _state_value(_value("new")))]),
        )


@pytest.mark.parametrize(
    "record",
    [
        _minimal_record(change_pb2.OPERATION_CREATE, delta=change_pb2.DeltaChange(patch=change_pb2.RecordPatch())),
        _minimal_record(change_pb2.OPERATION_UPDATE, delta=change_pb2.DeltaChange(result=_record(id=_value("1")))),
        _minimal_record(change_pb2.OPERATION_DELETE, delta=change_pb2.DeltaChange(result=_record(id=_value("1")))),
        _minimal_record(change_pb2.OPERATION_TRUNCATE, delta=change_pb2.DeltaChange(delete=change_pb2.DeleteDelta())),
        _minimal_record(
            change_pb2.OPERATION_SOURCE_MESSAGE, delta=change_pb2.DeltaChange(delete=change_pb2.DeleteDelta())
        ),
    ],
)
def test_invalid_operation_representation_pairs_are_rejected(record: change_pb2.ChangeRecord) -> None:
    with pytest.raises(ReplayError):
        _validate_shape(record)


def test_keyless_row_is_valid_wire_data_but_not_strictly_replayable() -> None:
    record = _minimal_record(
        change_pb2.OPERATION_CREATE,
        delta=change_pb2.DeltaChange(result=_record(name=_value("keyless"))),
    )
    record.ClearField("key")
    _validate_shape(record)
    with pytest.raises(ReplayError, match="keyless record"):
        _apply_event({}, cloudevents_pb2.CloudEvent(source="urn:test"), record)


def test_absent_and_explicit_null_are_distinct_patch_states() -> None:
    null = change_pb2.Value(null_value=change_pb2.NullValue())
    added = _apply_patch(
        _record(id=_value("1")), change_pb2.RecordPatch(changes=[_change(["note"], _absent(), _state_value(null))])
    )
    observed = _lookup(added, ("note",))
    assert observed is not _MISSING
    assert isinstance(observed, change_pb2.Value)
    assert observed.WhichOneof("kind") == "null_value"

    removed = _apply_patch(added, change_pb2.RecordPatch(changes=[_change(["note"], _state_value(null), _absent())]))
    assert _lookup(removed, ("note",)) is _MISSING


def test_future_protobuf_fields_preserve_known_v2_semantics() -> None:
    _, record = _read_history(DELTA_FIXTURE)[0]
    # Field 100, varint value 1, stands in for a field added by a future writer.
    future_wire = record.SerializeToString() + b"\xa0\x06\x01"
    decoded = change_pb2.ChangeRecord.FromString(future_wire)

    assert decoded.operation == record.operation
    assert decoded.WhichOneof("representation") == record.WhichOneof("representation")
    _validate_shape(decoded)
    assert decoded.SerializeToString() == future_wire

    # A future oneof member has unknown semantics to this version. Python
    # retains its bytes, but the known oneof remains unset and validation must
    # fail closed instead of treating it as an empty/no-op delta or state.
    future_oneof_wire = b"\xa2\x06\x01x"  # field 100, length-delimited "x"
    future_delta = change_pb2.DeltaChange.FromString(future_oneof_wire)
    unknown_effect = _minimal_record(change_pb2.OPERATION_UPDATE)
    unknown_effect.delta.CopyFrom(future_delta)
    with pytest.raises(ReplayError, match="requires patch"):
        _validate_shape(unknown_effect)
    assert future_delta.SerializeToString() == future_oneof_wire

    future_state = change_pb2.FieldState.FromString(future_oneof_wire)
    with pytest.raises(ReplayError, match="field state is required"):
        _validate_patch(
            change_pb2.RecordPatch(changes=[_change(["field"], future_state, _state_value(_value("after")))])
        )

    future_extension = change_pb2.SourceExtension.FromString(future_oneof_wire)
    with_unknown_extension = _clone_change_record(record)
    with_unknown_extension.source_extension.CopyFrom(future_extension)
    _validate_shape(with_unknown_extension)


def test_event_validation_recurses_through_every_canonical_value_location() -> None:
    full = _read_history(FULL_FIXTURE)
    delta = _read_history(DELTA_FIXTURE)
    invalid: list[change_pb2.ChangeRecord] = []

    duplicate_key = _clone_change_record(delta[0][1])
    duplicate_key.key.fields.append(duplicate_key.key.fields[0])
    invalid.append(duplicate_key)

    invalid_full_before = _clone_change_record(full[1][1])
    invalid_full_before.full.before.fields.add(name="invalid").value.SetInParent()
    invalid.append(invalid_full_before)

    invalid_full_after = _clone_change_record(full[0][1])
    invalid_full_after.full.after.fields.add(name="invalid").value.SetInParent()
    invalid.append(invalid_full_after)

    invalid_delta_result = _clone_change_record(delta[0][1])
    invalid_delta_result.delta.result.fields.add(name="invalid").value.SetInParent()
    invalid.append(invalid_delta_result)

    invalid_patch_state = _clone_change_record(delta[1][1])
    invalid_patch_state.delta.patch.changes[0].before.ClearField("state")
    invalid.append(invalid_patch_state)

    invalid_source_message = _clone_change_record(full[7][1])
    invalid_source_message.source_message.ClearField("kind")
    invalid.append(invalid_source_message)

    invalid_nested_timestamp = _clone_change_record(full[0][1])
    _fields(invalid_nested_timestamp.full.after)["created_at"].timestamp_value.seconds = 253_402_300_800
    invalid.append(invalid_nested_timestamp)

    invalid_capture_timestamp = _clone_change_record(full[0][1])
    invalid_capture_timestamp.capture_time.nanos = -1
    invalid.append(invalid_capture_timestamp)

    duplicate_map_key = _clone_change_record(full[0][1])
    map_value = _fields(duplicate_map_key.full.after)["attributes"].map_value
    map_value.entries.append(map_value.entries[0])
    invalid.append(duplicate_map_key)

    incomplete_map_entry = _clone_change_record(full[0][1])
    _fields(incomplete_map_entry.full.after)["attributes"].map_value.entries.add(key=_value("incomplete"))
    invalid.append(incomplete_map_entry)

    for record in invalid:
        with pytest.raises(ReplayError):
            _validate_shape(record)
