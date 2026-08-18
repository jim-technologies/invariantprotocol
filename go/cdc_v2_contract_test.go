package invariant_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	cdcv2 "github.com/jim-technologies/invariantprotocol/go/gen/invariant/cdc/v2"
	cloudeventsv1 "github.com/jim-technologies/invariantprotocol/go/gen/io/cloudevents/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	cdcV2EventType  = "io.invariantprotocol.cdc.v2.change"
	cdcV2DataSchema = "type.googleapis.com/invariant.cdc.v2.ChangeRecord"
)

type cdcV2Manifest struct {
	FixtureFormatVersion int                 `json:"fixture_format_version"`
	Contract             string              `json:"contract"`
	Envelope             string              `json:"envelope"`
	EventType            string              `json:"event_type"`
	Source               string              `json:"source"`
	EventCount           int                 `json:"event_count"`
	UniqueEventCount     int                 `json:"unique_event_count"`
	Operations           []string            `json:"operations"`
	Positions            []string            `json:"positions"`
	RetryIndexes         []int               `json:"retry_indexes"`
	StateAtPosition      map[string][]int64  `json:"state_at_position"`
	Files                []cdcV2ManifestFile `json:"files"`
}

type cdcV2ManifestFile struct {
	Path           string `json:"path"`
	Representation string `json:"representation"`
	SHA256         string `json:"sha256"`
	Size           int    `json:"size"`
}

func TestCDCV2ReplayFixtureEnvelopesAndShapes(t *testing.T) {
	t.Parallel()

	manifest := cdcV2LoadManifest(t)
	require.Equal(t, 1, manifest.FixtureFormatVersion)
	require.Equal(t, "invariant.cdc.v2.ChangeRecord", manifest.Contract)
	require.Equal(t, "io.cloudevents.v1.CloudEventBatch", manifest.Envelope)
	require.Equal(t, cdcV2EventType, manifest.EventType)
	require.Equal(t, 8, manifest.EventCount)
	require.Equal(t, 7, manifest.UniqueEventCount)
	require.Equal(t, []int{2}, manifest.RetryIndexes)

	fullBytes, full := cdcV2LoadBatch(t, "full.binpb")
	deltaBytes, delta := cdcV2LoadBatch(t, "delta.binpb")
	require.Less(t, len(deltaBytes), len(fullBytes), "sparse delta history must be smaller than repeated full images")
	require.Len(t, full.GetEvents(), manifest.EventCount)
	require.Len(t, delta.GetEvents(), manifest.EventCount)
	cdcV2AssertManifestFile(t, manifest, "full.binpb", fullBytes)
	cdcV2AssertManifestFile(t, manifest, "delta.binpb", deltaBytes)

	expectedOperations := []cdcv2.Operation{
		cdcv2.Operation_OPERATION_SNAPSHOT_READ,
		cdcv2.Operation_OPERATION_UPDATE,
		cdcv2.Operation_OPERATION_UPDATE,
		cdcv2.Operation_OPERATION_UPDATE,
		cdcv2.Operation_OPERATION_DELETE,
		cdcv2.Operation_OPERATION_CREATE,
		cdcv2.Operation_OPERATION_TRUNCATE,
		cdcv2.Operation_OPERATION_SOURCE_MESSAGE,
	}
	for index := range full.GetEvents() {
		fullEvent := full.GetEvents()[index]
		deltaEvent := delta.GetEvents()[index]
		for _, event := range []*cloudeventsv1.CloudEvent{fullEvent, deltaEvent} {
			require.Equal(t, manifest.Source, event.GetSource())
			require.Equal(t, "1.0", event.GetSpecVersion())
			require.Equal(t, cdcV2EventType, event.GetType())
			require.Equal(t, "application/protobuf", event.GetAttributes()["datacontenttype"].GetCeString())
			require.Equal(t, cdcV2DataSchema, event.GetAttributes()["dataschema"].GetCeUri())
			require.NotNil(t, event.GetAttributes()["time"].GetCeTimestamp())
			require.Equal(t, cdcV2DataSchema, event.GetProtoData().GetTypeUrl())
		}
		require.Equal(t, fullEvent.GetSource(), deltaEvent.GetSource())
		require.Equal(t, fullEvent.GetId(), deltaEvent.GetId())

		fullRecord := cdcV2Unpack(t, fullEvent)
		deltaRecord := cdcV2Unpack(t, deltaEvent)
		require.Equal(t, expectedOperations[index], fullRecord.GetOperation())
		require.Equal(t, expectedOperations[index], deltaRecord.GetOperation())
		require.Equal(t, manifest.Positions[index], string(fullRecord.GetSourcePosition().GetValue()))
		require.Equal(t, manifest.Positions[index], string(deltaRecord.GetSourcePosition().GetValue()))
		require.True(t, proto.Equal(fullRecord.GetCaptureTime(), deltaRecord.GetCaptureTime()))
		require.NoError(t, cdcV2ValidateShape(fullRecord, "full"))
		require.NoError(t, cdcV2ValidateShape(deltaRecord, "delta"))
	}

	require.True(t, proto.Equal(full.GetEvents()[1], full.GetEvents()[2]))
	require.True(t, proto.Equal(delta.GetEvents()[1], delta.GetEvents()[2]))
	require.Equal(t, full.GetEvents()[1].GetSource(), full.GetEvents()[2].GetSource())
	require.Equal(t, full.GetEvents()[1].GetId(), full.GetEvents()[2].GetId())

	initial := cdcV2Unpack(t, delta.GetEvents()[0]).GetDelta().GetResult()
	cdcV2AssertWideRecord(t, initial)
	firstPatch := cdcV2Unpack(t, delta.GetEvents()[1]).GetDelta().GetPatch()
	require.Equal(t, [][]string{{"nickname"}, {"profile", "display_name"}}, cdcV2PatchPaths(firstPatch))
	secondPatch := cdcV2Unpack(t, delta.GetEvents()[3]).GetDelta().GetPatch()
	require.Equal(t, [][]string{{"nickname"}, {"profile", "level"}}, cdcV2PatchPaths(secondPatch))
	require.NotNil(t, firstPatch.GetChanges()[0].GetBefore().GetAbsent())
	require.NotNil(t, firstPatch.GetChanges()[0].GetAfter().GetValue().GetNullValue())
	require.NotNil(t, secondPatch.GetChanges()[0].GetBefore().GetValue().GetNullValue())
	require.NotNil(t, secondPatch.GetChanges()[0].GetAfter().GetAbsent())
}

func TestCDCV2FullAndDeltaReplayEquivalence(t *testing.T) {
	t.Parallel()

	_, full := cdcV2LoadBatch(t, "full.binpb")
	_, delta := cdcV2LoadBatch(t, "delta.binpb")
	fullSnapshots, err := cdcV2Replay(full)
	require.NoError(t, err)
	deltaSnapshots, err := cdcV2Replay(delta)
	require.NoError(t, err)
	require.Len(t, fullSnapshots, len(full.GetEvents()))
	require.Len(t, deltaSnapshots, len(delta.GetEvents()))
	for index := range fullSnapshots {
		cdcV2RequireStateEqual(t, fullSnapshots[index], deltaSnapshots[index])
	}
	cdcV2RequireStateEqual(t, fullSnapshots[1], fullSnapshots[2])
	require.Empty(t, fullSnapshots[4])
	require.Len(t, fullSnapshots[5], 1)
	require.Empty(t, fullSnapshots[6])
	require.Empty(t, fullSnapshots[7], "SOURCE_MESSAGE must not mutate replay state")
	require.EqualValues(t, 42, cdcV2FindField(cdcV2Unpack(t, delta.GetEvents()[4]).GetKey(), "id").GetValue().GetInt64Value())
	require.EqualValues(t, 84, cdcV2FindField(cdcV2Unpack(t, delta.GetEvents()[5]).GetKey(), "id").GetValue().GetInt64Value())

	// Deduplication happens before interpreting a retry. Even a corrupt retry
	// body with the same stable source + id cannot reapply a stale before state.
	tamperedRetry := proto.Clone(delta).(*cloudeventsv1.CloudEventBatch)
	tamperedRecord := cdcV2Unpack(t, tamperedRetry.GetEvents()[2])
	tamperedRecord.GetDelta().GetPatch().Changes[1].Before = cdcV2Present(cdcV2String("stale retry body"))
	tamperedWire, err := proto.Marshal(tamperedRecord)
	require.NoError(t, err)
	tamperedRetry.GetEvents()[2].GetProtoData().Value = tamperedWire
	tamperedSnapshots, err := cdcV2Replay(tamperedRetry)
	require.NoError(t, err)
	cdcV2RequireStateEqual(t, deltaSnapshots[len(deltaSnapshots)-1], tamperedSnapshots[len(tamperedSnapshots)-1])
	malformedRetry := proto.Clone(delta).(*cloudeventsv1.CloudEventBatch)
	malformedRetry.GetEvents()[2].GetProtoData().Value = []byte{0xff}
	stateAfterMalformedRetry, err := cdcV2StateAtPosition(malformedRetry, malformedRetry.GetEvents()[0].GetSource(), cdcV2FixturePosition("0003"))
	require.NoError(t, err, "state lookup deduplicates a retry before decoding its body")
	require.Len(t, stateAfterMalformedRetry, 1)

	manifest := cdcV2LoadManifest(t)
	for position, expectedIDs := range manifest.StateAtPosition {
		fullState, stateErr := cdcV2StateAtPosition(full, manifest.Source, cdcV2FixturePosition(position))
		require.NoError(t, stateErr)
		deltaState, stateErr := cdcV2StateAtPosition(delta, manifest.Source, cdcV2FixturePosition(position))
		require.NoError(t, stateErr)
		cdcV2RequireStateEqual(t, fullState, deltaState)
		require.Equal(t, expectedIDs, cdcV2StateIDs(fullState))
	}
	_, err = cdcV2StateAtPosition(delta, manifest.Source, cdcV2FixturePosition("does-not-exist"))
	require.EqualError(t, err, "source position not found")
	_, err = cdcV2StateAtPosition(delta, "urn:other-source", cdcV2FixturePosition("0002"))
	require.EqualError(t, err, "source position not found")
	wrongStream := cdcV2FixturePosition("0002")
	wrongStream.Stream = "other-stream"
	_, err = cdcV2StateAtPosition(delta, manifest.Source, wrongStream)
	require.EqualError(t, err, "source position not found")
	wrongFormat := cdcV2FixturePosition("0002")
	wrongFormat.Format = "application/x.other-position"
	_, err = cdcV2StateAtPosition(delta, manifest.Source, wrongFormat)
	require.EqualError(t, err, "source position not found")

	atFirstUpdate, err := cdcV2StateAtPosition(delta, manifest.Source, cdcV2FixturePosition("0002"))
	require.NoError(t, err)
	row := cdcV2OnlyRow(t, atFirstUpdate)
	require.NotNil(t, cdcV2FindField(row, "nickname").GetValue().GetNullValue())
	profile := cdcV2FindField(row, "profile").GetValue().GetRecordValue()
	require.Equal(t, "Ada Lovelace", cdcV2FindField(profile, "display_name").GetValue().GetStringValue())
	cdcV2AssertWideRecord(t, row)

	atSecondUpdate, err := cdcV2StateAtPosition(delta, manifest.Source, cdcV2FixturePosition("0003"))
	require.NoError(t, err)
	row = cdcV2OnlyRow(t, atSecondUpdate)
	require.Nil(t, cdcV2FindField(row, "nickname"))
	profile = cdcV2FindField(row, "profile").GetValue().GetRecordValue()
	require.EqualValues(t, 2, cdcV2FindField(profile, "level").GetValue().GetInt32Value())
}

