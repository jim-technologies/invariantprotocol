package iceberg_test

import (
	"encoding/json"
	"testing"

	iceberglib "github.com/apache/iceberg-go"
	"github.com/jim-technologies/invariantprotocol/go/data"
	invarianticeberg "github.com/jim-technologies/invariantprotocol/go/data/iceberg"
	datav1 "github.com/jim-technologies/invariantprotocol/go/gen/invariant/data/v1"
	greetpb "github.com/jim-technologies/invariantprotocol/go/tests/gen"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestSchemaJSONAndGloballyUniqueIDs(t *testing.T) {
	dataset, err := data.CompileMessage((&greetpb.CanonicalRecord{}).ProtoReflect().Descriptor(), nil)
	require.NoError(t, err)

	schema, diagnostics, err := invarianticeberg.Schema(dataset)
	require.NoError(t, err)
	require.Equal(t, len(dataset.GetFields()), schema.NumFields())

	uint64Field := icebergField(t, schema, "uint64_value")
	decimal, ok := uint64Field.Type.(iceberglib.DecimalType)
	require.True(t, ok)
	require.Equal(t, 20, decimal.Precision())
	require.Zero(t, decimal.Scale())

	_, ok = icebergField(t, schema, "created_at").Type.(iceberglib.TimestampTzNsType)
	require.True(t, ok)
	require.Equal(t, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_REDUCED, diagnostic(t, diagnostics, "created_at").GetCompatibility())
	_, ok = icebergField(t, schema, "elapsed").Type.(iceberglib.Int64Type)
	require.True(t, ok)
	require.Equal(t, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_REDUCED, diagnostic(t, diagnostics, "elapsed").GetCompatibility())
	_, ok = icebergField(t, schema, "attributes").Type.(iceberglib.StringType)
	require.True(t, ok)
	for _, test := range []struct {
		name       string
		limitation string
	}{
		{name: "attributes", limitation: "numbers to be finite"},
		{name: "opaque", limitation: "type URL to resolve"},
	} {
		jsonDiagnostic := diagnostic(t, diagnostics, test.name)
		require.Equal(t, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_REDUCED, jsonDiagnostic.GetCompatibility())
		require.Contains(t, jsonDiagnostic.GetMessage(), test.limitation)
	}

	for _, name := range []string{
		"double_value", "float_value", "int64_value", "int32_value",
		"fixed32_value", "uint32_value", "sfixed32_value", "sfixed64_value",
		"sint32_value", "sint64_value", "state",
	} {
		assertDefaults(t, icebergField(t, schema, name), float64(0))
	}
	for _, name := range []string{"uint64_value", "fixed64_value"} {
		assertDefaults(t, icebergField(t, schema, name), "0")
	}
	assertDefaults(t, icebergField(t, schema, "bool_value"), false)
	assertDefaults(t, icebergField(t, schema, "string_value"), "")
	assertDefaults(t, icebergField(t, schema, "bytes_value"), "")
	assertDefaults(t, icebergField(t, schema, "labels"), []any{})
	assertDefaults(t, icebergField(t, schema, "children"), []any{})
	emptyMap := map[string]any{"keys": []any{}, "values": []any{}}
	assertDefaults(t, icebergField(t, schema, "counters"), emptyMap)
	for _, name := range []string{
		"optional_note", "nested", "choice_count", "choice_name", "created_at",
		"elapsed", "wrapped_count", "attributes", "opaque",
	} {
		field := icebergField(t, schema, name)
		require.Nil(t, field.InitialDefault, name)
		require.Nil(t, field.WriteDefault, name)
	}

	nested := icebergField(t, schema, "nested").Type.(*iceberglib.StructType)
	assertDefaults(t, nested.FieldList[0], float64(0))
	require.Nil(t, nested.FieldList[1].InitialDefault)
	require.Nil(t, nested.FieldList[1].WriteDefault)

	labelsIR := datasetField(t, dataset, "labels")
	labels := icebergField(t, schema, "labels").Type.(*iceberglib.ListType)
	require.Equal(t, int(labelsIR.GetType().GetList().GetElement().GetStableId()), labels.ElementID)
	require.True(t, labels.ElementRequired)

	countersIR := datasetField(t, dataset, "counters")
	counters := icebergField(t, schema, "counters").Type.(*iceberglib.MapType)
	require.Equal(t, int(countersIR.GetType().GetMap().GetKey().GetStableId()), counters.KeyID)
	require.Equal(t, int(countersIR.GetType().GetMap().GetValue().GetStableId()), counters.ValueID)
	require.NotEqual(t, counters.KeyID, counters.ValueID)

	flat, err := schema.FlatFields()
	require.NoError(t, err)
	ids := make(map[int]string)
	for field := range flat {
		_, duplicate := ids[field.ID]
		require.False(t, duplicate, "duplicate ID %d", field.ID)
		ids[field.ID] = field.Name
	}
	require.Greater(t, len(ids), schema.NumFields())

	encoded, err := invarianticeberg.JSON(schema)
	require.NoError(t, err)
	var decoded iceberglib.Schema
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	reencoded, err := invarianticeberg.JSON(&decoded)
	require.NoError(t, err)
	require.JSONEq(t, string(encoded), string(reencoded))
	assertDefaults(t, icebergField(t, &decoded, "int32_value"), float64(0))
	assertDefaults(t, icebergField(t, &decoded, "uint64_value"), "0")
	assertDefaults(t, icebergField(t, &decoded, "bool_value"), false)
	assertDefaults(t, icebergField(t, &decoded, "string_value"), "")
	assertDefaults(t, icebergField(t, &decoded, "bytes_value"), "")
	assertDefaults(t, icebergField(t, &decoded, "labels"), []any{})
	assertDefaults(t, icebergField(t, &decoded, "counters"), emptyMap)
	require.Contains(t, string(encoded), `"initial-default":{"keys":[],"values":[]}`)

	for _, name := range []string{"choice_count", "choice_name"} {
		oneofDiagnostic := diagnostic(t, diagnostics, name)
		require.Equal(t, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_WIDENED, oneofDiagnostic.GetCompatibility())
		require.Contains(t, oneofDiagnostic.GetMessage(), "does not enforce mutual exclusivity")
	}

	closed := proto.Clone(dataset).(*datav1.DatasetSchema)
	datasetField(t, closed, "state").GetType().GetEnum().Closed = true
	_, closedDiagnostics, err := invarianticeberg.Schema(closed)
	require.NoError(t, err)
	closedDiagnostic := diagnostic(t, closedDiagnostics, "state")
	require.Equal(t, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_WIDENED, closedDiagnostic.GetCompatibility())
	require.Contains(t, closedDiagnostic.GetMessage(), "admits undeclared values")

	invalid := proto.Clone(dataset).(*datav1.DatasetSchema)
	invalidLabels := datasetField(t, invalid, "labels")
	invalidLabels.GetType().GetList().GetElement().StableId = invalidLabels.GetStableId()
	_, _, err = invarianticeberg.Schema(invalid)
	require.ErrorContains(t, err, "is shared")
}

func TestSchemaEvolutionCarriesDefaultsForHistoricalRows(t *testing.T) {
	dataset, err := data.CompileMessage((&greetpb.CanonicalRecord{}).ProtoReflect().Descriptor(), nil)
	require.NoError(t, err)

	previous := datasetWithFields(dataset, "int32_value")
	current := datasetWithFields(dataset, "int32_value", "string_value", "state", "labels", "counters")
	previousSchema, _, err := invarianticeberg.Schema(previous)
	require.NoError(t, err)
	currentSchema, _, err := invarianticeberg.Schema(current)
	require.NoError(t, err)

	for name, expected := range map[string]any{
		"string_value": "",
		"state":        float64(0),
		"labels":       []any{},
		"counters":     map[string]any{"keys": []any{}, "values": []any{}},
	} {
		logical := datasetField(t, current, name)
		_, existed := previousSchema.FindFieldByID(int(logical.GetStableId()))
		require.False(t, existed, name)
		assertDefaults(t, icebergField(t, currentSchema, name), expected)
	}

	encoded, err := invarianticeberg.JSON(currentSchema)
	require.NoError(t, err)
	var decoded iceberglib.Schema
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	reencoded, err := invarianticeberg.JSON(&decoded)
	require.NoError(t, err)
	require.JSONEq(t, string(encoded), string(reencoded))
}

func TestSchemaRejectsRequiredProtobufFieldsWithoutSafeHistory(t *testing.T) {
	requiredScalar, err := data.CompileMessage((&greetpb.Proto2Record{}).ProtoReflect().Descriptor(), nil)
	require.NoError(t, err)
	optionalDefaultSchema, _, err := invarianticeberg.Schema(datasetWithFields(requiredScalar, "label"))
	require.NoError(t, err)
	optionalDefault := icebergField(t, optionalDefaultSchema, "label")
	require.Nil(t, optionalDefault.InitialDefault)
	require.Nil(t, optionalDefault.WriteDefault)

	requiredID := datasetField(t, requiredScalar, "id")
	requiredID.HasDefault = true
	requiredID.ProtobufDefault = "7"
	_, _, err = invarianticeberg.Schema(requiredScalar)
	require.ErrorContains(t, err, "required protobuf field \"id\"")
	require.ErrorContains(t, err, "no safe canonical value for historical rows")

	requiredMessage, err := data.CompileMessage((&greetpb.CanonicalRecord{}).ProtoReflect().Descriptor(), nil)
	require.NoError(t, err)
	nested := datasetField(t, requiredMessage, "nested")
	nested.Presence = datav1.Presence_PRESENCE_REQUIRED
	nested.Nullable = false
	_, _, err = invarianticeberg.Schema(requiredMessage)
	require.ErrorContains(t, err, "required protobuf field \"nested\"")
	require.ErrorContains(t, err, "no safe canonical value for historical rows")
}

func icebergField(t *testing.T, schema *iceberglib.Schema, name string) iceberglib.NestedField {
	t.Helper()
	for _, field := range schema.Fields() {
		if field.Name == name {
			return field
		}
	}
	require.FailNow(t, "missing Iceberg field", "name %q", name)
	return iceberglib.NestedField{}
}

func assertDefaults(t *testing.T, field iceberglib.NestedField, expected any) {
	t.Helper()
	require.Equal(t, expected, field.InitialDefault, field.Name+" initial-default")
	require.Equal(t, expected, field.WriteDefault, field.Name+" write-default")
}

func datasetWithFields(dataset *datav1.DatasetSchema, names ...string) *datav1.DatasetSchema {
	selected := proto.Clone(dataset).(*datav1.DatasetSchema)
	selected.Fields = nil
	for _, name := range names {
		selected.Fields = append(selected.Fields, proto.Clone(datasetFieldByName(dataset, name)).(*datav1.Field))
	}
	return selected
}

func datasetFieldByName(dataset *datav1.DatasetSchema, name string) *datav1.Field {
	for _, field := range dataset.GetFields() {
		if field.GetName() == name {
			return field
		}
	}
	return nil
}

func datasetField(t *testing.T, dataset *datav1.DatasetSchema, name string) *datav1.Field {
	t.Helper()
	for _, field := range dataset.GetFields() {
		if field.GetName() == name {
			return field
		}
	}
	require.FailNow(t, "missing dataset field", "name %q", name)
	return nil
}

func diagnostic(t *testing.T, diagnostics []*datav1.MappingDiagnostic, path string) *datav1.MappingDiagnostic {
	t.Helper()
	for _, item := range diagnostics {
		if item.GetFieldPath() == path {
			return item
		}
	}
	require.FailNow(t, "missing diagnostic", "path %q", path)
	return nil
}
