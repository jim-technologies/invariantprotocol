package invariant_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	cdcv1 "github.com/jim-technologies/invariantprotocol/go/gen/invariant/cdc/v1"
	cloudeventsv1 "github.com/jim-technologies/invariantprotocol/go/gen/io/cloudevents/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	cdcEventType  = "io.invariantprotocol.cdc.v1.change"
	cdcDataSchema = "type.googleapis.com/invariant.cdc.v1.ChangeRecord"
)

func TestCDCCloudEventContract(t *testing.T) {
	t.Parallel()

	sourceTime := timestamppb.New(time.Date(2026, time.August, 17, 16, 30, 0, 123456789, time.UTC))
	change := &cdcv1.ChangeRecord{
		Operation:      cdcv1.Operation_OPERATION_CREATE,
		Key:            cdcRecord(cdcField("id", cdcInt64(1001))),
		After:          cdcRecord(cdcField("id", cdcInt64(1001)), cdcField("name", cdcString("Ada"))),
		DataCollection: &cdcv1.DataCollection{Id: "inventory.customers"},
		SchemaReference: &cdcv1.SchemaReference{
			Uri:     "urn:example:schema:inventory.customers",
			Version: "7",
		},
		SourcePosition: &cdcv1.SourcePosition{
			Stream: "orders-0",
			Format: "application/octet-stream",
			Value:  []byte{0x01, 0x00, 0xff},
		},
		SourceTime:  sourceTime,
		CaptureTime: timestamppb.New(sourceTime.AsTime().Add(250 * time.Millisecond)),
	}
	payload, err := anypb.New(change)
	require.NoError(t, err)

	event := &cloudeventsv1.CloudEvent{
		Id:          "01J5A6M9V9Y8G4W6Z4X1R9P7K3",
		Source:      "urn:example:source:inventory",
		SpecVersion: "1.0",
		Type:        cdcEventType,
		Attributes: map[string]*cloudeventsv1.CloudEvent_CloudEventAttributeValue{
			"time":            cdcTimestampAttribute(sourceTime),
			"datacontenttype": cdcStringAttribute("application/protobuf"),
			"dataschema":      cdcURIAttribute(cdcDataSchema),
			"correlationid":   cdcStringAttribute("order-import-42"),
			"causationid":     cdcStringAttribute("command-17"),
			"traceparent":     cdcStringAttribute("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"),
		},
		Data: &cloudeventsv1.CloudEvent_ProtoData{ProtoData: payload},
	}

	wire, err := proto.Marshal(event)
	require.NoError(t, err)
	var decoded cloudeventsv1.CloudEvent
	require.NoError(t, proto.Unmarshal(wire, &decoded))
	require.Equal(t, "1.0", decoded.GetSpecVersion())
	require.Equal(t, cdcEventType, decoded.GetType())
	require.Equal(t, "application/protobuf", decoded.GetAttributes()["datacontenttype"].GetCeString())
	require.Equal(t, cdcDataSchema, decoded.GetAttributes()["dataschema"].GetCeUri())
	require.True(t, proto.Equal(sourceTime, decoded.GetAttributes()["time"].GetCeTimestamp()))
	require.Equal(t, cdcDataSchema, decoded.GetProtoData().GetTypeUrl())

	var unpacked cdcv1.ChangeRecord
	require.NoError(t, decoded.GetProtoData().UnmarshalTo(&unpacked))
	require.True(t, proto.Equal(change, &unpacked))

	// Retrying delivery does not create a new occurrence: source + id stays
	// stable while transport-owned delivery metadata may change elsewhere.
	retry := proto.Clone(event).(*cloudeventsv1.CloudEvent)
	require.Equal(t, event.GetSource(), retry.GetSource())
	require.Equal(t, event.GetId(), retry.GetId())

	// Guard the upstream CloudEvents v1.0.2 field numbers that make this local
	// generated type wire-compatible with io.cloudevents.v1.CloudEvent.
	descriptor := event.ProtoReflect().Descriptor().Fields()
	require.EqualValues(t, 1, descriptor.ByName("id").Number())
	require.EqualValues(t, 2, descriptor.ByName("source").Number())
	require.EqualValues(t, 3, descriptor.ByName("spec_version").Number())
	require.EqualValues(t, 4, descriptor.ByName("type").Number())
	require.EqualValues(t, 5, descriptor.ByName("attributes").Number())
	require.EqualValues(t, 8, descriptor.ByName("proto_data").Number())
}

func TestCDCOperationPresenceRules(t *testing.T) {
	t.Parallel()

	after := cdcRecord(cdcField("id", cdcInt64(1)))
	key := cdcRecord(cdcField("id", cdcInt64(1)))
	collection := &cdcv1.DataCollection{Id: "inventory.customers"}
	message := &cdcv1.Value{Kind: &cdcv1.Value_BytesValue{BytesValue: []byte("logical message")}}
	common := func(operation cdcv1.Operation) *cdcv1.ChangeRecord {
		return &cdcv1.ChangeRecord{
			Operation:      operation,
			DataCollection: collection,
			SourcePosition: &cdcv1.SourcePosition{Stream: "source-0", Value: []byte{1}},
			SourceTime:     timestamppb.Now(),
			CaptureTime:    timestamppb.Now(),
		}
	}

	tests := []struct {
		name    string
		mutate  func(*cdcv1.ChangeRecord)
		wantErr string
	}{
		{name: "create", mutate: func(record *cdcv1.ChangeRecord) { record.After = after }},
		{name: "create without after", wantErr: "CREATE requires after"},
		{name: "create without data collection", mutate: func(record *cdcv1.ChangeRecord) {
			record.After = after
			record.DataCollection = nil
		}, wantErr: "data_collection is required"},
		{name: "create without capture time", mutate: func(record *cdcv1.ChangeRecord) {
			record.After = after
			record.CaptureTime = nil
		}, wantErr: "capture_time is required"},
		{name: "present field with unset value kind", mutate: func(record *cdcv1.ChangeRecord) {
			record.After = cdcRecord(cdcField("id", &cdcv1.Value{}))
		}, wantErr: "after.id value kind must be specified"},
		{name: "snapshot", mutate: func(record *cdcv1.ChangeRecord) {
			record.Operation = cdcv1.Operation_OPERATION_SNAPSHOT_READ
			record.After = after
		}},
		{name: "snapshot without after", mutate: func(record *cdcv1.ChangeRecord) { record.Operation = cdcv1.Operation_OPERATION_SNAPSHOT_READ }, wantErr: "SNAPSHOT_READ requires after"},
		{name: "update with optional before absent", mutate: func(record *cdcv1.ChangeRecord) {
			record.Operation = cdcv1.Operation_OPERATION_UPDATE
			record.After = after
		}},
		{name: "update without after", mutate: func(record *cdcv1.ChangeRecord) { record.Operation = cdcv1.Operation_OPERATION_UPDATE }, wantErr: "UPDATE requires after"},
		{name: "delete with key and optional before", mutate: func(record *cdcv1.ChangeRecord) {
			record.Operation = cdcv1.Operation_OPERATION_DELETE
			record.Key = key
			record.Before = after
		}},
		{name: "delete with after", mutate: func(record *cdcv1.ChangeRecord) {
			record.Operation = cdcv1.Operation_OPERATION_DELETE
			record.After = after
		}, wantErr: "DELETE prohibits after"},
		{name: "truncate", mutate: func(record *cdcv1.ChangeRecord) { record.Operation = cdcv1.Operation_OPERATION_TRUNCATE }},
		{name: "truncate with key", mutate: func(record *cdcv1.ChangeRecord) {
			record.Operation = cdcv1.Operation_OPERATION_TRUNCATE
			record.Key = key
		}, wantErr: "TRUNCATE prohibits row data"},
		{name: "source message", mutate: func(record *cdcv1.ChangeRecord) {
			record.Operation = cdcv1.Operation_OPERATION_SOURCE_MESSAGE
			record.DataCollection = nil
			record.SourceMessage = message
		}},
		{name: "source message represented as row", mutate: func(record *cdcv1.ChangeRecord) {
			record.Operation = cdcv1.Operation_OPERATION_SOURCE_MESSAGE
			record.SourceMessage = message
			record.After = after
		}, wantErr: "SOURCE_MESSAGE prohibits row data"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			record := common(cdcv1.Operation_OPERATION_CREATE)
			if test.mutate != nil {
				test.mutate(record)
			}
			err := validateCDCRecord(record)
			if test.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.EqualError(t, err, test.wantErr)
			}
		})
	}
}

func TestCDCExactValuesAndForwardCompatibility(t *testing.T) {
	t.Parallel()

	precision := uint32(38)
	record := &cdcv1.ChangeRecord{
		Operation: cdcv1.Operation_OPERATION_UPDATE,
		After: cdcRecord(
			cdcField("explicit_null", &cdcv1.Value{Kind: &cdcv1.Value_NullValue{NullValue: &cdcv1.NullValue{}}}),
			cdcField("unsigned_max", &cdcv1.Value{Kind: &cdcv1.Value_Uint64Value{Uint64Value: ^uint64(0)}}),
			cdcField("amount", &cdcv1.Value{
				TypeName: "org.apache.kafka.connect.data.Decimal",
				Kind: &cdcv1.Value_DecimalValue{DecimalValue: &cdcv1.DecimalValue{
					Value:     "12345678901234567890.123400",
					Scale:     6,
					Precision: &precision,
				}},
			}),
			cdcField("binary", &cdcv1.Value{Kind: &cdcv1.Value_BytesValue{BytesValue: []byte{0x00, 0x7f, 0x80, 0xff}}}),
			cdcField("occurred_at", &cdcv1.Value{
				TypeName: "io.debezium.time.NanoTimestamp",
				Kind: &cdcv1.Value_TimestampValue{TimestampValue: &timestamppb.Timestamp{
					Seconds: 1_723_912_200,
					Nanos:   987654321,
				}},
			}),
			cdcField("tags", &cdcv1.Value{Kind: &cdcv1.Value_ListValue{ListValue: &cdcv1.ListValue{Values: []*cdcv1.Value{
				cdcString("new"),
				{Kind: &cdcv1.Value_NullValue{NullValue: &cdcv1.NullValue{}}},
			}}}}),
			cdcField("address", &cdcv1.Value{Kind: &cdcv1.Value_RecordValue{RecordValue: cdcRecord(
				cdcField("city", cdcString("Oslo")),
			)}}),
		),
	}

	// "not_captured" is absent because no RecordField carries that name; the
	// explicit-null field remains present with the null oneof selected.
	require.Nil(t, cdcFieldByName(record.GetAfter(), "not_captured"))
	require.NotNil(t, cdcFieldByName(record.GetAfter(), "explicit_null").GetValue().GetNullValue())

	wire, err := proto.Marshal(record)
	require.NoError(t, err)
	wire = protowire.AppendTag(wire, 1000, protowire.BytesType)
	wire = protowire.AppendString(wire, "future ChangeRecord field")

	var oldReader cdcv1.ChangeRecord
	require.NoError(t, proto.Unmarshal(wire, &oldReader))
	require.Equal(t, ^uint64(0), cdcFieldByName(oldReader.GetAfter(), "unsigned_max").GetValue().GetUint64Value())
	require.Equal(t, "12345678901234567890.123400", cdcFieldByName(oldReader.GetAfter(), "amount").GetValue().GetDecimalValue().GetValue())
	require.EqualValues(t, 987654321, cdcFieldByName(oldReader.GetAfter(), "occurred_at").GetValue().GetTimestampValue().GetNanos())
	require.NotEmpty(t, oldReader.ProtoReflect().GetUnknown())

	roundTrip, err := proto.Marshal(&oldReader)
	require.NoError(t, err)
	var relayed cdcv1.ChangeRecord
	require.NoError(t, proto.Unmarshal(roundTrip, &relayed))
	require.Equal(t, oldReader.ProtoReflect().GetUnknown(), relayed.ProtoReflect().GetUnknown())

	// Proto enum values are open on the wire. An older relay need not interpret
	// a future operation, but it must retain its numeric value.
	futureOperation := &cdcv1.ChangeRecord{Operation: cdcv1.Operation(77)}
	futureWire, err := proto.Marshal(futureOperation)
	require.NoError(t, err)
	var futureRelayed cdcv1.ChangeRecord
	require.NoError(t, proto.Unmarshal(futureWire, &futureRelayed))
	require.Equal(t, cdcv1.Operation(77), futureRelayed.GetOperation())
}