func TestCDCV2FullDeltaSemanticConstruction(t *testing.T) {
	t.Parallel()

	_, full := cdcV2LoadBatch(t, "full.binpb")
	_, delta := cdcV2LoadBatch(t, "delta.binpb")
	fullState := make(cdcV2Rows)
	deltaState := make(cdcV2Rows)
	seen := make(map[string]bool)
	for index := range full.GetEvents() {
		fullEvent := full.GetEvents()[index]
		identity := fullEvent.GetSource() + "\x00" + fullEvent.GetId()
		if seen[identity] {
			continue
		}
		seen[identity] = true
		fullRecord := cdcV2Unpack(t, fullEvent)
		deltaRecord := cdcV2Unpack(t, delta.GetEvents()[index])
		key := ""
		var base *cdcv2.Record
		if fullRecord.GetKey() != nil {
			var keyErr error
			key, keyErr = cdcV2RowKey(fullEvent.GetSource(), fullRecord)
			require.NoError(t, keyErr)
			base = fullState[key]
		}

		constructedDelta, err := cdcV2FullToDelta(fullRecord, base)
		require.NoError(t, err)
		require.True(t, proto.Equal(deltaRecord, constructedDelta), "full to delta mismatch at index %d", index)
		constructedFull, err := cdcV2DeltaToFull(deltaRecord, deltaState[key])
		require.NoError(t, err)
		require.True(t, proto.Equal(fullRecord, constructedFull), "delta to full mismatch at index %d", index)
		if fullRecord.GetOperation() == cdcv2.Operation_OPERATION_UPDATE {
			fromCompleteBefore, conversionErr := cdcV2FullToDelta(fullRecord, nil)
			require.NoError(t, conversionErr)
			require.True(t, proto.Equal(deltaRecord, fromCompleteBefore))

			withoutBefore := proto.Clone(fullRecord).(*cdcv2.ChangeRecord)
			withoutBefore.GetFull().Before = nil
			fromMaterializedBase, conversionErr := cdcV2FullToDelta(withoutBefore, base)
			require.NoError(t, conversionErr)
			require.True(t, proto.Equal(deltaRecord, fromMaterializedBase))
		}

		require.NoError(t, cdcV2ApplyRecord(fullState, fullEvent.GetSource(), fullRecord))
		require.NoError(t, cdcV2ApplyRecord(deltaState, delta.GetEvents()[index].GetSource(), deltaRecord))
		cdcV2RequireStateEqual(t, fullState, deltaState)
	}
}

func TestCDCV2PatchAndReplayFailures(t *testing.T) {
	t.Parallel()

	_, delta := cdcV2LoadBatch(t, "delta.binpb")
	initial := cdcV2Unpack(t, delta.GetEvents()[0]).GetDelta().GetResult()
	firstUpdate := cdcV2Unpack(t, delta.GetEvents()[1])

	t.Run("before mismatch", func(t *testing.T) {
		patch := proto.Clone(firstUpdate.GetDelta().GetPatch()).(*cdcv2.RecordPatch)
		patch.Changes[1].Before = cdcV2Present(cdcV2String("wrong"))
		_, err := cdcV2ApplyPatch(initial, patch)
		require.ErrorContains(t, err, "before state mismatch")

		_, full := cdcV2LoadBatch(t, "full.binpb")
		rows := make(cdcV2Rows)
		require.NoError(t, cdcV2ApplyRecord(rows, full.GetEvents()[0].GetSource(), cdcV2Unpack(t, full.GetEvents()[0])))
		beforeFailure := cdcV2CloneRows(rows)
		mismatchedFull := cdcV2Unpack(t, full.GetEvents()[1])
		cdcV2FindField(mismatchedFull.GetFull().GetBefore(), "profile").Value = cdcV2String("not the materialized record")
		require.EqualError(t, cdcV2ApplyRecord(rows, full.GetEvents()[1].GetSource(), mismatchedFull), "UPDATE full before does not match materialized state")
		cdcV2RequireStateEqual(t, beforeFailure, rows)
	})

	t.Run("duplicate and overlapping paths", func(t *testing.T) {
		state := cdcV2Present(cdcV2String("x"))
		for name, paths := range map[string][][]string{
			"duplicate":         {{"profile"}, {"profile"}},
			"ancestor first":    {{"profile"}, {"profile", "level"}},
			"descendant first":  {{"profile", "level"}, {"profile"}},
			"empty path":        {{}, {"profile"}},
			"second empty path": {{"profile"}, {}},
		} {
			t.Run(name, func(t *testing.T) {
				patch := &cdcv2.RecordPatch{}
				for _, segments := range paths {
					patch.Changes = append(patch.Changes, &cdcv2.FieldChange{Path: &cdcv2.FieldPath{Segments: segments}, Before: state, After: cdcV2Present(cdcV2String("y"))})
				}
				require.Error(t, cdcV2ValidatePatch(patch))
			})
		}
	})

	t.Run("non-transition and missing state", func(t *testing.T) {
		equal := &cdcv2.RecordPatch{Changes: []*cdcv2.FieldChange{{Path: &cdcv2.FieldPath{Segments: []string{"x"}}, Before: cdcV2Present(cdcV2String("same")), After: cdcV2Present(cdcV2String("same"))}}}
		require.EqualError(t, cdcV2ValidatePatch(equal), "patch change 0 does not change state")
		absentPair := &cdcv2.RecordPatch{Changes: []*cdcv2.FieldChange{{Path: &cdcv2.FieldPath{Segments: []string{"x"}}, Before: cdcV2Absent(), After: cdcV2Absent()}}}
		require.EqualError(t, cdcV2ValidatePatch(absentPair), "patch change 0 does not change state")
		missing := &cdcv2.RecordPatch{Changes: []*cdcv2.FieldChange{{Path: &cdcv2.FieldPath{Segments: []string{"x"}}, Before: &cdcv2.FieldState{}, After: cdcV2Absent()}}}
		require.EqualError(t, cdcV2ValidatePatch(missing), "patch change 0 before state is unspecified")
		unsetKind := &cdcv2.RecordPatch{Changes: []*cdcv2.FieldChange{{Path: &cdcv2.FieldPath{Segments: []string{"x"}}, Before: &cdcv2.FieldState{State: &cdcv2.FieldState_Value{Value: &cdcv2.Value{}}}, After: cdcV2Absent()}}}
		require.EqualError(t, cdcV2ValidatePatch(unsetKind), "patch change 0 before value kind is unspecified")
	})

	t.Run("missing base and keyless replay", func(t *testing.T) {
		rows := make(cdcV2Rows)
		require.EqualError(t, cdcV2ApplyRecord(rows, delta.GetEvents()[1].GetSource(), firstUpdate), "UPDATE delta requires materialized base state")
		keyless := cdcV2Unpack(t, delta.GetEvents()[0])
		keyless.Key = nil
		require.EqualError(t, cdcV2ApplyRecord(rows, delta.GetEvents()[0].GetSource(), keyless), "SNAPSHOT_READ is keyless and cannot be replayed")
		_, err := cdcV2FullToDelta(&cdcv2.ChangeRecord{Operation: cdcv2.Operation_OPERATION_UPDATE, Representation: &cdcv2.ChangeRecord_Full{Full: &cdcv2.FullChange{After: initial}}}, nil)
		require.EqualError(t, err, "UPDATE requires complete before or materialized base state")
		_, err = cdcV2DeltaToFull(firstUpdate, nil)
		require.EqualError(t, err, "UPDATE requires materialized base state")
		deleteRecord := cdcV2Unpack(t, delta.GetEvents()[4])
		fullDelete, err := cdcV2DeltaToFull(deleteRecord, nil)
		require.NoError(t, err)
		require.NotNil(t, fullDelete.GetFull())
		require.Nil(t, fullDelete.GetFull().GetBefore())
		deltaDelete, err := cdcV2FullToDelta(fullDelete, nil)
		require.NoError(t, err)
		require.True(t, proto.Equal(deleteRecord, deltaDelete))
	})

	t.Run("lists and maps are atomic", func(t *testing.T) {
		tags := cdcV2FindField(initial, "tags").GetValue()
		invalidTraversal := &cdcv2.RecordPatch{Changes: []*cdcv2.FieldChange{{
			Path: &cdcv2.FieldPath{Segments: []string{"tags", "0"}}, Before: cdcV2Present(cdcV2String("vip")), After: cdcV2Present(cdcV2String("other")),
		}}}
		_, err := cdcV2ApplyPatch(initial, invalidTraversal)
		require.ErrorContains(t, err, "does not traverse a record")
		replacement := proto.Clone(tags).(*cdcv2.Value)
		replacement.GetListValue().Values = append(replacement.GetListValue().Values, cdcV2String("atomic"))
		attributes := cdcV2FindField(initial, "attributes").GetValue()
		replacementMap := proto.Clone(attributes).(*cdcv2.Value)
		replacementMap.GetMapValue().Entries = append(replacementMap.GetMapValue().Entries, &cdcv2.MapEntry{Key: cdcV2String("atomic"), Value: cdcV2String("map")})
		atomic := &cdcv2.RecordPatch{Changes: []*cdcv2.FieldChange{
			{Path: &cdcv2.FieldPath{Segments: []string{"tags"}}, Before: cdcV2Present(tags), After: cdcV2Present(replacement)},
			{Path: &cdcv2.FieldPath{Segments: []string{"attributes"}}, Before: cdcV2Present(attributes), After: cdcV2Present(replacementMap)},
		}}
		updated, err := cdcV2ApplyPatch(initial, atomic)
		require.NoError(t, err)
		require.Len(t, cdcV2FindField(updated, "tags").GetValue().GetListValue().GetValues(), 4)
		require.Len(t, cdcV2FindField(updated, "attributes").GetValue().GetMapValue().GetEntries(), 3)

		baseBeforeFailure := proto.Clone(initial).(*cdcv2.Record)
		partiallyValid := &cdcv2.RecordPatch{Changes: []*cdcv2.FieldChange{
			{Path: &cdcv2.FieldPath{Segments: []string{"tags"}}, Before: cdcV2Present(tags), After: cdcV2Present(replacement)},
			{Path: &cdcv2.FieldPath{Segments: []string{"attributes", "0"}}, Before: cdcV2Present(cdcV2String("x")), After: cdcV2Present(cdcV2String("y"))},
		}}
		_, err = cdcV2ApplyPatch(initial, partiallyValid)
		require.Error(t, err)
		require.True(t, proto.Equal(baseBeforeFailure, initial), "failed patches must not partially mutate their base")
	})

	t.Run("literal punctuation and numeric segments", func(t *testing.T) {
		base := &cdcv2.Record{Fields: []*cdcv2.RecordField{{Name: "a.b", Value: &cdcv2.Value{Kind: &cdcv2.Value_RecordValue{RecordValue: &cdcv2.Record{Fields: []*cdcv2.RecordField{{Name: "0", Value: cdcV2String("before")}}}}}}}}
		patch := &cdcv2.RecordPatch{Changes: []*cdcv2.FieldChange{{Path: &cdcv2.FieldPath{Segments: []string{"a.b", "0"}}, Before: cdcV2Present(cdcV2String("before")), After: cdcV2Present(cdcV2String("after"))}}}
		updated, err := cdcV2ApplyPatch(base, patch)
		require.NoError(t, err)
		nested := cdcV2FindField(updated, "a.b").GetValue().GetRecordValue()
		require.Equal(t, "after", cdcV2FindField(nested, "0").GetValue().GetStringValue())
	})

	t.Run("empty update is a semantic no-op", func(t *testing.T) {
		updated, err := cdcV2ApplyPatch(initial, &cdcv2.RecordPatch{})
		require.NoError(t, err)
		require.True(t, proto.Equal(initial, updated))
	})

	t.Run("unknown effects fail closed", func(t *testing.T) {
		wire := protowire.AppendTag(nil, 1000, protowire.BytesType)
		wire = protowire.AppendString(wire, "future delta effect")
		var futureDelta cdcv2.DeltaChange
		require.NoError(t, proto.Unmarshal(wire, &futureDelta))
		unknown := proto.Clone(firstUpdate).(*cdcv2.ChangeRecord)
		unknown.Representation = &cdcv2.ChangeRecord_Delta{Delta: &futureDelta}
		require.ErrorContains(t, cdcV2ApplyRecord(make(cdcV2Rows), delta.GetEvents()[1].GetSource(), unknown), "unknown delta effect")

		var futureState cdcv2.FieldState
		require.NoError(t, proto.Unmarshal(wire, &futureState))
		patch := &cdcv2.RecordPatch{Changes: []*cdcv2.FieldChange{{Path: &cdcv2.FieldPath{Segments: []string{"x"}}, Before: &futureState, After: cdcV2Absent()}}}
		require.EqualError(t, cdcV2ValidatePatch(patch), "patch change 0 before state is unspecified")
	})
}

