package parquet

import (
	"bytes"
	"testing"

	parquetlib "github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"
	parquetschema "github.com/apache/arrow-go/v18/parquet/schema"
	"github.com/jim-technologies/invariantprotocol/go/data"
	invariantarrow "github.com/jim-technologies/invariantprotocol/go/data/arrow"
	datav1 "github.com/jim-technologies/invariantprotocol/go/gen/invariant/data/v1"
	greetpb "github.com/jim-technologies/invariantprotocol/go/tests/gen"
	"github.com/stretchr/testify/require"
)

func TestSchemaFieldIDsAndEmptyFileRoundTrip(t *testing.T) {
	dataset, err := data.CompileMessage((&greetpb.CanonicalRecord{}).ProtoReflect().Descriptor(), nil)
	require.NoError(t, err)

	mapped, diagnostics, err := Schema(dataset)
	require.NoError(t, err)
	require.Equal(t, dataset.GetName(), mapped.Root().Name())
	require.Positive(t, mapped.NumColumns())
	require.Greater(t, len(diagnostics), len(dataset.GetFields()))
	require.Equal(t, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_LOSSLESS, diagnostic(t, diagnostics, "state").GetCompatibility())
	for _, path := range []string{"choice_count", "choice_name"} {
		oneofDiagnostic := diagnostic(t, diagnostics, path)
		require.Equal(t, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_WIDENED, oneofDiagnostic.GetCompatibility())
		require.Contains(t, oneofDiagnostic.GetMessage(), "does not enforce mutual exclusivity")
	}

	uint64Field := datasetField(t, dataset, "uint64_value")
	require.Equal(t, uint64Field.GetStableId(), mapped.Root().Field(mapped.Root().FieldIndexByName("uint64_value")).FieldID())

	nestedField := datasetField(t, dataset, "nested")
	nestedNode := mapped.Root().Field(mapped.Root().FieldIndexByName("nested")).(*parquetschema.GroupNode)
	require.Equal(t, nestedField.GetStableId(), nestedNode.FieldID())
	require.Equal(t,
		nestedField.GetType().GetStruct().GetFields()[0].GetStableId(),
		nestedNode.Field(0).FieldID(),
	)

	labelsField := datasetField(t, dataset, "labels")
	labelsNode := mapped.Root().Field(mapped.Root().FieldIndexByName("labels")).(*parquetschema.GroupNode)
	labelsElement := labelsNode.Field(0).(*parquetschema.GroupNode).Field(0)
	require.Equal(t, labelsField.GetStableId(), labelsNode.FieldID())
	require.Equal(t, labelsField.GetType().GetList().GetElement().GetStableId(), labelsElement.FieldID())

	countersField := datasetField(t, dataset, "counters")
	countersNode := mapped.Root().Field(mapped.Root().FieldIndexByName("counters")).(*parquetschema.GroupNode)
	keyValue := countersNode.Field(0).(*parquetschema.GroupNode)
	require.Equal(t, countersField.GetStableId(), countersNode.FieldID())
	require.Equal(t, countersField.GetType().GetMap().GetKey().GetStableId(), keyValue.Field(0).FieldID())
	require.Equal(t, countersField.GetType().GetMap().GetValue().GetStableId(), keyValue.Field(1).FieldID())

	createdNode := mapped.Root().Field(mapped.Root().FieldIndexByName("created_at")).(*parquetschema.PrimitiveNode)
	require.Equal(t, parquetlib.Types.Int64, createdNode.PhysicalType())
	require.Contains(t, createdNode.LogicalType().String(), "nanoseconds")
	require.Equal(t, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_REDUCED, diagnostic(t, diagnostics, "created_at").GetCompatibility())

	elapsedNode := mapped.Root().Field(mapped.Root().FieldIndexByName("elapsed")).(*parquetschema.PrimitiveNode)
	require.Equal(t, parquetlib.Types.Int64, elapsedNode.PhysicalType())
	require.Equal(t, "Int(bitWidth=64, isSigned=true)", elapsedNode.LogicalType().String())
	require.Equal(t, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_REDUCED, diagnostic(t, diagnostics, "elapsed").GetCompatibility())

	attributesNode := mapped.Root().Field(mapped.Root().FieldIndexByName("attributes")).(*parquetschema.PrimitiveNode)
	require.Equal(t, "JSON", attributesNode.LogicalType().String())

	arrowSchema, _, err := invariantarrow.Schema(dataset)
	require.NoError(t, err)
	bridge, err := compatibleArrowSchema(arrowSchema, dataset)
	require.NoError(t, err)
	properties := parquetlib.NewWriterProperties(
		parquetlib.WithRootName(dataset.GetName()),
		parquetlib.WithVersion(parquetlib.V2_LATEST),
	)
	var encoded bytes.Buffer
	writer, err := pqarrow.NewFileWriter(bridge, &encoded, properties, pqarrow.NewArrowWriterProperties())
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	reader, err := file.NewParquetReader(bytes.NewReader(encoded.Bytes()))
	require.NoError(t, err)
	require.True(t, reader.MetaData().Schema.Equals(mapped))

	datasetField(t, dataset, "state").GetType().GetEnum().Closed = true
	_, closedDiagnostics, err := Schema(dataset)
	require.NoError(t, err)
	require.Equal(t, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_WIDENED, diagnostic(t, closedDiagnostics, "state").GetCompatibility())
	require.Contains(t, diagnostic(t, closedDiagnostics, "state").GetMessage(), "admits undeclared values")
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