func TestDebeziumCDCProfileFixtures(t *testing.T) {
	t.Parallel()

	fixtureDir := cdcFixtureDir(t)
	manifestBytes, err := os.ReadFile(filepath.Join(fixtureDir, "manifest.json"))
	require.NoError(t, err)
	var manifest struct {
		DebeziumVersion             string `json:"debezium_version"`
		CloudEventsSpecification    string `json:"cloudevents_specification_version"`
		CloudEventsEventSpecVersion string `json:"cloudevents_event_specversion"`
		Fixtures                    []struct {
			Path               string  `json:"path"`
			KeyPath            string  `json:"key_path"`
			Category           string  `json:"category"`
			Operation          *string `json:"operation"`
			CanonicalOperation *string `json:"canonical_operation"`
			RetryOf            string  `json:"retry_of"`
		} `json:"fixtures"`
	}
	require.NoError(t, json.Unmarshal(manifestBytes, &manifest))
	require.Equal(t, "3.6.1.Final", manifest.DebeziumVersion)
	require.Equal(t, "1.0.2", manifest.CloudEventsSpecification)
	require.Equal(t, "1.0", manifest.CloudEventsEventSpecVersion)
	require.Len(t, manifest.Fixtures, 13)

	operations := map[string]cdcv1.Operation{
		"c": cdcv1.Operation_OPERATION_CREATE,
		"u": cdcv1.Operation_OPERATION_UPDATE,
		"d": cdcv1.Operation_OPERATION_DELETE,
		"r": cdcv1.Operation_OPERATION_SNAPSHOT_READ,
		"t": cdcv1.Operation_OPERATION_TRUNCATE,
		"m": cdcv1.Operation_OPERATION_SOURCE_MESSAGE,
	}
	seenOperations := make(map[string]bool)
	seenAuxiliary := make(map[string]bool)
	for _, fixture := range manifest.Fixtures {
		t.Run(fixture.Path, func(t *testing.T) {
			content, readErr := os.ReadFile(filepath.Join(fixtureDir, fixture.Path))
			require.NoError(t, readErr)
			decoded := cdcDecodeJSON(t, content)

			if fixture.Category != "data_change" {
				seenAuxiliary[fixture.Category] = true
				require.Nil(t, fixture.Operation)
				require.Nil(t, fixture.CanonicalOperation)
				assertAuxiliaryIsNotRowChange(t, fixture.Category, decoded)
				return
			}

			require.NotNil(t, fixture.Operation)
			operation, ok := operations[*fixture.Operation]
			require.True(t, ok)
			seenOperations[*fixture.Operation] = true
			require.Equal(t, operation.String()[len("OPERATION_"):], *fixture.CanonicalOperation)

			if strings.HasPrefix(fixture.Path, "structured-cloudevent") {
				require.NotEmpty(t, fixture.KeyPath)
				keyBytes, keyErr := os.ReadFile(filepath.Join(fixtureDir, fixture.KeyPath))
				require.NoError(t, keyErr)
				key := cdcDecodeJSON(t, keyBytes)
				require.Equal(t, json.Number("1002"), key["id"])
				require.Equal(t, "1.0", decoded["specversion"])
				require.Equal(t, *fixture.Operation, decoded["iodebeziumop"])
				require.NotEmpty(t, decoded["id"])
				require.NotEmpty(t, decoded["source"])
				require.Contains(t, decoded, "dataschema")
				require.Contains(t, decoded, "iodebeziumfuturetoken")
				data, dataOK := decoded["data"].(map[string]any)
				require.True(t, dataOK)
				require.NotNil(t, data["after"])
				return
			}

			capture, captureOK := decoded["value"].(map[string]any)
			require.True(t, captureOK)
			payload := cdcPayload(capture)
			require.Equal(t, *fixture.Operation, payload["op"])
			assertDebeziumPresence(t, *fixture.Operation, decoded["key"], payload)
		})
	}

	for _, operation := range []string{"c", "u", "d", "r", "t", "m"} {
		require.True(t, seenOperations[operation], "missing operation %s", operation)
	}
	for _, category := range []string{"kafka_tombstone", "heartbeat", "schema_change", "transaction_boundary"} {
		require.True(t, seenAuxiliary[category], "missing auxiliary category %s", category)
	}

	original, err := os.ReadFile(filepath.Join(fixtureDir, "structured-cloudevent-snapshot.json"))
	require.NoError(t, err)
	retry, err := os.ReadFile(filepath.Join(fixtureDir, "structured-cloudevent-snapshot-retry.json"))
	require.NoError(t, err)
	require.Equal(t, original, retry)
	originalEvent := cdcDecodeJSON(t, original)
	retryEvent := cdcDecodeJSON(t, retry)
	require.Equal(t, originalEvent["source"], retryEvent["source"])
	require.Equal(t, originalEvent["id"], retryEvent["id"])

	createBytes, err := os.ReadFile(filepath.Join(fixtureDir, "native-create-schemaful.json"))
	require.NoError(t, err)
	create := cdcDecodeJSON(t, createBytes)
	value := create["value"].(map[string]any)
	payload := cdcPayload(value)
	after := payload["after"].(map[string]any)
	require.Equal(t, json.Number("9007199254740993"), after["profile"].(map[string]any)["score"])
	require.Nil(t, after["nickname"])
	require.NotContains(t, after, "middle_name")
	require.Equal(t, []byte{0x00, 0xff, 0x10}, cdcDecodeBase64(t, after["raw_payload"].(string)))
	require.Equal(t, []byte{0x01, 0xe2, 0x40}, cdcDecodeBase64(t, after["account_balance"].(string)))
	require.Equal(t, []byte{0x00, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, cdcDecodeBase64(t, after["unsigned_counter"].(string)))
	require.Contains(t, payload["source"].(map[string]any), "future_connector_metadata")
}

func TestDebeziumCanonicalSemanticRoundTrips(t *testing.T) {
	t.Parallel()

	fixtureDir := cdcFixtureDir(t)
	nativeFixtures := []struct {
		path      string
		operation cdcv1.Operation
	}{
		{path: "native-create-schemaful.json", operation: cdcv1.Operation_OPERATION_CREATE},
		{path: "native-update-schemaless.json", operation: cdcv1.Operation_OPERATION_UPDATE},
		{path: "native-delete-schemaless.json", operation: cdcv1.Operation_OPERATION_DELETE},
		{path: "native-snapshot-schemaless.json", operation: cdcv1.Operation_OPERATION_SNAPSHOT_READ},
		{path: "native-truncate-schemaless.json", operation: cdcv1.Operation_OPERATION_TRUNCATE},
		{path: "native-logical-message-schemaless.json", operation: cdcv1.Operation_OPERATION_SOURCE_MESSAGE},
	}
	for _, fixture := range nativeFixtures {
		t.Run(fixture.path, func(t *testing.T) {
			fixtureBytes, err := os.ReadFile(filepath.Join(fixtureDir, fixture.path))
			require.NoError(t, err)
			native := cdcDecodeJSON(t, fixtureBytes)

			canonical, err := cdcDebeziumToCanonical(native)
			require.NoError(t, err)
			require.Equal(t, fixture.operation, canonical.GetOperation())
			require.NoError(t, validateCDCRecord(canonical))

			backToNative, err := cdcCanonicalToDebezium(canonical)
			require.NoError(t, err)
			require.Equal(t, native, backToNative)

			canonicalAgain, err := cdcDebeziumToCanonical(backToNative)
			require.NoError(t, err)
			require.True(t, proto.Equal(canonical, canonicalAgain), "canonical: %s\nround trip: %s", canonical, canonicalAgain)

			checkpoint, err := cdcDecodeJSONObject(canonical.GetSourcePosition().GetValue())
			require.NoError(t, err)
			if fixture.operation == cdcv1.Operation_OPERATION_CREATE {
				require.Equal(t, "mysql-bin.000003", checkpoint["file"])
				require.Equal(t, json.Number("154"), checkpoint["pos"])
				require.Equal(t, json.Number("0"), checkpoint["row"])
				require.Equal(t, "3E11FA47-71CA-11E1-9E33-C80AA9429562:23", checkpoint["gtid"])
			}
			if fixture.operation == cdcv1.Operation_OPERATION_UPDATE {
				require.Equal(t, "556:24023128", canonical.GetTransaction().GetId())
				require.EqualValues(t, 2, canonical.GetTransaction().GetTotalOrder())
				require.EqualValues(t, 1, canonical.GetTransaction().GetDataCollectionOrder())
				extension := string(canonical.GetSourceExtension().GetOpaqueData().GetData())
				require.Contains(t, extension, "future_connector_metadata")
				require.Contains(t, extension, "future_envelope_metadata")
				require.Contains(t, extension, "future_transaction_metadata")
				require.Equal(t, json.Number("24023128"), checkpoint["lsn"])
				require.Equal(t, "[\"24023119\",\"24023128\"]", checkpoint["sequence"])
				require.Equal(t, json.Number("556"), checkpoint["txId"])
				require.Contains(t, checkpoint, "xmin")
			}
			if fixture.operation == cdcv1.Operation_OPERATION_SOURCE_MESSAGE {
				require.Nil(t, canonical.GetKey(), "a logical-message prefix is not a row key")
				message := canonical.GetSourceMessage().GetRecordValue()
				require.Equal(t, "fixture", cdcFieldByName(message, "prefix").GetValue().GetStringValue())
				require.Equal(t, []byte("hello-cdc"), cdcFieldByName(message, "content").GetValue().GetBytesValue())
				require.Equal(t, map[string]any{"prefix": "fixture"}, backToNative["key"])
			}
		})
	}

	t.Run("logical message rejects mismatched duplicate prefix", func(t *testing.T) {
		fixtureBytes, err := os.ReadFile(filepath.Join(fixtureDir, "native-logical-message-schemaless.json"))
		require.NoError(t, err)
		native := cdcDecodeJSON(t, fixtureBytes)
		native["key"].(map[string]any)["prefix"] = "different-prefix"
		_, err = cdcDebeziumToCanonical(native)
		require.EqualError(t, err, "debezium source message key and value prefixes differ")
	})

	t.Run("schemaful create exact domains and presence", func(t *testing.T) {
		fixtureBytes, err := os.ReadFile(filepath.Join(fixtureDir, "native-create-schemaful.json"))
		require.NoError(t, err)
		canonical, err := cdcDebeziumToCanonical(cdcDecodeJSON(t, fixtureBytes))
		require.NoError(t, err)

		require.EqualValues(t, 1001, cdcFieldByName(canonical.GetKey(), "id").GetValue().GetInt32Value())
		balance := cdcFieldByName(canonical.GetAfter(), "account_balance").GetValue()
		require.Equal(t, "org.apache.kafka.connect.data.Decimal", balance.GetTypeName())
		require.Equal(t, "1234.56", balance.GetDecimalValue().GetValue())
		require.EqualValues(t, 2, balance.GetDecimalValue().GetScale())
		require.EqualValues(t, 12, balance.GetDecimalValue().GetPrecision())

		unsigned := cdcFieldByName(canonical.GetAfter(), "unsigned_counter").GetValue()
		require.Equal(t, "BIGINT UNSIGNED", unsigned.GetTypeName())
		require.Equal(t, ^uint64(0), unsigned.GetUint64Value())
		require.Equal(t, []byte{0x00, 0xff, 0x10}, cdcFieldByName(canonical.GetAfter(), "raw_payload").GetValue().GetBytesValue())

		occurred := cdcFieldByName(canonical.GetAfter(), "occurred_at").GetValue()
		require.Equal(t, "io.debezium.time.MicroTimestamp", occurred.GetTypeName())
		require.Equal(t, int64(1_704_164_645), occurred.GetTimestampValue().GetSeconds())
		require.EqualValues(t, 123_456_000, occurred.GetTimestampValue().GetNanos())
		profile := cdcFieldByName(canonical.GetAfter(), "profile").GetValue().GetRecordValue()
		require.Equal(t, int64(9_007_199_254_740_993), cdcFieldByName(profile, "score").GetValue().GetInt64Value())
		require.NotNil(t, cdcFieldByName(canonical.GetAfter(), "nickname").GetValue().GetNullValue())
		require.Nil(t, cdcFieldByName(canonical.GetAfter(), "middle_name"))
	})

	t.Run("structured CloudEvent with separate key", func(t *testing.T) {
		eventBytes, err := os.ReadFile(filepath.Join(fixtureDir, "structured-cloudevent-snapshot.json"))
		require.NoError(t, err)
		keyBytes, err := os.ReadFile(filepath.Join(fixtureDir, "structured-cloudevent-snapshot-key.json"))
		require.NoError(t, err)
		event := cdcDecodeJSON(t, eventBytes)
		key := cdcDecodeJSON(t, keyBytes)

		captureTime := cdcTimestampFromNanos(1_704_164_600_123_456_789)
		canonicalEvent, err := cdcStructuredToCanonicalCloudEvent(event, key, captureTime)
		require.NoError(t, err)
		require.Equal(t, event["source"], canonicalEvent.GetSource())
		require.Equal(t, event["id"], canonicalEvent.GetId())
		require.Equal(t, "1.0", canonicalEvent.GetSpecVersion())
		require.Equal(t, cdcEventType, canonicalEvent.GetType())
		require.Equal(t, "application/protobuf", canonicalEvent.GetAttributes()["datacontenttype"].GetCeString())
		require.Equal(t, cdcDataSchema, canonicalEvent.GetAttributes()["dataschema"].GetCeUri())
		require.Equal(t, cdcDataSchema, canonicalEvent.GetProtoData().GetTypeUrl())
		var canonical cdcv1.ChangeRecord
		require.NoError(t, canonicalEvent.GetProtoData().UnmarshalTo(&canonical))
		require.Equal(t, cdcv1.Operation_OPERATION_SNAPSHOT_READ, canonical.GetOperation())
		require.EqualValues(t, 1002, cdcFieldByName(canonical.GetKey(), "id").GetValue().GetInt64Value())
		require.Equal(t, "550:24023000", canonical.GetTransaction().GetId())
		require.Contains(t, string(canonical.GetSourceExtension().GetOpaqueData().GetData()), "iodebeziumfuturetoken")
		require.True(t, proto.Equal(captureTime, canonical.GetCaptureTime()))
		require.True(t, proto.Equal(canonical.GetSourceTime(), canonicalEvent.GetAttributes()["time"].GetCeTimestamp()))

		eventAgain, keyAgain, err := cdcCanonicalToStructured(&canonical)
		require.NoError(t, err)
		require.Equal(t, event, eventAgain)
		require.Equal(t, key, keyAgain)
		canonicalAgain, err := cdcStructuredToCanonical(eventAgain, keyAgain, captureTime)
		require.NoError(t, err)
		require.True(t, proto.Equal(&canonical, canonicalAgain), "canonical: %s\nround trip: %s", &canonical, canonicalAgain)

		retryBytes, err := os.ReadFile(filepath.Join(fixtureDir, "structured-cloudevent-snapshot-retry.json"))
		require.NoError(t, err)
		retryEvent, err := cdcStructuredToCanonicalCloudEvent(cdcDecodeJSON(t, retryBytes), key, captureTime)
		require.NoError(t, err)
		require.Equal(t, canonicalEvent.GetSource(), retryEvent.GetSource())
		require.Equal(t, canonicalEvent.GetId(), retryEvent.GetId())

		withoutSourceTime := maps.Clone(event)
		delete(withoutSourceTime, "time")
		delete(withoutSourceTime, "iodebeziumtsms")
		fallbackEvent, err := cdcStructuredToCanonicalCloudEvent(withoutSourceTime, key, captureTime)
		require.NoError(t, err)
		require.True(t, proto.Equal(captureTime, fallbackEvent.GetAttributes()["time"].GetCeTimestamp()))

		tamperedCanonical := proto.Clone(&canonical).(*cdcv1.ChangeRecord)
		tamperedCanonical.Operation = cdcv1.Operation_OPERATION_UPDATE
		tamperedEvent, _, err := cdcCanonicalToStructured(tamperedCanonical)
		require.NoError(t, err)
		require.Equal(t, "u", tamperedEvent["iodebeziumop"], "canonical operation must override preserved input metadata")
	})

	metadata := cdcDebeziumMetadata{
		Source: map[string]any{
			"version":   "3.6.1.Final",
			"connector": "postgresql",
			"name":      "example_postgres",
			"db":        "inventory",
			"schema":    "public",
			"table":     "customers",
			"lsn":       json.Number("24023129"),
			"ts_ns":     json.Number("1704164647000123456"),
			"future":    map[string]any{"token": "opaque"},
		},
		Capture: map[string]any{
			"ts_ms": json.Number("1704164647123"),
			"ts_us": json.Number("1704164647123456"),
			"ts_ns": json.Number("1704164647123456789"),
		},
	}
	metadataBytes, err := json.Marshal(metadata)
	require.NoError(t, err)
	totalOrder := uint64(3)
	collectionOrder := uint64(2)
	decimalPrecision := uint32(18)
	canonicalOriginal := &cdcv1.ChangeRecord{
		Operation: cdcv1.Operation_OPERATION_UPDATE,
		Key:       cdcRecord(cdcField("id", cdcInt64(1001))),
		Before:    cdcRecord(cdcField("display_name", cdcString("Ada"))),
		After: cdcRecord(
			cdcField("amount", &cdcv1.Value{TypeName: "example.Decimal", Kind: &cdcv1.Value_DecimalValue{DecimalValue: &cdcv1.DecimalValue{Value: "1234.5600", Scale: 4, Precision: &decimalPrecision}}}),
			cdcField("binary", &cdcv1.Value{TypeName: "example.Binary", Kind: &cdcv1.Value_BytesValue{BytesValue: []byte{0x00, 0x80, 0xff}}}),
			cdcField("display_name", cdcString("Ada Lovelace")),
			cdcField("float32", &cdcv1.Value{Kind: &cdcv1.Value_Float32Value{Float32Value: 1.25}}),
			cdcField("float64", &cdcv1.Value{Kind: &cdcv1.Value_Float64Value{Float64Value: 1.0 / 3.0}}),
			cdcField("instant", &cdcv1.Value{TypeName: "example.NanosecondInstant", Kind: &cdcv1.Value_TimestampValue{TimestampValue: cdcTimestampFromNanos(1_704_164_647_111_222_333)}}),
			cdcField("int32", &cdcv1.Value{Kind: &cdcv1.Value_Int32Value{Int32Value: -17}}),
			cdcField("labels", &cdcv1.Value{Kind: &cdcv1.Value_ListValue{ListValue: &cdcv1.ListValue{Values: []*cdcv1.Value{cdcString("a"), cdcString("b")}}}}),
			cdcField("map", &cdcv1.Value{Kind: &cdcv1.Value_MapValue{MapValue: &cdcv1.MapValue{Entries: []*cdcv1.MapEntry{
				{Key: cdcString("attempts"), Value: &cdcv1.Value{Kind: &cdcv1.Value_Uint32Value{Uint32Value: 3}}},
			}}}}),
			cdcField("nested", &cdcv1.Value{Kind: &cdcv1.Value_RecordValue{RecordValue: cdcRecord(
				cdcField("payload", &cdcv1.Value{Kind: &cdcv1.Value_BytesValue{BytesValue: []byte("nested")}}),
			)}}),
			cdcField("nullable", &cdcv1.Value{Kind: &cdcv1.Value_NullValue{NullValue: &cdcv1.NullValue{}}}),
			cdcField("revision", &cdcv1.Value{Kind: &cdcv1.Value_Uint64Value{Uint64Value: ^uint64(0)}}),
			cdcField("uint32", &cdcv1.Value{Kind: &cdcv1.Value_Uint32Value{Uint32Value: ^uint32(0)}}),
		),
		DataCollection: &cdcv1.DataCollection{Id: "inventory.public.customers"},
		SchemaReference: &cdcv1.SchemaReference{
			Uri: "urn:example:schema:customers", Version: "12", Fingerprint: []byte{0xde, 0xad, 0xbe, 0xef},
		},
		SourcePosition: &cdcv1.SourcePosition{Stream: "example_postgres", Format: "application/json", Value: []byte("24023129")},
		Transaction: &cdcv1.TransactionContext{
			Id:                  "557:24023129",
			TotalOrder:          &totalOrder,
			DataCollectionOrder: &collectionOrder,
		},
		SourceTime:  cdcTimestampFromNanos(1_704_164_647_000_123_456),
		CaptureTime: cdcTimestampFromNanos(1_704_164_647_123_456_789),
		ChangedFields: &cdcv1.ChangedFieldMask{Paths: []*cdcv1.FieldPath{
			{Segments: []string{"display_name"}}, {Segments: []string{"nested", "payload"}},
		}},
		SourceExtension: &cdcv1.SourceExtension{Representation: &cdcv1.SourceExtension_OpaqueData{
			OpaqueData: &cdcv1.OpaqueData{
				MediaType: "application/json",
				Schema:    "urn:invariant:cdc:profile:debezium:3.6.1.Final:metadata",
				Data:      metadataBytes,
			},
		}},
	}

	nativeFromCanonical, err := cdcCanonicalToDebezium(canonicalOriginal)
	require.NoError(t, err)
	nativePayload := nativeFromCanonical["value"].(map[string]any)
	adapterContext := nativePayload["__invariant_adapter_context"].(map[string]any)
	contextJSON, err := json.Marshal(adapterContext)
	require.NoError(t, err)
	require.NotContains(t, string(contextJSON), "Ada Lovelace", "adapter context describes types and canonical-only fields, not record values")
	canonicalAgain, err := cdcDebeziumToCanonical(nativeFromCanonical)
	require.NoError(t, err)
	require.True(t, proto.Equal(canonicalOriginal, canonicalAgain), "original: %s\nround trip: %s", canonicalOriginal, canonicalAgain)

	staleSourceTime := proto.Clone(canonicalOriginal).(*cdcv1.ChangeRecord)
	staleSourceTime.SourceTime = timestamppb.New(staleSourceTime.GetSourceTime().AsTime().Add(time.Nanosecond))
	_, err = cdcCanonicalToDebezium(staleSourceTime)
	require.EqualError(t, err, "canonical source_time disagrees with preserved Debezium timestamp fields")
	staleCaptureTime := proto.Clone(canonicalOriginal).(*cdcv1.ChangeRecord)
	staleCaptureTime.CaptureTime = timestamppb.New(staleCaptureTime.GetCaptureTime().AsTime().Add(time.Nanosecond))
	_, err = cdcCanonicalToDebezium(staleCaptureTime)
	require.EqualError(t, err, "canonical capture_time disagrees with preserved Debezium timestamp fields")

	richMessage := &cdcv1.ChangeRecord{
		Operation:      cdcv1.Operation_OPERATION_SOURCE_MESSAGE,
		SourcePosition: cdcDebeziumSourcePosition(metadata.Source),
		SourceTime:     cdcTimestampFromNanos(1_704_164_647_000_123_456),
		CaptureTime:    cdcTimestampFromNanos(1_704_164_647_123_456_789),
		SourceExtension: &cdcv1.SourceExtension{Representation: &cdcv1.SourceExtension_OpaqueData{
			OpaqueData: &cdcv1.OpaqueData{
				MediaType: "application/json",
				Schema:    "urn:invariant:cdc:profile:debezium:3.6.1.Final:metadata",
				Data:      metadataBytes,
			},
		}},
		SourceMessage: &cdcv1.Value{
			TypeName: "example.LogicalMessage",
			Kind: &cdcv1.Value_RecordValue{RecordValue: cdcRecord(
				cdcField("content", &cdcv1.Value{TypeName: "example.MessageBytes", Kind: &cdcv1.Value_BytesValue{BytesValue: []byte("rich-cdc")}}),
				cdcField("prefix", &cdcv1.Value{TypeName: "example.MessagePrefix", Kind: &cdcv1.Value_StringValue{StringValue: "rich"}}),
			)},
		},
	}
	richNative, err := cdcCanonicalToDebezium(richMessage)
	require.NoError(t, err)
	require.Equal(t, map[string]any{"prefix": "rich"}, richNative["key"])
	richAgain, err := cdcDebeziumToCanonical(richNative)
	require.NoError(t, err)
	require.True(t, proto.Equal(richMessage, richAgain), "original: %s\nround trip: %s", richMessage, richAgain)
	richNativeAgain, err := cdcCanonicalToDebezium(richAgain)
	require.NoError(t, err)
	require.Equal(t, richNative, richNativeAgain)
}

func validateCDCRecord(record *cdcv1.ChangeRecord) error {
	if record.GetOperation() != cdcv1.Operation_OPERATION_SOURCE_MESSAGE && record.GetDataCollection() == nil {
		return errCDC("data_collection is required")
	}
	if record.GetCaptureTime() == nil {
		return errCDC("capture_time is required")
	}
	for name, image := range map[string]*cdcv1.Record{
		"key": record.GetKey(), "before": record.GetBefore(), "after": record.GetAfter(),
	} {
		if err := validateCDCRecordValues(name, image); err != nil {
			return err
		}
	}
	if record.GetSourceMessage() != nil && record.GetSourceMessage().GetKind() == nil {
		return errCDC("source_message value kind must be specified")
	}
	if sourceMessage := record.GetSourceMessage().GetRecordValue(); sourceMessage != nil {
		if err := validateCDCRecordValues("source_message", sourceMessage); err != nil {
			return err
		}
	}
	switch record.GetOperation() {
	case cdcv1.Operation_OPERATION_CREATE:
		if record.GetAfter() == nil {
			return errCDC("CREATE requires after")
		}
		if record.GetSourceMessage() != nil {
			return errCDC("CREATE prohibits source_message")
		}
	case cdcv1.Operation_OPERATION_SNAPSHOT_READ:
		if record.GetAfter() == nil {
			return errCDC("SNAPSHOT_READ requires after")
		}
		if record.GetSourceMessage() != nil {
			return errCDC("SNAPSHOT_READ prohibits source_message")
		}
	case cdcv1.Operation_OPERATION_UPDATE:
		if record.GetAfter() == nil {
			return errCDC("UPDATE requires after")
		}
		if record.GetSourceMessage() != nil {
			return errCDC("UPDATE prohibits source_message")
		}
	case cdcv1.Operation_OPERATION_DELETE:
		if record.GetAfter() != nil {
			return errCDC("DELETE prohibits after")
		}
		if record.GetSourceMessage() != nil {
			return errCDC("DELETE prohibits source_message")
		}
	case cdcv1.Operation_OPERATION_TRUNCATE:
		if record.GetDataCollection() == nil {
			return errCDC("TRUNCATE requires data_collection")
		}
		if record.GetKey() != nil || record.GetBefore() != nil || record.GetAfter() != nil || record.GetSourceMessage() != nil {
			return errCDC("TRUNCATE prohibits row data")
		}
	case cdcv1.Operation_OPERATION_SOURCE_MESSAGE:
		if record.GetSourceMessage() == nil {
			return errCDC("SOURCE_MESSAGE requires source_message")
		}
		if record.GetKey() != nil || record.GetBefore() != nil || record.GetAfter() != nil {
			return errCDC("SOURCE_MESSAGE prohibits row data")
		}
	default:
		return errCDC("operation must be specified")
	}
	return nil
}

func validateCDCRecordValues(path string, record *cdcv1.Record) error {
	for _, field := range record.GetFields() {
		fieldPath := path + "." + field.GetName()
		value := field.GetValue()
		if value == nil || value.GetKind() == nil {
			return errCDC(fieldPath + " value kind must be specified")
		}
		if nested := value.GetRecordValue(); nested != nil {
			if err := validateCDCRecordValues(fieldPath, nested); err != nil {
				return err
			}
		}
	}
	return nil
}

type cdcValidationError string

func (err cdcValidationError) Error() string { return string(err) }

func errCDC(message string) error { return cdcValidationError(message) }

func cdcRecord(fields ...*cdcv1.RecordField) *cdcv1.Record {
	return &cdcv1.Record{Fields: fields}
}

func cdcField(name string, value *cdcv1.Value) *cdcv1.RecordField {
	return &cdcv1.RecordField{Name: name, Value: value}
}

func cdcFieldByName(record *cdcv1.Record, name string) *cdcv1.RecordField {
	for _, field := range record.GetFields() {
		if field.GetName() == name {
			return field
		}
	}
	return nil
}

func cdcInt64(value int64) *cdcv1.Value {
	return &cdcv1.Value{Kind: &cdcv1.Value_Int64Value{Int64Value: value}}
}

func cdcString(value string) *cdcv1.Value {
	return &cdcv1.Value{Kind: &cdcv1.Value_StringValue{StringValue: value}}
}

func cdcStringAttribute(value string) *cloudeventsv1.CloudEvent_CloudEventAttributeValue {
	return &cloudeventsv1.CloudEvent_CloudEventAttributeValue{
		Attr: &cloudeventsv1.CloudEvent_CloudEventAttributeValue_CeString{CeString: value},
	}
}

func cdcURIAttribute(value string) *cloudeventsv1.CloudEvent_CloudEventAttributeValue {
	return &cloudeventsv1.CloudEvent_CloudEventAttributeValue{
		Attr: &cloudeventsv1.CloudEvent_CloudEventAttributeValue_CeUri{CeUri: value},
	}
}

func cdcTimestampAttribute(value *timestamppb.Timestamp) *cloudeventsv1.CloudEvent_CloudEventAttributeValue {
	return &cloudeventsv1.CloudEvent_CloudEventAttributeValue{
		Attr: &cloudeventsv1.CloudEvent_CloudEventAttributeValue_CeTimestamp{CeTimestamp: value},
	}
}

func cdcFixtureDir(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	require.True(t, ok)
	return filepath.Join(filepath.Dir(filename), "..", "testdata", "cdc", "debezium", "3.6.1.Final")
}

func cdcDecodeJSON(t *testing.T, data []byte) map[string]any {
	t.Helper()
	decoded, err := cdcDecodeJSONObject(data)
	require.NoError(t, err)
	return decoded
}

func cdcDecodeJSONObject(data []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var decoded map[string]any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}

func cdcPayload(value map[string]any) map[string]any {
	if payload, ok := value["payload"].(map[string]any); ok {
		return payload
	}
	return value
}

func cdcDecodeBase64(t *testing.T, value string) []byte {
	t.Helper()
	decoded, err := base64.StdEncoding.DecodeString(value)
	require.NoError(t, err)
	return decoded
}

func assertDebeziumPresence(t *testing.T, operation string, key any, payload map[string]any) {
	t.Helper()
	switch operation {
	case "c", "u", "r":
		require.NotNil(t, payload["after"])
	case "d":
		require.NotNil(t, key)
		after, present := payload["after"]
		require.True(t, present)
		require.Nil(t, after)
	case "t":
		require.Nil(t, key)
		require.NotEmpty(t, payload["source"].(map[string]any)["table"])
		require.NotContains(t, payload, "before")
		require.NotContains(t, payload, "after")
	case "m":
		require.NotNil(t, payload["message"])
		require.NotContains(t, payload, "before")
		require.NotContains(t, payload, "after")
	default:
		t.Fatalf("unexpected Debezium operation %q", operation)
	}
}

func assertAuxiliaryIsNotRowChange(t *testing.T, category string, fixture map[string]any) {
	t.Helper()
	if category == "kafka_tombstone" {
		value, present := fixture["value"]
		require.True(t, present)
		require.Nil(t, value)
		return
	}
	if value, ok := fixture["value"].(map[string]any); ok {
		require.NotContains(t, cdcPayload(value), "op")
	} else {
		require.NotContains(t, fixture, "op")
	}
}

type cdcDebeziumMetadata struct {
	Source         map[string]any `json:"source,omitempty"`
	Capture        map[string]any `json:"capture,omitempty"`
	KeySchema      map[string]any `json:"key_schema,omitempty"`
	ValueSchema    map[string]any `json:"value_schema,omitempty"`
	Envelope       map[string]any `json:"envelope,omitempty"`
	Transaction    map[string]any `json:"transaction,omitempty"`
	NativeEnvelope map[string]any `json:"native_envelope,omitempty"`
	CloudEvent     map[string]any `json:"cloud_event,omitempty"`
}

func cdcDebeziumToCanonical(native map[string]any) (*cdcv1.ChangeRecord, error) {
	value, ok := native["value"].(map[string]any)
	if !ok {
		return nil, errors.New("debezium value is not an object")
	}
	payload := cdcPayload(value)
	op, ok := payload["op"].(string)
	if !ok {
		return nil, errors.New("debezium payload has no operation")
	}
	operations := map[string]cdcv1.Operation{
		"c": cdcv1.Operation_OPERATION_CREATE,
		"u": cdcv1.Operation_OPERATION_UPDATE,
		"d": cdcv1.Operation_OPERATION_DELETE,
		"r": cdcv1.Operation_OPERATION_SNAPSHOT_READ,
		"t": cdcv1.Operation_OPERATION_TRUNCATE,
		"m": cdcv1.Operation_OPERATION_SOURCE_MESSAGE,
	}
	operation, ok := operations[op]
	if !ok {
		return nil, fmt.Errorf("unsupported Debezium operation %q", op)
	}
	source, ok := payload["source"].(map[string]any)
	if !ok {
		return nil, errors.New("debezium payload has no source object")
	}
	capture := make(map[string]any)
	for _, name := range []string{"ts_ms", "ts_us", "ts_ns"} {
		if value, present := payload[name]; present {
			capture[name] = value
		}
	}
	adapterContext, _ := payload["__invariant_adapter_context"].(map[string]any)
	envelope := maps.Clone(payload)
	for _, name := range []string{"before", "after", "source", "op", "ts_ms", "ts_us", "ts_ns", "transaction", "message", "__invariant_adapter_context"} {
		delete(envelope, name)
	}
	transactionMetadata := make(map[string]any)
	if transaction, present := payload["transaction"].(map[string]any); present {
		transactionMetadata = maps.Clone(transaction)
		for _, name := range []string{"id", "total_order", "data_collection_order"} {
			delete(transactionMetadata, name)
		}
	}
	nativeEnvelope := maps.Clone(native)
	delete(nativeEnvelope, "key")
	delete(nativeEnvelope, "value")
	metadata := cdcDebeziumMetadata{
		Source: source, Capture: capture, Envelope: envelope,
		Transaction: transactionMetadata, NativeEnvelope: nativeEnvelope,
	}
	metadata.ValueSchema, _ = value["schema"].(map[string]any)
	if key, keyOK := native["key"].(map[string]any); keyOK {
		metadata.KeySchema, _ = key["schema"].(map[string]any)
	}
	metadataBytes, err := json.Marshal(metadata)
	if err != nil {
		return nil, err
	}

	record := &cdcv1.ChangeRecord{
		Operation: operation,
		SourceExtension: &cdcv1.SourceExtension{Representation: &cdcv1.SourceExtension_OpaqueData{
			OpaqueData: &cdcv1.OpaqueData{
				MediaType: "application/json",
				Schema:    "urn:invariant:cdc:profile:debezium:3.6.1.Final:metadata",
				Data:      metadataBytes,
			},
		}},
	}
	if operation != cdcv1.Operation_OPERATION_SOURCE_MESSAGE {
		if key, keyOK := native["key"].(map[string]any); keyOK {
			if payload, payloadOK := key["payload"].(map[string]any); payloadOK {
				key = payload
			}
			if metadata.KeySchema == nil {
				record.Key = cdcAnyToRecordWithCompanion(key, cdcCompanionRecord(adapterContext, "key"))
			} else {
				record.Key, err = cdcConnectToRecord(key, metadata.KeySchema)
				if err != nil {
					return nil, err
				}
			}
		}
	}
	if before, beforeOK := payload["before"].(map[string]any); beforeOK {
		if metadata.ValueSchema == nil {
			record.Before = cdcAnyToRecordWithCompanion(before, cdcCompanionRecord(adapterContext, "before"))
		} else {
			record.Before, err = cdcConnectToRecord(before, cdcConnectFieldSchema(metadata.ValueSchema, "before"))
			if err != nil {
				return nil, err
			}
		}
	}
	if after, afterOK := payload["after"].(map[string]any); afterOK {
		if metadata.ValueSchema == nil {
			record.After = cdcAnyToRecordWithCompanion(after, cdcCompanionRecord(adapterContext, "after"))
		} else {
			record.After, err = cdcConnectToRecord(after, cdcConnectFieldSchema(metadata.ValueSchema, "after"))
			if err != nil {
				return nil, err
			}
		}
	}
	if operation == cdcv1.Operation_OPERATION_SOURCE_MESSAGE {
		if message, present := payload["message"]; present {
			if hint := cdcCompanionValue(adapterContext, "source_message"); hint != nil {
				record.SourceMessage = cdcAnyToValueWithCompanion(message, hint)
			} else {
				record.SourceMessage, err = cdcDebeziumMessageToValue(message)
				if err != nil {
					return nil, err
				}
			}
			messagePrefix := cdcFieldByName(record.GetSourceMessage().GetRecordValue(), "prefix")
			if nativeKey, keyPresent := native["key"].(map[string]any); keyPresent {
				if keyPayload, payloadPresent := nativeKey["payload"].(map[string]any); payloadPresent {
					nativeKey = keyPayload
				}
				keyPrefix, _ := nativeKey["prefix"].(string)
				if messagePrefix == nil || keyPrefix != messagePrefix.GetValue().GetStringValue() {
					return nil, errors.New("debezium source message key and value prefixes differ")
				}
			}
		}
	}

	collectionParts := make([]string, 0, 3)
	for _, name := range []string{"db", "schema", "table"} {
		if part, partOK := source[name].(string); partOK && part != "" {
			collectionParts = append(collectionParts, part)
		}
	}
	if len(collectionParts) > 0 && operation != cdcv1.Operation_OPERATION_SOURCE_MESSAGE {
		record.DataCollection = &cdcv1.DataCollection{Id: strings.Join(collectionParts, ".")}
	}
	if metadata.ValueSchema != nil {
		if schemaName, nameOK := metadata.ValueSchema["name"].(string); nameOK {
			record.SchemaReference = &cdcv1.SchemaReference{Uri: "urn:debezium:kafka-connect:" + schemaName}
			if version, present := metadata.ValueSchema["version"]; present {
				record.SchemaReference.Version = cdcJSONScalar(version)
			}
		}
	}
	record.SourcePosition = cdcDebeziumSourcePosition(source)
	timestamp, timestampErr := cdcTimestampFromDebezium(source)
	if timestampErr != nil {
		return nil, timestampErr
	}
	record.SourceTime = timestamp
	timestamp, timestampErr = cdcTimestampFromDebezium(capture)
	if timestampErr != nil {
		return nil, timestampErr
	}
	record.CaptureTime = timestamp
	if transaction, transactionOK := payload["transaction"].(map[string]any); transactionOK {
		context := &cdcv1.TransactionContext{}
		context.Id, _ = transaction["id"].(string)
		if order, present := transaction["total_order"]; present {
			parsed, parseErr := strconv.ParseUint(cdcJSONScalar(order), 10, 64)
			if parseErr != nil {
				return nil, parseErr
			}
			context.TotalOrder = &parsed
		}
		if order, present := transaction["data_collection_order"]; present {
			parsed, parseErr := strconv.ParseUint(cdcJSONScalar(order), 10, 64)
			if parseErr != nil {
				return nil, parseErr
			}
			context.DataCollectionOrder = &parsed
		}
		record.Transaction = context
	}
	if adapterContext != nil {
		if err := cdcApplyAdapterContext(record, adapterContext); err != nil {
			return nil, err
		}
	}
	return record, nil
}

func cdcCanonicalToDebezium(record *cdcv1.ChangeRecord) (map[string]any, error) {
	extension := record.GetSourceExtension().GetOpaqueData()
	if extension == nil || extension.GetMediaType() != "application/json" {
		return nil, errors.New("debezium metadata extension is missing")
	}
	metadataObject, err := cdcDecodeJSONObject(extension.GetData())
	if err != nil {
		return nil, err
	}
	storedSource, ok := metadataObject["source"].(map[string]any)
	if !ok {
		return nil, errors.New("debezium metadata source is missing")
	}
	source := maps.Clone(storedSource)
	capture, _ := metadataObject["capture"].(map[string]any)
	keySchema, _ := metadataObject["key_schema"].(map[string]any)
	valueSchema, _ := metadataObject["value_schema"].(map[string]any)
	envelope, _ := metadataObject["envelope"].(map[string]any)
	transactionMetadata, _ := metadataObject["transaction"].(map[string]any)
	nativeEnvelope, _ := metadataObject["native_envelope"].(map[string]any)
	if err := cdcValidateTimestampAgreement("source_time", record.GetSourceTime(), source); err != nil {
		return nil, err
	}
	if err := cdcValidateTimestampAgreement("capture_time", record.GetCaptureTime(), capture); err != nil {
		return nil, err
	}
	operations := map[cdcv1.Operation]string{
		cdcv1.Operation_OPERATION_CREATE:         "c",
		cdcv1.Operation_OPERATION_UPDATE:         "u",
		cdcv1.Operation_OPERATION_DELETE:         "d",
		cdcv1.Operation_OPERATION_SNAPSHOT_READ:  "r",
		cdcv1.Operation_OPERATION_TRUNCATE:       "t",
		cdcv1.Operation_OPERATION_SOURCE_MESSAGE: "m",
	}
	op, ok := operations[record.GetOperation()]
	if !ok {
		return nil, fmt.Errorf("unsupported canonical operation %s", record.GetOperation())
	}
	payload := maps.Clone(envelope)
	if payload == nil {
		payload = make(map[string]any)
	}
	payload["source"] = source
	payload["op"] = op
	maps.Copy(payload, capture)
	switch record.GetOperation() {
	case cdcv1.Operation_OPERATION_CREATE, cdcv1.Operation_OPERATION_UPDATE, cdcv1.Operation_OPERATION_SNAPSHOT_READ:
		if record.GetBefore() == nil {
			payload["before"] = nil
		} else if valueSchema != nil {
			payload["before"], err = cdcRecordToConnectAny(record.GetBefore(), cdcConnectFieldSchema(valueSchema, "before"))
			if err != nil {
				return nil, err
			}
		} else {
			payload["before"] = cdcRecordToAny(record.GetBefore())
		}
		if valueSchema != nil {
			payload["after"], err = cdcRecordToConnectAny(record.GetAfter(), cdcConnectFieldSchema(valueSchema, "after"))
			if err != nil {
				return nil, err
			}
		} else {
			payload["after"] = cdcRecordToAny(record.GetAfter())
		}
	case cdcv1.Operation_OPERATION_DELETE:
		if record.GetBefore() == nil {
			payload["before"] = nil
		} else if valueSchema != nil {
			payload["before"], err = cdcRecordToConnectAny(record.GetBefore(), cdcConnectFieldSchema(valueSchema, "before"))
			if err != nil {
				return nil, err
			}
		} else {
			payload["before"] = cdcRecordToAny(record.GetBefore())
		}
		payload["after"] = nil
	case cdcv1.Operation_OPERATION_SOURCE_MESSAGE:
		payload["message"] = cdcValueToAny(record.GetSourceMessage())
	}
	if transaction := record.GetTransaction(); transaction != nil {
		mapped := maps.Clone(transactionMetadata)
		if mapped == nil {
			mapped = make(map[string]any)
		}
		mapped["id"] = transaction.GetId()
		if transaction.TotalOrder != nil {
			mapped["total_order"] = strconv.FormatUint(transaction.GetTotalOrder(), 10)
		}
		if transaction.DataCollectionOrder != nil {
			mapped["data_collection_order"] = strconv.FormatUint(transaction.GetDataCollectionOrder(), 10)
		}
		payload["transaction"] = mapped
	}
	if cdcNeedsAdapterContext(record, source, valueSchema) {
		payload["__invariant_adapter_context"] = cdcCanonicalAdapterContext(record)
	}
	var key any
	if record.GetOperation() == cdcv1.Operation_OPERATION_SOURCE_MESSAGE {
		message := record.GetSourceMessage().GetRecordValue()
		prefix := cdcFieldByName(message, "prefix")
		if prefix == nil || prefix.GetValue().GetStringValue() == "" {
			return nil, errors.New("debezium source message prefix is missing")
		}
		key = map[string]any{"prefix": prefix.GetValue().GetStringValue()}
	} else if record.GetKey() != nil {
		if keySchema != nil {
			keyPayload, keyErr := cdcRecordToConnectAny(record.GetKey(), keySchema)
			if keyErr != nil {
				return nil, keyErr
			}
			key = map[string]any{"schema": keySchema, "payload": keyPayload}
		} else {
			key = cdcRecordToAny(record.GetKey())
		}
	}
	var serializedValue any = payload
	if valueSchema != nil {
		serializedValue = map[string]any{"schema": valueSchema, "payload": payload}
	}
	native := maps.Clone(nativeEnvelope)
	if native == nil {
		native = make(map[string]any)
	}
	native["key"] = key
	native["value"] = serializedValue
	return native, nil
}

func cdcStructuredToCanonical(event, key map[string]any, captureTime *timestamppb.Timestamp) (*cdcv1.ChangeRecord, error) {
	if captureTime == nil || !captureTime.IsValid() {
		return nil, errors.New("adapter observation capture time is required")
	}
	op, ok := event["iodebeziumop"].(string)
	if !ok {
		return nil, errors.New("structured Debezium CloudEvent has no operation")
	}
	operations := map[string]cdcv1.Operation{
		"c": cdcv1.Operation_OPERATION_CREATE,
		"u": cdcv1.Operation_OPERATION_UPDATE,
		"d": cdcv1.Operation_OPERATION_DELETE,
		"r": cdcv1.Operation_OPERATION_SNAPSHOT_READ,
		"t": cdcv1.Operation_OPERATION_TRUNCATE,
		"m": cdcv1.Operation_OPERATION_SOURCE_MESSAGE,
	}
	operation, ok := operations[op]
	if !ok {
		return nil, fmt.Errorf("unsupported structured Debezium operation %q", op)
	}
	data, ok := event["data"].(map[string]any)
	if !ok {
		return nil, errors.New("structured Debezium CloudEvent has no data object")
	}
	attributes := maps.Clone(event)
	delete(attributes, "data")
	metadataBytes, err := json.Marshal(cdcDebeziumMetadata{CloudEvent: attributes})
	if err != nil {
		return nil, err
	}
	record := &cdcv1.ChangeRecord{
		Operation:   operation,
		CaptureTime: proto.Clone(captureTime).(*timestamppb.Timestamp),
		SourceExtension: &cdcv1.SourceExtension{Representation: &cdcv1.SourceExtension_OpaqueData{
			OpaqueData: &cdcv1.OpaqueData{
				MediaType: "application/json",
				Schema:    "urn:invariant:cdc:profile:debezium:3.6.1.Final:metadata",
				Data:      metadataBytes,
			},
		}},
	}
	if operation != cdcv1.Operation_OPERATION_SOURCE_MESSAGE && len(key) > 0 {
		record.Key = cdcAnyToRecord(key)
	}
	if before, present := data["before"].(map[string]any); present {
		record.Before = cdcAnyToRecord(before)
	}
	if after, present := data["after"].(map[string]any); present {
		record.After = cdcAnyToRecord(after)
	}
	if operation == cdcv1.Operation_OPERATION_SOURCE_MESSAGE {
		record.SourceMessage, err = cdcDebeziumMessageToValue(data["message"])
		if err != nil {
			return nil, err
		}
	}
	collectionParts := make([]string, 0, 3)
	for _, name := range []string{"iodebeziumdb", "iodebeziumschema", "iodebeziumtable"} {
		if part, present := event[name].(string); present && part != "" {
			collectionParts = append(collectionParts, part)
		}
	}
	if len(collectionParts) > 0 && operation != cdcv1.Operation_OPERATION_SOURCE_MESSAGE {
		record.DataCollection = &cdcv1.DataCollection{Id: strings.Join(collectionParts, ".")}
	}
	stream, _ := event["iodebeziumname"].(string)
	checkpoint := make(map[string]any)
	for _, name := range []string{"iodebeziumlsn", "iodebeziumtxid", "iodebeziumxmin"} {
		if value, present := event[name]; present {
			checkpoint[strings.TrimPrefix(name, "iodebezium")] = value
		}
	}
	checkpointBytes, err := json.Marshal(checkpoint)
	if err != nil {
		return nil, err
	}
	record.SourcePosition = &cdcv1.SourcePosition{Stream: stream, Format: "application/json", Value: checkpointBytes}
	if schema, present := event["dataschema"].(string); present {
		record.SchemaReference = &cdcv1.SchemaReference{Uri: schema}
	}
	if eventTime, present := event["time"].(string); present {
		parsed, parseErr := time.Parse(time.RFC3339Nano, eventTime)
		if parseErr != nil {
			return nil, parseErr
		}
		record.SourceTime = timestamppb.New(parsed)
	}
	if sourceMillis, present := event["iodebeziumtsms"]; present {
		fromExtension, timestampErr := cdcTimestampFromDebezium(map[string]any{"ts_ms": sourceMillis})
		if timestampErr != nil {
			return nil, timestampErr
		}
		if record.GetSourceTime() != nil && !proto.Equal(record.GetSourceTime(), fromExtension) {
			return nil, errors.New("structured CloudEvent time disagrees with Debezium source timestamp")
		}
		record.SourceTime = fromExtension
	}
	if transactionID, present := event["iodebeziumtxid"].(string); present {
		transaction := &cdcv1.TransactionContext{Id: transactionID}
		if order, orderPresent := event["iodebeziumtxtotalorder"]; orderPresent {
			parsed, parseErr := strconv.ParseUint(cdcJSONScalar(order), 10, 64)
			if parseErr != nil {
				return nil, parseErr
			}
			transaction.TotalOrder = &parsed
		}
		if order, orderPresent := event["iodebeziumtxdatacollectionorder"]; orderPresent {
			parsed, parseErr := strconv.ParseUint(cdcJSONScalar(order), 10, 64)
			if parseErr != nil {
				return nil, parseErr
			}
			transaction.DataCollectionOrder = &parsed
		}
		record.Transaction = transaction
	}
	return record, nil
}

func cdcStructuredToCanonicalCloudEvent(
	event, key map[string]any,
	captureTime *timestamppb.Timestamp,
) (*cloudeventsv1.CloudEvent, error) {
	if event["specversion"] != "1.0" {
		return nil, errors.New("structured CloudEvent specversion must be 1.0")
	}
	record, err := cdcStructuredToCanonical(event, key, captureTime)
	if err != nil {
		return nil, err
	}
	payload, err := anypb.New(record)
	if err != nil {
		return nil, err
	}
	id, _ := event["id"].(string)
	source, _ := event["source"].(string)
	eventTime := record.GetSourceTime()
	if eventTime == nil {
		eventTime = record.GetCaptureTime()
	}
	return &cloudeventsv1.CloudEvent{
		Id: id, Source: source, SpecVersion: "1.0", Type: cdcEventType,
		Attributes: map[string]*cloudeventsv1.CloudEvent_CloudEventAttributeValue{
			"time":            cdcTimestampAttribute(eventTime),
			"datacontenttype": cdcStringAttribute("application/protobuf"),
			"dataschema":      cdcURIAttribute(cdcDataSchema),
		},
		Data: &cloudeventsv1.CloudEvent_ProtoData{ProtoData: payload},
	}, nil
}

func cdcCanonicalToStructured(record *cdcv1.ChangeRecord) (map[string]any, map[string]any, error) {
	extension := record.GetSourceExtension().GetOpaqueData()
	if extension == nil || extension.GetMediaType() != "application/json" {
		return nil, nil, errors.New("debezium metadata extension is missing")
	}
	metadataObject, err := cdcDecodeJSONObject(extension.GetData())
	if err != nil {
		return nil, nil, err
	}
	attributes, ok := metadataObject["cloud_event"].(map[string]any)
	if !ok {
		return nil, nil, errors.New("structured CloudEvent metadata is missing")
	}
	event := maps.Clone(attributes)
	operations := map[cdcv1.Operation]string{
		cdcv1.Operation_OPERATION_CREATE:         "c",
		cdcv1.Operation_OPERATION_UPDATE:         "u",
		cdcv1.Operation_OPERATION_DELETE:         "d",
		cdcv1.Operation_OPERATION_SNAPSHOT_READ:  "r",
		cdcv1.Operation_OPERATION_TRUNCATE:       "t",
		cdcv1.Operation_OPERATION_SOURCE_MESSAGE: "m",
	}
	op, ok := operations[record.GetOperation()]
	if !ok {
		return nil, nil, fmt.Errorf("unsupported canonical operation %s", record.GetOperation())
	}
	event["iodebeziumop"] = op
	data := make(map[string]any)
	switch record.GetOperation() {
	case cdcv1.Operation_OPERATION_CREATE, cdcv1.Operation_OPERATION_UPDATE, cdcv1.Operation_OPERATION_SNAPSHOT_READ:
		if record.GetBefore() == nil {
			data["before"] = nil
		} else {
			data["before"] = cdcRecordToAny(record.GetBefore())
		}
		data["after"] = cdcRecordToAny(record.GetAfter())
	case cdcv1.Operation_OPERATION_DELETE:
		if record.GetBefore() == nil {
			data["before"] = nil
		} else {
			data["before"] = cdcRecordToAny(record.GetBefore())
		}
		data["after"] = nil
	case cdcv1.Operation_OPERATION_SOURCE_MESSAGE:
		data["message"] = cdcValueToAny(record.GetSourceMessage())
	}
	event["data"] = data
	var key map[string]any
	if record.GetOperation() == cdcv1.Operation_OPERATION_SOURCE_MESSAGE {
		prefix := cdcFieldByName(record.GetSourceMessage().GetRecordValue(), "prefix")
		if prefix != nil {
			key = map[string]any{"prefix": prefix.GetValue().GetStringValue()}
		}
	} else if record.GetKey() != nil {
		key = cdcRecordToAny(record.GetKey())
	}
	return event, key, nil
}

func cdcDebeziumMessageToValue(message any) (*cdcv1.Value, error) {
	object, ok := message.(map[string]any)
	if !ok {
		return nil, errors.New("debezium source message is not an object")
	}
	fields := make([]*cdcv1.RecordField, 0, len(object))
	names := make([]string, 0, len(object))
	for name := range object {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		value := cdcAnyToValue(object[name])
		if name == "content" {
			encoded, stringOK := object[name].(string)
			if !stringOK {
				return nil, errors.New("debezium source message content is not base64 text")
			}
			decoded, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				return nil, err
			}
			value = &cdcv1.Value{Kind: &cdcv1.Value_BytesValue{BytesValue: decoded}}
		}
		fields = append(fields, cdcField(name, value))
	}
	return &cdcv1.Value{Kind: &cdcv1.Value_RecordValue{RecordValue: cdcRecord(fields...)}}, nil
}

