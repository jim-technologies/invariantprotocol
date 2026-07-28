package clickhouse

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jim-technologies/invariantprotocol/go/data"
	datav1 "github.com/jim-technologies/invariantprotocol/go/gen/invariant/data/v1"
	greetpb "github.com/jim-technologies/invariantprotocol/go/tests/gen"
	"github.com/stretchr/testify/require"
)

func TestClickHouseDDLAndValueRoundTrip(t *testing.T) {
	endpoint := os.Getenv("INVARIANT_CLICKHOUSE_URL")
	if endpoint == "" {
		t.Skip("set INVARIANT_CLICKHOUSE_URL to run the real ClickHouse integration test")
	}

	dataset := integrationDataset()
	schema, _, err := Schema(dataset)
	require.NoError(t, err)
	table := fmt.Sprintf("invariant_clickhouse_%d", os.Getpid())
	runClickHouse(t, endpoint, "DROP TABLE IF EXISTS "+quoteIdentifier(table))
	t.Cleanup(func() {
		runClickHouse(t, endpoint, "DROP TABLE IF EXISTS "+quoteIdentifier(table))
	})

	// The integration test owns this disposable physical policy. The renderer
	// itself emits only the declarations between the parentheses.
	runClickHouse(t, endpoint,
		"CREATE TABLE "+quoteIdentifier(table)+" (\n"+
			schema.ColumnDeclarations()+
			"\n) ENGINE = MergeTree ORDER BY tuple()",
	)
	commentHex := runClickHouse(t, endpoint,
		"SELECT hex(comment) FROM system.columns WHERE database = currentDatabase() AND table = "+
			quoteLiteral(table)+" AND name = 'note' FORMAT TabSeparated",
	)
	require.Equal(t, fmt.Sprintf("%X\n", []byte("Owner's \\ note.\nSecond line.")), commentHex)

	canonical, err := data.CompileMessage((&greetpb.CanonicalRecord{}).ProtoReflect().Descriptor(), nil)
	require.NoError(t, err)
	canonicalSchema, _, err := Schema(canonical)
	require.NoError(t, err)
	canonicalTable := table + "_canonical"
	runClickHouse(t, endpoint, "DROP TABLE IF EXISTS "+quoteIdentifier(canonicalTable))
	t.Cleanup(func() {
		runClickHouse(t, endpoint, "DROP TABLE IF EXISTS "+quoteIdentifier(canonicalTable))
	})
	runClickHouse(t, endpoint,
		"CREATE TABLE "+quoteIdentifier(canonicalTable)+" (\n"+
			canonicalSchema.ColumnDeclarations()+
			"\n) ENGINE = MergeTree ORDER BY tuple()",
	)

	projection, _, err := ProjectToIceberg(canonical)
	require.NoError(t, err)
	uint64Projection := findProjection(t, projection.Fields, "uint64_value")
	uint64Expression := strings.ReplaceAll(
		uint64Projection.ValueExpression,
		projectionValuePlaceholder,
		quoteIdentifier("id"),
	)
	uint32Projection := findProjection(t, projection.Fields, "uint32_value")
	uint32Expression := strings.ReplaceAll(
		uint32Projection.ValueExpression,
		projectionValuePlaceholder,
		quoteIdentifier("counter32"),
	)
	timestampProjection := findProjection(t, projection.Fields, "created_at")
	timestampExpression := strings.ReplaceAll(
		timestampProjection.ValueExpression,
		projectionValuePlaceholder,
		quoteIdentifier("created_at"),
	)

	runClickHouse(t, endpoint, "INSERT INTO "+quoteIdentifier(table)+
		" (`id`, `note`, `nested`, `labels`, `attributes`, `events`, `__invariant_oneof_choice_case`, `choice_count`, `choice_name`, `digest`, `required_id`, "+
		"`counter32`, `amount`, `record_id`, `created_at`, `elapsed`, `state`, `json_object`, `json_any`, `json_value`, `json_list`) VALUES "+
		"(18446744073709551615, NULL, (false, (0, NULL)), ['a'], map('x', toUInt64(1)), [(1, (true, 'ok'), (false, 0))], 0, (false, 0), (false, ''), 'ABCD', (true, 7), "+
		"0, NULL, NULL, NULL, NULL, 0, NULL, NULL, NULL, NULL),"+
		"(0, '', (true, (0, '')), [], map(), [], 6, (true, 0), (false, ''), 'WXYZ', (true, 0), "+
		"4294967295, 12345678901234.5678, '550e8400-e29b-41d4-a716-446655440000', "+
		"toDateTime64('1969-12-31 23:59:59.123456789', 9, 'UTC'), -1234567890123456789, 123, "+
		quoteLiteral(`{"name":"Ada"}`)+", "+
		quoteLiteral(`{"@type":"type.googleapis.com/google.protobuf.Int32Value","value":7}`)+", "+
		quoteLiteral(`42`)+", "+
		quoteLiteral(`[1,"x",null]`)+")",
	)

	rows := runClickHouse(t, endpoint,
		"SELECT toString(`id`), isNull(`note`), `nested`.`present`, `__invariant_oneof_choice_case`, `choice_count`.`value`, "+
			"toString("+uint64Expression+"), hex(`digest`), `required_id`.`value`, "+
			"toString("+uint32Expression+"), ifNull(toString(`amount`), 'NULL'), ifNull(toString(`record_id`), 'NULL'), "+
			"ifNull(toString(`created_at`), 'NULL'), ifNull(toString(`elapsed`), 'NULL'), `state`, "+
			"ifNull(`json_object`, 'NULL'), ifNull(`json_any`, 'NULL'), ifNull(`json_value`, 'NULL'), ifNull(`json_list`, 'NULL') "+
			"FROM "+quoteIdentifier(table)+" ORDER BY `id` FORMAT TabSeparated",
	)
	require.Equal(t,
		"0\t0\ttrue\t6\t0\t0\t5758595A\t0\t4294967295\t12345678901234.5678\t550e8400-e29b-41d4-a716-446655440000\t"+
			"1969-12-31 23:59:59.123456789\t-1234567890123456789\t123\t"+
			"{\"name\":\"Ada\"}\t{\"@type\":\"type.googleapis.com/google.protobuf.Int32Value\",\"value\":7}\t42\t[1,\"x\",null]\n"+
			"18446744073709551615\t1\tfalse\t0\t0\t18446744073709551615\t41424344\t7\t0\tNULL\tNULL\tNULL\tNULL\t0\tNULL\tNULL\tNULL\tNULL\n",
		rows,
	)
	convertedTimestamp := runClickHouse(t, endpoint,
		"SELECT toString("+timestampExpression+") FROM "+quoteIdentifier(table)+
			" WHERE `id` = 0 FORMAT TabSeparated",
	)
	require.Equal(t, "-876543211\n", convertedTimestamp)

	testFixedListRoundTrip(t, endpoint, table+"_fixed_list")

	for _, invalid := range []struct {
		name string
		sql  string
	}{
		{
			name: "unknown oneof case",
			sql: "INSERT INTO " + quoteIdentifier(table) +
				" (`__invariant_oneof_choice_case`, `required_id`) VALUES (99, (true, 1))",
		},
		{
			name: "oneof case and member disagree",
			sql: "INSERT INTO " + quoteIdentifier(table) +
				" (`__invariant_oneof_choice_case`, `choice_count`, `required_id`) VALUES (0, (true, 0), (true, 1))",
		},
		{
			name: "nested oneof case and member disagree",
			sql: "INSERT INTO " + quoteIdentifier(table) +
				" (`events`, `required_id`) VALUES ([(1, (false, ''), (false, 0))], (true, 1))",
		},
		{
			name: "missing required value",
			sql: "INSERT INTO " + quoteIdentifier(table) +
				" (`__invariant_oneof_choice_case`) VALUES (0)",
		},
		{
			name: "duplicate protobuf map key",
			sql: "INSERT INTO " + quoteIdentifier(table) +
				" (`attributes`, `required_id`) VALUES (map('dup', toUInt64(1), 'dup', toUInt64(2)), (true, 1))",
		},
		{
			name: "invalid protobuf UTF-8",
			sql: "INSERT INTO " + quoteIdentifier(table) +
				" (`note`, `required_id`) SELECT reinterpretAsString(unhex('FF')), (true, 1)",
		},
		{
			name: "invalid ProtoJSON",
			sql: "INSERT INTO " + quoteIdentifier(table) +
				" (`json_object`, `required_id`) VALUES ('{bad', (true, 1))",
		},
	} {
		t.Run(invalid.name, func(t *testing.T) {
			err := executeClickHouse(endpoint, invalid.sql)
			require.Error(t, err)
			require.Contains(t, err.Error(), "VIOLATED_CONSTRAINT")
		})
	}
}

