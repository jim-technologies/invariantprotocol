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

	runClickHouse(t, endpoint, "INSERT INTO "+quoteIdentifier(table)+
		" (`id`, `note`, `nested`, `labels`, `attributes`, `events`, `__invariant_oneof_choice_case`, `choice_count`, `choice_name`, `digest`, `required_id`) VALUES "+
		"(18446744073709551615, NULL, (false, (0, NULL)), ['a'], map('x', toUInt64(1)), [(1, (true, 'ok'), (false, 0))], 0, (false, 0), (false, ''), 'ABCD', (true, 7)),"+
		"(0, '', (true, (0, '')), [], map(), [], 6, (true, 0), (false, ''), 'WXYZ', (true, 0))",
	)

	rows := runClickHouse(t, endpoint,
		"SELECT toString(`id`), isNull(`note`), `nested`.`present`, `__invariant_oneof_choice_case`, `choice_count`.`value`, "+
			"toString("+uint64Expression+"), hex(`digest`), `required_id`.`value` "+
			"FROM "+quoteIdentifier(table)+" ORDER BY `id` FORMAT TabSeparated",
	)
	require.Equal(t,
		"0\t0\ttrue\t6\t0\t0\t5758595A\t0\n"+
			"18446744073709551615\t1\tfalse\t0\t0\t18446744073709551615\t41424344\t7\n",
		rows,
	)

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
	} {
		t.Run(invalid.name, func(t *testing.T) {
			err := executeClickHouse(endpoint, invalid.sql)
			require.Error(t, err)
			require.Contains(t, err.Error(), "VIOLATED_CONSTRAINT")
		})
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