func cdcConnectFieldSchema(schema map[string]any, name string) map[string]any {
	fields, _ := schema["fields"].([]any)
	for _, candidate := range fields {
		field, ok := candidate.(map[string]any)
		if ok && field["field"] == name {
			return field
		}
	}
	return nil
}

func cdcConnectToRecord(value, schema map[string]any) (*cdcv1.Record, error) {
	names := make([]string, 0, len(value))
	for name := range value {
		names = append(names, name)
	}
	slices.Sort(names)
	fields := make([]*cdcv1.RecordField, 0, len(names))
	for _, name := range names {
		converted, err := cdcConnectToValue(value[name], cdcConnectFieldSchema(schema, name))
		if err != nil {
			return nil, fmt.Errorf("connect field %s: %w", name, err)
		}
		fields = append(fields, cdcField(name, converted))
	}
	return cdcRecord(fields...), nil
}

func cdcConnectToValue(value any, schema map[string]any) (*cdcv1.Value, error) {
	if value == nil || schema == nil {
		return cdcAnyToValue(value), nil
	}
	typeName, _ := schema["name"].(string)
	typeKind, _ := schema["type"].(string)
	number, _ := value.(json.Number)
	switch typeKind {
	case "boolean", "float32", "float64", "string":
		converted := cdcAnyToValue(value)
		converted.TypeName = typeName
		return converted, nil
	case "int8", "int16", "int32":
		parsed, err := strconv.ParseInt(number.String(), 10, 32)
		if err != nil {
			return nil, err
		}
		return &cdcv1.Value{TypeName: typeName, Kind: &cdcv1.Value_Int32Value{Int32Value: int32(parsed)}}, nil
	case "int64":
		parsed, err := strconv.ParseInt(number.String(), 10, 64)
		if err != nil {
			return nil, err
		}
		if typeName == "io.debezium.time.MicroTimestamp" {
			return &cdcv1.Value{TypeName: typeName, Kind: &cdcv1.Value_TimestampValue{
				TimestampValue: cdcTimestampFromNanos(parsed * 1_000),
			}}, nil
		}
		return &cdcv1.Value{TypeName: typeName, Kind: &cdcv1.Value_Int64Value{Int64Value: parsed}}, nil
	case "bytes":
		encoded, ok := value.(string)
		if !ok {
			return nil, errors.New("connect bytes value is not base64 text")
		}
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, err
		}
		if typeName != "org.apache.kafka.connect.data.Decimal" {
			return &cdcv1.Value{TypeName: typeName, Kind: &cdcv1.Value_BytesValue{BytesValue: decoded}}, nil
		}
		parameters, _ := schema["parameters"].(map[string]any)
		scale64, err := strconv.ParseInt(cdcJSONScalar(parameters["scale"]), 10, 32)
		if err != nil {
			return nil, err
		}
		sourceType, _ := parameters["__debezium.source.column.type"].(string)
		if scale64 == 0 && strings.Contains(strings.ToUpper(sourceType), "UNSIGNED") {
			integer := cdcSignedBytesToBigInt(decoded)
			if integer.Sign() < 0 || integer.BitLen() > 64 {
				return nil, fmt.Errorf("unsigned source value %s does not fit uint64", integer)
			}
			return &cdcv1.Value{
				TypeName: sourceType,
				Kind:     &cdcv1.Value_Uint64Value{Uint64Value: integer.Uint64()},
			}, nil
		}
		decimal := &cdcv1.DecimalValue{Value: cdcFormatScaledInteger(cdcSignedBytesToBigInt(decoded), int32(scale64)), Scale: int32(scale64)}
		if precisionValue, present := parameters["connect.decimal.precision"]; present {
			precision64, parseErr := strconv.ParseUint(cdcJSONScalar(precisionValue), 10, 32)
			if parseErr != nil {
				return nil, parseErr
			}
			precision := uint32(precision64)
			decimal.Precision = &precision
		}
		return &cdcv1.Value{TypeName: typeName, Kind: &cdcv1.Value_DecimalValue{DecimalValue: decimal}}, nil
	case "array":
		items, ok := value.([]any)
		if !ok {
			return nil, errors.New("connect array value is not an array")
		}
		itemSchema, _ := schema["items"].(map[string]any)
		converted := make([]*cdcv1.Value, 0, len(items))
		for _, item := range items {
			convertedItem, err := cdcConnectToValue(item, itemSchema)
			if err != nil {
				return nil, err
			}
			converted = append(converted, convertedItem)
		}
		return &cdcv1.Value{TypeName: typeName, Kind: &cdcv1.Value_ListValue{ListValue: &cdcv1.ListValue{Values: converted}}}, nil
	case "struct":
		object, ok := value.(map[string]any)
		if !ok {
			return nil, errors.New("connect struct value is not an object")
		}
		converted, err := cdcConnectToRecord(object, schema)
		if err != nil {
			return nil, err
		}
		return &cdcv1.Value{TypeName: typeName, Kind: &cdcv1.Value_RecordValue{RecordValue: converted}}, nil
	default:
		return cdcAnyToValue(value), nil
	}
}

