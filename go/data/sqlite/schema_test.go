package sqlite

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jim-technologies/invariantprotocol/go/data"
	datav1 "github.com/jim-technologies/invariantprotocol/go/gen/invariant/data/v1"
	greetpb "github.com/jim-technologies/invariantprotocol/go/tests/gen"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func sql(t *testing.T, database, statement string, succeeds bool) string {
	t.Helper()
	command := exec.Command("sqlite3", "-batch", "-bail", database)
	command.Stdin = strings.NewReader(statement)
	output, err := command.CombinedOutput()
	if succeeds {
		require.NoError(t, err, "%s\n%s", statement, output)
	} else {
		require.Error(t, err, "%s\n%s", statement, output)
	}
	return strings.TrimSpace(string(output))
}

func TestCanonicalRoundTripAndRejectedDomains(t *testing.T) {
	dataset, err := data.CompileMessage((&greetpb.CanonicalRecord{}).ProtoReflect().Descriptor(), nil)
	require.NoError(t, err)
	dataset.Description = "comment\rDROP TABLE ignored;\nmore"
	ddl, diagnostics, err := DDL(dataset)
	require.NoError(t, err)
	require.Contains(t, ddl, "-- comment\n-- DROP TABLE ignored;\n-- more")
	seen := map[string]*datav1.MappingDiagnostic{}
	for _, item := range diagnostics {
		seen[item.GetFieldPath()] = item
	}
	for _, path := range []string{"nested.label", "labels[]", "counters.key", "counters.value"} {
		require.Equal(t, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_WIDENED, seen[path].GetCompatibility())
	}
	require.Equal(t, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_PRECISION_REDUCED, seen["created_at"].GetCompatibility())
	require.Equal(t, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_REDUCED, seen["elapsed"].GetCompatibility())
	database := filepath.Join(t.TempDir(), "records.sqlite")
	sql(t, database, ddl, true)
	table := quote(dataset.GetName())
	sql(t, database, "INSERT INTO "+table+" DEFAULT VALUES;", true)
	require.Equal(t, "0|0|text|0|[]|{}", sql(t, database, "SELECT int64_value, uint64_value, typeof(uint64_value), bool_value, labels, counters FROM "+table+";", true))
	sql(t, database, "UPDATE "+table+" SET int64_value = -9223372036854775808, uint64_value = '18446744073709551615', uint32_value = 4294967295, bytes_value = X'00FF', string_value = 'a' || char(0) || 'b', created_at = 253402300799999999, elapsed = 9223372036854775807, choice_count = 0;", true)
	require.Equal(t, "18446744073709551615|00FF|610062|253402300799999999", sql(t, database, "SELECT uint64_value, hex(bytes_value), hex(string_value), created_at FROM "+table+";", true))
	for _, invalid := range []string{
		"uint64_value = '18446744073709551616'", "uint64_value = '01'", "uint64_value = '-1'", "uint64_value = '1.0'", "uint64_value = '1' || char(0) || 'x'",
		"uint64_value = 18446744073709551615", "uint32_value = 4294967296", "uint32_value = -1", "int32_value = 2147483648", "int64_value = 9223372036854775808",
		"int64_value = 0.5", "bool_value = 2", "string_value = X'01'", "bytes_value = 'bytes'", "state = 2147483648",
		"created_at = 253402300800000000", "created_at = -62135596800000001", "elapsed = 9223372036854775808",
		"labels = '{}'", "labels = 'broken'", "counters = '[]'", "nested = '1'", "choice_name = ''",
		"double_value = 'NaN'", "int64_value = NULL",
	} {
		t.Run(invalid, func(t *testing.T) { sql(t, database, "UPDATE "+table+" SET "+invalid+";", false) })
	}
	sql(t, database, "UPDATE "+table+" SET choice_count = NULL, choice_name = '', labels = '[\"valid\"]', counters = '{\"one\":\"2\"}', created_at = -62135596800000000;", true)
	proto2, err := data.CompileMessage((&greetpb.Proto2Record{}).ProtoReflect().Descriptor(), nil)
	require.NoError(t, err)
	ddl, _, err = DDL(proto2)
	require.NoError(t, err)
	sql(t, database, ddl, true)
	sql(t, database, "INSERT INTO "+quote(proto2.GetName())+" DEFAULT VALUES;", false)
	sql(t, database, "INSERT INTO "+quote(proto2.GetName())+" (id) VALUES (1);", true)
	require.Equal(t, "1", sql(t, database, "SELECT label IS NULL FROM "+quote(proto2.GetName())+";", true))
}

