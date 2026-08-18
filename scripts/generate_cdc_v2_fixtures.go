//go:build ignore

// generate_cdc_v2_fixtures writes the deterministic shared CDC v2 replay
// histories. Run it from the repository root with:
//
//	go run ./scripts/generate_cdc_v2_fixtures.go
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	cdcv2 "github.com/jim-technologies/invariantprotocol/go/gen/invariant/cdc/v2"
	cloudeventsv1 "github.com/jim-technologies/invariantprotocol/go/gen/io/cloudevents/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	fixtureDirectory = "testdata/cdc/v2"
	eventSource      = "urn:invariantprotocol:fixture:cdc:v2"
	eventType        = "io.invariantprotocol.cdc.v2.change"
	dataSchema       = "type.googleapis.com/invariant.cdc.v2.ChangeRecord"
)

type manifest struct {
	FixtureFormatVersion int                `json:"fixture_format_version"`
	Contract             string             `json:"contract"`
	Envelope             string             `json:"envelope"`
	EventType            string             `json:"event_type"`
	Source               string             `json:"source"`
	EventCount           int                `json:"event_count"`
	UniqueEventCount     int                `json:"unique_event_count"`
	Operations           []string           `json:"operations"`
	Positions            []string           `json:"positions"`
	RetryIndexes         []int              `json:"retry_indexes"`
	StateAtPosition      map[string][]int64 `json:"state_at_position"`
	Files                []manifestFile     `json:"files"`
}

type manifestFile struct {
	Path           string `json:"path"`
	Representation string `json:"representation"`
	SHA256         string `json:"sha256"`
	Size           int    `json:"size"`
}

func main() {
	full, delta := fixtureBatches()
	if err := os.MkdirAll(fixtureDirectory, 0o755); err != nil {
		panic(err)
	}
	fullBytes := marshal(full)
	deltaBytes := marshal(delta)
	write(filepath.Join(fixtureDirectory, "full.binpb"), fullBytes)
	write(filepath.Join(fixtureDirectory, "delta.binpb"), deltaBytes)

	index := manifest{
		FixtureFormatVersion: 1,
		Contract:             "invariant.cdc.v2.ChangeRecord",
		Envelope:             "io.cloudevents.v1.CloudEventBatch",
		EventType:            eventType,
		Source:               eventSource,
		EventCount:           len(full.GetEvents()),
		UniqueEventCount:     7,
		Operations:           []string{"SNAPSHOT_READ", "UPDATE", "UPDATE", "UPDATE", "DELETE", "CREATE", "TRUNCATE", "SOURCE_MESSAGE"},
		Positions:            []string{"0001", "0002", "0002", "0003", "0004", "0005", "0006", "0007"},
		RetryIndexes:         []int{2},
		StateAtPosition: map[string][]int64{
			"0001": {42},
			"0002": {42},
			"0003": {42},
			"0004": {},
			"0005": {84},
			"0006": {},
			"0007": {},
		},
		Files: []manifestFile{
			{Path: "full.binpb", Representation: "full", SHA256: digest(fullBytes), Size: len(fullBytes)},
			{Path: "delta.binpb", Representation: "delta", SHA256: digest(deltaBytes), Size: len(deltaBytes)},
		},
	}
	manifestBytes, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		panic(err)
	}
	manifestBytes = append(manifestBytes, '\n')
	write(filepath.Join(fixtureDirectory, "manifest.json"), manifestBytes)
}

