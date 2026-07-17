package data_test

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	arrowlib "github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/extensions"
	parquetlib "github.com/apache/arrow-go/v18/parquet"
	parquetschema "github.com/apache/arrow-go/v18/parquet/schema"
	iceberglib "github.com/apache/iceberg-go"
	"github.com/jim-technologies/invariantprotocol/go/data"
	invariantarrow "github.com/jim-technologies/invariantprotocol/go/data/arrow"
	invarianticeberg "github.com/jim-technologies/invariantprotocol/go/data/iceberg"
	invariantparquet "github.com/jim-technologies/invariantprotocol/go/data/parquet"
	"github.com/jim-technologies/invariantprotocol/go/data/postgres"
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
	require.Len(t, bundle.GetDatasets(), 1)

	dataset := bundle.GetDatasets()[0]
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
}