func TestCDCV2CanonicalValueEquality(t *testing.T) {
	t.Parallel()

	recordLeft := &cdcv2.Record{Fields: []*cdcv2.RecordField{
		{Name: "z", Value: cdcV2String("last")},
		{Name: "nested", Value: &cdcv2.Value{TypeName: "example.Nested", Kind: &cdcv2.Value_RecordValue{RecordValue: &cdcv2.Record{Fields: []*cdcv2.RecordField{
			{Name: "b", Value: &cdcv2.Value{Kind: &cdcv2.Value_Int32Value{Int32Value: 2}}},
			{Name: "a", Value: cdcV2String("first")},
		}}}}},
	}}
	recordRight := &cdcv2.Record{Fields: []*cdcv2.RecordField{
		{Name: "nested", Value: &cdcv2.Value{TypeName: "example.Nested", Kind: &cdcv2.Value_RecordValue{RecordValue: &cdcv2.Record{Fields: []*cdcv2.RecordField{
			{Name: "a", Value: cdcV2String("first")},
			{Name: "b", Value: &cdcv2.Value{Kind: &cdcv2.Value_Int32Value{Int32Value: 2}}},
		}}}}},
		{Name: "z", Value: cdcV2String("last")},
	}}
	mapLeft := &cdcv2.Value{Kind: &cdcv2.Value_MapValue{MapValue: &cdcv2.MapValue{Entries: []*cdcv2.MapEntry{
		{Key: cdcV2String("b"), Value: &cdcv2.Value{Kind: &cdcv2.Value_Uint32Value{Uint32Value: 2}}},
		{Key: cdcV2String("a"), Value: &cdcv2.Value{Kind: &cdcv2.Value_NullValue{NullValue: &cdcv2.NullValue{}}}},
	}}}}
	mapRight := &cdcv2.Value{Kind: &cdcv2.Value_MapValue{MapValue: &cdcv2.MapValue{Entries: []*cdcv2.MapEntry{
		{Key: cdcV2String("a"), Value: &cdcv2.Value{Kind: &cdcv2.Value_NullValue{NullValue: &cdcv2.NullValue{}}}},
		{Key: cdcV2String("b"), Value: &cdcv2.Value{Kind: &cdcv2.Value_Uint32Value{Uint32Value: 2}}},
	}}}}
	precisionZero := uint32(0)
	cases := []struct {
		name        string
		left, right *cdcv2.Value
		equal       bool
	}{
		{"null", &cdcv2.Value{Kind: &cdcv2.Value_NullValue{NullValue: &cdcv2.NullValue{}}}, &cdcv2.Value{Kind: &cdcv2.Value_NullValue{NullValue: &cdcv2.NullValue{}}}, true},
		{"bool", &cdcv2.Value{Kind: &cdcv2.Value_BoolValue{BoolValue: true}}, &cdcv2.Value{Kind: &cdcv2.Value_BoolValue{BoolValue: true}}, true},
		{"bool differs", &cdcv2.Value{Kind: &cdcv2.Value_BoolValue{BoolValue: true}}, &cdcv2.Value{Kind: &cdcv2.Value_BoolValue{BoolValue: false}}, false},
		{"int32 minimum", &cdcv2.Value{Kind: &cdcv2.Value_Int32Value{Int32Value: -1 << 31}}, &cdcv2.Value{Kind: &cdcv2.Value_Int32Value{Int32Value: -1 << 31}}, true},
		{"int64 minimum", &cdcv2.Value{Kind: &cdcv2.Value_Int64Value{Int64Value: -1 << 63}}, &cdcv2.Value{Kind: &cdcv2.Value_Int64Value{Int64Value: -1 << 63}}, true},
		{"uint32 maximum", &cdcv2.Value{Kind: &cdcv2.Value_Uint32Value{Uint32Value: ^uint32(0)}}, &cdcv2.Value{Kind: &cdcv2.Value_Uint32Value{Uint32Value: ^uint32(0)}}, true},
		{"uint64 maximum", &cdcv2.Value{Kind: &cdcv2.Value_Uint64Value{Uint64Value: ^uint64(0)}}, &cdcv2.Value{Kind: &cdcv2.Value_Uint64Value{Uint64Value: ^uint64(0)}}, true},
		{"integer kind exact", &cdcv2.Value{Kind: &cdcv2.Value_Int32Value{Int32Value: 1}}, &cdcv2.Value{Kind: &cdcv2.Value_Int64Value{Int64Value: 1}}, false},
		{"float32 NaNs", &cdcv2.Value{Kind: &cdcv2.Value_Float32Value{Float32Value: math.Float32frombits(0x7fc00001)}}, &cdcv2.Value{Kind: &cdcv2.Value_Float32Value{Float32Value: math.Float32frombits(0x7fffffff)}}, true},
		{"float32 positive infinity", &cdcv2.Value{Kind: &cdcv2.Value_Float32Value{Float32Value: float32(math.Inf(1))}}, &cdcv2.Value{Kind: &cdcv2.Value_Float32Value{Float32Value: float32(math.Inf(1))}}, true},
		{"float32 negative infinity", &cdcv2.Value{Kind: &cdcv2.Value_Float32Value{Float32Value: float32(math.Inf(-1))}}, &cdcv2.Value{Kind: &cdcv2.Value_Float32Value{Float32Value: float32(math.Inf(-1))}}, true},
		{"float32 signed zero", &cdcv2.Value{Kind: &cdcv2.Value_Float32Value{Float32Value: 0}}, &cdcv2.Value{Kind: &cdcv2.Value_Float32Value{Float32Value: float32(math.Copysign(0, -1))}}, false},
		{"float64 NaNs", &cdcv2.Value{Kind: &cdcv2.Value_Float64Value{Float64Value: math.Float64frombits(0x7ff8000000000001)}}, &cdcv2.Value{Kind: &cdcv2.Value_Float64Value{Float64Value: math.Float64frombits(0x7fffffffffffffff)}}, true},
		{"float64 positive infinity", &cdcv2.Value{Kind: &cdcv2.Value_Float64Value{Float64Value: math.Inf(1)}}, &cdcv2.Value{Kind: &cdcv2.Value_Float64Value{Float64Value: math.Inf(1)}}, true},
		{"float64 negative infinity", &cdcv2.Value{Kind: &cdcv2.Value_Float64Value{Float64Value: math.Inf(-1)}}, &cdcv2.Value{Kind: &cdcv2.Value_Float64Value{Float64Value: math.Inf(-1)}}, true},
		{"float64 signed zero", &cdcv2.Value{Kind: &cdcv2.Value_Float64Value{Float64Value: 0}}, &cdcv2.Value{Kind: &cdcv2.Value_Float64Value{Float64Value: math.Copysign(0, -1)}}, false},
		{"float kind exact", &cdcv2.Value{Kind: &cdcv2.Value_Float32Value{Float32Value: 1.5}}, &cdcv2.Value{Kind: &cdcv2.Value_Float64Value{Float64Value: 1.5}}, false},
		{"string", cdcV2String("λ"), cdcV2String("λ"), true},
		{"bytes", &cdcv2.Value{Kind: &cdcv2.Value_BytesValue{BytesValue: []byte{0, 0x80, 0xff}}}, &cdcv2.Value{Kind: &cdcv2.Value_BytesValue{BytesValue: []byte{0, 0x80, 0xff}}}, true},
		{"decimal negative scale", &cdcv2.Value{Kind: &cdcv2.Value_DecimalValue{DecimalValue: &cdcv2.DecimalValue{Value: "1200", Scale: -2}}}, &cdcv2.Value{Kind: &cdcv2.Value_DecimalValue{DecimalValue: &cdcv2.DecimalValue{Value: "1200", Scale: -2}}}, true},
		{"decimal precision presence", &cdcv2.Value{Kind: &cdcv2.Value_DecimalValue{DecimalValue: &cdcv2.DecimalValue{Value: "0", Precision: nil}}}, &cdcv2.Value{Kind: &cdcv2.Value_DecimalValue{DecimalValue: &cdcv2.DecimalValue{Value: "0", Precision: &precisionZero}}}, false},
		{"timestamp", &cdcv2.Value{Kind: &cdcv2.Value_TimestampValue{TimestampValue: &timestamppb.Timestamp{Seconds: -1, Nanos: 999999999}}}, &cdcv2.Value{Kind: &cdcv2.Value_TimestampValue{TimestampValue: &timestamppb.Timestamp{Seconds: -1, Nanos: 999999999}}}, true},
		{"nested record order", &cdcv2.Value{Kind: &cdcv2.Value_RecordValue{RecordValue: recordLeft}}, &cdcv2.Value{Kind: &cdcv2.Value_RecordValue{RecordValue: recordRight}}, true},
		{"ordered list", &cdcv2.Value{Kind: &cdcv2.Value_ListValue{ListValue: &cdcv2.ListValue{Values: []*cdcv2.Value{cdcV2String("a"), cdcV2String("b")}}}}, &cdcv2.Value{Kind: &cdcv2.Value_ListValue{ListValue: &cdcv2.ListValue{Values: []*cdcv2.Value{cdcV2String("b"), cdcV2String("a")}}}}, false},
		{"unordered map", mapLeft, mapRight, true},
		{"type name exact", &cdcv2.Value{TypeName: "one", Kind: &cdcv2.Value_RecordValue{RecordValue: recordLeft}}, &cdcv2.Value{TypeName: "two", Kind: &cdcv2.Value_RecordValue{RecordValue: recordRight}}, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			equal, err := cdcV2ValuesEqual(test.left, test.right)
			require.NoError(t, err)
			require.Equal(t, test.equal, equal)
		})
	}

	withUnknown := proto.Clone(cdcV2String("known")).(*cdcv2.Value)
	withUnknown.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 1000, protowire.VarintType), 1))
	equal, err := cdcV2ValuesEqual(cdcV2String("known"), withUnknown)
	require.NoError(t, err)
	require.True(t, equal, "ordinary protobuf unknown fields do not alter known semantics")

	unknownWire := protowire.AppendString(protowire.AppendTag(nil, 1000, protowire.BytesType), "future value kind")
	var unknown cdcv2.Value
	require.NoError(t, proto.Unmarshal(unknownWire, &unknown))
	_, err = cdcV2CanonicalValue(&unknown)
	require.ErrorContains(t, err, "unspecified or unknown")

	duplicateRecord := &cdcv2.Record{Fields: []*cdcv2.RecordField{
		{Name: "same", Value: cdcV2String("one")},
		{Name: "same", Value: cdcV2String("two")},
	}}
	_, err = cdcV2CanonicalRecord(duplicateRecord)
	require.ErrorContains(t, err, "duplicated")
	duplicateMap := &cdcv2.Value{Kind: &cdcv2.Value_MapValue{MapValue: &cdcv2.MapValue{Entries: []*cdcv2.MapEntry{
		{Key: &cdcv2.Value{Kind: &cdcv2.Value_RecordValue{RecordValue: recordLeft}}, Value: cdcV2String("one")},
		{Key: &cdcv2.Value{Kind: &cdcv2.Value_RecordValue{RecordValue: recordRight}}, Value: cdcV2String("two")},
	}}}}
	_, err = cdcV2CanonicalValue(duplicateMap)
	require.ErrorContains(t, err, "map key")

	keyRecord := &cdcv2.ChangeRecord{Key: recordLeft, DataCollection: &cdcv2.DataCollection{Id: "accounts"}}
	reorderedKeyRecord := &cdcv2.ChangeRecord{Key: recordRight, DataCollection: &cdcv2.DataCollection{Id: "accounts"}}
	leftKey, err := cdcV2RowKey("urn:source:a", keyRecord)
	require.NoError(t, err)
	rightKey, err := cdcV2RowKey("urn:source:a", reorderedKeyRecord)
	require.NoError(t, err)
	require.Equal(t, leftKey, rightKey)
	otherSourceKey, err := cdcV2RowKey("urn:source:b", reorderedKeyRecord)
	require.NoError(t, err)
	require.NotEqual(t, leftKey, otherSourceKey)

	before := &cdcv2.Record{Fields: []*cdcv2.RecordField{{Name: "typed", Value: &cdcv2.Value{TypeName: "old", Kind: &cdcv2.Value_RecordValue{RecordValue: recordLeft}}}}}
	after := &cdcv2.Record{Fields: []*cdcv2.RecordField{{Name: "typed", Value: &cdcv2.Value{TypeName: "new", Kind: &cdcv2.Value_RecordValue{RecordValue: recordRight}}}}}
	changes, err := cdcV2DiffRecords(nil, before, after)
	require.NoError(t, err)
	require.Len(t, changes, 1)
	require.Equal(t, []string{"typed"}, changes[0].GetPath().GetSegments(), "type_name changes replace the typed record atomically")
	mapOrderChanges, err := cdcV2DiffRecords(nil,
		&cdcv2.Record{Fields: []*cdcv2.RecordField{{Name: "map", Value: mapLeft}}},
		&cdcv2.Record{Fields: []*cdcv2.RecordField{{Name: "map", Value: mapRight}}},
	)
	require.NoError(t, err)
	require.Empty(t, mapOrderChanges, "wire-only map entry reordering is not a v2 change")
}