func testFixedListRoundTrip(t *testing.T, endpoint, table string) {
	t.Helper()
	schema, diagnostics, err := Schema(fixedListIntegrationDataset())
	require.NoError(t, err)
	require.Equal(t,
		datav1.MappingCompatibility_MAPPING_COMPATIBILITY_LOSSLESS,
		findDiagnostic(t, diagnostics, "vector").GetCompatibility(),
	)
	require.Contains(t, schema.ColumnDeclarations(), "`vector` Array(Float32)")
	require.Contains(t, schema.ColumnDeclarations(), "CHECK length(`vector`) = 4")
	require.Contains(t, schema.ColumnDeclarations(), "`vector64` Array(Float64)")
	require.Contains(t, schema.ColumnDeclarations(), "CHECK length(`vector64`) = 2")
	require.Contains(t, schema.ColumnDeclarations(),
		"`nested` Tuple(`present` Bool, `value` Tuple(`vector` Array(Float32)))")
	require.Contains(t, schema.ColumnDeclarations(),
		"NOT tupleElement(`nested`, 'present') OR (length(tupleElement(tupleElement(`nested`, 'value'), 'vector')) = 3)")
	require.NotContains(t, schema.ColumnDeclarations(), "`vector` Array(Float32) DEFAULT []")

	runClickHouse(t, endpoint, "DROP TABLE IF EXISTS "+quoteIdentifier(table))
	t.Cleanup(func() {
		runClickHouse(t, endpoint, "DROP TABLE IF EXISTS "+quoteIdentifier(table))
	})
	runClickHouse(t, endpoint,
		"CREATE TABLE "+quoteIdentifier(table)+" (\n"+
			schema.ColumnDeclarations()+
			"\n) ENGINE = MergeTree ORDER BY tuple()",
	)

	runClickHouse(t, endpoint,
		"INSERT INTO "+quoteIdentifier(table)+" (`id`, `vector`, `vector64`) VALUES "+
			"('a', [1, 2, 3, 4], [1.5, 2.5]),"+
			"('b', [-1, -2, -3, -4], [-1.25, -2.25])",
	)
	rows := runClickHouse(t, endpoint,
		"SELECT `id`, arrayStringConcat(arrayMap(value -> toString(value), `vector`), ','), "+
			"arrayStringConcat(arrayMap(value -> toString(value), `vector64`), ',') "+
			"FROM "+quoteIdentifier(table)+" ORDER BY `id` FORMAT TabSeparated",
	)
	require.Equal(t, "a\t1,2,3,4\t1.5,2.5\nb\t-1,-2,-3,-4\t-1.25,-2.25\n", rows)
	runClickHouse(t, endpoint,
		"INSERT INTO "+quoteIdentifier(table)+" (`id`, `vector`, `vector64`, `nested`) VALUES "+
			"('nested-valid', [1, 2, 3, 4], [1, 2], tuple(true, tuple([5, 6, 7])))",
	)

	for _, invalid := range []struct {
		name string
		sql  string
	}{
		{
			name: "short float vector",
			sql: "INSERT INTO " + quoteIdentifier(table) +
				" (`id`, `vector`, `vector64`) VALUES ('short', [1, 2, 3], [1, 2])",
		},
		{
			name: "long float vector",
			sql: "INSERT INTO " + quoteIdentifier(table) +
				" (`id`, `vector`, `vector64`) VALUES ('long', [1, 2, 3, 4, 5], [1, 2])",
		},
		{
			name: "empty float vector",
			sql: "INSERT INTO " + quoteIdentifier(table) +
				" (`id`, `vector`, `vector64`) VALUES ('empty', [], [1, 2])",
		},
		{
			name: "wrong double vector",
			sql: "INSERT INTO " + quoteIdentifier(table) +
				" (`id`, `vector`, `vector64`) VALUES ('double', [1, 2, 3, 4], [1])",
		},
		{
			name: "omitted float vector",
			sql: "INSERT INTO " + quoteIdentifier(table) +
				" (`id`, `vector64`) VALUES ('omitted-float', [1, 2])",
		},
		{
			name: "omitted double vector",
			sql: "INSERT INTO " + quoteIdentifier(table) +
				" (`id`, `vector`) VALUES ('omitted-double', [1, 2, 3, 4])",
		},
		{
			name: "short nested vector",
			sql: "INSERT INTO " + quoteIdentifier(table) +
				" (`id`, `vector`, `vector64`, `nested`) VALUES " +
				"('nested-short', [1, 2, 3, 4], [1, 2], tuple(true, tuple([1, 2])))",
		},
	} {
		t.Run("fixed list "+invalid.name, func(t *testing.T) {
			err := executeClickHouse(endpoint, invalid.sql)
			require.Error(t, err)
			require.Contains(t, err.Error(), "VIOLATED_CONSTRAINT")
		})
	}
}

