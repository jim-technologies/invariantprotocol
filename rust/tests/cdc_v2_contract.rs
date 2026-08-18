use std::collections::{BTreeMap, BTreeSet, HashMap, HashSet};
use std::path::{Path, PathBuf};

use invariant::cdc::v2::change_record::Representation;
use invariant::cdc::v2::delta_change::Change as DeltaKind;
use invariant::cdc::v2::field_state::State as FieldStateKind;
use invariant::cdc::v2::value::Kind;
use invariant::cdc::v2::{
    Absent, ChangeRecord, ChangedFieldMask, DataCollection, DecimalValue, DeleteDelta, DeltaChange,
    FieldChange, FieldPath, FieldState, FullChange, ListValue, MapEntry, MapValue, NullValue,
    Operation, Record, RecordField, RecordPatch, SourceExtension, Value,
};
use invariant::cloudevents::v1::cloud_event::Data;
use invariant::cloudevents::v1::cloud_event::cloud_event_attribute_value::Attr;
use invariant::cloudevents::v1::{CloudEvent, CloudEventBatch};
use prost::Message;
use prost_types::Timestamp;

const EVENT_TYPE: &str = "io.invariantprotocol.cdc.v2.change";
const CHANGE_RECORD_TYPE_URL: &str = "type.googleapis.com/invariant.cdc.v2.ChangeRecord";

#[derive(Clone, Debug, Eq, Hash, PartialEq)]
struct StateKey {
    source: String,
    collection: String,
    key: SemanticRecord,
}

type MaterializedState = HashMap<StateKey, Record>;
type History = Vec<(CloudEvent, ChangeRecord)>;
type Snapshot = ((String, String), MaterializedState);
type SemanticRecord = Vec<(String, SemanticValue)>;
type SemanticStateKey = (String, String, SemanticRecord);
type SemanticState = BTreeMap<SemanticStateKey, SemanticRecord>;

#[derive(Clone, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
struct SemanticValue {
    type_name: String,
    kind: SemanticKind,
}

#[derive(Clone, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
enum SemanticKind {
    Null,
    Bool(bool),
    Int32(i32),
    Int64(i64),
    Uint32(u32),
    Uint64(u64),
    Float32(u32),
    Float64(u64),
    String(String),
    Bytes(Vec<u8>),
    Decimal(String, i32, Option<u32>),
    Timestamp(i64, i32),
    Record(SemanticRecord),
    List(Vec<SemanticValue>),
    Map(Vec<(SemanticValue, SemanticValue)>),
}

#[derive(Clone, Debug, Eq, PartialEq)]
enum SemanticFieldState {
    Absent,
    Value(SemanticValue),
}

#[derive(Clone, Debug, Eq, PartialEq)]
enum SemanticDelta {
    None,
    Result(SemanticRecord),
    Patch(BTreeMap<Vec<String>, (SemanticFieldState, SemanticFieldState)>),
    Delete,
}

#[derive(Clone, Debug, Eq, PartialEq)]
enum SemanticFull {
    None,
    Change {
        before: Option<SemanticRecord>,
        after: Option<SemanticRecord>,
        changed_fields: Option<Vec<Vec<String>>>,
    },
}

#[derive(Clone, Debug, Eq, PartialEq)]
struct SourceFrontier {
    source: String,
    stream: String,
    format: String,
    value: Vec<u8>,
}

#[derive(Debug, Eq, PartialEq)]
struct ReplayError(String);

type ReplayResult<T> = Result<T, ReplayError>;

fn replay_error(message: impl Into<String>) -> ReplayError {
    ReplayError(message.into())
}

fn fixture_path(name: &str) -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("../testdata/cdc/v2")
        .join(name)
}

fn attr<'a>(event: &'a CloudEvent, name: &str) -> Option<&'a Attr> {
    event
        .attributes
        .get(name)
        .and_then(|attribute| attribute.attr.as_ref())
}

fn validate_event(event: &CloudEvent) -> ReplayResult<ChangeRecord> {
    if event.source.is_empty() || event.id.is_empty() {
        return Err(replay_error("CloudEvent source and id are required"));
    }
    if event.spec_version != "1.0" || event.r#type != EVENT_TYPE {
        return Err(replay_error("unexpected CloudEvents contract"));
    }
    if !matches!(attr(event, "datacontenttype"), Some(Attr::CeString(value)) if value == "application/protobuf")
    {
        return Err(replay_error("unexpected datacontenttype"));
    }
    if !matches!(attr(event, "dataschema"), Some(Attr::CeUri(value)) if value == CHANGE_RECORD_TYPE_URL)
    {
        return Err(replay_error("unexpected dataschema"));
    }
    let Some(Data::ProtoData(any)) = event.data.as_ref() else {
        return Err(replay_error(
            "CloudEvent must contain Any<invariant.cdc.v2.ChangeRecord>",
        ));
    };
    if any.type_url != CHANGE_RECORD_TYPE_URL {
        return Err(replay_error("unexpected ChangeRecord Any type URL"));
    }
    let record = ChangeRecord::decode(any.value.as_slice())
        .map_err(|error| replay_error(format!("invalid ChangeRecord: {error}")))?;
    validate_shape(&record)?;
    let expected_time = record
        .source_time
        .as_ref()
        .or(record.capture_time.as_ref())
        .ok_or_else(|| replay_error("capture_time is required"))?;
    if !matches!(attr(event, "time"), Some(Attr::CeTimestamp(value)) if value == expected_time) {
        return Err(replay_error(
            "CloudEvent time must be source_time or capture_time fallback",
        ));
    }
    Ok(record)
}

fn read_history(path: &Path) -> ReplayResult<History> {
    let bytes = std::fs::read(path)
        .map_err(|error| replay_error(format!("read {}: {error}", path.display())))?;
    let batch = CloudEventBatch::decode(bytes.as_slice())
        .map_err(|error| replay_error(format!("decode {}: {error}", path.display())))?;
    if batch.events.is_empty() {
        return Err(replay_error(format!("empty fixture {}", path.display())));
    }
    batch
        .events
        .into_iter()
        .map(|event| {
            let record = validate_event(&event)?;
            Ok((event, record))
        })
        .collect()
}

fn validate_shape(record: &ChangeRecord) -> ReplayResult<()> {
    let operation = Operation::try_from(record.operation)
        .map_err(|_| replay_error("operation must be specified"))?;
    if operation != Operation::SourceMessage && record.data_collection.is_none() {
        return Err(replay_error("data_collection is required"));
    }
    if record.capture_time.is_none() {
        return Err(replay_error("capture_time is required"));
    }

    match operation {
        Operation::Create | Operation::SnapshotRead => match record.representation.as_ref() {
            Some(Representation::Full(full))
                if full.before.is_none()
                    && full.after.is_some()
                    && full.changed_fields.is_none() => {}
            Some(Representation::Delta(delta))
                if matches!(delta.change, Some(DeltaKind::Result(_))) => {}
            _ => {
                return Err(replay_error(
                    "CREATE and SNAPSHOT_READ require a complete result anchor",
                ));
            }
        },
        Operation::Update => match record.representation.as_ref() {
            Some(Representation::Full(full)) if full.after.is_some() => {}
            Some(Representation::Delta(delta))
                if matches!(delta.change, Some(DeltaKind::Patch(_))) => {}
            _ => return Err(replay_error("UPDATE requires full.after or delta.patch")),
        },
        Operation::Delete => match record.representation.as_ref() {
            Some(Representation::Full(full))
                if full.after.is_none() && full.changed_fields.is_none() => {}
            Some(Representation::Delta(delta))
                if matches!(delta.change, Some(DeltaKind::Delete(_))) => {}
            _ => return Err(replay_error("DELETE requires full or delta.delete")),
        },
        Operation::Truncate => {
            if record.representation.is_some()
                || record.key.is_some()
                || record.source_message.is_some()
            {
                return Err(replay_error("TRUNCATE prohibits row data"));
            }
        }
        Operation::SourceMessage => {
            if record.representation.is_some()
                || record.key.is_some()
                || record.source_message.is_none()
            {
                return Err(replay_error("SOURCE_MESSAGE requires only source_message"));
            }
        }
        Operation::Unspecified => return Err(replay_error("operation must be specified")),
    }
    if operation != Operation::SourceMessage && record.source_message.is_some() {
        return Err(replay_error("row changes prohibit source_message"));
    }
    validate_canonical_payload(record)?;
    Ok(())
}