func TestCDCV2MixedReplayAndSourceScoping(t *testing.T) {
	t.Parallel()

	_, full := cdcV2LoadBatch(t, "full.binpb")
	_, delta := cdcV2LoadBatch(t, "delta.binpb")
	mixed := &cloudeventsv1.CloudEventBatch{Events: []*cloudeventsv1.CloudEvent{
		full.GetEvents()[0],
		delta.GetEvents()[1],
		full.GetEvents()[3],
		delta.GetEvents()[4],
		full.GetEvents()[5],
		delta.GetEvents()[6],
		full.GetEvents()[7],
	}}
	snapshots, err := cdcV2Replay(mixed)
	require.NoError(t, err)
	require.Equal(t, [][]int64{{42}, {42}, {42}, {}, {84}, {}, {}}, [][]int64{
		cdcV2StateIDs(snapshots[0]),
		cdcV2StateIDs(snapshots[1]),
		cdcV2StateIDs(snapshots[2]),
		cdcV2StateIDs(snapshots[3]),
		cdcV2StateIDs(snapshots[4]),
		cdcV2StateIDs(snapshots[5]),
		cdcV2StateIDs(snapshots[6]),
	})

	fullUpdate := cdcV2Unpack(t, full.GetEvents()[3])
	rows := make(cdcV2Rows)
	require.NoError(t, cdcV2ApplyRecord(rows, full.GetEvents()[3].GetSource(), fullUpdate), "full UPDATE is an outcome anchor")
	require.Len(t, rows, 1)
	rows = make(cdcV2Rows)
	require.NoError(t, cdcV2ApplyRecord(rows, delta.GetEvents()[4].GetSource(), cdcV2Unpack(t, delta.GetEvents()[4])), "DELETE may establish absence")
	require.Empty(t, rows)

	create := proto.Clone(cdcV2Unpack(t, full.GetEvents()[0])).(*cdcv2.ChangeRecord)
	create.Operation = cdcv2.Operation_OPERATION_CREATE
	rows = make(cdcV2Rows)
	require.NoError(t, cdcV2ApplyRecord(rows, "urn:create", create))
	require.EqualError(t, cdcV2ApplyRecord(rows, "urn:create", create), "CREATE requires an absent materialized key")
	snapshot := proto.Clone(create).(*cdcv2.ChangeRecord)
	snapshot.Operation = cdcv2.Operation_OPERATION_SNAPSHOT_READ
	require.NoError(t, cdcV2ApplyRecord(rows, "urn:create", snapshot), "snapshot replaces an existing row")

	anchor := cdcV2Unpack(t, full.GetEvents()[0])
	fromA := cdcV2EventWithRecord(t, full.GetEvents()[0], "urn:source:a", "same-id", anchor)
	fromB := cdcV2EventWithRecord(t, full.GetEvents()[0], "urn:source:b", "same-id", anchor)
	truncateA := cdcV2EventWithRecord(t, full.GetEvents()[6], "urn:source:a", "truncate", cdcV2Unpack(t, full.GetEvents()[6]))
	scopedBatch := &cloudeventsv1.CloudEventBatch{Events: []*cloudeventsv1.CloudEvent{fromA, fromB, truncateA}}
	scoped, err := cdcV2Replay(scopedBatch)
	require.NoError(t, err)
	require.Len(t, scoped[0], 1)
	require.Len(t, scoped[1], 2, "the same collection/key in another CloudEvent source is distinct state")
	require.Len(t, scoped[2], 1, "TRUNCATE clears only its CloudEvent source collection")
	remainingKey, err := cdcV2RowKey(fromB.GetSource(), anchor)
	require.NoError(t, err)
	require.Contains(t, scoped[2], remainingKey)
	atSourceA, err := cdcV2StateAtPosition(scopedBatch, fromA.GetSource(), cdcV2FixturePosition("0001"))
	require.NoError(t, err)
	require.Len(t, atSourceA, 1)
	atSourceB, err := cdcV2StateAtPosition(scopedBatch, fromB.GetSource(), cdcV2FixturePosition("0001"))
	require.NoError(t, err)
	require.Len(t, atSourceB, 2, "opaque tuple collisions are disambiguated by CloudEvent source")
}