func fixedListIntegrationDataset() *datav1.DatasetSchema {
	floatElement := primitiveField("element", 202, datav1.PrimitiveKind_PRIMITIVE_KIND_FLOAT)
	floatElement.Presence = datav1.Presence_PRESENCE_NOT_APPLICABLE
	floatElement.SyntheticRole = datav1.SyntheticRole_SYNTHETIC_ROLE_LIST_ELEMENT
	doubleElement := primitiveField("element", 204, datav1.PrimitiveKind_PRIMITIVE_KIND_DOUBLE)
	doubleElement.Presence = datav1.Presence_PRESENCE_NOT_APPLICABLE
	doubleElement.SyntheticRole = datav1.SyntheticRole_SYNTHETIC_ROLE_LIST_ELEMENT
	nestedElement := primitiveField("element", 207, datav1.PrimitiveKind_PRIMITIVE_KIND_FLOAT)
	nestedElement.Presence = datav1.Presence_PRESENCE_NOT_APPLICABLE
	nestedElement.SyntheticRole = datav1.SyntheticRole_SYNTHETIC_ROLE_LIST_ELEMENT
	nestedVector := &datav1.Field{
		Name:            "vector",
		ProtoFullName:   "test.FixedListRecord.Nested.vector",
		ProtoNumberPath: []uint32{4, 1},
		StableId:        206,
		Presence:        datav1.Presence_PRESENCE_REPEATED,
		Type: &datav1.DataType{Kind: &datav1.DataType_List{List: &datav1.ListType{
			Element:     nestedElement,
			FixedLength: 3,
		}}},
		SyntheticRole:     datav1.SyntheticRole_SYNTHETIC_ROLE_PROTO_FIELD,
		JsonName:          "vector",
		StorageNameSource: "vector",
	}
	nested := &datav1.Field{
		Name:            "nested",
		ProtoFullName:   "test.FixedListRecord.nested",
		ProtoNumberPath: []uint32{4},
		StableId:        205,
		Presence:        datav1.Presence_PRESENCE_EXPLICIT,
		Nullable:        true,
		Type: &datav1.DataType{Kind: &datav1.DataType_Struct{Struct: &datav1.StructType{
			Fields: []*datav1.Field{nestedVector},
		}}},
		SyntheticRole:     datav1.SyntheticRole_SYNTHETIC_ROLE_PROTO_FIELD,
		JsonName:          "nested",
		StorageNameSource: "nested",
	}

	return &datav1.DatasetSchema{
		Name:          "fixed_list_record",
		SourceMessage: "test.FixedListRecord",
		Fields: []*datav1.Field{
			primitiveField("id", 200, datav1.PrimitiveKind_PRIMITIVE_KIND_STRING),
			{
				Name:            "vector",
				ProtoFullName:   "test.FixedListRecord.vector",
				ProtoNumberPath: []uint32{1},
				StableId:        201,
				Presence:        datav1.Presence_PRESENCE_REPEATED,
				Type: &datav1.DataType{Kind: &datav1.DataType_List{List: &datav1.ListType{
					Element:     floatElement,
					FixedLength: 4,
				}}},
				SyntheticRole:     datav1.SyntheticRole_SYNTHETIC_ROLE_PROTO_FIELD,
				JsonName:          "vector",
				StorageNameSource: "vector",
			},
			{
				Name:            "vector64",
				ProtoFullName:   "test.FixedListRecord.vector64",
				ProtoNumberPath: []uint32{2},
				StableId:        203,
				Presence:        datav1.Presence_PRESENCE_REPEATED,
				Type: &datav1.DataType{Kind: &datav1.DataType_List{List: &datav1.ListType{
					Element:     doubleElement,
					FixedLength: 2,
				}}},
				SyntheticRole:     datav1.SyntheticRole_SYNTHETIC_ROLE_PROTO_FIELD,
				JsonName:          "vector64",
				StorageNameSource: "vector64",
			},
			nested,
		},
	}
}

