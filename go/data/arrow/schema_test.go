package arrow_test

import (
	"bytes"
	"testing"

	arrowlib "github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/extensions"
	"github.com/apache/arrow-go/v18/arrow/ipc"
	"github.com/jim-technologies/invariantprotocol/go/data"
	invariantarrow "github.com/jim-technologies/invariantprotocol/go/data/arrow"
	datav1 "github.com/jim-technologies/invariantprotocol/go/gen/invariant/data/v1"
	greetpb "github.com/jim-technologies/invariantprotocol/go/tests/gen"
	"github.com/stretchr/testify/require"
)

func TestSchemaAndIPCRoundTrip(t *testing.T) {
	dataset, err := data.CompileMessage((&greetpb.CanonicalRecord{}).ProtoReflect().Descriptor(), nil)
	require.NoError(t, err)

	schema, diagnostics, err := invariantarrow.Schema(dataset)
	require.NoError(t, err)
	require.Equal(t, len(dataset.GetFields()), schema.NumFields())
	require.Greater(t, len(diagnostics), schema.NumFields())
	require.Equal(t, dataset.GetSourceMessage(), metadataValue(t, schema.Metadata(), "invariant.source_message"))

	uint64Field := fieldByName(t, schema, "uint64_value")
	require.Equal(t, arrowlib.UINT64, uint64Field.Type.ID())
	require.Equal(t, "4", metadataValue(t, uint64Field.Metadata, "PARQUET:field_id"))
	require.Equal(t, "uint64Value", metadataValue(t, uint64Field.Metadata, "invariant.proto.json_name"))

	enumField := fieldByName(t, schema, "state")
	require.Equal(t, arrowlib.INT32, enumField.Type.ID())
	require.Contains(t, metadataValue(t, enumField.Metadata, "invariant.enum.values"), "DATA_STATE_ACTIVE")
	require.Equal(t, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_LOSSLESS, diagnostic(t, diagnostics, "state").GetCompatibility())

	for _, path := range []string{"choice_count", "choice_name"} {
		oneofDiagnostic := diagnostic(t, diagnostics, path)
		require.Equal(t, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_WIDENED, oneofDiagnostic.GetCompatibility())
		require.Contains(t, oneofDiagnostic.GetMessage(), "does not enforce mutual exclusivity")
	}

	nestedField := fieldByName(t, schema, "nested")
	nested, ok := nestedField.Type.(*arrowlib.StructType)
	require.True(t, ok)
	require.Positive(t, nested.NumFields())

	labelsField := fieldByName(t, schema, "labels")
	labels, ok := labelsField.Type.(*arrowlib.ListType)
	require.True(t, ok)
	require.False(t, labels.ElemField().Nullable)
	require.NotEqual(t,
		metadataValue(t, labelsField.Metadata, "PARQUET:field_id"),
		metadataValue(t, labels.ElemField().Metadata, "PARQUET:field_id"),
	)

	countersField := fieldByName(t, schema, "counters")
	counters, ok := countersField.Type.(*arrowlib.MapType)
	require.True(t, ok)
	require.Equal(t, arrowlib.STRING, counters.KeyType().ID())
	require.Equal(t, arrowlib.UINT64, counters.ItemType().ID())
	require.False(t, counters.KeyField().Nullable)
	require.NotEqual(t,
		metadataValue(t, counters.KeyField().Metadata, "PARQUET:field_id"),
		metadataValue(t, counters.ItemField().Metadata, "PARQUET:field_id"),
	)

	createdAt := fieldByName(t, schema, "created_at").Type.(*arrowlib.TimestampType)
	require.Equal(t, arrowlib.Nanosecond, createdAt.Unit)
	require.Equal(t, "UTC", createdAt.TimeZone)
	require.Equal(t, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_REDUCED, diagnostic(t, diagnostics, "created_at").GetCompatibility())

	elapsed := fieldByName(t, schema, "elapsed").Type.(*arrowlib.DurationType)
	require.Equal(t, arrowlib.Nanosecond, elapsed.Unit)
	require.Equal(t, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_REDUCED, diagnostic(t, diagnostics, "elapsed").GetCompatibility())

	_, ok = fieldByName(t, schema, "attributes").Type.(*extensions.JSONType)
	require.True(t, ok)
	require.Equal(t, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_REPRESENTATION_CHANGED, diagnostic(t, diagnostics, "attributes").GetCompatibility())

	var encoded bytes.Buffer
	require.NoError(t, invariantarrow.WriteIPC(&encoded, schema))
	reader, err := ipc.NewReader(bytes.NewReader(encoded.Bytes()))
	require.NoError(t, err)
	defer reader.Release()
	require.Equal(t, schema.NumFields(), reader.Schema().NumFields())
	for i := range schema.NumFields() {
		want, got := schema.Field(i), reader.Schema().Field(i)
		require.Equal(t, want.Name, got.Name)
		require.Equal(t, want.Nullable, got.Nullable)
		require.True(t, want.Metadata.Equal(got.Metadata))
		require.True(t, arrowlib.TypeEqual(want.Type, got.Type))
	}
	require.False(t, reader.Next())
	require.NoError(t, reader.Err())

	logicalFieldByName(t, dataset, "state").GetType().GetEnum().Closed = true
	_, closedDiagnostics, err := invariantarrow.Schema(dataset)
	require.NoError(t, err)
	require.Equal(t, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_WIDENED, diagnostic(t, closedDiagnostics, "state").GetCompatibility())
	require.Contains(t, diagnostic(t, closedDiagnostics, "state").GetMessage(), "closed value set")
}

func fieldByName(t *testing.T, schema *arrowlib.Schema, name string) arrowlib.Field {
	t.Helper()
	fields, ok := schema.FieldsByName(name)
	require.True(t, ok)
	require.Len(t, fields, 1)
	return fields[0]
}

func logicalFieldByName(t *testing.T, dataset *datav1.DatasetSchema, name string) *datav1.Field {
	t.Helper()
	for _, field := range dataset.GetFields() {
		if field.GetName() == name {
			return field
		}
	}
	require.FailNow(t, "missing dataset field", "name %q", name)
	return nil
}

func metadataValue(t *testing.T, metadata arrowlib.Metadata, key string) string {
	t.Helper()
	value, ok := metadata.GetValue(key)
	require.True(t, ok, "missing metadata key %q", key)
	return value
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