func TestCDCV2UnknownFieldsAndForwardCompatibility(t *testing.T) {
	t.Parallel()

	_, delta := cdcV2LoadBatch(t, "delta.binpb")
	record := cdcV2Unpack(t, delta.GetEvents()[1])
	wire, err := proto.Marshal(record)
	require.NoError(t, err)
	wire = protowire.AppendTag(wire, 1000, protowire.BytesType)
	wire = protowire.AppendString(wire, "future ChangeRecord field")
	var older cdcv2.ChangeRecord
	require.NoError(t, proto.Unmarshal(wire, &older))
	require.NotEmpty(t, older.ProtoReflect().GetUnknown())
	relayedWire, err := proto.Marshal(&older)
	require.NoError(t, err)
	var relayed cdcv2.ChangeRecord
	require.NoError(t, proto.Unmarshal(relayedWire, &relayed))
	require.Equal(t, older.ProtoReflect().GetUnknown(), relayed.ProtoReflect().GetUnknown())

	extensionWire := protowire.AppendString(protowire.AppendTag(nil, 1000, protowire.BytesType), "future source extension")
	var futureExtension cdcv2.SourceExtension
	require.NoError(t, proto.Unmarshal(extensionWire, &futureExtension))
	withFutureExtension := proto.Clone(record).(*cdcv2.ChangeRecord)
	withFutureExtension.SourceExtension = &futureExtension
	decoded, err := cdcV2DecodeEvent(cdcV2EventWithRecord(t, delta.GetEvents()[1], delta.GetEvents()[1].GetSource(), "future-extension", withFutureExtension))
	require.NoError(t, err, "unknown source-extension variants are isolated metadata, not state effects")
	require.NotEmpty(t, decoded.GetSourceExtension().ProtoReflect().GetUnknown())

	_, full := cdcV2LoadBatch(t, "full.binpb")
	duplicateField := cdcV2Unpack(t, full.GetEvents()[0])
	duplicateField.GetFull().GetAfter().Fields = append(duplicateField.GetFull().GetAfter().Fields,
		proto.Clone(duplicateField.GetFull().GetAfter().Fields[0]).(*cdcv2.RecordField))
	_, err = cdcV2DecodeEvent(cdcV2EventWithRecord(t, full.GetEvents()[0], full.GetEvents()[0].GetSource(), "duplicate-field", duplicateField))
	require.ErrorContains(t, err, "duplicated")

	duplicateMapKey := cdcV2Unpack(t, full.GetEvents()[0])
	attributes := cdcV2FindField(duplicateMapKey.GetFull().GetAfter(), "attributes").GetValue().GetMapValue()
	attributes.Entries = append(attributes.Entries, proto.Clone(attributes.Entries[0]).(*cdcv2.MapEntry))
	_, err = cdcV2DecodeEvent(cdcV2EventWithRecord(t, full.GetEvents()[0], full.GetEvents()[0].GetSource(), "duplicate-map-key", duplicateMapKey))
	require.ErrorContains(t, err, "map key")
}

func cdcV2FixtureDir(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	require.True(t, ok)
	return filepath.Join(filepath.Dir(filename), "..", "testdata", "cdc", "v2")
}

func cdcV2LoadManifest(t *testing.T) cdcV2Manifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(cdcV2FixtureDir(t), "manifest.json"))
	require.NoError(t, err)
	var manifest cdcV2Manifest
	require.NoError(t, json.Unmarshal(data, &manifest))
	return manifest
}

func cdcV2LoadBatch(t *testing.T, name string) ([]byte, *cloudeventsv1.CloudEventBatch) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(cdcV2FixtureDir(t), name))
	require.NoError(t, err)
	var batch cloudeventsv1.CloudEventBatch
	require.NoError(t, proto.Unmarshal(data, &batch))
	return data, &batch
}

func cdcV2AssertManifestFile(t *testing.T, manifest cdcV2Manifest, name string, data []byte) {
	t.Helper()
	index := slices.IndexFunc(manifest.Files, func(file cdcV2ManifestFile) bool {
		return file.Path == name
	})
	require.NotEqual(t, -1, index)
	file := manifest.Files[index]
	sum := sha256.Sum256(data)
	require.Equal(t, file.SHA256, hex.EncodeToString(sum[:]))
	require.Len(t, data, file.Size)
}

func cdcV2Unpack(t *testing.T, event *cloudeventsv1.CloudEvent) *cdcv2.ChangeRecord {
	t.Helper()
	var record cdcv2.ChangeRecord
	require.NoError(t, event.GetProtoData().UnmarshalTo(&record))
	return &record
}

func cdcV2EventWithRecord(t *testing.T, template *cloudeventsv1.CloudEvent, source, id string, record *cdcv2.ChangeRecord) *cloudeventsv1.CloudEvent {
	t.Helper()
	event := proto.Clone(template).(*cloudeventsv1.CloudEvent)
	event.Source = source
	event.Id = id
	data, err := anypb.New(record)
	require.NoError(t, err)
	event.Data = &cloudeventsv1.CloudEvent_ProtoData{ProtoData: data}
	return event
}

// cdcV2CanonicalRecord builds a known-field-only semantic form. Deterministic
// protobuf bytes are used only after applying the contract's recursive
// equality rules; raw serialized messages are never compared as semantics.
func cdcV2CanonicalRecord(record *cdcv2.Record) (*cdcv2.Record, error) {
	if record == nil {
		return nil, errors.New("record is missing")
	}
	canonical := &cdcv2.Record{Fields: make([]*cdcv2.RecordField, 0, len(record.GetFields()))}
	seen := make(map[string]struct{}, len(record.GetFields()))
	for index, field := range record.GetFields() {
		if field == nil || field.GetValue() == nil {
			return nil, fmt.Errorf("record field %d has no value", index)
		}
		if _, duplicate := seen[field.GetName()]; duplicate {
			return nil, fmt.Errorf("record field name %q is duplicated", field.GetName())
		}
		seen[field.GetName()] = struct{}{}
		value, err := cdcV2CanonicalValue(field.GetValue())
		if err != nil {
			return nil, fmt.Errorf("record field %q: %w", field.GetName(), err)
		}
		canonical.Fields = append(canonical.Fields, &cdcv2.RecordField{Name: field.GetName(), Value: value})
	}
	slices.SortFunc(canonical.Fields, func(left, right *cdcv2.RecordField) int {
		return strings.Compare(left.GetName(), right.GetName())
	})
	return canonical, nil
}

func cdcV2CanonicalValue(value *cdcv2.Value) (*cdcv2.Value, error) {
	if value == nil || value.GetKind() == nil {
		return nil, errors.New("value kind is unspecified or unknown")
	}
	canonical := &cdcv2.Value{TypeName: value.GetTypeName()}
	switch kind := value.GetKind().(type) {
	case *cdcv2.Value_NullValue:
		canonical.Kind = &cdcv2.Value_NullValue{NullValue: &cdcv2.NullValue{}}
	case *cdcv2.Value_BoolValue:
		canonical.Kind = &cdcv2.Value_BoolValue{BoolValue: kind.BoolValue}
	case *cdcv2.Value_Int32Value:
		canonical.Kind = &cdcv2.Value_Int32Value{Int32Value: kind.Int32Value}
	case *cdcv2.Value_Int64Value:
		canonical.Kind = &cdcv2.Value_Int64Value{Int64Value: kind.Int64Value}
	case *cdcv2.Value_Uint32Value:
		canonical.Kind = &cdcv2.Value_Uint32Value{Uint32Value: kind.Uint32Value}
	case *cdcv2.Value_Uint64Value:
		canonical.Kind = &cdcv2.Value_Uint64Value{Uint64Value: kind.Uint64Value}
	case *cdcv2.Value_Float32Value:
		floatValue := kind.Float32Value
		if math.IsNaN(float64(floatValue)) {
			floatValue = math.Float32frombits(0x7fc00000)
		}
		canonical.Kind = &cdcv2.Value_Float32Value{Float32Value: floatValue}
	case *cdcv2.Value_Float64Value:
		floatValue := kind.Float64Value
		if math.IsNaN(floatValue) {
			floatValue = math.Float64frombits(0x7ff8000000000000)
		}
		canonical.Kind = &cdcv2.Value_Float64Value{Float64Value: floatValue}
	case *cdcv2.Value_StringValue:
		canonical.Kind = &cdcv2.Value_StringValue{StringValue: kind.StringValue}
	case *cdcv2.Value_BytesValue:
		canonical.Kind = &cdcv2.Value_BytesValue{BytesValue: append([]byte(nil), kind.BytesValue...)}
	case *cdcv2.Value_DecimalValue:
		if kind.DecimalValue == nil {
			return nil, errors.New("decimal value is missing")
		}
		decimal := &cdcv2.DecimalValue{Value: kind.DecimalValue.GetValue(), Scale: kind.DecimalValue.GetScale()}
		if kind.DecimalValue.Precision != nil {
			precision := kind.DecimalValue.GetPrecision()
			decimal.Precision = &precision
		}
		canonical.Kind = &cdcv2.Value_DecimalValue{DecimalValue: decimal}
	case *cdcv2.Value_TimestampValue:
		if kind.TimestampValue == nil || !kind.TimestampValue.IsValid() {
			return nil, errors.New("timestamp value is invalid")
		}
		canonical.Kind = &cdcv2.Value_TimestampValue{TimestampValue: &timestamppb.Timestamp{
			Seconds: kind.TimestampValue.GetSeconds(),
			Nanos:   kind.TimestampValue.GetNanos(),
		}}
	case *cdcv2.Value_RecordValue:
		record, err := cdcV2CanonicalRecord(kind.RecordValue)
		if err != nil {
			return nil, err
		}
		canonical.Kind = &cdcv2.Value_RecordValue{RecordValue: record}
	case *cdcv2.Value_ListValue:
		if kind.ListValue == nil {
			return nil, errors.New("list value is missing")
		}
		list := &cdcv2.ListValue{Values: make([]*cdcv2.Value, 0, len(kind.ListValue.GetValues()))}
		for index, item := range kind.ListValue.GetValues() {
			canonicalItem, err := cdcV2CanonicalValue(item)
			if err != nil {
				return nil, fmt.Errorf("list element %d: %w", index, err)
			}
			list.Values = append(list.Values, canonicalItem)
		}
		canonical.Kind = &cdcv2.Value_ListValue{ListValue: list}
	case *cdcv2.Value_MapValue:
		if kind.MapValue == nil {
			return nil, errors.New("map value is missing")
		}
		type canonicalEntry struct {
			entry   *cdcv2.MapEntry
			keyWire []byte
		}
		entries := make([]canonicalEntry, 0, len(kind.MapValue.GetEntries()))
		for index, entry := range kind.MapValue.GetEntries() {
			if entry == nil || entry.GetKey() == nil || entry.GetValue() == nil {
				return nil, fmt.Errorf("map entry %d is incomplete", index)
			}
			key, err := cdcV2CanonicalValue(entry.GetKey())
			if err != nil {
				return nil, fmt.Errorf("map entry %d key: %w", index, err)
			}
			entryValue, err := cdcV2CanonicalValue(entry.GetValue())
			if err != nil {
				return nil, fmt.Errorf("map entry %d value: %w", index, err)
			}
			keyWire, err := (proto.MarshalOptions{Deterministic: true}).Marshal(key)
			if err != nil {
				return nil, fmt.Errorf("map entry %d key: %w", index, err)
			}
			entries = append(entries, canonicalEntry{entry: &cdcv2.MapEntry{Key: key, Value: entryValue}, keyWire: keyWire})
		}
		slices.SortFunc(entries, func(left, right canonicalEntry) int {
			return bytes.Compare(left.keyWire, right.keyWire)
		})
		mapped := &cdcv2.MapValue{Entries: make([]*cdcv2.MapEntry, 0, len(entries))}
		for index, entry := range entries {
			if index > 0 && bytes.Equal(entries[index-1].keyWire, entry.keyWire) {
				return nil, fmt.Errorf("map key at canonical index %d is duplicated", index)
			}
			mapped.Entries = append(mapped.Entries, entry.entry)
		}
		canonical.Kind = &cdcv2.Value_MapValue{MapValue: mapped}
	default:
		return nil, errors.New("value kind is unknown")
	}
	return canonical, nil
}