func fixtureBatches() (*cloudeventsv1.CloudEventBatch, *cloudeventsv1.CloudEventBatch) {
	initial := initialRecord()
	afterFirst := clone(initial)
	setNested(afterFirst, []string{"profile", "display_name"}, stringValue("Ada Lovelace"))
	afterFirst.Fields = append(afterFirst.Fields, field("nickname", nullValue()))
	afterSecond := clone(afterFirst)
	setNested(afterSecond, []string{"profile", "level"}, int32Value(2))
	removeField(afterSecond, "nickname")
	fresh := freshRecord()

	key42 := record(field("id", int64Value(42)))
	key84 := record(field("id", int64Value(84)))
	firstPaths := mask(path("nickname"), path("profile", "display_name"))
	secondPaths := mask(path("nickname"), path("profile", "level"))

	fullRecords := []*cdcv2.ChangeRecord{
		baseRecord(cdcv2.Operation_OPERATION_SNAPSHOT_READ, key42, "0001"),
		baseRecord(cdcv2.Operation_OPERATION_UPDATE, key42, "0002"),
		nil,
		baseRecord(cdcv2.Operation_OPERATION_UPDATE, key42, "0003"),
		baseRecord(cdcv2.Operation_OPERATION_DELETE, key42, "0004"),
		baseRecord(cdcv2.Operation_OPERATION_CREATE, key84, "0005"),
		baseRecord(cdcv2.Operation_OPERATION_TRUNCATE, nil, "0006"),
		baseRecord(cdcv2.Operation_OPERATION_SOURCE_MESSAGE, nil, "0007"),
	}
	fullRecords[0].Representation = &cdcv2.ChangeRecord_Full{Full: &cdcv2.FullChange{After: clone(initial)}}
	fullRecords[1].Representation = &cdcv2.ChangeRecord_Full{Full: &cdcv2.FullChange{Before: clone(initial), After: clone(afterFirst), ChangedFields: firstPaths}}
	fullRecords[2] = clone(fullRecords[1])
	fullRecords[3].Representation = &cdcv2.ChangeRecord_Full{Full: &cdcv2.FullChange{Before: clone(afterFirst), After: clone(afterSecond), ChangedFields: secondPaths}}
	fullRecords[4].Representation = &cdcv2.ChangeRecord_Full{Full: &cdcv2.FullChange{Before: clone(afterSecond)}}
	fullRecords[5].Representation = &cdcv2.ChangeRecord_Full{Full: &cdcv2.FullChange{After: clone(fresh)}}
	fullRecords[7].DataCollection = nil
	fullRecords[7].SourceMessage = logicalMessage()

	deltaRecords := []*cdcv2.ChangeRecord{
		baseRecord(cdcv2.Operation_OPERATION_SNAPSHOT_READ, key42, "0001"),
		baseRecord(cdcv2.Operation_OPERATION_UPDATE, key42, "0002"),
		nil,
		baseRecord(cdcv2.Operation_OPERATION_UPDATE, key42, "0003"),
		baseRecord(cdcv2.Operation_OPERATION_DELETE, key42, "0004"),
		baseRecord(cdcv2.Operation_OPERATION_CREATE, key84, "0005"),
		baseRecord(cdcv2.Operation_OPERATION_TRUNCATE, nil, "0006"),
		baseRecord(cdcv2.Operation_OPERATION_SOURCE_MESSAGE, nil, "0007"),
	}
	deltaRecords[0].Representation = &cdcv2.ChangeRecord_Delta{Delta: &cdcv2.DeltaChange{Change: &cdcv2.DeltaChange_Result{Result: clone(initial)}}}
	deltaRecords[1].Representation = &cdcv2.ChangeRecord_Delta{Delta: &cdcv2.DeltaChange{Change: &cdcv2.DeltaChange_Patch{Patch: &cdcv2.RecordPatch{Changes: []*cdcv2.FieldChange{
		change(path("nickname"), absent(), present(nullValue())),
		change(path("profile", "display_name"), present(stringValue("Ada")), present(stringValue("Ada Lovelace"))),
	}}}}}
	deltaRecords[2] = clone(deltaRecords[1])
	deltaRecords[3].Representation = &cdcv2.ChangeRecord_Delta{Delta: &cdcv2.DeltaChange{Change: &cdcv2.DeltaChange_Patch{Patch: &cdcv2.RecordPatch{Changes: []*cdcv2.FieldChange{
		change(path("nickname"), present(nullValue()), absent()),
		change(path("profile", "level"), present(int32Value(1)), present(int32Value(2))),
	}}}}}
	deltaRecords[4].Representation = &cdcv2.ChangeRecord_Delta{Delta: &cdcv2.DeltaChange{Change: &cdcv2.DeltaChange_Delete{Delete: &cdcv2.DeleteDelta{}}}}
	deltaRecords[5].Representation = &cdcv2.ChangeRecord_Delta{Delta: &cdcv2.DeltaChange{Change: &cdcv2.DeltaChange_Result{Result: clone(fresh)}}}
	deltaRecords[7].DataCollection = nil
	deltaRecords[7].SourceMessage = logicalMessage()

	fullEvents := make([]*cloudeventsv1.CloudEvent, len(fullRecords))
	deltaEvents := make([]*cloudeventsv1.CloudEvent, len(deltaRecords))
	for i := range fullRecords {
		fullEvents[i] = cloudEvent(fullRecords[i])
		deltaEvents[i] = cloudEvent(deltaRecords[i])
	}
	// Model a transport retry as the exact same occurrence. The two batches use
	// the same source + id sequence even though their payload representations differ.
	fullEvents[2] = clone(fullEvents[1])
	deltaEvents[2] = clone(deltaEvents[1])
	return &cloudeventsv1.CloudEventBatch{Events: fullEvents}, &cloudeventsv1.CloudEventBatch{Events: deltaEvents}
}