fn validate_canonical_payload(record: &ChangeRecord) -> ReplayResult<()> {
    if let Some(key) = record.key.as_ref() {
        semantic_record(key)?;
    }
    if let Some(source_time) = record.source_time.as_ref() {
        validate_timestamp(source_time)?;
    }
    if let Some(capture_time) = record.capture_time.as_ref() {
        validate_timestamp(capture_time)?;
    }
    if let Some(message) = record.source_message.as_ref() {
        semantic_value(message)?;
    }
    match record.representation.as_ref() {
        Some(Representation::Full(full)) => {
            if let Some(before) = full.before.as_ref() {
                semantic_record(before)?;
            }
            if let Some(after) = full.after.as_ref() {
                semantic_record(after)?;
            }
        }
        Some(Representation::Delta(delta)) => match delta.change.as_ref() {
            Some(DeltaKind::Result(result)) => {
                semantic_record(result)?;
            }
            Some(DeltaKind::Patch(patch)) => {
                validate_patch(patch)?;
            }
            Some(DeltaKind::Delete(_)) | None => {}
        },
        None => {}
    }
    Ok(())
}

fn validate_timestamp(timestamp: &Timestamp) -> ReplayResult<(i64, i32)> {
    if !(-62_135_596_800..=253_402_300_799).contains(&timestamp.seconds)
        || !(0..=999_999_999).contains(&timestamp.nanos)
    {
        return Err(replay_error(
            "timestamp is outside the protobuf Timestamp domain",
        ));
    }
    Ok((timestamp.seconds, timestamp.nanos))
}

fn semantic_record(record: &Record) -> ReplayResult<SemanticRecord> {
    let mut result = Vec::with_capacity(record.fields.len());
    let mut names = HashSet::new();
    for field in &record.fields {
        if !names.insert(field.name.clone()) {
            return Err(replay_error(format!(
                "duplicate record field {:?}",
                field.name
            )));
        }
        let value = field
            .value
            .as_ref()
            .ok_or_else(|| replay_error(format!("field {:?} has no value", field.name)))?;
        result.push((field.name.clone(), semantic_value(value)?));
    }
    result.sort_by(|left, right| left.0.cmp(&right.0));
    Ok(result)
}

fn semantic_value(value: &Value) -> ReplayResult<SemanticValue> {
    let kind = match value
        .kind
        .as_ref()
        .ok_or_else(|| replay_error("value kind is required"))?
    {
        Kind::NullValue(_) => SemanticKind::Null,
        Kind::BoolValue(value) => SemanticKind::Bool(*value),
        Kind::Int32Value(value) => SemanticKind::Int32(*value),
        Kind::Int64Value(value) => SemanticKind::Int64(*value),
        Kind::Uint32Value(value) => SemanticKind::Uint32(*value),
        Kind::Uint64Value(value) => SemanticKind::Uint64(*value),
        Kind::Float32Value(value) => SemanticKind::Float32(if value.is_nan() {
            f32::NAN.to_bits()
        } else {
            value.to_bits()
        }),
        Kind::Float64Value(value) => SemanticKind::Float64(if value.is_nan() {
            f64::NAN.to_bits()
        } else {
            value.to_bits()
        }),
        Kind::StringValue(value) => SemanticKind::String(value.clone()),
        Kind::BytesValue(value) => SemanticKind::Bytes(value.clone()),
        Kind::DecimalValue(value) => {
            SemanticKind::Decimal(value.value.clone(), value.scale, value.precision)
        }
        Kind::TimestampValue(value) => {
            let (seconds, nanos) = validate_timestamp(value)?;
            SemanticKind::Timestamp(seconds, nanos)
        }
        Kind::RecordValue(record) => SemanticKind::Record(semantic_record(record)?),
        Kind::ListValue(list) => {
            let mut values = Vec::with_capacity(list.values.len());
            for item in &list.values {
                values.push(semantic_value(item)?);
            }
            SemanticKind::List(values)
        }
        Kind::MapValue(map) => {
            let mut entries = Vec::with_capacity(map.entries.len());
            for entry in &map.entries {
                let key = semantic_value(
                    entry
                        .key
                        .as_ref()
                        .ok_or_else(|| replay_error("map entry requires key and value"))?,
                )?;
                let value = semantic_value(
                    entry
                        .value
                        .as_ref()
                        .ok_or_else(|| replay_error("map entry requires key and value"))?,
                )?;
                entries.push((key, value));
            }
            entries.sort();
            if entries.windows(2).any(|pair| pair[0].0 == pair[1].0) {
                return Err(replay_error("duplicate canonical map key"));
            }
            SemanticKind::Map(entries)
        }
    };
    Ok(SemanticValue {
        type_name: value.type_name.clone(),
        kind,
    })
}

fn records_equal(left: &Record, right: &Record) -> ReplayResult<bool> {
    Ok(semantic_record(left)? == semantic_record(right)?)
}

fn semantic_state(state: &MaterializedState) -> ReplayResult<SemanticState> {
    state
        .iter()
        .map(|(key, value)| {
            Ok((
                (key.source.clone(), key.collection.clone(), key.key.clone()),
                semantic_record(value)?,
            ))
        })
        .collect()
}

fn find_field<'a>(record: &'a Record, name: &str) -> ReplayResult<Option<&'a RecordField>> {
    let mut matches = record.fields.iter().filter(|field| field.name == name);
    let found = matches.next();
    if matches.next().is_some() {
        return Err(replay_error(format!("duplicate record field {name:?}")));
    }
    Ok(found)
}

fn record_value<'a>(record: &'a Record, name: &str) -> &'a Value {
    find_field(record, name)
        .expect("valid record")
        .and_then(|field| field.value.as_ref())
        .unwrap_or_else(|| panic!("missing record field {name}"))
}

fn record_value_mut<'a>(record: &'a mut Record, name: &str) -> &'a mut Value {
    record
        .fields
        .iter_mut()
        .find(|field| field.name == name)
        .and_then(|field| field.value.as_mut())
        .unwrap_or_else(|| panic!("missing record field {name}"))
}

fn lookup<'a>(record: &'a Record, path: &[String]) -> ReplayResult<Option<&'a Value>> {
    if path.is_empty() {
        return Err(replay_error("empty patch path"));
    }
    let mut current = record;
    for (index, segment) in path.iter().enumerate() {
        let Some(field) = find_field(current, segment)? else {
            if index == path.len() - 1 {
                return Ok(None);
            }
            return Err(replay_error(format!(
                "missing ancestor at {}",
                path[..=index].join(".")
            )));
        };
        let value = field
            .value
            .as_ref()
            .ok_or_else(|| replay_error(format!("field {segment:?} has no value")))?;
        if index == path.len() - 1 {
            return Ok(Some(value));
        }
        let Some(Kind::RecordValue(nested)) = value.kind.as_ref() else {
            return Err(replay_error(format!(
                "non-record ancestor at {}",
                path[..=index].join(".")
            )));
        };
        current = nested;
    }
    Err(replay_error("empty patch path"))
}

fn field_state_matches(observed: Option<&Value>, expected: &FieldState) -> ReplayResult<bool> {
    match (observed, expected.state.as_ref()) {
        (None, Some(FieldStateKind::Absent(_))) => Ok(true),
        (Some(actual), Some(FieldStateKind::Value(expected))) => {
            Ok(semantic_value(actual)? == semantic_value(expected)?)
        }
        (_, None) => Err(replay_error("field state is required")),
        _ => Ok(false),
    }
}

fn field_states_equal(left: &FieldState, right: &FieldState) -> ReplayResult<bool> {
    match (left.state.as_ref(), right.state.as_ref()) {
        (Some(FieldStateKind::Absent(_)), Some(FieldStateKind::Absent(_))) => Ok(true),
        (Some(FieldStateKind::Value(left)), Some(FieldStateKind::Value(right))) => {
            Ok(semantic_value(left)? == semantic_value(right)?)
        }
        (None, _) | (_, None) => Err(replay_error("field state is required")),
        _ => Ok(false),
    }
}