func cdcV2CanonicalRecordBytes(record *cdcv2.Record) ([]byte, error) {
	canonical, err := cdcV2CanonicalRecord(record)
	if err != nil {
		return nil, err
	}
	return (proto.MarshalOptions{Deterministic: true}).Marshal(canonical)
}

func cdcV2CanonicalValueBytes(value *cdcv2.Value) ([]byte, error) {
	canonical, err := cdcV2CanonicalValue(value)
	if err != nil {
		return nil, err
	}
	return (proto.MarshalOptions{Deterministic: true}).Marshal(canonical)
}

func cdcV2RecordsEqual(left, right *cdcv2.Record) (bool, error) {
	leftWire, err := cdcV2CanonicalRecordBytes(left)
	if err != nil {
		return false, err
	}
	rightWire, err := cdcV2CanonicalRecordBytes(right)
	if err != nil {
		return false, err
	}
	return bytes.Equal(leftWire, rightWire), nil
}

func cdcV2ValuesEqual(left, right *cdcv2.Value) (bool, error) {
	leftWire, err := cdcV2CanonicalValueBytes(left)
	if err != nil {
		return false, err
	}
	rightWire, err := cdcV2CanonicalValueBytes(right)
	if err != nil {
		return false, err
	}
	return bytes.Equal(leftWire, rightWire), nil
}

func cdcV2ValidateShape(record *cdcv2.ChangeRecord, representation string) error {
	if record.GetCaptureTime() == nil || !record.GetCaptureTime().IsValid() {
		return errors.New("capture_time is required")
	}
	if record.GetSourceTime() != nil && !record.GetSourceTime().IsValid() {
		return errors.New("source_time is invalid")
	}
	if record.GetOperation() != cdcv2.Operation_OPERATION_SOURCE_MESSAGE && record.GetDataCollection() == nil {
		return errors.New("data_collection is required")
	}
	rowOperation := record.GetOperation() == cdcv2.Operation_OPERATION_CREATE ||
		record.GetOperation() == cdcv2.Operation_OPERATION_UPDATE ||
		record.GetOperation() == cdcv2.Operation_OPERATION_DELETE ||
		record.GetOperation() == cdcv2.Operation_OPERATION_SNAPSHOT_READ
	if representation != "" && rowOperation && record.GetKey() == nil {
		return fmt.Errorf("%s fixture requires key", record.GetOperation())
	}
	if representation == "full" && rowOperation && record.GetFull() == nil {
		return fmt.Errorf("%s requires full representation", record.GetOperation())
	}
	if representation == "delta" && rowOperation && record.GetDelta() == nil {
		return fmt.Errorf("%s requires delta representation", record.GetOperation())
	}
	if delta := record.GetDelta(); delta != nil && delta.GetChange() == nil {
		return errors.New("unknown delta effect")
	}
	switch record.GetOperation() {
	case cdcv2.Operation_OPERATION_CREATE, cdcv2.Operation_OPERATION_SNAPSHOT_READ:
		if full := record.GetFull(); full != nil {
			if full.GetBefore() != nil || full.GetAfter() == nil || full.GetChangedFields() != nil {
				return fmt.Errorf("%s has invalid full anchor", record.GetOperation())
			}
		} else if delta := record.GetDelta(); delta == nil || delta.GetResult() == nil {
			return fmt.Errorf("%s has invalid delta anchor", record.GetOperation())
		}
	case cdcv2.Operation_OPERATION_UPDATE:
		if full := record.GetFull(); full != nil {
			if full.GetAfter() == nil {
				return errors.New("UPDATE full representation requires after")
			}
		} else if delta := record.GetDelta(); delta == nil || delta.GetPatch() == nil {
			return errors.New("UPDATE delta representation requires patch")
		} else if err := cdcV2ValidatePatch(delta.GetPatch()); err != nil {
			return err
		}
	case cdcv2.Operation_OPERATION_DELETE:
		if full := record.GetFull(); full != nil {
			if full.GetAfter() != nil || full.GetChangedFields() != nil {
				return errors.New("DELETE full representation prohibits after and changed_fields")
			}
		} else if delta := record.GetDelta(); delta == nil || delta.GetDelete() == nil {
			return errors.New("DELETE delta representation requires delete marker")
		}
	case cdcv2.Operation_OPERATION_TRUNCATE:
		if record.GetKey() != nil || record.GetRepresentation() != nil || record.GetSourceMessage() != nil {
			return errors.New("TRUNCATE prohibits row representation")
		}
	case cdcv2.Operation_OPERATION_SOURCE_MESSAGE:
		if record.GetKey() != nil || record.GetRepresentation() != nil || record.GetSourceMessage() == nil {
			return errors.New("SOURCE_MESSAGE has invalid shape")
		}
	default:
		return errors.New("operation is unspecified")
	}
	type namedRecord struct {
		name   string
		record *cdcv2.Record
	}
	records := []namedRecord{{"key", record.GetKey()}}
	if full := record.GetFull(); full != nil {
		records = append(records,
			namedRecord{"full.before", full.GetBefore()},
			namedRecord{"full.after", full.GetAfter()},
		)
	}
	if delta := record.GetDelta(); delta != nil && delta.GetResult() != nil {
		records = append(records, namedRecord{"delta.result", delta.GetResult()})
	}
	for _, candidate := range records {
		if candidate.record == nil {
			continue
		}
		if _, err := cdcV2CanonicalRecord(candidate.record); err != nil {
			return fmt.Errorf("%s: %w", candidate.name, err)
		}
	}
	if record.GetSourceMessage() != nil {
		if _, err := cdcV2CanonicalValue(record.GetSourceMessage()); err != nil {
			return fmt.Errorf("source_message: %w", err)
		}
	}
	return nil
}

func cdcV2ValidatePatch(patch *cdcv2.RecordPatch) error {
	if patch == nil {
		return errors.New("patch is missing")
	}
	for index, change := range patch.GetChanges() {
		if change.GetPath() == nil || len(change.GetPath().GetSegments()) == 0 {
			return fmt.Errorf("patch change %d path is empty", index)
		}
		if change.GetBefore() == nil || change.GetBefore().GetState() == nil {
			return fmt.Errorf("patch change %d before state is unspecified", index)
		}
		if change.GetAfter() == nil || change.GetAfter().GetState() == nil {
			return fmt.Errorf("patch change %d after state is unspecified", index)
		}
		states := []struct {
			name  string
			state *cdcv2.FieldState
		}{{"before", change.GetBefore()}, {"after", change.GetAfter()}}
		for _, candidate := range states {
			if value := candidate.state.GetValue(); value != nil {
				if value.GetKind() == nil {
					return fmt.Errorf("patch change %d %s value kind is unspecified", index, candidate.name)
				}
				if _, err := cdcV2CanonicalValue(value); err != nil {
					return fmt.Errorf("patch change %d %s: %w", index, candidate.name, err)
				}
			}
		}
		equal, err := cdcV2FieldStatesEqual(change.GetBefore(), change.GetAfter())
		if err != nil {
			return fmt.Errorf("patch change %d: %w", index, err)
		}
		if equal {
			return fmt.Errorf("patch change %d does not change state", index)
		}
		for prior := range index {
			priorPath := patch.GetChanges()[prior].GetPath().GetSegments()
			currentPath := change.GetPath().GetSegments()
			if cdcV2PathPrefix(priorPath, currentPath) || cdcV2PathPrefix(currentPath, priorPath) {
				return fmt.Errorf("patch paths %d and %d overlap", prior, index)
			}
		}
	}
	return nil
}

func cdcV2PathPrefix(left, right []string) bool {
	return len(left) <= len(right) && slices.Equal(left, right[:len(left)])
}

func cdcV2FieldStatesEqual(left, right *cdcv2.FieldState) (bool, error) {
	if left.GetAbsent() != nil || right.GetAbsent() != nil {
		return left.GetAbsent() != nil && right.GetAbsent() != nil, nil
	}
	if left.GetValue() == nil || right.GetValue() == nil {
		return false, errors.New("field state is unspecified or unknown")
	}
	return cdcV2ValuesEqual(left.GetValue(), right.GetValue())
}

func cdcV2PatchPaths(patch *cdcv2.RecordPatch) [][]string {
	paths := make([][]string, 0, len(patch.GetChanges()))
	for _, change := range patch.GetChanges() {
		paths = append(paths, append([]string(nil), change.GetPath().GetSegments()...))
	}
	return paths
}