func cdcRecordToConnectAny(record *cdcv1.Record, schema map[string]any) (map[string]any, error) {
	result := make(map[string]any, len(record.GetFields()))
	for _, field := range record.GetFields() {
		converted, err := cdcValueToConnectAny(field.GetValue(), cdcConnectFieldSchema(schema, field.GetName()))
		if err != nil {
			return nil, fmt.Errorf("connect field %s: %w", field.GetName(), err)
		}
		result[field.GetName()] = converted
	}
	return result, nil
}

func cdcValueToConnectAny(value *cdcv1.Value, schema map[string]any) (any, error) {
	if value.GetNullValue() != nil || schema == nil {
		return cdcValueToAny(value), nil
	}
	typeName, _ := schema["name"].(string)
	typeKind, _ := schema["type"].(string)
	switch typeKind {
	case "bytes":
		if typeName != "org.apache.kafka.connect.data.Decimal" {
			return base64.StdEncoding.EncodeToString(value.GetBytesValue()), nil
		}
		parameters, _ := schema["parameters"].(map[string]any)
		scale64, err := strconv.ParseInt(cdcJSONScalar(parameters["scale"]), 10, 32)
		if err != nil {
			return nil, err
		}
		sourceType, _ := parameters["__debezium.source.column.type"].(string)
		if scale64 == 0 && strings.Contains(strings.ToUpper(sourceType), "UNSIGNED") {
			return base64.StdEncoding.EncodeToString(cdcBigIntToSignedBytes(new(big.Int).SetUint64(value.GetUint64Value()))), nil
		}
		unscaled, err := cdcDecimalUnscaled(value.GetDecimalValue().GetValue(), int32(scale64))
		if err != nil {
			return nil, err
		}
		return base64.StdEncoding.EncodeToString(cdcBigIntToSignedBytes(unscaled)), nil
	case "int64":
		if typeName == "io.debezium.time.MicroTimestamp" {
			timestamp := value.GetTimestampValue()
			return json.Number(strconv.FormatInt(timestamp.GetSeconds()*1_000_000+int64(timestamp.GetNanos())/1_000, 10)), nil
		}
		return json.Number(strconv.FormatInt(value.GetInt64Value(), 10)), nil
	case "int8", "int16", "int32":
		return json.Number(strconv.FormatInt(int64(value.GetInt32Value()), 10)), nil
	case "array":
		itemSchema, _ := schema["items"].(map[string]any)
		items := make([]any, 0, len(value.GetListValue().GetValues()))
		for _, item := range value.GetListValue().GetValues() {
			converted, err := cdcValueToConnectAny(item, itemSchema)
			if err != nil {
				return nil, err
			}
			items = append(items, converted)
		}
		return items, nil
	case "struct":
		return cdcRecordToConnectAny(value.GetRecordValue(), schema)
	default:
		return cdcValueToAny(value), nil
	}
}