fn set_path(record: &mut Record, path: &[String], after: &FieldState) -> ReplayResult<()> {
    let mut parent = record;
    for (index, segment) in path[..path.len() - 1].iter().enumerate() {
        let field_index = parent
            .fields
            .iter()
            .position(|field| field.name == *segment)
            .ok_or_else(|| {
                replay_error(format!("missing ancestor at {}", path[..=index].join(".")))
            })?;
        let value = parent.fields[field_index]
            .value
            .as_mut()
            .ok_or_else(|| replay_error(format!("field {segment:?} has no value")))?;
        let Some(Kind::RecordValue(nested)) = value.kind.as_mut() else {
            return Err(replay_error(format!(
                "non-record ancestor at {}",
                path[..=index].join(".")
            )));
        };
        parent = nested;
    }

    let name = path.last().expect("non-empty path");
    let index = parent.fields.iter().position(|field| field.name == *name);
    match after
        .state
        .as_ref()
        .ok_or_else(|| replay_error("field state is required"))?
    {
        FieldStateKind::Absent(_) => {
            let index = index.ok_or_else(|| {
                replay_error(format!("cannot remove absent field {}", path.join(".")))
            })?;
            parent.fields.remove(index);
        }
        FieldStateKind::Value(value) => {
            if let Some(index) = index {
                parent.fields[index].value = Some(value.clone());
            } else {
                parent.fields.push(RecordField {
                    name: name.clone(),
                    value: Some(value.clone()),
                });
            }
        }
    }
    Ok(())
}

fn validate_patch(patch: &RecordPatch) -> ReplayResult<Vec<&[String]>> {
    let mut paths: Vec<&[String]> = Vec::with_capacity(patch.changes.len());
    for change in &patch.changes {
        let path = change
            .path
            .as_ref()
            .ok_or_else(|| replay_error("empty patch path"))?
            .segments
            .as_slice();
        if path.is_empty() {
            return Err(replay_error("empty patch path"));
        }
        let before = change
            .before
            .as_ref()
            .ok_or_else(|| replay_error("patch before and after are required"))?;
        let after = change
            .after
            .as_ref()
            .ok_or_else(|| replay_error("patch before and after are required"))?;
        if field_states_equal(before, after)? {
            return Err(replay_error("patch change must describe a real transition"));
        }
        for existing in &paths {
            if path == *existing {
                return Err(replay_error("duplicate patch path"));
            }
            let common = path.len().min(existing.len());
            if path[..common] == existing[..common] {
                return Err(replay_error("overlapping patch paths"));
            }
        }
        paths.push(path);
    }
    Ok(paths)
}

fn apply_patch(base: &Record, patch: &RecordPatch) -> ReplayResult<Record> {
    let paths = validate_patch(patch)?;
    for (change, path) in patch.changes.iter().zip(&paths) {
        if !field_state_matches(
            lookup(base, path)?,
            change.before.as_ref().expect("validated before state"),
        )? {
            return Err(replay_error(format!(
                "before mismatch at {}",
                path.join(".")
            )));
        }
    }

    let mut result = base.clone();
    for (change, path) in patch.changes.iter().zip(paths) {
        set_path(
            &mut result,
            path,
            change.after.as_ref().expect("validated after state"),
        )?;
    }
    Ok(result)
}

fn state_key(event: &CloudEvent, record: &ChangeRecord) -> ReplayResult<StateKey> {
    let key = record
        .key
        .as_ref()
        .ok_or_else(|| replay_error("keyless record cannot be replayed as keyed state"))?;
    let collection = record
        .data_collection
        .as_ref()
        .ok_or_else(|| replay_error("data_collection is required"))?;
    Ok(StateKey {
        source: event.source.clone(),
        collection: collection.id.clone(),
        key: semantic_record(key)?,
    })
}

fn apply_event(
    state: &mut MaterializedState,
    event: &CloudEvent,
    record: &ChangeRecord,
) -> ReplayResult<()> {
    let operation = Operation::try_from(record.operation)
        .map_err(|_| replay_error("operation must be specified"))?;
    if operation == Operation::SourceMessage {
        return Ok(());
    }
    if operation == Operation::Truncate {
        let collection = record
            .data_collection
            .as_ref()
            .ok_or_else(|| replay_error("data_collection is required"))?;
        state.retain(|key, _| key.source != event.source || key.collection != collection.id);
        return Ok(());
    }

    let key = state_key(event, record)?;
    match operation {
        Operation::Create | Operation::SnapshotRead => {
            if operation == Operation::Create && state.contains_key(&key) {
                return Err(replay_error("CREATE base already exists"));
            }
            let result = match record
                .representation
                .as_ref()
                .expect("validated representation")
            {
                Representation::Full(full) => full.after.as_ref().expect("validated after"),
                Representation::Delta(delta) => match delta.change.as_ref() {
                    Some(DeltaKind::Result(result)) => result,
                    _ => unreachable!("validated delta result"),
                },
            };
            state.insert(key, result.clone());
        }
        Operation::Update => {
            let result = match record
                .representation
                .as_ref()
                .expect("validated representation")
            {
                Representation::Full(full) => {
                    if let Some(base) = state.get(&key)
                        && let Some(before) = full.before.as_ref()
                        && !records_equal(before, base)?
                    {
                        return Err(replay_error("full before mismatch"));
                    }
                    full.after.as_ref().expect("validated after").clone()
                }
                Representation::Delta(delta) => match delta.change.as_ref() {
                    Some(DeltaKind::Patch(patch)) => {
                        let base = state
                            .get(&key)
                            .ok_or_else(|| replay_error("row base is missing"))?;
                        apply_patch(base, patch)?
                    }
                    _ => unreachable!("validated delta patch"),
                },
            };
            state.insert(key, result);
        }
        Operation::Delete => {
            if let Some(base) = state.get(&key)
                && let Some(Representation::Full(full)) = record.representation.as_ref()
                && let Some(before) = full.before.as_ref()
                && !records_equal(before, base)?
            {
                return Err(replay_error("full before mismatch"));
            }
            state.remove(&key);
        }
        Operation::Unspecified | Operation::Truncate | Operation::SourceMessage => {
            return Err(replay_error("unsupported operation"));
        }
    }
    Ok(())
}

fn replay(
    history: &History,
    stop_position: Option<&SourceFrontier>,
) -> ReplayResult<(MaterializedState, Vec<Snapshot>)> {
    // The fixture records a declared source-stream sequence. CloudEventBatch is
    // only its deterministic container and does not itself promise ordering.
    let mut state = MaterializedState::new();
    let mut seen = HashSet::new();
    let mut snapshots = Vec::new();
    let mut found_position = stop_position.is_none();
    for (event, record) in history {
        let identity = (event.source.clone(), event.id.clone());
        if !seen.insert(identity.clone()) {
            continue;
        }
        apply_event(&mut state, event, record)?;
        snapshots.push((identity, state.clone()));
        if stop_position.is_some_and(|position| {
            record.source_position.as_ref().is_some_and(|source| {
                position.source == event.source
                    && position.stream == source.stream
                    && position.format == source.format
                    && position.value == source.value
            })
        }) {
            found_position = true;
            break;
        }
    }
    if !found_position {
        return Err(replay_error("source position was not found"));
    }
    Ok((state, snapshots))
}

fn string_value(text: &str) -> Value {
    value(Kind::StringValue(text.into()))
}

fn value(kind: Kind) -> Value {
    Value {
        type_name: String::new(),
        kind: Some(kind),
    }
}

fn field(name: &str, value: Value) -> RecordField {
    RecordField {
        name: name.into(),
        value: Some(value),
    }
}

fn record(fields: Vec<RecordField>) -> Record {
    Record { fields }
}

fn present(value: Value) -> FieldState {
    FieldState {
        state: Some(FieldStateKind::Value(value)),
    }
}

fn absent() -> FieldState {
    FieldState {
        state: Some(FieldStateKind::Absent(Absent {})),
    }
}

fn change(path: &[&str], before: FieldState, after: FieldState) -> FieldChange {
    FieldChange {
        path: Some(FieldPath {
            segments: path.iter().map(|segment| (*segment).into()).collect(),
        }),
        before: Some(before),
        after: Some(after),
    }
}

fn delta_record(operation: Operation, change: DeltaKind) -> ChangeRecord {
    ChangeRecord {
        operation: operation as i32,
        key: Some(record(vec![field("id", string_value("1"))])),
        data_collection: Some(DataCollection {
            id: "inventory.records".into(),
        }),
        schema_reference: None,
        source_position: None,
        transaction: None,
        source_time: None,
        capture_time: Some(Timestamp {
            seconds: 1,
            nanos: 0,
        }),
        source_extension: None,
        source_message: None,
        representation: Some(Representation::Delta(DeltaChange {
            change: Some(change),
        })),
    }
}

fn record_fields(record: &Record) -> ReplayResult<BTreeMap<String, &Value>> {
    semantic_record(record)?;
    Ok(record
        .fields
        .iter()
        .map(|field| {
            (
                field.name.clone(),
                field.value.as_ref().expect("validated record field value"),
            )
        })
        .collect())
}

fn state_for_value(value: Option<&Value>) -> FieldState {
    value.map_or_else(absent, |value| present(value.clone()))
}