func cdcV2AssertWideRecord(t *testing.T, record *cdcv2.Record) {
	t.Helper()
	amount := cdcV2FindField(record, "account_balance").GetValue()
	require.Equal(t, "example.Decimal", amount.GetTypeName())
	require.Equal(t, "12345678901234567890.123400", amount.GetDecimalValue().GetValue())
	require.EqualValues(t, 6, amount.GetDecimalValue().GetScale())
	require.EqualValues(t, 38, amount.GetDecimalValue().GetPrecision())
	require.Equal(t, []byte{0x00, 0x7f, 0x80, 0xff}, cdcV2FindField(record, "avatar").GetValue().GetBytesValue())
	timestamp := cdcV2FindField(record, "created_at").GetValue().GetTimestampValue()
	require.Equal(t, int64(1_723_912_200), timestamp.GetSeconds())
	require.EqualValues(t, 987_654_321, timestamp.GetNanos())
	require.Equal(t, ^uint64(0), cdcV2FindField(record, "revision").GetValue().GetUint64Value())
	tags := cdcV2FindField(record, "tags").GetValue().GetListValue().GetValues()
	require.Len(t, tags, 3)
	require.NotNil(t, tags[1].GetNullValue())
	entries := cdcV2FindField(record, "attributes").GetValue().GetMapValue().GetEntries()
	require.Len(t, entries, 2)
	require.EqualValues(t, 7, entries[1].GetKey().GetInt32Value())
	require.Equal(t, []byte("exact"), entries[1].GetValue().GetBytesValue())
}

type cdcV2Rows map[string]*cdcv2.Record

// cdcV2Replay follows the sequence declared by this fixture's manifest and
// source stream. CloudEventBatch itself does not confer ordering semantics.
func cdcV2Replay(batch *cloudeventsv1.CloudEventBatch) ([]cdcV2Rows, error) {
	rows := make(cdcV2Rows)
	seen := make(map[string]bool)
	snapshots := make([]cdcV2Rows, 0, len(batch.GetEvents()))
	for _, event := range batch.GetEvents() {
		identity := event.GetSource() + "\x00" + event.GetId()
		if !seen[identity] {
			record, err := cdcV2DecodeEvent(event)
			if err != nil {
				return nil, err
			}
			if err := cdcV2ApplyRecord(rows, event.GetSource(), record); err != nil {
				return nil, fmt.Errorf("event %s: %w", event.GetId(), err)
			}
			seen[identity] = true
		}
		snapshots = append(snapshots, cdcV2CloneRows(rows))
	}
	return snapshots, nil
}

func cdcV2StateAtPosition(batch *cloudeventsv1.CloudEventBatch, source string, position *cdcv2.SourcePosition) (cdcV2Rows, error) {
	if position == nil {
		return nil, errors.New("source position is required")
	}
	rows := make(cdcV2Rows)
	seen := make(map[string]bool)
	for _, event := range batch.GetEvents() {
		identity := event.GetSource() + "\x00" + event.GetId()
		if seen[identity] {
			continue
		}
		record, err := cdcV2DecodeEvent(event)
		if err != nil {
			return nil, err
		}
		if err := cdcV2ApplyRecord(rows, event.GetSource(), record); err != nil {
			return nil, err
		}
		seen[identity] = true
		if event.GetSource() == source && cdcV2SourcePositionsEqual(record.GetSourcePosition(), position) {
			return cdcV2CloneRows(rows), nil
		}
	}
	return nil, errors.New("source position not found")
}

func cdcV2DecodeEvent(event *cloudeventsv1.CloudEvent) (*cdcv2.ChangeRecord, error) {
	if event.GetProtoData() == nil {
		return nil, errors.New("CloudEvent has no proto_data")
	}
	var record cdcv2.ChangeRecord
	if err := event.GetProtoData().UnmarshalTo(&record); err != nil {
		return nil, err
	}
	if err := cdcV2ValidateShape(&record, ""); err != nil {
		return nil, err
	}
	return &record, nil
}

func cdcV2SourcePositionsEqual(left, right *cdcv2.SourcePosition) bool {
	return left != nil && right != nil &&
		left.GetStream() == right.GetStream() &&
		left.GetFormat() == right.GetFormat() &&
		bytes.Equal(left.GetValue(), right.GetValue())
}

func cdcV2FixturePosition(value string) *cdcv2.SourcePosition {
	return &cdcv2.SourcePosition{
		Stream: "fixture-stream-0",
		Format: "application/x.invariant.fixture-position",
		Value:  []byte(value),
	}
}

func cdcV2ApplyRecord(rows cdcV2Rows, source string, record *cdcv2.ChangeRecord) error {
	if err := cdcV2ValidateShape(record, ""); err != nil {
		return err
	}
	operation := record.GetOperation()
	if operation == cdcv2.Operation_OPERATION_TRUNCATE {
		prefix := cdcV2CollectionPrefix(source, record.GetDataCollection().GetId())
		for key := range rows {
			if strings.HasPrefix(key, prefix) {
				delete(rows, key)
			}
		}
		return nil
	}
	if operation == cdcv2.Operation_OPERATION_SOURCE_MESSAGE {
		if record.GetSourceMessage() == nil {
			return errors.New("SOURCE_MESSAGE requires source_message")
		}
		return nil
	}
	if record.GetKey() == nil {
		return fmt.Errorf("%s is keyless and cannot be replayed", strings.TrimPrefix(operation.String(), "OPERATION_"))
	}
	key, err := cdcV2RowKey(source, record)
	if err != nil {
		return err
	}
	base, exists := rows[key]
	switch operation {
	case cdcv2.Operation_OPERATION_CREATE:
		if exists {
			return errors.New("CREATE requires an absent materialized key")
		}
		var result *cdcv2.Record
		if full := record.GetFull(); full != nil {
			result = full.GetAfter()
		} else {
			result = record.GetDelta().GetResult()
		}
		rows[key] = proto.Clone(result).(*cdcv2.Record)
	case cdcv2.Operation_OPERATION_SNAPSHOT_READ:
		var result *cdcv2.Record
		if full := record.GetFull(); full != nil {
			result = full.GetAfter()
		} else {
			result = record.GetDelta().GetResult()
		}
		rows[key] = proto.Clone(result).(*cdcv2.Record)
	case cdcv2.Operation_OPERATION_UPDATE:
		if full := record.GetFull(); full != nil {
			if exists && full.GetBefore() != nil {
				equal, equalErr := cdcV2RecordsEqual(base, full.GetBefore())
				if equalErr != nil {
					return equalErr
				}
				if !equal {
					return errors.New("UPDATE full before does not match materialized state")
				}
			}
			rows[key] = proto.Clone(full.GetAfter()).(*cdcv2.Record)
		} else if delta := record.GetDelta(); delta != nil && delta.GetPatch() != nil {
			if !exists {
				return errors.New("UPDATE delta requires materialized base state")
			}
			updated, err := cdcV2ApplyPatch(base, delta.GetPatch())
			if err != nil {
				return err
			}
			rows[key] = updated
		} else {
			return errors.New("UPDATE has unknown representation")
		}
	case cdcv2.Operation_OPERATION_DELETE:
		if full := record.GetFull(); full != nil && exists && full.GetBefore() != nil {
			equal, equalErr := cdcV2RecordsEqual(base, full.GetBefore())
			if equalErr != nil {
				return equalErr
			}
			if !equal {
				return errors.New("DELETE full before does not match materialized state")
			}
		}
		delete(rows, key)
	default:
		return fmt.Errorf("unsupported operation %s", operation)
	}
	return nil
}

func cdcV2ApplyPatch(base *cdcv2.Record, patch *cdcv2.RecordPatch) (*cdcv2.Record, error) {
	if _, err := cdcV2CanonicalRecord(base); err != nil {
		return nil, fmt.Errorf("base: %w", err)
	}
	if err := cdcV2ValidatePatch(patch); err != nil {
		return nil, err
	}
	for index, change := range patch.GetChanges() {
		actual, err := cdcV2StateAtPath(base, change.GetPath().GetSegments())
		if err != nil {
			return nil, fmt.Errorf("patch change %d: %w", index, err)
		}
		equal, equalErr := cdcV2FieldStatesEqual(actual, change.GetBefore())
		if equalErr != nil {
			return nil, fmt.Errorf("patch change %d: %w", index, equalErr)
		}
		if !equal {
			return nil, fmt.Errorf("patch change %d before state mismatch", index)
		}
	}
	updated := proto.Clone(base).(*cdcv2.Record)
	for index, change := range patch.GetChanges() {
		if err := cdcV2SetStateAtPath(updated, change.GetPath().GetSegments(), change.GetAfter()); err != nil {
			return nil, fmt.Errorf("patch change %d: %w", index, err)
		}
	}
	return updated, nil
}

func cdcV2StateAtPath(record *cdcv2.Record, segments []string) (*cdcv2.FieldState, error) {
	current := record
	for _, segment := range segments[:len(segments)-1] {
		field := cdcV2FindField(current, segment)
		if field == nil {
			return nil, fmt.Errorf("path traverses missing field %q", segment)
		}
		nested := field.GetValue().GetRecordValue()
		if nested == nil {
			return nil, fmt.Errorf("path field %q does not traverse a record", segment)
		}
		current = nested
	}
	leaf := cdcV2FindField(current, segments[len(segments)-1])
	if leaf == nil {
		return cdcV2Absent(), nil
	}
	return cdcV2Present(leaf.GetValue()), nil
}

func cdcV2SetStateAtPath(record *cdcv2.Record, segments []string, state *cdcv2.FieldState) error {
	current := record
	for _, segment := range segments[:len(segments)-1] {
		field := cdcV2FindField(current, segment)
		if field == nil {
			return fmt.Errorf("path traverses missing field %q", segment)
		}
		nested := field.GetValue().GetRecordValue()
		if nested == nil {
			return fmt.Errorf("path field %q does not traverse a record", segment)
		}
		current = nested
	}
	leafName := segments[len(segments)-1]
	index := slices.IndexFunc(current.GetFields(), func(field *cdcv2.RecordField) bool { return field.GetName() == leafName })
	if state.GetAbsent() != nil {
		if index >= 0 {
			current.Fields = append(current.Fields[:index], current.Fields[index+1:]...)
		}
		return nil
	}
	if state.GetValue() == nil {
		return errors.New("after state is unspecified")
	}
	newField := &cdcv2.RecordField{Name: leafName, Value: proto.Clone(state.GetValue()).(*cdcv2.Value)}
	if index >= 0 {
		current.Fields[index] = newField
	} else {
		current.Fields = append(current.Fields, newField)
	}
	return nil
}