func TestRefinedRoundTripAndRejections(t *testing.T) {
	encoded, err := os.ReadFile("../../../testdata/schema/schema.binpb")
	require.NoError(t, err)
	bundle, err := data.ParseSchemaBundle(encoded)
	require.NoError(t, err)
	database := filepath.Join(t.TempDir(), "refined.sqlite")
	for _, dataset := range bundle.GetDatasets() {
		ddl, _, err := DDL(dataset)
		require.NoError(t, err)
		sql(t, database, ddl, true)
	}
	table := `"schema_test_v1_annotated_record"`
	sql(t, database, "INSERT INTO "+table+" (amount, record_id, digest) VALUES ('-12345678901234.5678', '00000000-0000-0000-0000-000000000000', zeroblob(24));", true)
	require.Equal(t, "-12345678901234.5678|text|24", sql(t, database, "SELECT amount, typeof(amount), length(digest) FROM "+table+";", true))
	for _, invalid := range []string{
		"amount = '123456789012345.0000'", "amount = '1.00'", "amount = '1.00000'", "amount = '01.0000'", "amount = '-0.0000'", "amount = '+1.0000'", "amount = '.0000'", "amount = '1e2'", "amount = '1.0000' || char(0)",
		"record_id = 'AAAAAAAA-0000-0000-0000-000000000000'", "record_id = '00000000-0000-0000-0000-00000000000g'", "record_id = '0000000--0000-0000-0000-000000000000'",
		"digest = zeroblob(23)", "digest = zeroblob(25)",
	} {
		t.Run(invalid, func(t *testing.T) { sql(t, database, "UPDATE "+table+" SET "+invalid+";", false) })
	}
	vector := `"schema_test_v1_lance_record"`
	sql(t, database, "INSERT INTO "+vector+" DEFAULT VALUES;", false)
	sql(t, database, "INSERT INTO "+vector+" (vector, vector64) VALUES ('[1,2,3,4,5,6,7,8]', '[1,2,3,4]');", true)
	for _, invalid := range []string{"'[]'", "'[1,2]'", "'[1,2,3,4,5,6,7,8,9]'", "'{}'", "'invalid'"} {
		sql(t, database, "UPDATE "+vector+" SET vector = "+invalid+";", false)
	}
}

func TestScaleEqualsPrecisionAndClosedEnum(t *testing.T) {
	dataset := &datav1.DatasetSchema{Name: `quoted"name`, Fields: []*datav1.Field{
		{Name: `decimal"value`, Nullable: true, Presence: datav1.Presence_PRESENCE_EXPLICIT, Type: &datav1.DataType{Kind: &datav1.DataType_Decimal{Decimal: &datav1.DecimalType{Precision: 3, Scale: 3}}}},
		{Name: "whole", Nullable: true, Presence: datav1.Presence_PRESENCE_EXPLICIT, Type: &datav1.DataType{Kind: &datav1.DataType_Decimal{Decimal: &datav1.DecimalType{Precision: 3}}}},
		{Name: "enum", Nullable: true, Presence: datav1.Presence_PRESENCE_EXPLICIT, Type: &datav1.DataType{Kind: &datav1.DataType_Enum{Enum: &datav1.EnumType{Closed: true, Values: []*datav1.EnumValue{{Number: -3}, {Number: 5}}}}}},
	}}
	ddl, _, err := DDL(dataset)
	require.NoError(t, err)
	database := filepath.Join(t.TempDir(), "special.sqlite")
	sql(t, database, ddl, true)
	table := quote(dataset.GetName())
	sql(t, database, "INSERT INTO "+table+" VALUES ('-0.999', '-999', -3);", true)
	for _, invalid := range []string{`"decimal""value" = '1.000'`, `"decimal""value" = '-0.000'`, `"decimal""value" = '0.00'`, "whole = '1000'", "whole = '0.0'", "whole = '-0'", "enum = 0"} {
		sql(t, database, "UPDATE "+table+" SET "+invalid+";", false)
	}
	sql(t, database, "UPDATE "+table+` SET "decimal""value" = '0.000', whole = '0', enum = 5;`, true)
}