fn diff_records(before: &Record, after: &Record) -> ReplayResult<RecordPatch> {
    fn collect(
        before: &Record,
        after: &Record,
        prefix: &mut Vec<String>,
        changes: &mut Vec<FieldChange>,
    ) -> ReplayResult<()> {
        let before_fields = record_fields(before)?;
        let after_fields = record_fields(after)?;
        let names = before_fields
            .keys()
            .chain(after_fields.keys())
            .cloned()
            .collect::<BTreeSet<_>>();
        for name in names {
            let before_value = before_fields.get(&name).copied();
            let after_value = after_fields.get(&name).copied();
            if let (Some(before_value), Some(after_value)) = (before_value, after_value)
                && semantic_value(before_value)? == semantic_value(after_value)?
            {
                continue;
            }
            if let (Some(before_value), Some(after_value)) = (before_value, after_value)
                && before_value.type_name == after_value.type_name
                && let (Some(Kind::RecordValue(before)), Some(Kind::RecordValue(after))) =
                    (before_value.kind.as_ref(), after_value.kind.as_ref())
            {
                prefix.push(name);
                collect(before, after, prefix, changes)?;
                prefix.pop();
                continue;
            }
            let mut path = prefix.clone();
            path.push(name);
            changes.push(FieldChange {
                path: Some(FieldPath { segments: path }),
                before: Some(state_for_value(before_value)),
                after: Some(state_for_value(after_value)),
            });
        }
        Ok(())
    }

    let mut changes = Vec::new();
    collect(before, after, &mut Vec::new(), &mut changes)?;
    Ok(RecordPatch { changes })
}

fn full_to_delta(record: &ChangeRecord, base: Option<&Record>) -> ReplayResult<ChangeRecord> {
    let mut converted = record.clone();
    let operation = Operation::try_from(record.operation)
        .map_err(|_| replay_error("operation must be specified"))?;
    if matches!(operation, Operation::Truncate | Operation::SourceMessage) {
        return Ok(converted);
    }
    let Some(Representation::Full(full)) = record.representation.as_ref() else {
        return Err(replay_error("FullChange is required"));
    };
    let change = match operation {
        Operation::Create | Operation::SnapshotRead => DeltaKind::Result(
            full.after
                .as_ref()
                .ok_or_else(|| replay_error("full anchor requires after"))?
                .clone(),
        ),
        Operation::Update => {
            let after = full
                .after
                .as_ref()
                .ok_or_else(|| replay_error("full UPDATE requires after"))?;
            if let (Some(base), Some(before)) = (base, full.before.as_ref())
                && !records_equal(base, before)?
            {
                return Err(replay_error("full before mismatch"));
            }
            let prior = base
                .or(full.before.as_ref())
                .ok_or_else(|| replay_error("full-to-delta base is missing"))?;
            DeltaKind::Patch(diff_records(prior, after)?)
        }
        Operation::Delete => {
            if let (Some(base), Some(before)) = (base, full.before.as_ref())
                && !records_equal(base, before)?
            {
                return Err(replay_error("full before mismatch"));
            }
            DeltaKind::Delete(DeleteDelta {})
        }
        Operation::Unspecified | Operation::Truncate | Operation::SourceMessage => {
            return Err(replay_error("unsupported operation"));
        }
    };
    converted.representation = Some(Representation::Delta(DeltaChange {
        change: Some(change),
    }));
    Ok(converted)
}

fn delta_to_full(record: &ChangeRecord, base: Option<&Record>) -> ReplayResult<ChangeRecord> {
    let mut converted = record.clone();
    let operation = Operation::try_from(record.operation)
        .map_err(|_| replay_error("operation must be specified"))?;
    if matches!(operation, Operation::Truncate | Operation::SourceMessage) {
        return Ok(converted);
    }
    let Some(Representation::Delta(delta)) = record.representation.as_ref() else {
        return Err(replay_error("DeltaChange is required"));
    };
    let full = match (operation, delta.change.as_ref()) {
        (Operation::Create | Operation::SnapshotRead, Some(DeltaKind::Result(result))) => {
            FullChange {
                before: None,
                after: Some(result.clone()),
                changed_fields: None,
            }
        }
        (Operation::Update, Some(DeltaKind::Patch(patch))) => {
            let base = base.ok_or_else(|| replay_error("delta-to-full base is missing"))?;
            FullChange {
                before: Some(base.clone()),
                after: Some(apply_patch(base, patch)?),
                changed_fields: Some(ChangedFieldMask {
                    paths: patch
                        .changes
                        .iter()
                        .map(|change| change.path.clone().expect("validated patch path"))
                        .collect(),
                }),
            }
        }
        (Operation::Delete, Some(DeltaKind::Delete(_))) => FullChange {
            before: base.cloned(),
            after: None,
            changed_fields: None,
        },
        _ => return Err(replay_error("invalid delta operation")),
    };
    converted.representation = Some(Representation::Full(full));
    Ok(converted)
}

fn semantic_field_state(state: &FieldState) -> ReplayResult<SemanticFieldState> {
    match state.state.as_ref() {
        Some(FieldStateKind::Absent(_)) => Ok(SemanticFieldState::Absent),
        Some(FieldStateKind::Value(value)) => Ok(SemanticFieldState::Value(semantic_value(value)?)),
        None => Err(replay_error("field state is required")),
    }
}

fn semantic_delta(record: &ChangeRecord) -> ReplayResult<SemanticDelta> {
    match record.representation.as_ref() {
        None => Ok(SemanticDelta::None),
        Some(Representation::Full(_)) => Err(replay_error("DeltaChange is required")),
        Some(Representation::Delta(delta)) => match delta.change.as_ref() {
            Some(DeltaKind::Result(result)) => Ok(SemanticDelta::Result(semantic_record(result)?)),
            Some(DeltaKind::Delete(_)) => Ok(SemanticDelta::Delete),
            Some(DeltaKind::Patch(patch)) => {
                validate_patch(patch)?;
                let transitions = patch
                    .changes
                    .iter()
                    .map(|change| {
                        Ok((
                            change
                                .path
                                .as_ref()
                                .expect("validated patch path")
                                .segments
                                .clone(),
                            (
                                semantic_field_state(
                                    change.before.as_ref().expect("validated before state"),
                                )?,
                                semantic_field_state(
                                    change.after.as_ref().expect("validated after state"),
                                )?,
                            ),
                        ))
                    })
                    .collect::<ReplayResult<BTreeMap<_, _>>>()?;
                Ok(SemanticDelta::Patch(transitions))
            }
            None => Err(replay_error("unknown delta effect")),
        },
    }
}

fn semantic_full(record: &ChangeRecord) -> ReplayResult<SemanticFull> {
    match record.representation.as_ref() {
        None => Ok(SemanticFull::None),
        Some(Representation::Delta(_)) => Err(replay_error("FullChange is required")),
        Some(Representation::Full(full)) => {
            let mut changed_fields = full.changed_fields.as_ref().map(|mask| {
                mask.paths
                    .iter()
                    .map(|path| path.segments.clone())
                    .collect::<Vec<_>>()
            });
            if let Some(paths) = changed_fields.as_mut() {
                paths.sort();
            }
            Ok(SemanticFull::Change {
                before: full.before.as_ref().map(semantic_record).transpose()?,
                after: full.after.as_ref().map(semantic_record).transpose()?,
                changed_fields,
            })
        }
    }
}

fn metadata_bytes(record: &ChangeRecord) -> Vec<u8> {
    let mut clone = record.clone();
    clone.representation = None;
    clone.encode_to_vec()
}

fn assert_error_contains<T: std::fmt::Debug>(result: ReplayResult<T>, expected: &str) {
    let error = result.expect_err("operation must fail");
    assert!(
        error.0.contains(expected),
        "expected error containing {expected:?}, got {:?}",
        error.0
    );
}