func cdcSignedBytesToBigInt(value []byte) *big.Int {
	integer := new(big.Int).SetBytes(value)
	if len(value) > 0 && value[0]&0x80 != 0 {
		integer.Sub(integer, new(big.Int).Lsh(big.NewInt(1), uint(8*len(value))))
	}
	return integer
}

func cdcBigIntToSignedBytes(value *big.Int) []byte {
	if value.Sign() >= 0 {
		encoded := value.Bytes()
		if len(encoded) == 0 {
			return []byte{0}
		}
		if encoded[0]&0x80 != 0 {
			return append([]byte{0}, encoded...)
		}
		return encoded
	}
	byteCount := (value.BitLen() + 8) / 8
	encoded := new(big.Int).Add(value, new(big.Int).Lsh(big.NewInt(1), uint(byteCount*8))).Bytes()
	for len(encoded) < byteCount {
		encoded = append([]byte{0xff}, encoded...)
	}
	return encoded
}

func cdcFormatScaledInteger(value *big.Int, scale int32) string {
	negative := value.Sign() < 0
	digits := new(big.Int).Abs(value).String()
	if scale > 0 {
		for len(digits) <= int(scale) {
			digits = "0" + digits
		}
		split := len(digits) - int(scale)
		digits = digits[:split] + "." + digits[split:]
	} else if scale < 0 {
		digits += strings.Repeat("0", int(-scale))
	}
	if negative {
		return "-" + digits
	}
	return digits
}

