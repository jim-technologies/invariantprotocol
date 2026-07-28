package arrow_test

import (
	"bytes"
	"testing"

	arrowlib "github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/extensions"
	"github.com/apache/arrow-go/v18/arrow/ipc"
	"github.com/apache/arrow-go/v18/arrow/memory"
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

	for _, test := range []struct {
		name       string
		limitation string
	}{
		{name: "attributes", limitation: "numbers to be finite"},
		{name: "opaque", limitation: "type URL to resolve"},
	} {
		_, ok = fieldByName(t, schema, test.name).Type.(*extensions.JSONType)
		require.True(t, ok)
		jsonDiagnostic := diagnostic(t, diagnostics, test.name)
		require.Equal(t, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_REDUCED, jsonDiagnostic.GetCompatibility())
		require.Contains(t, jsonDiagnostic.GetMessage(), test.limitation)
	}

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

func TestSemanticRefinementSchema(t *testing.T) {
	dataset := semanticDataset()
	schema, diagnostics, err := invariantarrow.Schema(dataset)
	require.NoError(t, err)

	decimal, ok := fieldByName(t, schema, "amount").Type.(*arrowlib.Decimal128Type)
	require.True(t, ok)
	require.EqualValues(t, 18, decimal.Precision)
	require.EqualValues(t, 4, decimal.Scale)

	uuid, ok := fieldByName(t, schema, "record_id").Type.(*extensions.UUIDType)
	require.True(t, ok)
	require.Equal(t, "arrow.uuid", uuid.ExtensionName())
	require.True(t, arrowlib.TypeEqual(&arrowlib.FixedSizeBinaryType{ByteWidth: 16}, uuid.StorageType()))

	fixed, ok := fieldByName(t, schema, "digest").Type.(*arrowlib.FixedSizeBinaryType)
	require.True(t, ok)
	require.Equal(t, 24, fixed.ByteWidth)

	for _, path := range []string{"amount", "record_id"} {
		require.Equal(t, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_REPRESENTATION_CHANGED, diagnostic(t, diagnostics, path).GetCompatibility())
	}
	require.Equal(t, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_LOSSLESS, diagnostic(t, diagnostics, "digest").GetCompatibility())

	var encoded bytes.Buffer
	require.NoError(t, invariantarrow.WriteIPC(&encoded, schema))
	reader, err := ipc.NewReader(bytes.NewReader(encoded.Bytes()))
	require.NoError(t, err)
	defer reader.Release()
	require.True(t, arrowlib.TypeEqual(decimal, fieldByName(t, reader.Schema(), "amount").Type))
	require.True(t, arrowlib.TypeEqual(uuid, fieldByName(t, reader.Schema(), "record_id").Type))
	require.True(t, arrowlib.TypeEqual(fixed, fieldByName(t, reader.Schema(), "digest").Type))
}

func TestFixedListRecordIPCRoundTrip(t *testing.T) {
	dataset := fixedListDataset()
	schema, diagnostics, err := invariantarrow.Schema(dataset)
	require.NoError(t, err)
	vectorType, ok := fieldByName(t, schema, "vector").Type.(*arrowlib.FixedSizeListType)
	require.True(t, ok)
	require.EqualValues(t, 8, vectorType.Len())
	require.Equal(t, arrowlib.FLOAT32, vectorType.Elem().ID())
	require.False(t, vectorType.ElemField().Nullable)
	require.Equal(t, "fixed_list", metadataValue(t, fieldByName(t, schema, "vector").Metadata, "invariant.logical_type"))
	require.Equal(t, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_LOSSLESS, diagnostic(t, diagnostics, "vector").GetCompatibility())

	allocator := memory.NewGoAllocator()
	idBuilder := array.NewStringBuilder(allocator)
	defer idBuilder.Release()
	labelBuilder := array.NewStringBuilder(allocator)
	defer labelBuilder.Release()
	vectorBuilder := array.NewBuilder(allocator, vectorType).(*array.FixedSizeListBuilder)
	defer vectorBuilder.Release()
	values := vectorBuilder.ValueBuilder().(*array.Float32Builder)

	idBuilder.AppendValues([]string{"a", "b"}, nil)
	labelBuilder.AppendValues([]string{"first", "second"}, nil)
	for row := range 2 {
		vectorBuilder.Append(true)
		for column := range 8 {
			values.Append(float32(row*8 + column))
		}
	}
	columns := []arrowlib.Array{idBuilder.NewArray(), labelBuilder.NewArray(), vectorBuilder.NewArray()}
	for _, column := range columns {
		defer column.Release()
	}
	record := array.NewRecordBatch(schema, columns, 2)
	defer record.Release()

	var encoded bytes.Buffer
	writer := ipc.NewWriter(&encoded, ipc.WithSchema(schema))
	require.NoError(t, writer.Write(record))
	require.NoError(t, writer.Close())

	reader, err := ipc.NewReader(bytes.NewReader(encoded.Bytes()))
	require.NoError(t, err)
	defer reader.Release()
	require.True(t, reader.Next())
	restored := reader.RecordBatch()
	require.EqualValues(t, 2, restored.NumRows())
	restoredVector, ok := restored.Column(2).(*array.FixedSizeList)
	require.True(t, ok)
	require.EqualValues(t, 8, restoredVector.DataType().(*arrowlib.FixedSizeListType).Len())
	require.Equal(t, []float32{0, 1, 2, 3, 4, 5, 6, 7}, restoredVector.ListValues().(*array.Float32).Float32Values()[:8])
	require.False(t, reader.Next())
	require.NoError(t, reader.Err())
}

func semanticDataset() *datav1.DatasetSchema {
	return &datav1.DatasetSchema{
		Name:          "semantic_record",
		SourceMessage: "example.SemanticRecord",
		Fields: []*datav1.Field{
			{
				Name:     "amount",
				StableId: 1,
				Presence: datav1.Presence_PRESENCE_EXPLICIT,
				Nullable: true,
				Type: &datav1.DataType{Kind: &datav1.DataType_Decimal{Decimal: &datav1.DecimalType{
					Precision: 18,
					Scale:     4,
				}}},
			},
			{
				Name:     "record_id",
				StableId: 2,
				Presence: datav1.Presence_PRESENCE_EXPLICIT,
				Nullable: true,
				Type:     &datav1.DataType{Kind: &datav1.DataType_Uuid{Uuid: &datav1.UuidType{}}},
			},
			{
				Name:     "digest",
				StableId: 3,
				Presence: datav1.Presence_PRESENCE_EXPLICIT,
				Nullable: true,
				Type: &datav1.DataType{Kind: &datav1.DataType_FixedBytes{FixedBytes: &datav1.FixedBytesType{
					ByteLength: 24,
				}}},
			},
		},
	}
}

func fixedListDataset() *datav1.DatasetSchema {
	primitive := func(name string, id int32, kind datav1.PrimitiveKind) *datav1.Field {
		return &datav1.Field{
			Name: name, StableId: id, Presence: datav1.Presence_PRESENCE_IMPLICIT,
			SyntheticRole: datav1.SyntheticRole_SYNTHETIC_ROLE_PROTO_FIELD,
			Type: &datav1.DataType{Kind: &datav1.DataType_Primitive{
				Primitive: &datav1.PrimitiveType{Kind: kind},
			}},
		}
	}
	return &datav1.DatasetSchema{
		Name: "example_v1_vector", SourceMessage: "example.v1.Vector",
		Fields: []*datav1.Field{
			primitive("id", 1, datav1.PrimitiveKind_PRIMITIVE_KIND_STRING),
			primitive("label", 2, datav1.PrimitiveKind_PRIMITIVE_KIND_STRING),
			{
				Name: "vector", StableId: 3, Presence: datav1.Presence_PRESENCE_REPEATED,
				SyntheticRole: datav1.SyntheticRole_SYNTHETIC_ROLE_PROTO_FIELD,
				Type: &datav1.DataType{Kind: &datav1.DataType_List{List: &datav1.ListType{
					FixedLength: 8,
					Element: &datav1.Field{
						Name: "element", StableId: 4, Presence: datav1.Presence_PRESENCE_NOT_APPLICABLE,
						SyntheticRole: datav1.SyntheticRole_SYNTHETIC_ROLE_LIST_ELEMENT,
						Type: &datav1.DataType{Kind: &datav1.DataType_Primitive{
							Primitive: &datav1.PrimitiveType{Kind: datav1.PrimitiveKind_PRIMITIVE_KIND_FLOAT},
						}},
					},
				}}},
			},
		},
	}
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