func integrationDataset() *datav1.DatasetSchema {
	note := primitiveField("note", 2, datav1.PrimitiveKind_PRIMITIVE_KIND_STRING)
	note.Presence = datav1.Presence_PRESENCE_EXPLICIT
	note.Nullable = true
	note.Description = "Owner's \\ note.\nSecond line."

	nestedLabel := primitiveField("label", 2, datav1.PrimitiveKind_PRIMITIVE_KIND_STRING)
	nestedLabel.Presence = datav1.Presence_PRESENCE_EXPLICIT
	nestedLabel.Nullable = true
	nested := &datav1.Field{
		Name:            "nested",
		StableId:        3,
		Presence:        datav1.Presence_PRESENCE_EXPLICIT,
		Nullable:        true,
		SyntheticRole:   datav1.SyntheticRole_SYNTHETIC_ROLE_PROTO_FIELD,
		ProtoNumberPath: []uint32{3},
		Type: &datav1.DataType{Kind: &datav1.DataType_Struct{Struct: &datav1.StructType{
			Fields: []*datav1.Field{
				primitiveField("id", 1, datav1.PrimitiveKind_PRIMITIVE_KIND_INT64),
				nestedLabel,
			},
		}}},
	}

	element := primitiveField("element", 1, datav1.PrimitiveKind_PRIMITIVE_KIND_STRING)
	element.Presence = datav1.Presence_PRESENCE_NOT_APPLICABLE
	element.SyntheticRole = datav1.SyntheticRole_SYNTHETIC_ROLE_LIST_ELEMENT
	labels := &datav1.Field{
		Name:            "labels",
		StableId:        6,
		Presence:        datav1.Presence_PRESENCE_REPEATED,
		SyntheticRole:   datav1.SyntheticRole_SYNTHETIC_ROLE_PROTO_FIELD,
		ProtoNumberPath: []uint32{4},
		Type: &datav1.DataType{Kind: &datav1.DataType_List{List: &datav1.ListType{
			Element: element,
		}}},
	}

	key := primitiveField("key", 1, datav1.PrimitiveKind_PRIMITIVE_KIND_STRING)
	key.Presence = datav1.Presence_PRESENCE_NOT_APPLICABLE
	key.SyntheticRole = datav1.SyntheticRole_SYNTHETIC_ROLE_MAP_KEY
	value := primitiveField("value", 2, datav1.PrimitiveKind_PRIMITIVE_KIND_UINT64)
	value.Presence = datav1.Presence_PRESENCE_NOT_APPLICABLE
	value.SyntheticRole = datav1.SyntheticRole_SYNTHETIC_ROLE_MAP_VALUE
	attributes := &datav1.Field{
		Name:            "attributes",
		StableId:        9,
		Presence:        datav1.Presence_PRESENCE_MAP,
		SyntheticRole:   datav1.SyntheticRole_SYNTHETIC_ROLE_PROTO_FIELD,
		ProtoNumberPath: []uint32{5},
		Type: &datav1.DataType{Kind: &datav1.DataType_Map{Map: &datav1.MapType{
			Key:   key,
			Value: value,
		}}},
	}

	digest := semanticField("digest", 8, &datav1.DataType{
		Kind: &datav1.DataType_FixedBytes{FixedBytes: &datav1.FixedBytesType{ByteLength: 4}},
	})
	required := primitiveField("required_id", 9, datav1.PrimitiveKind_PRIMITIVE_KIND_INT64)
	required.Presence = datav1.Presence_PRESENCE_REQUIRED
	eventElement := &datav1.Field{
		Name:          "element",
		StableId:      10,
		Presence:      datav1.Presence_PRESENCE_NOT_APPLICABLE,
		SyntheticRole: datav1.SyntheticRole_SYNTHETIC_ROLE_LIST_ELEMENT,
		Type:          nestedOneofType(11),
	}
	events := &datav1.Field{
		Name:            "events",
		StableId:        13,
		Presence:        datav1.Presence_PRESENCE_REPEATED,
		SyntheticRole:   datav1.SyntheticRole_SYNTHETIC_ROLE_PROTO_FIELD,
		ProtoNumberPath: []uint32{10},
		Type: &datav1.DataType{Kind: &datav1.DataType_List{List: &datav1.ListType{
			Element: eventElement,
		}}},
	}
	counter32 := primitiveField("counter32", 101, datav1.PrimitiveKind_PRIMITIVE_KIND_UINT32)
	amount := semanticField("amount", 102, &datav1.DataType{
		Kind: &datav1.DataType_Decimal{Decimal: &datav1.DecimalType{Precision: 18, Scale: 4}},
	})
	recordID := semanticField("record_id", 103, &datav1.DataType{
		Kind: &datav1.DataType_Uuid{Uuid: &datav1.UuidType{}},
	})
	createdAt := semanticField("created_at", 104, &datav1.DataType{
		Kind: &datav1.DataType_Timestamp{Timestamp: &datav1.TimestampType{
			Unit:     datav1.TimeUnit_TIME_UNIT_NANOSECOND,
			Timezone: "UTC",
		}},
	})
	elapsed := semanticField("elapsed", 105, &datav1.DataType{
		Kind: &datav1.DataType_Duration{Duration: &datav1.DurationType{
			Unit: datav1.TimeUnit_TIME_UNIT_NANOSECOND,
		}},
	})
	state := &datav1.Field{
		Name:            "state",
		ProtoFullName:   "test.IntegrationRecord.state",
		ProtoNumberPath: []uint32{106},
		StableId:        106,
		Presence:        datav1.Presence_PRESENCE_IMPLICIT,
		Type: &datav1.DataType{Kind: &datav1.DataType_Enum{Enum: &datav1.EnumType{
			FullName: "test.IntegrationState",
			Values: []*datav1.EnumValue{
				{Name: "INTEGRATION_STATE_UNSPECIFIED", Number: 0},
				{Name: "INTEGRATION_STATE_READY", Number: 1},
			},
		}}},
		SyntheticRole:     datav1.SyntheticRole_SYNTHETIC_ROLE_PROTO_FIELD,
		JsonName:          "state",
		StorageNameSource: "state",
	}
	jsonObject := semanticField("json_object", 107, &datav1.DataType{
		Kind: &datav1.DataType_Json{Json: &datav1.JsonType{Kind: datav1.JsonKind_JSON_KIND_STRUCT}},
	})
	jsonAny := semanticField("json_any", 108, &datav1.DataType{
		Kind: &datav1.DataType_Json{Json: &datav1.JsonType{Kind: datav1.JsonKind_JSON_KIND_ANY}},
	})
	jsonValue := semanticField("json_value", 109, &datav1.DataType{
		Kind: &datav1.DataType_Json{Json: &datav1.JsonType{Kind: datav1.JsonKind_JSON_KIND_VALUE}},
	})
	jsonList := semanticField("json_list", 110, &datav1.DataType{
		Kind: &datav1.DataType_Json{Json: &datav1.JsonType{Kind: datav1.JsonKind_JSON_KIND_LIST_VALUE}},
	})

	return &datav1.DatasetSchema{
		Name:          "integration_record",
		SourceMessage: "test.IntegrationRecord",
		Fields: []*datav1.Field{
			primitiveField("id", 1, datav1.PrimitiveKind_PRIMITIVE_KIND_UINT64),
			note,
			nested,
			labels,
			attributes,
			oneofField("choice_count", 6, "choice", datav1.PrimitiveKind_PRIMITIVE_KIND_INT32),
			oneofField("choice_name", 7, "choice", datav1.PrimitiveKind_PRIMITIVE_KIND_STRING),
			digest,
			required,
			events,
			counter32,
			amount,
			recordID,
			createdAt,
			elapsed,
			state,
			jsonObject,
			jsonAny,
			jsonValue,
			jsonList,
		},
	}
}

func runClickHouse(t *testing.T, endpoint, query string) string {
	t.Helper()
	body, err := executeClickHouseResponse(endpoint, query)
	require.NoError(t, err)
	return body
}

func executeClickHouse(endpoint, query string) error {
	_, err := executeClickHouseResponse(endpoint, query)
	return err
}

func executeClickHouseResponse(endpoint, query string) (string, error) {
	request, err := http.NewRequest(http.MethodPost, strings.TrimRight(endpoint, "/")+"/", strings.NewReader(query))
	if err != nil {
		return "", err
	}
	response, err := (&http.Client{Timeout: 15 * time.Second}).Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return "", err
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("ClickHouse HTTP %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	return string(body), nil
}