func cdcDecimalUnscaled(value string, scale int32) (*big.Int, error) {
	negative := strings.HasPrefix(value, "-")
	unsigned := strings.TrimPrefix(value, "-")
	parts := strings.Split(unsigned, ".")
	if len(parts) > 2 {
		return nil, fmt.Errorf("invalid decimal %q", value)
	}
	fraction := ""
	if len(parts) == 2 {
		fraction = parts[1]
	}
	digits := parts[0] + fraction
	delta := int(scale) - len(fraction)
	if delta < 0 {
		trim := -delta
		if trim > len(digits) || strings.Trim(digits[len(digits)-trim:], "0") != "" {
			return nil, fmt.Errorf("decimal %q exceeds scale %d", value, scale)
		}
		digits = digits[:len(digits)-trim]
	} else {
		digits += strings.Repeat("0", delta)
	}
	if digits == "" {
		digits = "0"
	}
	integer, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		return nil, fmt.Errorf("invalid decimal %q", value)
	}
	if negative {
		integer.Neg(integer)
	}
	return integer, nil
}

func cdcDebeziumSourcePosition(source map[string]any) *cdcv1.SourcePosition {
	connector, _ := source["connector"].(string)
	names := []string{"lsn", "pos", "sequence"}
	switch connector {
	case "mysql":
		names = []string{"file", "pos", "row", "gtid"}
	case "postgresql":
		names = []string{"lsn", "sequence", "txId", "xmin"}
	}
	checkpoint := make(map[string]any)
	for _, name := range names {
		if value, present := source[name]; present {
			checkpoint[name] = value
		}
	}
	encoded, _ := json.Marshal(checkpoint)
	stream, _ := source["name"].(string)
	return &cdcv1.SourcePosition{Stream: stream, Format: "application/json", Value: encoded}
}