func baseRecord(operation cdcv2.Operation, key *cdcv2.Record, position string) *cdcv2.ChangeRecord {
	sequence := uint64(position[3] - '0')
	sourceTime := time.Date(2026, time.August, 18, 12, 0, int(sequence), int(sequence)*111_111, time.UTC)
	record := &cdcv2.ChangeRecord{
		Operation:      operation,
		Key:            clone(key),
		DataCollection: &cdcv2.DataCollection{Id: "fixture.accounts"},
		SchemaReference: &cdcv2.SchemaReference{
			Uri: "urn:invariantprotocol:fixture:cdc:v2:accounts", Version: "1", Fingerprint: []byte{0xc0, 0xdc, 0x02},
		},
		SourcePosition: &cdcv2.SourcePosition{Stream: "fixture-stream-0", Format: "application/x.invariant.fixture-position", Value: []byte(position)},
		SourceTime:     timestamppb.New(sourceTime),
		CaptureTime:    timestamppb.New(sourceTime.Add(125 * time.Millisecond)),
		SourceExtension: &cdcv2.SourceExtension{Representation: &cdcv2.SourceExtension_OpaqueData{
			OpaqueData: &cdcv2.OpaqueData{MediaType: "application/json", Schema: "urn:invariantprotocol:fixture:source:v1", Data: []byte(`{"future":{"token":"opaque-v2"}}`)},
		}},
	}
	if operation == cdcv2.Operation_OPERATION_UPDATE {
		record.Transaction = &cdcv2.TransactionContext{Id: "fixture-transaction", TotalOrder: &sequence, DataCollectionOrder: &sequence}
	}
	return record
}

func cloudEvent(record *cdcv2.ChangeRecord) *cloudeventsv1.CloudEvent {
	payload, err := anypb.New(record)
	if err != nil {
		panic(err)
	}
	position := string(record.GetSourcePosition().GetValue())
	return &cloudeventsv1.CloudEvent{
		Id:          "fixture-v2-" + position,
		Source:      eventSource,
		SpecVersion: "1.0",
		Type:        eventType,
		Attributes: map[string]*cloudeventsv1.CloudEvent_CloudEventAttributeValue{
			"time":            timestampAttribute(record.GetSourceTime()),
			"datacontenttype": stringAttribute("application/protobuf"),
			"dataschema":      uriAttribute(dataSchema),
			"correlationid":   stringAttribute("fixture-replay-history"),
		},
		Data: &cloudeventsv1.CloudEvent_ProtoData{ProtoData: payload},
	}
}

func initialRecord() *cdcv2.Record {
	precision := uint32(38)
	return record(
		field("id", int64Value(42)),
		field("account_balance", &cdcv2.Value{TypeName: "example.Decimal", Kind: &cdcv2.Value_DecimalValue{DecimalValue: &cdcv2.DecimalValue{Value: "12345678901234567890.123400", Scale: 6, Precision: &precision}}}),
		field("avatar", &cdcv2.Value{TypeName: "example.Binary", Kind: &cdcv2.Value_BytesValue{BytesValue: []byte{0x00, 0x7f, 0x80, 0xff}}}),
		field("created_at", &cdcv2.Value{TypeName: "example.NanosecondInstant", Kind: &cdcv2.Value_TimestampValue{TimestampValue: &timestamppb.Timestamp{Seconds: 1_723_912_200, Nanos: 987_654_321}}}),
		field("revision", &cdcv2.Value{Kind: &cdcv2.Value_Uint64Value{Uint64Value: ^uint64(0)}}),
		field("tags", &cdcv2.Value{Kind: &cdcv2.Value_ListValue{ListValue: &cdcv2.ListValue{Values: []*cdcv2.Value{stringValue("vip"), nullValue(), stringValue("beta")}}}}),
		field("attributes", &cdcv2.Value{Kind: &cdcv2.Value_MapValue{MapValue: &cdcv2.MapValue{Entries: []*cdcv2.MapEntry{
			{Key: stringValue("tier"), Value: stringValue("platinum")},
			{Key: int32Value(7), Value: &cdcv2.Value{Kind: &cdcv2.Value_BytesValue{BytesValue: []byte("exact")}}},
		}}}}),
		field("profile", &cdcv2.Value{Kind: &cdcv2.Value_RecordValue{RecordValue: record(
			field("display_name", stringValue("Ada")),
			field("level", int32Value(1)),
			field("score", &cdcv2.Value{Kind: &cdcv2.Value_Float64Value{Float64Value: 0.125}}),
		)}}),
	)
}

func freshRecord() *cdcv2.Record {
	return record(
		field("id", int64Value(84)),
		field("account_balance", &cdcv2.Value{Kind: &cdcv2.Value_DecimalValue{DecimalValue: &cdcv2.DecimalValue{Value: "0.00", Scale: 2}}}),
		field("profile", &cdcv2.Value{Kind: &cdcv2.Value_RecordValue{RecordValue: record(field("display_name", stringValue("Grace")), field("level", int32Value(1)))}}),
	)
}