#[test]
fn full_and_delta_golden_histories_replay_to_the_same_state() {
    let full_path = fixture_path("full.binpb");
    let delta_path = fixture_path("delta.binpb");
    let full = read_history(&full_path).expect("full history");
    let delta = read_history(&delta_path).expect("delta history");
    let (full_state, full_snapshots) = replay(&full, None).expect("full replay");
    let (delta_state, delta_snapshots) = replay(&delta, None).expect("delta replay");

    assert_eq!(full.len(), 8);
    assert_eq!(delta.len(), 8);
    assert_eq!(full_snapshots.len(), 7, "retry must be deduplicated");
    assert_eq!(delta_snapshots.len(), 7, "retry must be deduplicated");
    assert_eq!(full[1], full[2]);
    assert_eq!(delta[1], delta[2]);
    assert_eq!(
        full_snapshots
            .iter()
            .map(|(identity, _)| identity)
            .collect::<Vec<_>>(),
        delta_snapshots
            .iter()
            .map(|(identity, _)| identity)
            .collect::<Vec<_>>()
    );
    for ((_, full_at_event), (_, delta_at_event)) in full_snapshots.iter().zip(&delta_snapshots) {
        assert_eq!(
            semantic_state(full_at_event).expect("full semantic state"),
            semantic_state(delta_at_event).expect("delta semantic state")
        );
    }
    assert_eq!(
        semantic_state(&full_state).expect("full final state"),
        semantic_state(&delta_state).expect("delta final state")
    );

    let operations = full
        .iter()
        .map(|(_, record)| record.operation)
        .collect::<HashSet<_>>();
    assert_eq!(
        operations,
        HashSet::from([
            Operation::Create as i32,
            Operation::Update as i32,
            Operation::Delete as i32,
            Operation::SnapshotRead as i32,
            Operation::Truncate as i32,
            Operation::SourceMessage as i32,
        ])
    );

    assert!(
        std::fs::metadata(delta_path).expect("delta metadata").len()
            < std::fs::metadata(full_path).expect("full metadata").len()
    );
}

#[test]
fn mixed_representations_reanchor_and_scope_collection_outcomes() {
    let full = read_history(&fixture_path("full.binpb")).expect("full history");
    let delta = read_history(&fixture_path("delta.binpb")).expect("delta history");
    let mixed = vec![
        delta[0].clone(),
        delta[1].clone(),
        delta[2].clone(),
        full[3].clone(),
        delta[4].clone(),
        full[5].clone(),
        delta[6].clone(),
        full[7].clone(),
    ];
    let (_, full_snapshots) = replay(&full, None).expect("full replay");
    let (mixed_state, mixed_snapshots) = replay(&mixed, None).expect("mixed replay");
    assert_eq!(
        mixed_snapshots
            .iter()
            .map(|(identity, _)| identity)
            .collect::<Vec<_>>(),
        full_snapshots
            .iter()
            .map(|(identity, _)| identity)
            .collect::<Vec<_>>()
    );
    for ((_, mixed), (_, expected)) in mixed_snapshots.iter().zip(&full_snapshots) {
        assert_eq!(
            semantic_state(mixed).expect("mixed state"),
            semantic_state(expected).expect("full state")
        );
    }
    assert_eq!(
        semantic_state(&mixed_state).expect("mixed final state"),
        semantic_state(&replay(&delta, None).expect("delta replay").0).expect("delta final state")
    );

    // A complete full UPDATE is a new outcome anchor. The following delta
    // UPDATE can then validate against that materialized result.
    let reanchor_history = vec![full[1].clone(), delta[3].clone()];
    let expected_history = vec![full[0].clone(), full[1].clone(), full[3].clone()];
    assert_eq!(
        semantic_state(&replay(&reanchor_history, None).expect("reanchor replay").0)
            .expect("reanchor state"),
        semantic_state(&replay(&expected_history, None).expect("expected replay").0)
            .expect("expected state")
    );

    let anchor_history = vec![full[0].clone()];
    let mut validated_base = replay(&anchor_history, None).expect("validated base").0;
    let state_before_failure = semantic_state(&validated_base).expect("state before failure");
    let mut stale_full = full[1].1.clone();
    let Some(Representation::Full(change)) = stale_full.representation.as_mut() else {
        panic!("full update");
    };
    record_value_mut(change.before.as_mut().expect("full before"), "profile").type_name =
        "urn:stale-before".into();
    assert_error_contains(
        apply_event(&mut validated_base, &full[1].0, &stale_full),
        "full before mismatch",
    );
    assert_eq!(
        semantic_state(&validated_base).expect("state after failure"),
        state_before_failure
    );

    let mut absent_state = MaterializedState::new();
    apply_event(&mut absent_state, &delta[4].0, &delta[4].1)
        .expect("DELETE establishes absence without a base");
    assert!(absent_state.is_empty());

    let mut checkpoint = MaterializedState::new();
    apply_event(&mut checkpoint, &delta[0].0, &delta[0].1).expect("snapshot anchor");
    let mut create_at_existing_key = delta[0].1.clone();
    create_at_existing_key.operation = Operation::Create as i32;
    assert_error_contains(
        apply_event(&mut checkpoint, &delta[0].0, &create_at_existing_key),
        "CREATE base already exists",
    );
    let mut replacement = delta[0].1.clone();
    let Some(Representation::Delta(DeltaChange {
        change: Some(DeltaKind::Result(result)),
    })) = replacement.representation.as_mut()
    else {
        panic!("snapshot delta result");
    };
    record_value_mut(result, "profile").type_name = "urn:profile:replacement".into();
    apply_event(&mut checkpoint, &delta[0].0, &replacement).expect("snapshot replacement");
    assert_eq!(
        record_value(
            checkpoint.values().next().expect("checkpoint row"),
            "profile"
        )
        .type_name,
        "urn:profile:replacement"
    );

    // Identical collection/key values under two CloudEvent sources are
    // independent, and TRUNCATE clears only its source-scoped collection.
    let source_a = CloudEvent {
        source: "urn:test:a".into(),
        id: "a".into(),
        ..CloudEvent::default()
    };
    let source_b = CloudEvent {
        source: "urn:test:b".into(),
        id: "b".into(),
        ..CloudEvent::default()
    };
    let mut scoped = MaterializedState::new();
    apply_event(&mut scoped, &source_a, &delta[0].1).expect("source A anchor");
    apply_event(&mut scoped, &source_b, &delta[0].1).expect("source B anchor");
    apply_event(&mut scoped, &source_a, &full[6].1).expect("source A truncate");
    assert_eq!(
        scoped
            .keys()
            .map(|key| key.source.as_str())
            .collect::<Vec<_>>(),
        vec!["urn:test:b"]
    );

    let source_position = delta[0]
        .1
        .source_position
        .as_ref()
        .expect("source position");
    let collision_history = vec![
        (source_a.clone(), delta[0].1.clone()),
        (source_b.clone(), delta[0].1.clone()),
    ];
    let source_a_frontier = SourceFrontier {
        source: source_a.source.clone(),
        stream: source_position.stream.clone(),
        format: source_position.format.clone(),
        value: source_position.value.clone(),
    };
    let source_b_frontier = SourceFrontier {
        source: source_b.source.clone(),
        ..source_a_frontier.clone()
    };
    assert_eq!(
        replay(&collision_history, Some(&source_a_frontier))
            .expect("source A frontier")
            .0
            .len(),
        1
    );
    assert_eq!(
        replay(&collision_history, Some(&source_b_frontier))
            .expect("source B frontier")
            .0
            .len(),
        2
    );

    // The fixture's key-change normalization exposes DELETE then CREATE. A
    // service that promises transactionally atomic reads publishes a complete
    // transaction frontier rather than this intermediate state.
    assert!(full_snapshots[3].1.is_empty());
    assert!(matches!(
        record_value(
            full_snapshots[4].1.values().next().expect("new key row"),
            "id"
        )
        .kind,
        Some(Kind::Int64Value(84))
    ));
}

#[test]
fn full_delta_construction_is_state_semantic_and_base_aware() {
    let full = read_history(&fixture_path("full.binpb")).expect("full history");
    let delta = read_history(&fixture_path("delta.binpb")).expect("delta history");
    let mut state = MaterializedState::new();
    let mut seen = HashSet::new();
    for ((full_event, full_record), (_, delta_record)) in full.iter().zip(&delta) {
        if !seen.insert((full_event.source.clone(), full_event.id.clone())) {
            continue;
        }
        let key = full_record
            .key
            .as_ref()
            .map(|_| state_key(full_event, full_record))
            .transpose()
            .expect("state key");
        let base = key.as_ref().and_then(|key| state.get(key));
        let constructed_delta = full_to_delta(full_record, base).expect("full to delta");
        let constructed_full = delta_to_full(delta_record, base).expect("delta to full");
        assert_eq!(
            semantic_delta(&constructed_delta).expect("constructed delta"),
            semantic_delta(delta_record).expect("fixture delta")
        );
        assert_eq!(
            semantic_full(&constructed_full).expect("constructed full"),
            semantic_full(full_record).expect("fixture full")
        );
        assert_eq!(
            metadata_bytes(&constructed_delta),
            metadata_bytes(full_record)
        );
        assert_eq!(
            metadata_bytes(&constructed_full),
            metadata_bytes(delta_record)
        );
        apply_event(&mut state, full_event, full_record).expect("advance full state");
    }

    assert_eq!(
        semantic_delta(&full_to_delta(&full[1].1, None).expect("complete before is a base"))
            .expect("constructed delta"),
        semantic_delta(&delta[1].1).expect("fixture delta")
    );
    let mut reanchored = MaterializedState::new();
    apply_event(&mut reanchored, &full[1].0, &full[1].1).expect("full update reanchor");
    let Some(Representation::Full(update)) = full[1].1.representation.as_ref() else {
        panic!("full update");
    };
    assert!(
        records_equal(
            reanchored.values().next().expect("reanchored row"),
            update.after.as_ref().expect("full after")
        )
        .expect("semantic record equality")
    );

    assert_eq!(
        semantic_delta(&full_to_delta(&full[4].1, None).expect("base-free full delete"))
            .expect("delete delta"),
        SemanticDelta::Delete
    );
    let base_free_delete = delta_to_full(&delta[4].1, None).expect("base-free delta delete");
    assert_eq!(
        semantic_full(&base_free_delete).expect("empty full delete"),
        SemanticFull::Change {
            before: None,
            after: None,
            changed_fields: None,
        }
    );
    assert_error_contains(
        delta_to_full(&delta[1].1, None),
        "delta-to-full base is missing",
    );
}