func cdcDataCollectionFromSource(source map[string]any) *cdcv1.DataCollection {
	parts := make([]string, 0, 3)
	for _, name := range []string{"db", "schema", "table"} {
		if part, ok := source[name].(string); ok && part != "" {
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return &cdcv1.DataCollection{Id: strings.Join(parts, ".")}
}

func cdcSchemaReferenceFromConnect(schema map[string]any) *cdcv1.SchemaReference {
	name, ok := schema["name"].(string)
	if !ok {
		return nil
	}
	reference := &cdcv1.SchemaReference{Uri: "urn:debezium:kafka-connect:" + name}
	if version, present := schema["version"]; present {
		reference.Version = cdcJSONScalar(version)
	}
	return reference
}

func cdcNeedsAdapterContext(record *cdcv1.ChangeRecord, source, valueSchema map[string]any) bool {
	if valueSchema == nil && (cdcRecordNeedsCompanion(record.GetKey()) || cdcRecordNeedsCompanion(record.GetBefore()) ||
		cdcRecordNeedsCompanion(record.GetAfter())) {
		return true
	}
	if record.GetOperation() == cdcv1.Operation_OPERATION_SOURCE_MESSAGE && cdcSourceMessageNeedsCompanion(record.GetSourceMessage()) {
		return true
	}
	expectedCollection := cdcDataCollectionFromSource(source)
	if record.GetOperation() == cdcv1.Operation_OPERATION_SOURCE_MESSAGE {
		expectedCollection = nil
	}
	if record.GetChangedFields() != nil || !proto.Equal(record.GetDataCollection(), expectedCollection) ||
		!proto.Equal(record.GetSourcePosition(), cdcDebeziumSourcePosition(source)) ||
		!proto.Equal(record.GetSchemaReference(), cdcSchemaReferenceFromConnect(valueSchema)) {
		return true
	}
	return false
}

func cdcSourceMessageNeedsCompanion(message *cdcv1.Value) bool {
	if message == nil || message.GetTypeName() != "" {
		return message != nil
	}
	record := message.GetRecordValue()
	if record == nil || len(record.GetFields()) != 2 {
		return true
	}
	prefix := cdcFieldByName(record, "prefix")
	content := cdcFieldByName(record, "content")
	if prefix == nil || prefix.GetValue().GetTypeName() != "" || content == nil || content.GetValue().GetTypeName() != "" {
		return true
	}
	_, prefixIsString := prefix.GetValue().GetKind().(*cdcv1.Value_StringValue)
	_, contentIsBytes := content.GetValue().GetKind().(*cdcv1.Value_BytesValue)
	return !prefixIsString || !contentIsBytes
}

func cdcRecordNeedsCompanion(record *cdcv1.Record) bool {
	for _, field := range record.GetFields() {
		if cdcValueNeedsCompanion(field.GetValue()) {
			return true
		}
	}
	return false
}

func cdcValueNeedsCompanion(value *cdcv1.Value) bool {
	if value == nil {
		return false
	}
	if value.GetTypeName() != "" {
		return true
	}
	switch kind := value.GetKind().(type) {
	case *cdcv1.Value_Int32Value, *cdcv1.Value_Uint32Value, *cdcv1.Value_Uint64Value,
		*cdcv1.Value_Float32Value, *cdcv1.Value_Float64Value, *cdcv1.Value_BytesValue,
		*cdcv1.Value_TimestampValue, *cdcv1.Value_MapValue:
		return true
	case *cdcv1.Value_DecimalValue:
		return kind.DecimalValue.Precision != nil
	case *cdcv1.Value_RecordValue:
		return cdcRecordNeedsCompanion(kind.RecordValue)
	case *cdcv1.Value_ListValue:
		return slices.ContainsFunc(kind.ListValue.GetValues(), cdcValueNeedsCompanion)
	}
	return false
}

func cdcCanonicalAdapterContext(record *cdcv1.ChangeRecord) map[string]any {
	context := map[string]any{
		"values": map[string]any{
			"key":            cdcRecordCompanion(record.GetKey()),
			"before":         cdcRecordCompanion(record.GetBefore()),
			"after":          cdcRecordCompanion(record.GetAfter()),
			"source_message": cdcValueCompanion(record.GetSourceMessage()),
		},
	}
	if collection := record.GetDataCollection(); collection != nil {
		context["data_collection"] = map[string]any{"id": collection.GetId()}
	}
	if reference := record.GetSchemaReference(); reference != nil {
		context["schema_reference"] = map[string]any{
			"uri": reference.GetUri(), "version": reference.GetVersion(),
			"fingerprint": base64.StdEncoding.EncodeToString(reference.GetFingerprint()),
		}
	}
	if position := record.GetSourcePosition(); position != nil {
		context["source_position"] = map[string]any{
			"stream": position.GetStream(), "format": position.GetFormat(),
			"value": base64.StdEncoding.EncodeToString(position.GetValue()),
		}
	}
	if changed := record.GetChangedFields(); changed != nil {
		paths := make([]any, 0, len(changed.GetPaths()))
		for _, path := range changed.GetPaths() {
			segments := make([]any, 0, len(path.GetSegments()))
			for _, segment := range path.GetSegments() {
				segments = append(segments, segment)
			}
			paths = append(paths, segments)
		}
		context["changed_fields"] = paths
	}
	return context
}

func cdcApplyAdapterContext(record *cdcv1.ChangeRecord, context map[string]any) error {
	if collection, ok := context["data_collection"].(map[string]any); ok {
		record.DataCollection = &cdcv1.DataCollection{}
		record.DataCollection.Id, _ = collection["id"].(string)
	}
	if reference, ok := context["schema_reference"].(map[string]any); ok {
		record.SchemaReference = &cdcv1.SchemaReference{}
		record.SchemaReference.Uri, _ = reference["uri"].(string)
		record.SchemaReference.Version, _ = reference["version"].(string)
		if encoded, present := reference["fingerprint"].(string); present {
			fingerprint, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				return err
			}
			record.SchemaReference.Fingerprint = fingerprint
		}
	}
	if position, ok := context["source_position"].(map[string]any); ok {
		record.SourcePosition = &cdcv1.SourcePosition{}
		record.SourcePosition.Stream, _ = position["stream"].(string)
		record.SourcePosition.Format, _ = position["format"].(string)
		encoded, _ := position["value"].(string)
		value, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return err
		}
		record.SourcePosition.Value = value
	}
	if paths, present := context["changed_fields"].([]any); present {
		record.ChangedFields = &cdcv1.ChangedFieldMask{}
		for _, rawPath := range paths {
			rawSegments, ok := rawPath.([]any)
			if !ok {
				return errors.New("adapter changed-field path is not an array")
			}
			path := &cdcv1.FieldPath{}
			for _, rawSegment := range rawSegments {
				segment, ok := rawSegment.(string)
				if !ok {
					return errors.New("adapter changed-field segment is not text")
				}
				path.Segments = append(path.Segments, segment)
			}
			record.ChangedFields.Paths = append(record.ChangedFields.Paths, path)
		}
	}
	return nil
}

func cdcCompanionRecord(context map[string]any, name string) map[string]any {
	values, _ := context["values"].(map[string]any)
	record, _ := values[name].(map[string]any)
	return record
}

func cdcCompanionValue(context map[string]any, name string) map[string]any {
	values, _ := context["values"].(map[string]any)
	value, _ := values[name].(map[string]any)
	return value
}

func cdcRecordCompanion(record *cdcv1.Record) map[string]any {
	if record == nil {
		return nil
	}
	fields := make(map[string]any, len(record.GetFields()))
	for _, field := range record.GetFields() {
		fields[field.GetName()] = cdcValueCompanion(field.GetValue())
	}
	return fields
}

func cdcValueCompanion(value *cdcv1.Value) map[string]any {
	if value == nil {
		return nil
	}
	hint := make(map[string]any)
	if value.GetTypeName() != "" {
		hint["type_name"] = value.GetTypeName()
	}
	switch kind := value.GetKind().(type) {
	case *cdcv1.Value_NullValue:
		hint["kind"] = "null"
	case *cdcv1.Value_BoolValue:
		hint["kind"] = "bool"
	case *cdcv1.Value_Int32Value:
		hint["kind"] = "int32"
	case *cdcv1.Value_Int64Value:
		hint["kind"] = "int64"
	case *cdcv1.Value_Uint32Value:
		hint["kind"] = "uint32"
	case *cdcv1.Value_Uint64Value:
		hint["kind"] = "uint64"
	case *cdcv1.Value_Float32Value:
		hint["kind"] = "float32"
	case *cdcv1.Value_Float64Value:
		hint["kind"] = "float64"
	case *cdcv1.Value_StringValue:
		hint["kind"] = "string"
	case *cdcv1.Value_BytesValue:
		hint["kind"] = "bytes"
	case *cdcv1.Value_DecimalValue:
		hint["kind"] = "decimal"
		hint["scale"] = kind.DecimalValue.GetScale()
		if kind.DecimalValue.Precision != nil {
			hint["precision"] = kind.DecimalValue.GetPrecision()
		}
	case *cdcv1.Value_TimestampValue:
		hint["kind"] = "timestamp"
	case *cdcv1.Value_RecordValue:
		hint["kind"] = "record"
		hint["fields"] = cdcRecordCompanion(kind.RecordValue)
	case *cdcv1.Value_ListValue:
		hint["kind"] = "list"
		items := make([]any, 0, len(kind.ListValue.GetValues()))
		for _, item := range kind.ListValue.GetValues() {
			items = append(items, cdcValueCompanion(item))
		}
		hint["items"] = items
	case *cdcv1.Value_MapValue:
		hint["kind"] = "map"
		entries := make([]any, 0, len(kind.MapValue.GetEntries()))
		for _, entry := range kind.MapValue.GetEntries() {
			entries = append(entries, map[string]any{"key": cdcValueCompanion(entry.GetKey()), "value": cdcValueCompanion(entry.GetValue())})
		}
		hint["entries"] = entries
	}
	return hint
}