func logicalMessage() *cdcv2.Value {
	return &cdcv2.Value{TypeName: "example.SourceMessage", Kind: &cdcv2.Value_RecordValue{RecordValue: record(
		field("content", &cdcv2.Value{Kind: &cdcv2.Value_BytesValue{BytesValue: []byte("replay-complete")}}),
		field("prefix", stringValue("fixture")),
	)}}
}

func record(fields ...*cdcv2.RecordField) *cdcv2.Record { return &cdcv2.Record{Fields: fields} }
func field(name string, value *cdcv2.Value) *cdcv2.RecordField {
	return &cdcv2.RecordField{Name: name, Value: value}
}
func path(segments ...string) *cdcv2.FieldPath { return &cdcv2.FieldPath{Segments: segments} }
func mask(paths ...*cdcv2.FieldPath) *cdcv2.ChangedFieldMask {
	return &cdcv2.ChangedFieldMask{Paths: paths}
}
func absent() *cdcv2.FieldState {
	return &cdcv2.FieldState{State: &cdcv2.FieldState_Absent{Absent: &cdcv2.Absent{}}}
}
func present(value *cdcv2.Value) *cdcv2.FieldState {
	return &cdcv2.FieldState{State: &cdcv2.FieldState_Value{Value: clone(value)}}
}
func change(path *cdcv2.FieldPath, before, after *cdcv2.FieldState) *cdcv2.FieldChange {
	return &cdcv2.FieldChange{Path: path, Before: before, After: after}
}
func nullValue() *cdcv2.Value {
	return &cdcv2.Value{Kind: &cdcv2.Value_NullValue{NullValue: &cdcv2.NullValue{}}}
}
func stringValue(value string) *cdcv2.Value {
	return &cdcv2.Value{Kind: &cdcv2.Value_StringValue{StringValue: value}}
}
func int32Value(value int32) *cdcv2.Value {
	return &cdcv2.Value{Kind: &cdcv2.Value_Int32Value{Int32Value: value}}
}
func int64Value(value int64) *cdcv2.Value {
	return &cdcv2.Value{Kind: &cdcv2.Value_Int64Value{Int64Value: value}}
}

func setNested(root *cdcv2.Record, segments []string, value *cdcv2.Value) {
	current := root
	for _, segment := range segments[:len(segments)-1] {
		field := findField(current, segment)
		if field == nil || field.GetValue().GetRecordValue() == nil {
			panic(fmt.Sprintf("fixture path %v does not traverse records", segments))
		}
		current = field.GetValue().GetRecordValue()
	}
	leaf := findField(current, segments[len(segments)-1])
	if leaf == nil {
		panic(fmt.Sprintf("fixture path %v has no leaf", segments))
	}
	leaf.Value = clone(value)
}

func removeField(record *cdcv2.Record, name string) {
	for index, field := range record.GetFields() {
		if field.GetName() == name {
			record.Fields = append(record.Fields[:index], record.Fields[index+1:]...)
			return
		}
	}
	panic("fixture field not found: " + name)
}

func findField(record *cdcv2.Record, name string) *cdcv2.RecordField {
	for _, field := range record.GetFields() {
		if field.GetName() == name {
			return field
		}
	}
	return nil
}

func stringAttribute(value string) *cloudeventsv1.CloudEvent_CloudEventAttributeValue {
	return &cloudeventsv1.CloudEvent_CloudEventAttributeValue{Attr: &cloudeventsv1.CloudEvent_CloudEventAttributeValue_CeString{CeString: value}}
}
func uriAttribute(value string) *cloudeventsv1.CloudEvent_CloudEventAttributeValue {
	return &cloudeventsv1.CloudEvent_CloudEventAttributeValue{Attr: &cloudeventsv1.CloudEvent_CloudEventAttributeValue_CeUri{CeUri: value}}
}
func timestampAttribute(value *timestamppb.Timestamp) *cloudeventsv1.CloudEvent_CloudEventAttributeValue {
	return &cloudeventsv1.CloudEvent_CloudEventAttributeValue{Attr: &cloudeventsv1.CloudEvent_CloudEventAttributeValue_CeTimestamp{CeTimestamp: value}}
}

func clone[T proto.Message](value T) T {
	if any(value) == nil {
		return value
	}
	return proto.Clone(value).(T)
}

func marshal(message proto.Message) []byte {
	data, err := (proto.MarshalOptions{Deterministic: true}).Marshal(message)
	if err != nil {
		panic(err)
	}
	return data
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func write(path string, data []byte) {
	if err := os.WriteFile(path, data, 0o644); err != nil {
		panic(err)
	}
}