func cdcV2CollectionPrefix(source, collection string) string {
	return strconv.Itoa(len(source)) + ":" + source + strconv.Itoa(len(collection)) + ":" + collection + "\x00"
}

func cdcV2RowKey(source string, record *cdcv2.ChangeRecord) (string, error) {
	if record.GetKey() == nil {
		return "", errors.New("record key is missing")
	}
	wire, err := cdcV2CanonicalRecordBytes(record.GetKey())
	if err != nil {
		return "", fmt.Errorf("record key: %w", err)
	}
	collection := ""
	if record.GetDataCollection() != nil {
		collection = record.GetDataCollection().GetId()
	}
	return cdcV2CollectionPrefix(source, collection) + hex.EncodeToString(wire), nil
}

func cdcV2CloneRows(rows cdcV2Rows) cdcV2Rows {
	cloned := make(cdcV2Rows, len(rows))
	for key, row := range rows {
		cloned[key] = proto.Clone(row).(*cdcv2.Record)
	}
	return cloned
}

func cdcV2RequireStateEqual(t *testing.T, left, right cdcV2Rows) {
	t.Helper()
	require.Len(t, right, len(left))
	for key, leftRow := range left {
		rightRow, present := right[key]
		require.True(t, present, "missing replay key %q", key)
		equal, err := cdcV2RecordsEqual(leftRow, rightRow)
		require.NoError(t, err)
		require.True(t, equal, "state differs for key %q", key)
	}
}

func cdcV2StateIDs(rows cdcV2Rows) []int64 {
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, cdcV2FindField(row, "id").GetValue().GetInt64Value())
	}
	slices.Sort(ids)
	return ids
}

func cdcV2OnlyRow(t *testing.T, rows cdcV2Rows) *cdcv2.Record {
	t.Helper()
	require.Len(t, rows, 1)
	for _, row := range rows {
		return row
	}
	return nil
}

func cdcV2FindField(record *cdcv2.Record, name string) *cdcv2.RecordField {
	for _, field := range record.GetFields() {
		if field.GetName() == name {
			return field
		}
	}
	return nil
}

func cdcV2Present(value *cdcv2.Value) *cdcv2.FieldState {
	return &cdcv2.FieldState{State: &cdcv2.FieldState_Value{Value: proto.Clone(value).(*cdcv2.Value)}}
}

func cdcV2Absent() *cdcv2.FieldState {
	return &cdcv2.FieldState{State: &cdcv2.FieldState_Absent{Absent: &cdcv2.Absent{}}}
}

func cdcV2String(value string) *cdcv2.Value {
	return &cdcv2.Value{Kind: &cdcv2.Value_StringValue{StringValue: value}}
}

func cdcV2FullToDelta(record *cdcv2.ChangeRecord, base *cdcv2.Record) (*cdcv2.ChangeRecord, error) {
	converted := proto.Clone(record).(*cdcv2.ChangeRecord)
	full := record.GetFull()
	switch record.GetOperation() {
	case cdcv2.Operation_OPERATION_CREATE, cdcv2.Operation_OPERATION_SNAPSHOT_READ:
		if full == nil || full.GetAfter() == nil {
			return nil, errors.New("full anchor requires after")
		}
		if _, err := cdcV2CanonicalRecord(full.GetAfter()); err != nil {
			return nil, err
		}
		converted.Representation = &cdcv2.ChangeRecord_Delta{Delta: &cdcv2.DeltaChange{Change: &cdcv2.DeltaChange_Result{Result: proto.Clone(full.GetAfter()).(*cdcv2.Record)}}}
	case cdcv2.Operation_OPERATION_UPDATE:
		if full == nil || full.GetAfter() == nil {
			return nil, errors.New("full UPDATE requires after")
		}
		prior := base
		if prior == nil {
			prior = full.GetBefore()
		}
		if prior == nil {
			return nil, errors.New("UPDATE requires complete before or materialized base state")
		}
		if base != nil && full.GetBefore() != nil {
			equal, err := cdcV2RecordsEqual(base, full.GetBefore())
			if err != nil {
				return nil, err
			}
			if !equal {
				return nil, errors.New("full UPDATE before does not match base state")
			}
		}
		changes, err := cdcV2DiffRecords(nil, prior, full.GetAfter())
		if err != nil {
			return nil, err
		}
		converted.Representation = &cdcv2.ChangeRecord_Delta{Delta: &cdcv2.DeltaChange{Change: &cdcv2.DeltaChange_Patch{Patch: &cdcv2.RecordPatch{Changes: changes}}}}
	case cdcv2.Operation_OPERATION_DELETE:
		if full == nil {
			return nil, errors.New("full DELETE representation is missing")
		}
		if full.GetBefore() != nil {
			if _, err := cdcV2CanonicalRecord(full.GetBefore()); err != nil {
				return nil, err
			}
		}
		if base != nil && full.GetBefore() != nil {
			equal, err := cdcV2RecordsEqual(base, full.GetBefore())
			if err != nil {
				return nil, err
			}
			if !equal {
				return nil, errors.New("full DELETE before does not match base state")
			}
		}
		converted.Representation = &cdcv2.ChangeRecord_Delta{Delta: &cdcv2.DeltaChange{Change: &cdcv2.DeltaChange_Delete{Delete: &cdcv2.DeleteDelta{}}}}
	case cdcv2.Operation_OPERATION_TRUNCATE, cdcv2.Operation_OPERATION_SOURCE_MESSAGE:
		converted.Representation = nil
	default:
		return nil, fmt.Errorf("unsupported operation %s", record.GetOperation())
	}
	return converted, nil
}

func cdcV2DeltaToFull(record *cdcv2.ChangeRecord, base *cdcv2.Record) (*cdcv2.ChangeRecord, error) {
	converted := proto.Clone(record).(*cdcv2.ChangeRecord)
	delta := record.GetDelta()
	switch record.GetOperation() {
	case cdcv2.Operation_OPERATION_CREATE, cdcv2.Operation_OPERATION_SNAPSHOT_READ:
		if delta == nil || delta.GetResult() == nil {
			return nil, errors.New("delta anchor requires result")
		}
		if _, err := cdcV2CanonicalRecord(delta.GetResult()); err != nil {
			return nil, err
		}
		converted.Representation = &cdcv2.ChangeRecord_Full{Full: &cdcv2.FullChange{After: proto.Clone(delta.GetResult()).(*cdcv2.Record)}}
	case cdcv2.Operation_OPERATION_UPDATE:
		if base == nil {
			return nil, errors.New("UPDATE requires materialized base state")
		}
		if delta == nil || delta.GetPatch() == nil {
			return nil, errors.New("delta UPDATE requires patch")
		}
		after, err := cdcV2ApplyPatch(base, delta.GetPatch())
		if err != nil {
			return nil, err
		}
		paths := make([]*cdcv2.FieldPath, 0, len(delta.GetPatch().GetChanges()))
		for _, change := range delta.GetPatch().GetChanges() {
			paths = append(paths, proto.Clone(change.GetPath()).(*cdcv2.FieldPath))
		}
		converted.Representation = &cdcv2.ChangeRecord_Full{Full: &cdcv2.FullChange{
			Before: proto.Clone(base).(*cdcv2.Record), After: after, ChangedFields: &cdcv2.ChangedFieldMask{Paths: paths},
		}}
	case cdcv2.Operation_OPERATION_DELETE:
		if delta == nil || delta.GetDelete() == nil {
			return nil, errors.New("delta DELETE requires delete marker")
		}
		full := &cdcv2.FullChange{}
		if base != nil {
			if _, err := cdcV2CanonicalRecord(base); err != nil {
				return nil, err
			}
			full.Before = proto.Clone(base).(*cdcv2.Record)
		}
		converted.Representation = &cdcv2.ChangeRecord_Full{Full: full}
	case cdcv2.Operation_OPERATION_TRUNCATE, cdcv2.Operation_OPERATION_SOURCE_MESSAGE:
		converted.Representation = nil
	default:
		return nil, fmt.Errorf("unsupported operation %s", record.GetOperation())
	}
	return converted, nil
}

func cdcV2DiffRecords(prefix []string, before, after *cdcv2.Record) ([]*cdcv2.FieldChange, error) {
	if _, err := cdcV2CanonicalRecord(before); err != nil {
		return nil, fmt.Errorf("prior record: %w", err)
	}
	if _, err := cdcV2CanonicalRecord(after); err != nil {
		return nil, fmt.Errorf("result record: %w", err)
	}
	names := make(map[string]struct{}, len(before.GetFields())+len(after.GetFields()))
	for _, field := range before.GetFields() {
		names[field.GetName()] = struct{}{}
	}
	for _, field := range after.GetFields() {
		names[field.GetName()] = struct{}{}
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	slices.Sort(ordered)
	var changes []*cdcv2.FieldChange
	for _, name := range ordered {
		beforeField := cdcV2FindField(before, name)
		afterField := cdcV2FindField(after, name)
		path := append(append([]string(nil), prefix...), name)
		if beforeField == nil {
			changes = append(changes, &cdcv2.FieldChange{Path: &cdcv2.FieldPath{Segments: path}, Before: cdcV2Absent(), After: cdcV2Present(afterField.GetValue())})
			continue
		}
		if afterField == nil {
			changes = append(changes, &cdcv2.FieldChange{Path: &cdcv2.FieldPath{Segments: path}, Before: cdcV2Present(beforeField.GetValue()), After: cdcV2Absent()})
			continue
		}
		beforeValue := beforeField.GetValue()
		afterValue := afterField.GetValue()
		equal, err := cdcV2ValuesEqual(beforeValue, afterValue)
		if err != nil {
			return nil, fmt.Errorf("field %q: %w", name, err)
		}
		if equal {
			continue
		}
		if beforeValue.GetTypeName() == afterValue.GetTypeName() && beforeValue.GetRecordValue() != nil && afterValue.GetRecordValue() != nil {
			nested, err := cdcV2DiffRecords(path, beforeValue.GetRecordValue(), afterValue.GetRecordValue())
			if err != nil {
				return nil, err
			}
			changes = append(changes, nested...)
			continue
		}
		changes = append(changes, &cdcv2.FieldChange{Path: &cdcv2.FieldPath{Segments: path}, Before: cdcV2Present(beforeValue), After: cdcV2Present(afterValue)})
	}
	return changes, nil
}