func cdcAnyToRecordWithCompanion(value, companion map[string]any) *cdcv1.Record {
	names := make([]string, 0, len(value))
	for name := range value {
		names = append(names, name)
	}
	slices.Sort(names)
	fields := make([]*cdcv1.RecordField, 0, len(names))
	for _, name := range names {
		hint, _ := companion[name].(map[string]any)
		fields = append(fields, cdcField(name, cdcAnyToValueWithCompanion(value[name], hint)))
	}
	return cdcRecord(fields...)
}

func cdcAnyToValueWithCompanion(value any, hint map[string]any) *cdcv1.Value {
	if hint == nil {
		return cdcAnyToValue(value)
	}
	typeName, _ := hint["type_name"].(string)
	kind, _ := hint["kind"].(string)
	converted := &cdcv1.Value{TypeName: typeName}
	scalar := cdcJSONScalar(value)
	switch kind {
	case "null":
		converted.Kind = &cdcv1.Value_NullValue{NullValue: &cdcv1.NullValue{}}
	case "bool":
		converted.Kind = &cdcv1.Value_BoolValue{BoolValue: value.(bool)}
	case "int32":
		parsed, _ := strconv.ParseInt(scalar, 10, 32)
		converted.Kind = &cdcv1.Value_Int32Value{Int32Value: int32(parsed)}
	case "int64":
		parsed, _ := strconv.ParseInt(scalar, 10, 64)
		converted.Kind = &cdcv1.Value_Int64Value{Int64Value: parsed}
	case "uint32":
		parsed, _ := strconv.ParseUint(scalar, 10, 32)
		converted.Kind = &cdcv1.Value_Uint32Value{Uint32Value: uint32(parsed)}
	case "uint64":
		parsed, _ := strconv.ParseUint(scalar, 10, 64)
		converted.Kind = &cdcv1.Value_Uint64Value{Uint64Value: parsed}
	case "float32":
		parsed, _ := strconv.ParseFloat(scalar, 32)
		converted.Kind = &cdcv1.Value_Float32Value{Float32Value: float32(parsed)}
	case "float64":
		parsed, _ := strconv.ParseFloat(scalar, 64)
		converted.Kind = &cdcv1.Value_Float64Value{Float64Value: parsed}
	case "string":
		converted.Kind = &cdcv1.Value_StringValue{StringValue: value.(string)}
	case "bytes":
		decoded, _ := base64.StdEncoding.DecodeString(value.(string))
		converted.Kind = &cdcv1.Value_BytesValue{BytesValue: decoded}
	case "decimal":
		scale, _ := strconv.ParseInt(cdcJSONScalar(hint["scale"]), 10, 32)
		decimal := &cdcv1.DecimalValue{Value: scalar, Scale: int32(scale)}
		if rawPrecision, present := hint["precision"]; present {
			precision, _ := strconv.ParseUint(cdcJSONScalar(rawPrecision), 10, 32)
			value := uint32(precision)
			decimal.Precision = &value
		}
		converted.Kind = &cdcv1.Value_DecimalValue{DecimalValue: decimal}
	case "timestamp":
		parsed, _ := time.Parse(time.RFC3339Nano, value.(string))
		converted.Kind = &cdcv1.Value_TimestampValue{TimestampValue: timestamppb.New(parsed)}
	case "record":
		fields, _ := hint["fields"].(map[string]any)
		converted.Kind = &cdcv1.Value_RecordValue{RecordValue: cdcAnyToRecordWithCompanion(value.(map[string]any), fields)}
	case "list":
		rawItems := value.([]any)
		itemHints, _ := hint["items"].([]any)
		items := make([]*cdcv1.Value, 0, len(rawItems))
		for index, rawItem := range rawItems {
			itemHint, _ := itemHints[index].(map[string]any)
			items = append(items, cdcAnyToValueWithCompanion(rawItem, itemHint))
		}
		converted.Kind = &cdcv1.Value_ListValue{ListValue: &cdcv1.ListValue{Values: items}}
	case "map":
		rawEntries := value.([]any)
		entryHints, _ := hint["entries"].([]any)
		entries := make([]*cdcv1.MapEntry, 0, len(rawEntries))
		for index, rawEntry := range rawEntries {
			entry := rawEntry.(map[string]any)
			entryHint := entryHints[index].(map[string]any)
			keyHint, _ := entryHint["key"].(map[string]any)
			valueHint, _ := entryHint["value"].(map[string]any)
			entries = append(entries, &cdcv1.MapEntry{
				Key: cdcAnyToValueWithCompanion(entry["key"], keyHint), Value: cdcAnyToValueWithCompanion(entry["value"], valueHint),
			})
		}
		converted.Kind = &cdcv1.Value_MapValue{MapValue: &cdcv1.MapValue{Entries: entries}}
	default:
		return cdcAnyToValue(value)
	}
	return converted
}

func cdcAnyToRecord(value map[string]any) *cdcv1.Record {
	names := make([]string, 0, len(value))
	for name := range value {
		names = append(names, name)
	}
	slices.Sort(names)
	fields := make([]*cdcv1.RecordField, 0, len(names))
	for _, name := range names {
		fields = append(fields, cdcField(name, cdcAnyToValue(value[name])))
	}
	return cdcRecord(fields...)
}

func cdcAnyToValue(value any) *cdcv1.Value {
	switch value := value.(type) {
	case nil:
		return &cdcv1.Value{Kind: &cdcv1.Value_NullValue{NullValue: &cdcv1.NullValue{}}}
	case bool:
		return &cdcv1.Value{Kind: &cdcv1.Value_BoolValue{BoolValue: value}}
	case string:
		return cdcString(value)
	case json.Number:
		text := value.String()
		if strings.ContainsAny(text, ".eE") {
			scale := int32(0)
			if dot := strings.IndexByte(text, '.'); dot >= 0 {
				end := len(text)
				if exponent := strings.IndexAny(text, "eE"); exponent >= 0 {
					end = exponent
				}
				scale = int32(end - dot - 1)
			}
			return &cdcv1.Value{Kind: &cdcv1.Value_DecimalValue{DecimalValue: &cdcv1.DecimalValue{Value: text, Scale: scale}}}
		}
		if signed, err := strconv.ParseInt(text, 10, 64); err == nil {
			return cdcInt64(signed)
		}
		unsigned, err := strconv.ParseUint(text, 10, 64)
		if err != nil {
			return &cdcv1.Value{Kind: &cdcv1.Value_DecimalValue{DecimalValue: &cdcv1.DecimalValue{Value: text}}}
		}
		return &cdcv1.Value{Kind: &cdcv1.Value_Uint64Value{Uint64Value: unsigned}}
	case []any:
		items := make([]*cdcv1.Value, 0, len(value))
		for _, item := range value {
			items = append(items, cdcAnyToValue(item))
		}
		return &cdcv1.Value{Kind: &cdcv1.Value_ListValue{ListValue: &cdcv1.ListValue{Values: items}}}
	case map[string]any:
		return &cdcv1.Value{Kind: &cdcv1.Value_RecordValue{RecordValue: cdcAnyToRecord(value)}}
	default:
		panic(fmt.Sprintf("unsupported decoded JSON value %T", value))
	}
}

func cdcRecordToAny(record *cdcv1.Record) map[string]any {
	result := make(map[string]any, len(record.GetFields()))
	for _, field := range record.GetFields() {
		result[field.GetName()] = cdcValueToAny(field.GetValue())
	}
	return result
}

func cdcValueToAny(value *cdcv1.Value) any {
	switch kind := value.GetKind().(type) {
	case *cdcv1.Value_NullValue:
		return nil
	case *cdcv1.Value_BoolValue:
		return kind.BoolValue
	case *cdcv1.Value_Int32Value:
		return json.Number(strconv.FormatInt(int64(kind.Int32Value), 10))
	case *cdcv1.Value_Int64Value:
		return json.Number(strconv.FormatInt(kind.Int64Value, 10))
	case *cdcv1.Value_Uint32Value:
		return json.Number(strconv.FormatUint(uint64(kind.Uint32Value), 10))
	case *cdcv1.Value_Uint64Value:
		return json.Number(strconv.FormatUint(kind.Uint64Value, 10))
	case *cdcv1.Value_Float32Value:
		return json.Number(strconv.FormatFloat(float64(kind.Float32Value), 'g', -1, 32))
	case *cdcv1.Value_Float64Value:
		return json.Number(strconv.FormatFloat(kind.Float64Value, 'g', -1, 64))
	case *cdcv1.Value_StringValue:
		return kind.StringValue
	case *cdcv1.Value_BytesValue:
		return base64.StdEncoding.EncodeToString(kind.BytesValue)
	case *cdcv1.Value_DecimalValue:
		return json.Number(kind.DecimalValue.GetValue())
	case *cdcv1.Value_TimestampValue:
		return kind.TimestampValue.AsTime().Format(time.RFC3339Nano)
	case *cdcv1.Value_RecordValue:
		return cdcRecordToAny(kind.RecordValue)
	case *cdcv1.Value_ListValue:
		result := make([]any, 0, len(kind.ListValue.GetValues()))
		for _, item := range kind.ListValue.GetValues() {
			result = append(result, cdcValueToAny(item))
		}
		return result
	case *cdcv1.Value_MapValue:
		result := make([]any, 0, len(kind.MapValue.GetEntries()))
		for _, entry := range kind.MapValue.GetEntries() {
			result = append(result, map[string]any{"key": cdcValueToAny(entry.GetKey()), "value": cdcValueToAny(entry.GetValue())})
		}
		return result
	default:
		return nil
	}
}

func cdcTimestampFromDebezium(values map[string]any) (*timestamppb.Timestamp, error) {
	if value, present := values["ts_ns"]; present {
		nanoseconds, err := strconv.ParseInt(cdcJSONScalar(value), 10, 64)
		if err != nil {
			return nil, err
		}
		return cdcTimestampFromNanos(nanoseconds), nil
	}
	if value, present := values["ts_us"]; present {
		microseconds, err := strconv.ParseInt(cdcJSONScalar(value), 10, 64)
		if err != nil {
			return nil, err
		}
		return cdcTimestampFromNanos(microseconds * 1_000), nil
	}
	if value, present := values["ts_ms"]; present {
		milliseconds, err := strconv.ParseInt(cdcJSONScalar(value), 10, 64)
		if err != nil {
			return nil, err
		}
		return cdcTimestampFromNanos(milliseconds * 1_000_000), nil
	}
	return nil, errors.New("debezium timestamp is missing")
}

func cdcValidateTimestampAgreement(name string, canonical *timestamppb.Timestamp, native map[string]any) error {
	hasNative := false
	for _, field := range []string{"ts_ns", "ts_us", "ts_ms"} {
		if _, present := native[field]; present {
			hasNative = true
			break
		}
	}
	if !hasNative {
		if canonical == nil {
			return nil
		}
		return fmt.Errorf("canonical %s cannot be represented by preserved Debezium timestamp fields", name)
	}
	decoded, err := cdcTimestampFromDebezium(native)
	if err != nil {
		return err
	}
	if canonical == nil || !proto.Equal(canonical, decoded) {
		return fmt.Errorf("canonical %s disagrees with preserved Debezium timestamp fields", name)
	}
	return nil
}

func cdcTimestampFromNanos(value int64) *timestamppb.Timestamp {
	seconds := value / int64(time.Second)
	nanoseconds := value % int64(time.Second)
	if nanoseconds < 0 {
		seconds--
		nanoseconds += int64(time.Second)
	}
	return &timestamppb.Timestamp{Seconds: seconds, Nanos: int32(nanoseconds)}
}

func cdcJSONScalar(value any) string {
	switch value := value.(type) {
	case json.Number:
		return value.String()
	case string:
		return value
	default:
		return fmt.Sprint(value)
	}
}
