package data_test

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	arrowlib "github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/extensions"
	"github.com/apache/arrow-go/v18/arrow/ipc"
	parquetlib "github.com/apache/arrow-go/v18/parquet"
	parquetschema "github.com/apache/arrow-go/v18/parquet/schema"
	iceberglib "github.com/apache/iceberg-go"
	"github.com/jim-technologies/invariantprotocol/go/data"
	invariantarrow "github.com/jim-technologies/invariantprotocol/go/data/arrow"
	"github.com/jim-technologies/invariantprotocol/go/data/clickhouse"
	invarianticeberg "github.com/jim-technologies/invariantprotocol/go/data/iceberg"
	invariantparquet "github.com/jim-technologies/invariantprotocol/go/data/parquet"
	"github.com/jim-technologies/invariantprotocol/go/data/postgres"
	datav1 "github.com/jim-technologies/invariantprotocol/go/gen/invariant/data/v1"
	"github.com/stretchr/testify/require"
)

func TestAnnotatedFixtureCompilesAndRendersEveryTarget(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	require.True(t, ok)
	fixture := filepath.Join(filepath.Dir(filename), "..", "..", "testdata", "schema")

	descriptor, err := os.ReadFile(filepath.Join(fixture, "descriptor.binpb"))
	require.NoError(t, err)
	committed, err := os.ReadFile(filepath.Join(fixture, "schema.binpb"))
	require.NoError(t, err)
	previous, err := data.ParseSchemaBundle(committed)
	require.NoError(t, err)

	bundle, err := data.CompileDescriptorBytes(descriptor, nil, previous)
	require.NoError(t, err, "dataset discovery must come from the authored protobuf annotation")
	encoded, err := data.MarshalSchemaBundle(bundle)
	require.NoError(t, err)
	require.Equal(t, committed, encoded, "the committed bundle must be the deterministic descriptor compilation")
	require.Len(t, bundle.GetDatasets(), 2)

	dataset := data.FindDataset(bundle, "schema.test.v1.AnnotatedRecord")
	require.NotNil(t, dataset)
	require.Equal(t, "schema.test.v1.AnnotatedRecord", dataset.GetSourceMessage())
	require.Equal(t, "AnnotatedRecord exercises the complete authored-proto data-schema path.", dataset.GetDescription())
	require.Len(t, dataset.GetFields(), 5)
	require.EqualValues(t, 18, dataset.GetFields()[0].GetType().GetDecimal().GetPrecision())
	require.EqualValues(t, 4, dataset.GetFields()[0].GetType().GetDecimal().GetScale())
	require.NotNil(t, dataset.GetFields()[1].GetType().GetUuid())
	require.EqualValues(t, 24, dataset.GetFields()[2].GetType().GetFixedBytes().GetByteLength())

	arrowSchema, _, err := invariantarrow.Schema(dataset)
	require.NoError(t, err)
	decimal, ok := arrowSchema.Field(0).Type.(*arrowlib.Decimal128Type)
	require.True(t, ok)
	require.EqualValues(t, 18, decimal.Precision)
	require.EqualValues(t, 4, decimal.Scale)
	_, ok = arrowSchema.Field(1).Type.(*extensions.UUIDType)
	require.True(t, ok)
	fixed, ok := arrowSchema.Field(2).Type.(*arrowlib.FixedSizeBinaryType)
	require.True(t, ok)
	require.Equal(t, 24, fixed.ByteWidth)
	var arrowIPC bytes.Buffer
	require.NoError(t, invariantarrow.WriteIPC(&arrowIPC, arrowSchema))
	require.NotEmpty(t, arrowIPC.Bytes())

	parquetSchema, _, err := invariantparquet.Schema(dataset)
	require.NoError(t, err)
	parquetDecimal := parquetSchema.Root().Field(0).(*parquetschema.PrimitiveNode)
	require.Equal(t, parquetlib.Types.FixedLenByteArray, parquetDecimal.PhysicalType())
	decimalLogical, ok := parquetDecimal.LogicalType().(parquetschema.DecimalLogicalType)
	require.True(t, ok)
	require.EqualValues(t, 18, decimalLogical.Precision())
	require.EqualValues(t, 4, decimalLogical.Scale())
	_, ok = parquetSchema.Root().Field(1).(*parquetschema.PrimitiveNode).LogicalType().(parquetschema.UUIDLogicalType)
	require.True(t, ok)
	require.Equal(t, 24, parquetSchema.Root().Field(2).(*parquetschema.PrimitiveNode).TypeLength())

	icebergSchema, _, err := invarianticeberg.Schema(dataset)
	require.NoError(t, err)
	icebergDecimal, ok := icebergSchema.Fields()[0].Type.(iceberglib.DecimalType)
	require.True(t, ok)
	require.Equal(t, 18, icebergDecimal.Precision())
	require.Equal(t, 4, icebergDecimal.Scale())
	_, ok = icebergSchema.Fields()[1].Type.(iceberglib.UUIDType)
	require.True(t, ok)
	icebergFixed, ok := icebergSchema.Fields()[2].Type.(iceberglib.FixedType)
	require.True(t, ok)
	require.Equal(t, 24, icebergFixed.Len())
	icebergJSON, err := invarianticeberg.JSON(icebergSchema)
	require.NoError(t, err)
	require.Contains(t, string(icebergJSON), `"type":"decimal(18, 4)"`)

	ddl, _, err := postgres.DDL(dataset)
	require.NoError(t, err)
	require.Contains(t, ddl, `"amount" NUMERIC(18,4)`)
	require.Contains(t, ddl, `"record_id" UUID`)
	require.Contains(t, ddl, `CHECK (octet_length("digest") = 24)`)
	require.Contains(t, ddl, `CHECK (num_nonnulls("external_id", "sequence") <= 1)`)

	clickhouseSchema, _, err := clickhouse.Schema(dataset)
	require.NoError(t, err)
	clickhouseColumns := clickhouseSchema.ColumnDeclarations()
	require.Contains(t, clickhouseColumns, "`amount` Nullable(Decimal(18, 4))")
	require.Contains(t, clickhouseColumns, "`record_id` Nullable(UUID)")
	require.Contains(t, clickhouseColumns, "`digest` Nullable(FixedString(24))")
	require.Contains(t, clickhouseColumns, "`__invariant_oneof_reference_case` Int32 DEFAULT 0")
	require.Contains(t, clickhouseColumns, "`external_id` Tuple(`present` Bool, `value` String)")
	require.Contains(t, clickhouseColumns, "CHECK `__invariant_oneof_reference_case` IN (0, 4, 5)")

	lanceDataset := data.FindDataset(bundle, "schema.test.v1.LanceRecord")
	require.NotNil(t, lanceDataset)
	require.Len(t, lanceDataset.GetFields(), 4)
	vector := lanceDataset.GetFields()[2].GetType().GetList()
	require.NotNil(t, vector)
	require.EqualValues(t, 8, vector.GetFixedLength())
	require.Equal(t, datav1.PrimitiveKind_PRIMITIVE_KIND_FLOAT, vector.GetElement().GetType().GetPrimitive().GetKind())
	vector64 := lanceDataset.GetFields()[3].GetType().GetList()
	require.NotNil(t, vector64)
	require.EqualValues(t, 4, vector64.GetFixedLength())
	require.Equal(t, datav1.PrimitiveKind_PRIMITIVE_KIND_DOUBLE, vector64.GetElement().GetType().GetPrimitive().GetKind())

	lanceArrow, lanceArrowDiagnostics, err := invariantarrow.Schema(lanceDataset)
	require.NoError(t, err)
	fixedList, ok := lanceArrow.Field(2).Type.(*arrowlib.FixedSizeListType)
	require.True(t, ok)
	require.EqualValues(t, 8, fixedList.Len())
	require.Equal(t, arrowlib.PrimitiveTypes.Float32, fixedList.Elem())
	require.False(t, fixedList.ElemField().Nullable)
	fixedList64, ok := lanceArrow.Field(3).Type.(*arrowlib.FixedSizeListType)
	require.True(t, ok)
	require.EqualValues(t, 4, fixedList64.Len())
	require.Equal(t, arrowlib.PrimitiveTypes.Float64, fixedList64.Elem())
	require.Equal(t, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_LOSSLESS, diagnostic(t, lanceArrowDiagnostics, "vector").GetCompatibility())

	var lanceIPC bytes.Buffer
	require.NoError(t, invariantarrow.WriteIPC(&lanceIPC, lanceArrow))
	reader, err := ipc.NewReader(bytes.NewReader(lanceIPC.Bytes()))
	require.NoError(t, err)
	defer reader.Release()
	restored, ok := reader.Schema().Field(2).Type.(*arrowlib.FixedSizeListType)
	require.True(t, ok)
	require.EqualValues(t, 8, restored.Len())

	lanceParquet, lanceParquetDiagnostics, err := invariantparquet.Schema(lanceDataset)
	require.NoError(t, err)
	require.Contains(t, lanceParquet.String(), "vector (List)")
	parquetVector := diagnostic(t, lanceParquetDiagnostics, "vector")
	require.Equal(t, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_WIDENED, parquetVector.GetCompatibility())
	require.Contains(t, parquetVector.GetMessage(), "does not enforce element count")

	lanceIceberg, lanceIcebergDiagnostics, err := invarianticeberg.Schema(lanceDataset)
	require.NoError(t, err)
	icebergVector, ok := lanceIceberg.FindFieldByName("vector")
	require.True(t, ok)
	require.False(t, icebergVector.Required)
	require.Nil(t, icebergVector.InitialDefault)
	require.Nil(t, icebergVector.WriteDefault)
	icebergVectorDiagnostic := diagnostic(t, lanceIcebergDiagnostics, "vector")
	require.Equal(t, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_WIDENED, icebergVectorDiagnostic.GetCompatibility())
	require.Contains(t, icebergVectorDiagnostic.GetMessage(), "no fixed-cardinality list")

	lancePostgres, lancePostgresDiagnostics, err := postgres.DDL(lanceDataset)
	require.NoError(t, err)
	require.Contains(t, lancePostgres, `"vector" JSONB NOT NULL CONSTRAINT "schema_test_v1_lance_record_vector_fixed_list_check"`)
	require.Contains(t, lancePostgres, `jsonb_array_length("vector") = 8`)
	require.NotContains(t, lancePostgres, `"vector" JSONB NOT NULL DEFAULT '[]'`)
	require.Equal(t, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_WIDENED, diagnostic(t, lancePostgresDiagnostics, "vector").GetCompatibility())

	lanceClickHouse, lanceClickHouseDiagnostics, err := clickhouse.Schema(lanceDataset)
	require.NoError(t, err)
	require.Contains(t, lanceClickHouse.ColumnDeclarations(), "`vector` Array(Float32)")
	require.Contains(t, lanceClickHouse.ColumnDeclarations(), "CHECK length(`vector`) = 8")
	require.NotContains(t, lanceClickHouse.ColumnDeclarations(), "`vector` Array(Float32) DEFAULT []")
	require.Equal(t, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_LOSSLESS, diagnostic(t, lanceClickHouseDiagnostics, "vector").GetCompatibility())

	projection, projectionDiagnostics, err := clickhouse.ProjectToIceberg(lanceDataset)
	require.NoError(t, err)
	require.EqualValues(t, clickhouse.ProjectionVersion, projection.Version)
	require.EqualValues(t, 8, projection.Fields[2].FixedLength)
	require.EqualValues(t, 4, projection.Fields[3].FixedLength)
	projectionJSON, err := clickhouse.ProjectionJSON(projection)
	require.NoError(t, err)
	require.Contains(t, string(projectionJSON), `"field_path":"vector","name":"vector"`)
	require.Contains(t, string(projectionJSON), `"fixed_length":8`)
	require.Contains(t, string(projectionJSON), `"field_path":"vector64","name":"vector64"`)
	require.Contains(t, string(projectionJSON), `"fixed_length":4`)
	secondProjection, _, err := clickhouse.ProjectToIceberg(lanceDataset)
	require.NoError(t, err)
	secondProjectionJSON, err := clickhouse.ProjectionJSON(secondProjection)
	require.NoError(t, err)
	require.JSONEq(t, string(projectionJSON), string(secondProjectionJSON))
	var sawIcebergWidening bool
	for _, item := range projectionDiagnostics {
		if item.GetFieldPath() == "vector" &&
			item.GetCompatibility() == datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_WIDENED {
			sawIcebergWidening = true
			break
		}
	}
	require.True(t, sawIcebergWidening)
}

func diagnostic(
	t *testing.T,
	diagnostics []*datav1.MappingDiagnostic,
	path string,
) *datav1.MappingDiagnostic {
	t.Helper()
	for _, item := range diagnostics {
		if item.GetFieldPath() == path {
			return item
		}
	}
	require.FailNow(t, "mapping diagnostic not found", "path %q", path)
	return nil
}