#[test]
fn fixtures_preserve_exact_values_and_absent_null_transitions() {
    let full = read_history(&fixture_path("full.binpb")).expect("full history");
    let delta = read_history(&fixture_path("delta.binpb")).expect("delta history");
    let Some(Representation::Full(anchor)) = full[0].1.representation.as_ref() else {
        panic!("snapshot must use full representation");
    };
    let anchor = anchor.after.as_ref().expect("snapshot after");

    let Some(Kind::DecimalValue(decimal)) = record_value(anchor, "account_balance").kind.as_ref()
    else {
        panic!("account_balance must be decimal");
    };
    assert_eq!(
        (decimal.value.as_str(), decimal.scale, decimal.precision),
        ("12345678901234567890.123400", 6, Some(38))
    );
    assert!(matches!(
        record_value(anchor, "avatar").kind.as_ref(),
        Some(Kind::BytesValue(value)) if value == b"\x00\x7f\x80\xff"
    ));
    assert!(matches!(
        record_value(anchor, "created_at").kind.as_ref(),
        Some(Kind::TimestampValue(value))
            if *value == (Timestamp { seconds: 1_723_912_200, nanos: 987_654_321 })
    ));
    assert!(matches!(
        record_value(anchor, "revision").kind,
        Some(Kind::Uint64Value(u64::MAX))
    ));
    let Some(Kind::ListValue(tags)) = record_value(anchor, "tags").kind.as_ref() else {
        panic!("tags must be a list");
    };
    assert!(matches!(tags.values[1].kind, Some(Kind::NullValue(_))));
    let Some(Kind::MapValue(attributes)) = record_value(anchor, "attributes").kind.as_ref() else {
        panic!("attributes must be a map");
    };
    assert!(matches!(
        attributes.entries[1]
            .key
            .as_ref()
            .and_then(|value| value.kind.as_ref()),
        Some(Kind::Int32Value(7))
    ));
    let Some(Kind::RecordValue(profile)) = record_value(anchor, "profile").kind.as_ref() else {
        panic!("profile must be nested");
    };
    assert!(matches!(
        record_value(profile, "display_name").kind.as_ref(),
        Some(Kind::StringValue(value)) if value == "Ada"
    ));

    let patches = delta.iter().filter_map(|(_, record)| {
        let Some(Representation::Delta(delta)) = record.representation.as_ref() else {
            return None;
        };
        let Some(DeltaKind::Patch(patch)) = delta.change.as_ref() else {
            return None;
        };
        Some(patch)
    });
    let changes = patches.flat_map(|patch| &patch.changes).collect::<Vec<_>>();
    assert!(changes.iter().any(|change| {
        matches!(
            (
                change
                    .before
                    .as_ref()
                    .and_then(|state| state.state.as_ref()),
                change.after.as_ref().and_then(|state| state.state.as_ref())
            ),
            (
                Some(FieldStateKind::Absent(_)),
                Some(FieldStateKind::Value(Value {
                    kind: Some(Kind::NullValue(_)),
                    ..
                }))
            )
        )
    }));
    assert!(changes.iter().any(|change| {
        matches!(
            (
                change
                    .before
                    .as_ref()
                    .and_then(|state| state.state.as_ref()),
                change.after.as_ref().and_then(|state| state.state.as_ref())
            ),
            (
                Some(FieldStateKind::Value(Value {
                    kind: Some(Kind::NullValue(_)),
                    ..
                })),
                Some(FieldStateKind::Absent(_))
            )
        )
    }));
}