func TestInvalidSchemasFailBeforeDDL(t *testing.T) {
	base, err := data.CompileMessage((&greetpb.CanonicalRecord{}).ProtoReflect().Descriptor(), nil)
	require.NoError(t, err)
	for _, mutate := range []func(*datav1.DatasetSchema){
		func(d *datav1.DatasetSchema) { d.Name = "" },
		func(d *datav1.DatasetSchema) { d.Name = "SQLite_internal" },
		func(d *datav1.DatasetSchema) { d.Name = "bad\x00name" },
		func(d *datav1.DatasetSchema) { d.Description = "bad\x00comment" },
		func(d *datav1.DatasetSchema) { d.Fields = nil },
		func(d *datav1.DatasetSchema) { d.Fields[0] = nil },
		func(d *datav1.DatasetSchema) { d.Fields[0].Type = nil },
		func(d *datav1.DatasetSchema) { d.Fields[0].Name = "\xff" },
		func(d *datav1.DatasetSchema) { d.Fields[0].Description = "\xff" },
		func(d *datav1.DatasetSchema) { d.Fields[1].Name = strings.ToUpper(d.Fields[0].GetName()) },
		func(d *datav1.DatasetSchema) { d.Fields[0].Type = &datav1.DataType{} },
		func(d *datav1.DatasetSchema) {
			d.Fields[0].Type = &datav1.DataType{Kind: &datav1.DataType_Decimal{Decimal: &datav1.DecimalType{Precision: 39}}}
		},
		func(d *datav1.DatasetSchema) {
			d.Fields[0].Type = &datav1.DataType{Kind: &datav1.DataType_Decimal{Decimal: &datav1.DecimalType{Precision: 2, Scale: 3}}}
		},
		func(d *datav1.DatasetSchema) {
			d.Fields[0].Type = &datav1.DataType{Kind: &datav1.DataType_Decimal{Decimal: &datav1.DecimalType{Precision: 2}}}
		},
		func(d *datav1.DatasetSchema) {
			d.Fields[0].Type = &datav1.DataType{Kind: &datav1.DataType_FixedBytes{FixedBytes: &datav1.FixedBytesType{}}}
		},
		func(d *datav1.DatasetSchema) { d.Fields[0].Type = &datav1.DataType{Kind: &datav1.DataType_Uuid{}} },
		func(d *datav1.DatasetSchema) {
			d.Fields[0].Type = &datav1.DataType{Kind: &datav1.DataType_Timestamp{Timestamp: &datav1.TimestampType{}}}
		},
		func(d *datav1.DatasetSchema) {
			d.Fields[0].Type = &datav1.DataType{Kind: &datav1.DataType_Duration{Duration: &datav1.DurationType{}}}
		},
		func(d *datav1.DatasetSchema) {
			d.Fields[0].Type = &datav1.DataType{Kind: &datav1.DataType_Json{Json: &datav1.JsonType{}}}
		},
		func(d *datav1.DatasetSchema) {
			d.Fields[0].Type = &datav1.DataType{Kind: &datav1.DataType_Enum{Enum: &datav1.EnumType{}}}
		},
	} {
		dataset := proto.Clone(base).(*datav1.DatasetSchema)
		mutate(dataset)
		ddl, _, err := DDL(dataset)
		require.Error(t, err)
		require.Empty(t, ddl)
	}
	_, _, err = DDL(nil)
	require.Error(t, err)
}

func TestNestedUnsupportedSchemasFailBeforeDDL(t *testing.T) {
	for _, typ := range []*datav1.DataType{
		nil,
		{Kind: &datav1.DataType_Primitive{Primitive: &datav1.PrimitiveType{Kind: 999}}},
		{Kind: &datav1.DataType_Decimal{Decimal: &datav1.DecimalType{Precision: 39}}},
		{Kind: &datav1.DataType_Json{Json: &datav1.JsonType{}}},
		{Kind: &datav1.DataType_Struct{}},
	} {
		for _, container := range []string{"struct", "list", "map"} {
			child := &datav1.Field{Name: "child", Type: typ}
			var parent *datav1.DataType
			path := "parent.child"
			switch container {
			case "struct":
				parent = &datav1.DataType{Kind: &datav1.DataType_Struct{Struct: &datav1.StructType{Fields: []*datav1.Field{child}}}}
			case "list":
				parent = &datav1.DataType{Kind: &datav1.DataType_List{List: &datav1.ListType{Element: child}}}
				path = "parent[]"
			case "map":
				parent = &datav1.DataType{Kind: &datav1.DataType_Map{Map: &datav1.MapType{
					Key:   &datav1.Field{Name: "key", Type: &datav1.DataType{Kind: &datav1.DataType_Primitive{Primitive: &datav1.PrimitiveType{Kind: datav1.PrimitiveKind_PRIMITIVE_KIND_STRING}}}},
					Value: child,
				}}}
				path = "parent.value"
			}
			dataset := &datav1.DatasetSchema{Name: "nested", Fields: []*datav1.Field{{Name: "parent", Nullable: true, Type: parent}}}
			ddl, _, err := DDL(dataset)
			require.ErrorContains(t, err, path)
			require.Empty(t, ddl)
		}
	}
}