#[test]
fn canonical_value_equality_and_atomic_collection_changes() {
    let exact_values = vec![
        value(Kind::NullValue(NullValue {})),
        value(Kind::BoolValue(true)),
        value(Kind::Uint32Value(u32::MAX)),
        value(Kind::Int32Value(i32::MIN)),
        value(Kind::Int64Value(i64::MIN)),
        value(Kind::Float32Value(f32::NAN)),
        value(Kind::Float32Value(f32::INFINITY)),
        value(Kind::Float32Value(f32::NEG_INFINITY)),
        value(Kind::Float32Value(0.0)),
        value(Kind::Float32Value(-0.0)),
        value(Kind::Float64Value(f64::NAN)),
        value(Kind::Float64Value(f64::INFINITY)),
        value(Kind::Float64Value(f64::NEG_INFINITY)),
        value(Kind::Float64Value(0.0)),
        value(Kind::Float64Value(-0.0)),
        value(Kind::BytesValue(vec![0, 0x80, 0xff])),
        value(Kind::Uint64Value(u64::MAX)),
        value(Kind::DecimalValue(DecimalValue {
            value: "123".into(),
            scale: -2,
            precision: None,
        })),
        value(Kind::TimestampValue(Timestamp {
            seconds: 1_723_912_200,
            nanos: 987_654_321,
        })),
        value(Kind::RecordValue(record(vec![field(
            "name",
            string_value("nested"),
        )]))),
        value(Kind::ListValue(ListValue {
            values: vec![string_value("a"), string_value("b")],
        })),
        value(Kind::MapValue(MapValue {
            entries: vec![MapEntry {
                key: Some(string_value("key")),
                value: Some(string_value("value")),
            }],
        })),
    ];
    for exact in exact_values {
        let decoded = Value::decode(exact.encode_to_vec().as_slice()).expect("decode exact value");
        assert_eq!(
            semantic_value(&decoded).expect("decoded semantics"),
            semantic_value(&exact).expect("source semantics")
        );
    }

    assert_eq!(
        semantic_value(&value(Kind::Float32Value(f32::NAN))).expect("NaN"),
        semantic_value(&value(Kind::Float32Value(-f32::NAN))).expect("other NaN")
    );
    assert_eq!(
        semantic_value(&value(Kind::Float64Value(f64::NAN))).expect("NaN"),
        semantic_value(&value(Kind::Float64Value(-f64::NAN))).expect("other NaN")
    );
    assert_ne!(
        semantic_value(&value(Kind::Float32Value(0.0))).expect("positive zero"),
        semantic_value(&value(Kind::Float32Value(-0.0))).expect("negative zero")
    );
    assert_ne!(
        semantic_value(&value(Kind::Float64Value(0.0))).expect("positive zero"),
        semantic_value(&value(Kind::Float64Value(-0.0))).expect("negative zero")
    );

    let precision_absent = value(Kind::DecimalValue(DecimalValue {
        value: "1".into(),
        scale: -2,
        precision: None,
    }));
    let precision_zero = value(Kind::DecimalValue(DecimalValue {
        value: "1".into(),
        scale: -2,
        precision: Some(0),
    }));
    assert_ne!(
        semantic_value(&precision_absent).expect("absent precision"),
        semantic_value(&precision_zero).expect("present precision")
    );
    assert_ne!(
        semantic_value(&string_value("1")).expect("string"),
        semantic_value(&value(Kind::Int64Value(1))).expect("int64")
    );
    let mut typed_a = string_value("1");
    typed_a.type_name = "urn:a".into();
    let mut typed_b = typed_a.clone();
    typed_b.type_name = "urn:b".into();
    assert_ne!(
        semantic_value(&typed_a).expect("type A"),
        semantic_value(&typed_b).expect("type B")
    );

    let old_map = value(Kind::MapValue(MapValue {
        entries: vec![
            MapEntry {
                key: Some(string_value("a")),
                value: Some(string_value("1")),
            },
            MapEntry {
                key: Some(string_value("b")),
                value: Some(string_value("2")),
            },
        ],
    }));
    let mut reordered_map = old_map.clone();
    let Some(Kind::MapValue(map)) = reordered_map.kind.as_mut() else {
        panic!("map value");
    };
    map.entries.reverse();
    let old_record = record(vec![
        field("map", old_map.clone()),
        field("name", string_value("x")),
    ]);
    let reordered_record = record(vec![
        field("name", string_value("x")),
        field("map", reordered_map),
    ]);
    assert!(records_equal(&old_record, &reordered_record).expect("record equality"));
    assert!(
        diff_records(&old_record, &reordered_record)
            .expect("no-op diff")
            .changes
            .is_empty()
    );

    let key_a = value(Kind::RecordValue(record(vec![
        field("a", string_value("1")),
        field("b", string_value("2")),
    ])));
    let key_a_reordered = value(Kind::RecordValue(record(vec![
        field("b", string_value("2")),
        field("a", string_value("1")),
    ])));
    let duplicate_map = value(Kind::MapValue(MapValue {
        entries: vec![
            MapEntry {
                key: Some(key_a),
                value: Some(string_value("first")),
            },
            MapEntry {
                key: Some(key_a_reordered),
                value: Some(string_value("second")),
            },
        ],
    }));
    assert_error_contains(
        semantic_value(&duplicate_map),
        "duplicate canonical map key",
    );

    let old_list = value(Kind::ListValue(ListValue {
        values: vec![string_value("a"), string_value("b")],
    }));
    let new_list = value(Kind::ListValue(ListValue {
        values: vec![string_value("b"), string_value("a")],
    }));
    assert_ne!(
        semantic_value(&old_list).expect("old list"),
        semantic_value(&new_list).expect("new list")
    );
    let new_map = value(Kind::MapValue(MapValue {
        entries: vec![
            MapEntry {
                key: Some(string_value("a")),
                value: Some(string_value("changed")),
            },
            MapEntry {
                key: Some(string_value("b")),
                value: Some(string_value("2")),
            },
        ],
    }));
    let base = record(vec![
        field("items", old_list.clone()),
        field("attributes", old_map.clone()),
    ]);
    let collections = RecordPatch {
        changes: vec![
            change(
                &["items"],
                present(old_list.clone()),
                present(new_list.clone()),
            ),
            change(
                &["attributes"],
                present(old_map.clone()),
                present(new_map.clone()),
            ),
        ],
    };
    let changed = apply_patch(&base, &collections).expect("atomic collection replacement");
    assert_eq!(
        semantic_value(record_value(&changed, "items")).expect("changed list"),
        semantic_value(&new_list).expect("new list")
    );
    assert_eq!(
        semantic_value(record_value(&changed, "attributes")).expect("changed map"),
        semantic_value(&new_map).expect("new map")
    );

    let before_failure = base.clone();
    let stale = RecordPatch {
        changes: vec![
            change(&["items"], present(old_list), present(new_list)),
            change(&["attributes"], present(new_map), present(old_map.clone())),
        ],
    };
    assert_error_contains(apply_patch(&base, &stale), "before mismatch");
    assert_eq!(
        base, before_failure,
        "failed patch must not mutate its base"
    );

    let nested_before = record(vec![field(
        "profile",
        Value {
            type_name: "urn:profile:v1".into(),
            kind: Some(Kind::RecordValue(record(vec![field(
                "name",
                string_value("same"),
            )]))),
        },
    )]);
    let nested_after = record(vec![field(
        "profile",
        Value {
            type_name: "urn:profile:v2".into(),
            kind: Some(Kind::RecordValue(record(vec![field(
                "name",
                string_value("same"),
            )]))),
        },
    )]);
    assert_eq!(
        diff_records(&nested_before, &nested_after)
            .expect("nested type-name diff")
            .changes[0]
            .path
            .as_ref()
            .expect("path")
            .segments,
        vec!["profile"]
    );
    assert_eq!(
        diff_records(
            &record(vec![field("amount", precision_absent)]),
            &record(vec![field("amount", precision_zero)]),
        )
        .expect("precision-presence diff")
        .changes
        .len(),
        1
    );
    assert_eq!(
        diff_records(
            &record(vec![field("number", value(Kind::Float64Value(0.0)))]),
            &record(vec![field("number", value(Kind::Float64Value(-0.0)))]),
        )
        .expect("signed-zero diff")
        .changes
        .len(),
        1
    );
    assert!(
        diff_records(
            &record(vec![field("number", value(Kind::Float64Value(f64::NAN)),)]),
            &record(vec![field("number", value(Kind::Float64Value(-f64::NAN)),)]),
        )
        .expect("NaN no-op")
        .changes
        .is_empty()
    );
}

#[test]
fn histories_are_queryable_at_an_opaque_source_position() {
    let full = read_history(&fixture_path("full.binpb")).expect("full history");
    let delta = read_history(&fixture_path("delta.binpb")).expect("delta history");
    let source_position = full[1]
        .1
        .source_position
        .as_ref()
        .expect("fixture source position");
    let frontier = SourceFrontier {
        source: full[1].0.source.clone(),
        stream: source_position.stream.clone(),
        format: source_position.format.clone(),
        value: source_position.value.clone(),
    };
    let (full_at_position, _) = replay(&full, Some(&frontier)).expect("full position");
    let (delta_at_position, _) = replay(&delta, Some(&frontier)).expect("delta position");
    assert_eq!(
        semantic_state(&full_at_position).expect("full state"),
        semantic_state(&delta_at_position).expect("delta state")
    );
    assert_eq!(full_at_position.len(), 1);
    assert!(matches!(
        record_value(full_at_position.values().next().expect("record"), "id").kind,
        Some(Kind::Int64Value(42))
    ));

    let wrong_format = SourceFrontier {
        format: "application/x-wrong-position".into(),
        ..frontier
    };
    assert_error_contains(
        replay(&full, Some(&wrong_format)),
        "source position was not found",
    );
}

#[test]
fn invalid_patches_fail_atomically() {
    let base = record(vec![
        field("name", string_value("before")),
        field(
            "profile",
            Value {
                type_name: String::new(),
                kind: Some(Kind::RecordValue(record(vec![field(
                    "city",
                    string_value("Oslo"),
                )]))),
            },
        ),
    ]);

    let update = delta_record(
        Operation::Update,
        DeltaKind::Patch(RecordPatch {
            changes: vec![change(
                &["name"],
                present(string_value("before")),
                present(string_value("after")),
            )],
        }),
    );
    let mut empty = MaterializedState::new();
    assert_error_contains(
        apply_event(
            &mut empty,
            &CloudEvent {
                source: "urn:test".into(),
                ..CloudEvent::default()
            },
            &update,
        ),
        "base is missing",
    );

    let stale = RecordPatch {
        changes: vec![
            change(
                &["name"],
                present(string_value("before")),
                present(string_value("after")),
            ),
            change(
                &["profile", "city"],
                present(string_value("wrong")),
                present(string_value("Bergen")),
            ),
        ],
    };
    assert_error_contains(apply_patch(&base, &stale), "before mismatch");
    assert!(matches!(
        record_value(&base, "name").kind.as_ref(),
        Some(Kind::StringValue(value)) if value == "before"
    ));

    let duplicate = RecordPatch {
        changes: vec![
            change(
                &["name"],
                present(string_value("before")),
                present(string_value("one")),
            ),
            change(
                &["name"],
                present(string_value("before")),
                present(string_value("two")),
            ),
        ],
    };
    assert_error_contains(apply_patch(&base, &duplicate), "duplicate patch path");

    let overlap = RecordPatch {
        changes: vec![
            change(
                &["profile"],
                present(record_value(&base, "profile").clone()),
                present(Value {
                    type_name: String::new(),
                    kind: Some(Kind::RecordValue(record(vec![field(
                        "city",
                        string_value("Bergen"),
                    )]))),
                }),
            ),
            change(
                &["profile", "city"],
                present(string_value("Oslo")),
                present(string_value("Bergen")),
            ),
        ],
    };
    assert_error_contains(apply_patch(&base, &overlap), "overlapping patch paths");

    let empty_path = RecordPatch {
        changes: vec![change(&[], absent(), present(string_value("new")))],
    };
    assert_error_contains(apply_patch(&base, &empty_path), "empty patch path");

    let missing_ancestor = RecordPatch {
        changes: vec![change(
            &["missing", "leaf"],
            absent(),
            present(string_value("new")),
        )],
    };
    assert_error_contains(apply_patch(&base, &missing_ancestor), "missing ancestor");
}

#[test]
fn invalid_operation_representation_pairs_and_keyless_replay_are_rejected() {
    let invalid = [
        delta_record(
            Operation::Create,
            DeltaKind::Patch(RecordPatch { changes: vec![] }),
        ),
        delta_record(
            Operation::Update,
            DeltaKind::Result(record(vec![field("id", string_value("1"))])),
        ),
        delta_record(
            Operation::Delete,
            DeltaKind::Result(record(vec![field("id", string_value("1"))])),
        ),
        delta_record(Operation::Truncate, DeltaKind::Delete(DeleteDelta {})),
        delta_record(Operation::SourceMessage, DeltaKind::Delete(DeleteDelta {})),
    ];
    for record in invalid {
        assert!(validate_shape(&record).is_err());
    }

    let mut keyless = delta_record(
        Operation::Create,
        DeltaKind::Result(record(vec![field("name", string_value("keyless"))])),
    );
    keyless.key = None;
    validate_shape(&keyless).expect("keyless source remains valid wire data");
    assert_error_contains(
        apply_event(
            &mut MaterializedState::new(),
            &CloudEvent {
                source: "urn:test".into(),
                ..CloudEvent::default()
            },
            &keyless,
        ),
        "keyless record",
    );
}

#[test]
fn event_validation_recurses_through_every_canonical_value_location() {
    let full = read_history(&fixture_path("full.binpb")).expect("full history");
    let delta = read_history(&fixture_path("delta.binpb")).expect("delta history");
    let mut invalid = Vec::new();

    let mut duplicate_key = delta[0].1.clone();
    let duplicate = duplicate_key
        .key
        .as_ref()
        .expect("key")
        .fields
        .first()
        .expect("key field")
        .clone();
    duplicate_key
        .key
        .as_mut()
        .expect("key")
        .fields
        .push(duplicate);
    invalid.push(duplicate_key);

    let mut invalid_full_before = full[1].1.clone();
    let Some(Representation::Full(change)) = invalid_full_before.representation.as_mut() else {
        panic!("full update");
    };
    change
        .before
        .as_mut()
        .expect("before")
        .fields
        .push(RecordField {
            name: "invalid".into(),
            value: Some(Value::default()),
        });
    invalid.push(invalid_full_before);

    let mut invalid_full_after = full[0].1.clone();
    let Some(Representation::Full(change)) = invalid_full_after.representation.as_mut() else {
        panic!("full snapshot");
    };
    change
        .after
        .as_mut()
        .expect("after")
        .fields
        .push(RecordField {
            name: "invalid".into(),
            value: Some(Value::default()),
        });
    invalid.push(invalid_full_after);

    let mut invalid_delta_result = delta[0].1.clone();
    let Some(Representation::Delta(DeltaChange {
        change: Some(DeltaKind::Result(result)),
    })) = invalid_delta_result.representation.as_mut()
    else {
        panic!("delta result");
    };
    result.fields.push(RecordField {
        name: "invalid".into(),
        value: Some(Value::default()),
    });
    invalid.push(invalid_delta_result);

    let mut invalid_patch_state = delta[1].1.clone();
    let Some(Representation::Delta(DeltaChange {
        change: Some(DeltaKind::Patch(patch)),
    })) = invalid_patch_state.representation.as_mut()
    else {
        panic!("delta patch");
    };
    patch.changes[0].before = Some(FieldState::default());
    invalid.push(invalid_patch_state);

    let mut invalid_source_message = full[7].1.clone();
    invalid_source_message.source_message = Some(Value::default());
    invalid.push(invalid_source_message);

    let mut invalid_nested_timestamp = full[0].1.clone();
    let Some(Representation::Full(change)) = invalid_nested_timestamp.representation.as_mut()
    else {
        panic!("full snapshot");
    };
    let Some(Kind::TimestampValue(timestamp)) =
        record_value_mut(change.after.as_mut().expect("after"), "created_at")
            .kind
            .as_mut()
    else {
        panic!("timestamp value");
    };
    timestamp.seconds = 253_402_300_800;
    invalid.push(invalid_nested_timestamp);

    let mut invalid_capture_timestamp = full[0].1.clone();
    invalid_capture_timestamp
        .capture_time
        .as_mut()
        .expect("capture time")
        .nanos = -1;
    invalid.push(invalid_capture_timestamp);

    let mut duplicate_map_key = full[0].1.clone();
    let Some(Representation::Full(change)) = duplicate_map_key.representation.as_mut() else {
        panic!("full snapshot");
    };
    let Some(Kind::MapValue(map)) =
        record_value_mut(change.after.as_mut().expect("after"), "attributes")
            .kind
            .as_mut()
    else {
        panic!("map value");
    };
    map.entries.push(map.entries[0].clone());
    invalid.push(duplicate_map_key);

    let mut incomplete_map_entry = full[0].1.clone();
    let Some(Representation::Full(change)) = incomplete_map_entry.representation.as_mut() else {
        panic!("full snapshot");
    };
    let Some(Kind::MapValue(map)) =
        record_value_mut(change.after.as_mut().expect("after"), "attributes")
            .kind
            .as_mut()
    else {
        panic!("map value");
    };
    map.entries.push(MapEntry {
        key: Some(string_value("incomplete")),
        value: None,
    });
    invalid.push(incomplete_map_entry);

    for record in invalid {
        assert!(validate_shape(&record).is_err());
    }
}

#[test]
fn explicit_null_and_absent_are_distinct_patch_states() {
    let null = Value {
        type_name: String::new(),
        kind: Some(Kind::NullValue(NullValue {})),
    };
    let base = record(vec![field("id", string_value("1"))]);
    let added = apply_patch(
        &base,
        &RecordPatch {
            changes: vec![change(&["note"], absent(), present(null.clone()))],
        },
    )
    .expect("add explicit null");
    assert!(matches!(
        record_value(&added, "note").kind,
        Some(Kind::NullValue(_))
    ));
    let removed = apply_patch(
        &added,
        &RecordPatch {
            changes: vec![change(&["note"], present(null), absent())],
        },
    )
    .expect("remove null field");
    assert!(
        find_field(&removed, "note")
            .expect("valid record")
            .is_none()
    );
}

#[test]
fn future_fields_are_accepted_without_losing_known_semantics() {
    let history = read_history(&fixture_path("delta.binpb")).expect("delta history");
    let known = history[0].1.encode_to_vec();
    let mut future = known.clone();
    // Field 100, varint value 1, stands in for a future writer's additive field.
    future.extend_from_slice(&[0xa0, 0x06, 0x01]);
    let decoded = ChangeRecord::decode(future.as_slice()).expect("future ChangeRecord");

    assert_eq!(decoded.operation, history[0].1.operation);
    assert!(matches!(
        decoded.representation,
        Some(Representation::Delta(DeltaChange {
            change: Some(DeltaKind::Result(_))
        }))
    ));
    validate_shape(&decoded).expect("known semantics remain valid");

    // Prost accepts future fields but generated messages do not retain their
    // raw bytes. A lossless relay must therefore forward the original Any.
    assert_eq!(decoded.encode_to_vec(), known);
    assert_ne!(decoded.encode_to_vec(), future);

    // Future oneof members decode with the known oneof unset in Prost. The raw
    // bytes are not retained, and validation must fail closed rather than
    // interpreting the event as an empty/no-op effect or state.
    let future_oneof = [0xa2, 0x06, 0x01, b'x']; // field 100, bytes "x"
    let future_delta = DeltaChange::decode(future_oneof.as_slice()).expect("future delta");
    assert!(future_delta.change.is_none());
    assert!(future_delta.encode_to_vec().is_empty());
    let mut unknown_effect = delta_record(
        Operation::Update,
        DeltaKind::Patch(RecordPatch { changes: vec![] }),
    );
    unknown_effect.representation = Some(Representation::Delta(future_delta));
    assert_error_contains(validate_shape(&unknown_effect), "UPDATE requires");

    let future_state = FieldState::decode(future_oneof.as_slice()).expect("future field state");
    assert!(future_state.state.is_none());
    assert_error_contains(
        validate_patch(&RecordPatch {
            changes: vec![change(
                &["field"],
                future_state,
                present(string_value("after")),
            )],
        }),
        "field state is required",
    );

    let future_extension =
        SourceExtension::decode(future_oneof.as_slice()).expect("future source extension");
    let mut with_unknown_extension = history[0].1.clone();
    with_unknown_extension.source_extension = Some(future_extension);
    validate_shape(&with_unknown_extension).expect("unknown isolated extension is non-semantic");
}
