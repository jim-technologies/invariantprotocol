package clickhouse

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jim-technologies/invariantprotocol/go/data"
	datav1 "github.com/jim-technologies/invariantprotocol/go/gen/invariant/data/v1"
	greetpb "github.com/jim-technologies/invariantprotocol/go/tests/gen"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

func TestSchemaGoldenCoversEveryLogicalType(t *testing.T) {
	dataset := allTypesDataset(t)
	schema, diagnostics, err := Schema(dataset)
	require.NoError(t, err)

	actual := schema.ColumnDeclarations() + "\n"
	goldenPath := filepath.Join("testdata", "canonical.columns.sql")
	golden, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden %q: %v\nactual:\n%s", goldenPath, err, actual)
	}
	require.Equal(t, string(golden), actual)

	for name, wantType := range map[string]string{
		"double_value":   "Float64",
		"float_value":    "Float32",
		"int64_value":    "Int64",
		"uint64_value":   "UInt64",
		"int32_value":    "Int32",
		"fixed64_value":  "UInt64",
		"fixed32_value":  "UInt32",
		"bool_value":     "Bool",
		"string_value":   "String",
		"bytes_value":    "String",
		"uint32_value":   "UInt32",
		"sfixed32_value": "Int32",
		"sfixed64_value": "Int64",
		"sint32_value":   "Int32",
		"sint64_value":   "Int64",
		"state":          "Int32",
		"optional_note":  "Nullable(String)",
		"labels":         "Array(String)",
		"counters":       "Map(String, UInt64)",
		"created_at":     "Nullable(DateTime64(9, 'UTC'))",
		"elapsed":        "Nullable(Int64)",
		"amount":         "Nullable(Decimal(18, 4))",
		"record_id":      "Nullable(UUID)",
		"digest":         "Nullable(FixedString(24))",
		"json_value":     "Nullable(String)",
		"json_list":      "Nullable(String)",
	} {
		require.Equal(t, wantType, findColumn(t, schema, name).Type, name)
	}
	require.Equal(t,
		"Tuple(`present` Bool, `value` Tuple(`id` Int64, `label` Nullable(String)))",
		findColumn(t, schema, "nested").Type,
	)
	require.Equal(t,
		"Array(Tuple(`id` Int64, `label` Nullable(String)))",
		findColumn(t, schema, "children").Type,
	)
	caseColumn := findColumn(t, schema, "__invariant_oneof_choice_case")
	require.Equal(t, "Int32", caseColumn.Type)
	require.True(t, caseColumn.Synthetic)
	require.Contains(t, caseColumn.Comment, "22=choice_count")
	require.Equal(t,
		"Tuple(`present` Bool, `value` Int32)",
		findColumn(t, schema, "choice_count").Type,
	)
	require.Equal(t,
		"Tuple(`present` Bool, `value` String)",
		findColumn(t, schema, "choice_name").Type,
	)

	require.Equal(t,
		datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_REDUCED,
		findDiagnostic(t, diagnostics, "created_at").GetCompatibility(),
	)
	require.Equal(t,
		datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_REDUCED,
		findDiagnostic(t, diagnostics, "elapsed").GetCompatibility(),
	)
	require.Equal(t,
		datav1.MappingCompatibility_MAPPING_COMPATIBILITY_REPRESENTATION_CHANGED,
		findDiagnostic(t, diagnostics, "nested").GetCompatibility(),
	)
	require.Contains(t, findDiagnostic(t, diagnostics, "digest").GetMessage(), "pads shorter input")
	require.Contains(t, findDiagnostic(t, diagnostics, "opaque").GetMessage(), "type URL")
	for _, path := range []string{
		"double_value", "float_value", "int64_value", "uint64_value",
		"int32_value", "fixed64_value", "fixed32_value", "bool_value",
		"string_value", "bytes_value", "uint32_value", "sfixed32_value",
		"sfixed64_value", "sint32_value", "sint64_value", "state",
		"optional_note", "nested", "nested.id", "nested.label", "labels",
		"labels[]", "children", "children[]", "children[].id",
		"children[].label", "counters", "counters.key", "counters.value",
		"choice_count", "choice_name", "created_at", "elapsed",
		"wrapped_count", "attributes", "opaque", "amount", "record_id",
		"digest", "json_value", "json_list",
	} {
		findDiagnostic(t, diagnostics, path)
	}
}

func TestPresenceCollectionsEnumsAndOneofsAreEnforced(t *testing.T) {
	dataset := allTypesDataset(t)
	datasetField(t, dataset, "state").GetType().GetEnum().Closed = true

	schema, diagnostics, err := Schema(dataset)
	require.NoError(t, err)
	declarations := schema.ColumnDeclarations()

	require.Contains(t, declarations, "`optional_note` Nullable(String) DEFAULT NULL")
	require.Contains(t, declarations, "`nested` Tuple(`present` Bool, `value` Tuple(")
	require.Contains(t, declarations, "`labels` Array(String) DEFAULT []")
	require.Contains(t, declarations, "`counters` Map(String, UInt64) DEFAULT map()")
	require.Contains(t, declarations, "`__invariant_oneof_choice_case` Int32 DEFAULT 0")
	require.Contains(t, declarations, "CHECK `__invariant_oneof_choice_case` IN (0, 22, 23)")
	require.Contains(t, declarations,
		"(`__invariant_oneof_choice_case` = 22) = tupleElement(`choice_count`, 'present')")
	require.Contains(t, declarations, "length(arrayDistinct(mapKeys(`counters`)))")
	require.Contains(t, declarations, "arrayAll(element -> (isValidUTF8(element)), `labels`)")
	require.Contains(t, declarations, "`state` IN (0, 1)")
	require.Equal(t,
		datav1.MappingCompatibility_MAPPING_COMPATIBILITY_LOSSLESS,
		findDiagnostic(t, diagnostics, "state").GetCompatibility(),
	)

	proto2, err := data.CompileMessage((&greetpb.Proto2Record{}).ProtoReflect().Descriptor(), nil)
	require.NoError(t, err)
	required, _, err := Schema(proto2)
	require.NoError(t, err)
	require.Equal(t, "Tuple(`present` Bool, `value` Int64)", findColumn(t, required, "id").Type)
	require.Contains(t, required.ColumnDeclarations(), "CHECK tupleElement(`id`, 'present')")
	require.Equal(t, "Nullable(String)", findColumn(t, required, "label").Type)
	require.Equal(t, "NULL", findColumn(t, required, "label").DefaultExpression)
}

func TestNestedOneofChecksUseTupleElementsInEveryContainer(t *testing.T) {
	nested := &datav1.Field{
		Name:            "nested",
		StableId:        1,
		Presence:        datav1.Presence_PRESENCE_EXPLICIT,
		Nullable:        true,
		SyntheticRole:   datav1.SyntheticRole_SYNTHETIC_ROLE_PROTO_FIELD,
		ProtoNumberPath: []uint32{1},
		Type:            nestedOneofType(10),
	}
	element := &datav1.Field{
		Name:          "element",
		StableId:      20,
		Presence:      datav1.Presence_PRESENCE_NOT_APPLICABLE,
		SyntheticRole: datav1.SyntheticRole_SYNTHETIC_ROLE_LIST_ELEMENT,
		Type:          nestedOneofType(21),
	}
	events := &datav1.Field{
		Name:            "events",
		StableId:        2,
		Presence:        datav1.Presence_PRESENCE_REPEATED,
		SyntheticRole:   datav1.SyntheticRole_SYNTHETIC_ROLE_PROTO_FIELD,
		ProtoNumberPath: []uint32{2},
		Type: &datav1.DataType{Kind: &datav1.DataType_List{List: &datav1.ListType{
			Element: element,
		}}},
	}
	key := primitiveField("key", 30, datav1.PrimitiveKind_PRIMITIVE_KIND_STRING)
	key.Presence = datav1.Presence_PRESENCE_NOT_APPLICABLE
	key.SyntheticRole = datav1.SyntheticRole_SYNTHETIC_ROLE_MAP_KEY
	value := &datav1.Field{
		Name:          "value",
		StableId:      31,
		Presence:      datav1.Presence_PRESENCE_NOT_APPLICABLE,
		SyntheticRole: datav1.SyntheticRole_SYNTHETIC_ROLE_MAP_VALUE,
		Type:          nestedOneofType(32),
	}
	indexed := &datav1.Field{
		Name:            "indexed",
		StableId:        3,
		Presence:        datav1.Presence_PRESENCE_MAP,
		SyntheticRole:   datav1.SyntheticRole_SYNTHETIC_ROLE_PROTO_FIELD,
		ProtoNumberPath: []uint32{3},
		Type: &datav1.DataType{Kind: &datav1.DataType_Map{Map: &datav1.MapType{
			Key:   key,
			Value: value,
		}}},
	}

	schema, _, err := Schema(&datav1.DatasetSchema{
		Name:   "nested_oneofs",
		Fields: []*datav1.Field{nested, events, indexed},
	})
	require.NoError(t, err)
	declarations := schema.ColumnDeclarations()

	require.Contains(t, declarations,
		"tupleElement(tupleElement(`nested`, 'value'), '__invariant_oneof_choice_case')")
	require.Contains(t, declarations,
		"arrayAll(element -> (tupleElement(element, '__invariant_oneof_choice_case') IN (0, 1, 2)), `events`)")
	require.Contains(t, declarations,
		"arrayAll(value -> (tupleElement(value, '__invariant_oneof_choice_case') IN (0, 1, 2)), mapValues(`indexed`))")
	require.NotContains(t, declarations, "element.`")
	require.NotContains(t, declarations, "value.`")
}

func TestSchemaIsDeterministicAndQuotesIdentifiers(t *testing.T) {
	dataset := &datav1.DatasetSchema{
		Name:          "select",
		SourceMessage: "example.Quoted",
		Description:   "owner's \\ table",
		Fields: []*datav1.Field{
			primitiveField("select", 1, datav1.PrimitiveKind_PRIMITIVE_KIND_STRING),
			primitiveField("tick`slash\\", 2, datav1.PrimitiveKind_PRIMITIVE_KIND_BYTES),
			{
				Name:            "nested_placeholder",
				StableId:        3,
				Presence:        datav1.Presence_PRESENCE_EXPLICIT,
				Nullable:        true,
				SyntheticRole:   datav1.SyntheticRole_SYNTHETIC_ROLE_PROTO_FIELD,
				ProtoNumberPath: []uint32{3},
				Type: &datav1.DataType{Kind: &datav1.DataType_Struct{Struct: &datav1.StructType{
					Fields: []*datav1.Field{
						primitiveField("{value}", 4, datav1.PrimitiveKind_PRIMITIVE_KIND_STRING),
					},
				}}},
			},
		},
	}
	dataset.Fields[0].Description = "owner's \\ value\nnext\tline"

	first, firstDiagnostics, err := Schema(dataset)
	require.NoError(t, err)
	second, secondDiagnostics, err := Schema(proto.Clone(dataset).(*datav1.DatasetSchema))
	require.NoError(t, err)
	require.Equal(t, first, second)
	require.Equal(t, firstDiagnostics, secondDiagnostics)
	require.Contains(t, first.ColumnDeclarations(), "`select` String")
	require.Contains(t, first.ColumnDeclarations(), "`tick\\`slash\\\\` String")
	require.Contains(t, first.ColumnDeclarations(), `COMMENT 'owner\'s \\ value\nnext\tline'`)
	require.Contains(t, first.ColumnDeclarations(),
		"tupleElement(tupleElement(`nested_placeholder`, 'value'), '{value}')")
	require.NotContains(t, first.ColumnDeclarations(), constraintValuePlaceholder)

	projection, _, err := ProjectToIceberg(allTypesDataset(t))
	require.NoError(t, err)
	firstJSON, err := ProjectionJSON(projection)
	require.NoError(t, err)
	secondProjection, _, err := ProjectToIceberg(allTypesDataset(t))
	require.NoError(t, err)
	secondJSON, err := ProjectionJSON(secondProjection)
	require.NoError(t, err)
	require.True(t, bytes.Equal(firstJSON, secondJSON), "projection JSON must be byte-for-byte deterministic")
}

func TestSchemaDiagnosesConflictsAndUnsupportedMappings(t *testing.T) {
	tests := []struct {
		name      string
		dataset   *datav1.DatasetSchema
		wantError string
		wantPath  string
	}{
		{
			name: "duplicate storage name",
			dataset: &datav1.DatasetSchema{Name: "record", Fields: []*datav1.Field{
				primitiveField("value", 1, datav1.PrimitiveKind_PRIMITIVE_KIND_INT32),
				primitiveField("value", 2, datav1.PrimitiveKind_PRIMITIVE_KIND_STRING),
			}},
			wantError: "collide as ClickHouse identifier",
			wantPath:  "value",
		},
		{
			name: "reserved renderer namespace",
			dataset: &datav1.DatasetSchema{Name: "record", Fields: []*datav1.Field{
				oneofField("member", 1, "choice", datav1.PrimitiveKind_PRIMITIVE_KIND_INT32),
				primitiveField("__invariant_oneof_choice_case", 2, datav1.PrimitiveKind_PRIMITIVE_KIND_INT32),
			}},
			wantError: "reserved \"__invariant_\" namespace",
			wantPath:  "__invariant_oneof_choice_case",
		},
		{
			name: "identifier control character",
			dataset: &datav1.DatasetSchema{Name: "record", Fields: []*datav1.Field{
				primitiveField("bad\nname", 1, datav1.PrimitiveKind_PRIMITIVE_KIND_INT32),
			}},
			wantError: "identifier contains a control character",
			wantPath:  "bad\nname",
		},
		{
			name: "tuple null element",
			dataset: &datav1.DatasetSchema{Name: "record", Fields: []*datav1.Field{{
				Name:            "nested",
				StableId:        1,
				Presence:        datav1.Presence_PRESENCE_EXPLICIT,
				Nullable:        true,
				SyntheticRole:   datav1.SyntheticRole_SYNTHETIC_ROLE_PROTO_FIELD,
				ProtoNumberPath: []uint32{1},
				Type: &datav1.DataType{Kind: &datav1.DataType_Struct{Struct: &datav1.StructType{
					Fields: []*datav1.Field{primitiveField("null", 2, datav1.PrimitiveKind_PRIMITIVE_KIND_STRING)},
				}}},
			}}},
			wantError: "reserves the Tuple element name",
			wantPath:  "nested.null",
		},
		{
			name: "fixed string requiring suspicious setting",
			dataset: &datav1.DatasetSchema{Name: "record", Fields: []*datav1.Field{{
				Name:            "digest",
				StableId:        1,
				Presence:        datav1.Presence_PRESENCE_EXPLICIT,
				Nullable:        true,
				SyntheticRole:   datav1.SyntheticRole_SYNTHETIC_ROLE_PROTO_FIELD,
				ProtoNumberPath: []uint32{1},
				Type: &datav1.DataType{Kind: &datav1.DataType_FixedBytes{FixedBytes: &datav1.FixedBytesType{
					ByteLength: 257,
				}}},
			}}},
			wantError: "allow_suspicious_fixed_string_types",
			wantPath:  "digest",
		},
		{
			name: "invalid timestamp semantics",
			dataset: &datav1.DatasetSchema{Name: "record", Fields: []*datav1.Field{{
				Name:            "created_at",
				StableId:        1,
				Presence:        datav1.Presence_PRESENCE_EXPLICIT,
				Nullable:        true,
				SyntheticRole:   datav1.SyntheticRole_SYNTHETIC_ROLE_PROTO_FIELD,
				ProtoNumberPath: []uint32{1},
				Type: &datav1.DataType{Kind: &datav1.DataType_Timestamp{Timestamp: &datav1.TimestampType{
					Unit:     datav1.TimeUnit_TIME_UNIT_NANOSECOND,
					Timezone: "local",
				}}},
			}}},
			wantError: "timestamp must use nanoseconds and UTC",
			wantPath:  "created_at",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, diagnostics, err := Schema(test.dataset)
			require.ErrorContains(t, err, test.wantError)
			require.NotEmpty(t, diagnostics)
			diagnostic := findDiagnostic(t, diagnostics, test.wantPath)
			require.Equal(t,
				datav1.MappingCompatibility_MAPPING_COMPATIBILITY_UNSUPPORTED,
				diagnostic.GetCompatibility(),
			)
		})
	}
}

func TestPublicAPIsRejectMalformedContracts(t *testing.T) {
	schema, diagnostics, err := Schema(nil)
	require.Nil(t, schema)
	require.Nil(t, diagnostics)
	require.EqualError(t, err, "clickhouse: nil dataset schema")
	require.Empty(t, (*TableSchema)(nil).ColumnDeclarations())
	_, _, err = ProjectToIceberg(nil)
	require.EqualError(t, err, "clickhouse: nil dataset schema")
	_, err = ProjectionJSON(nil)
	require.EqualError(t, err, "clickhouse: nil Iceberg projection")

	for _, test := range []struct {
		name      string
		field     *datav1.Field
		wantError string
	}{
		{
			name: "invalid field text",
			field: func() *datav1.Field {
				field := primitiveField("value", 1, datav1.PrimitiveKind_PRIMITIVE_KIND_STRING)
				field.Description = "invalid\x00comment"
				return field
			}(),
			wantError: "description: text contains an unsupported control character",
		},
		{
			name: "unspecified primitive",
			field: primitiveField("value", 1,
				datav1.PrimitiveKind_PRIMITIVE_KIND_UNSPECIFIED),
			wantError: "unsupported primitive kind",
		},
		{
			name: "empty enum",
			field: semanticField("value", 1, &datav1.DataType{
				Kind: &datav1.DataType_Enum{Enum: &datav1.EnumType{}},
			}),
			wantError: "enum has no declared values",
		},
		{
			name: "invalid decimal",
			field: semanticField("value", 1, &datav1.DataType{
				Kind: &datav1.DataType_Decimal{Decimal: &datav1.DecimalType{Precision: 0}},
			}),
			wantError: "decimal precision must be between 1 and 38",
		},
		{
			name: "invalid decimal scale",
			field: semanticField("value", 1, &datav1.DataType{
				Kind: &datav1.DataType_Decimal{Decimal: &datav1.DecimalType{Precision: 2, Scale: 3}},
			}),
			wantError: "decimal scale must not exceed precision",
		},
		{
			name: "missing list element",
			field: &datav1.Field{
				Name: "value", StableId: 1,
				Presence:      datav1.Presence_PRESENCE_REPEATED,
				Type:          &datav1.DataType{Kind: &datav1.DataType_List{List: &datav1.ListType{}}},
				SyntheticRole: datav1.SyntheticRole_SYNTHETIC_ROLE_PROTO_FIELD,
			},
			wantError: "list is missing its element",
		},
		{
			name: "missing map child",
			field: &datav1.Field{
				Name: "value", StableId: 1,
				Presence:      datav1.Presence_PRESENCE_MAP,
				Type:          &datav1.DataType{Kind: &datav1.DataType_Map{Map: &datav1.MapType{}}},
				SyntheticRole: datav1.SyntheticRole_SYNTHETIC_ROLE_PROTO_FIELD,
			},
			wantError: "map is missing its key or value",
		},
		{
			name: "malformed explicit presence",
			field: func() *datav1.Field {
				field := primitiveField("value", 1, datav1.PrimitiveKind_PRIMITIVE_KIND_INT32)
				field.Presence = datav1.Presence_PRESENCE_EXPLICIT
				return field
			}(),
			wantError: "explicit presence requires nullable=true",
		},
		{
			name: "malformed implicit presence",
			field: semanticField("value", 1, &datav1.DataType{
				Kind: &datav1.DataType_Uuid{Uuid: &datav1.UuidType{}},
			}),
			wantError: "implicit presence requires a non-null primitive or enum",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.name == "malformed implicit presence" {
				test.field.Presence = datav1.Presence_PRESENCE_IMPLICIT
				test.field.Nullable = false
			}
			_, diagnostics, err := Schema(&datav1.DatasetSchema{
				Name:   "record",
				Fields: []*datav1.Field{test.field},
			})
			require.ErrorContains(t, err, test.wantError)
			require.Equal(t,
				datav1.MappingCompatibility_MAPPING_COMPATIBILITY_UNSUPPORTED,
				findDiagnostic(t, diagnostics, "value").GetCompatibility(),
			)
		})
	}

	for _, dataset := range []*datav1.DatasetSchema{
		{Name: ""},
		{Name: "bad\x00name"},
		{Name: "record", Description: "bad\x00description"},
	} {
		_, _, err := Schema(dataset)
		require.Error(t, err)
	}
}

func TestEmptyMessagesHaveOneConstrainedRepresentation(t *testing.T) {
	emptyRoot, diagnostics, err := Schema(&datav1.DatasetSchema{
		Name:          "empty",
		SourceMessage: "example.Empty",
	})
	require.NoError(t, err)
	require.Empty(t, diagnostics)
	require.Equal(t, "__invariant_unit", emptyRoot.Columns[0].Name)
	require.True(t, emptyRoot.Columns[0].Synthetic)
	require.Contains(t, emptyRoot.ColumnDeclarations(), "CHECK `__invariant_unit` = false")

	emptyNested := &datav1.DatasetSchema{
		Name: "record",
		Fields: []*datav1.Field{{
			Name:            "empty",
			StableId:        1,
			Presence:        datav1.Presence_PRESENCE_EXPLICIT,
			Nullable:        true,
			SyntheticRole:   datav1.SyntheticRole_SYNTHETIC_ROLE_PROTO_FIELD,
			ProtoNumberPath: []uint32{1},
			Type: &datav1.DataType{Kind: &datav1.DataType_Struct{
				Struct: &datav1.StructType{},
			}},
		}},
	}
	nestedSchema, nestedDiagnostics, err := Schema(emptyNested)
	require.NoError(t, err)
	require.Equal(t,
		"Tuple(`present` Bool, `value` Tuple(`__invariant_unit` Bool))",
		nestedSchema.Columns[0].Type,
	)
	require.Contains(t, nestedSchema.ColumnDeclarations(),
		"NOT tupleElement(`empty`, 'present') OR (tupleElement(tupleElement(`empty`, 'value'), '__invariant_unit') = false)",
	)
	require.Equal(t,
		datav1.MappingCompatibility_MAPPING_COMPATIBILITY_REPRESENTATION_CHANGED,
		findDiagnostic(t, nestedDiagnostics, "empty").GetCompatibility(),
	)
}

func TestClickHouseToIcebergProjectionIsExact(t *testing.T) {
	projection, diagnostics, err := ProjectToIceberg(allTypesDataset(t))
	require.NoError(t, err)
	require.EqualValues(t, ProjectionVersion, projection.Version)
	require.NotEmpty(t, diagnostics)

	for _, path := range []string{"uint64_value", "fixed64_value"} {
		field := findProjection(t, projection.Fields, path)
		require.Equal(t, "UInt64", field.ClickHouseType)
		require.Equal(t, "decimal(20, 0)", field.IcebergType)
		require.Equal(t,
			"accurateCast({value}, 'Decimal(20, 0)')",
			field.ValueExpression,
		)
		require.Equal(t, "true", field.PresenceExpression)
	}
	uint32Field := findProjection(t, projection.Fields, "uint32_value")
	require.Equal(t, "toInt64({value})", uint32Field.ValueExpression)
	require.Equal(t, "long", uint32Field.IcebergType)

	optional := findProjection(t, projection.Fields, "optional_note")
	require.Equal(t, "isNotNull({value})", optional.PresenceExpression)
	require.Equal(t, "assumeNotNull({value})", optional.ValueExpression)

	nested := findProjection(t, projection.Fields, "nested")
	require.Equal(t, "struct", nested.IcebergType)
	require.Equal(t, "tupleElement({value}, 'present')", nested.PresenceExpression)
	require.Equal(t, "tupleElement({value}, 'value')", nested.ValueExpression)
	require.NotEmpty(t, nested.Children)

	oneof := findProjection(t, projection.Fields, "choice_count")
	require.Equal(t, "__invariant_oneof_choice_case", oneof.Discriminator)
	require.EqualValues(t, 22, oneof.ProtobufFieldNumber)
	require.Equal(t,
		"tupleElement({value}, 'present') AND {case} = 22",
		oneof.PresenceExpression,
	)
	require.Equal(t, "tupleElement({value}, 'value')", oneof.ValueExpression)

	mapValue := findProjection(t, projection.Fields, "counters.value")
	require.Equal(t, "accurateCast({value}, 'Decimal(20, 0)')", mapValue.ValueExpression)
	require.Equal(t, "decimal(20, 0)", mapValue.IcebergType)
	require.Equal(t, "list", findProjection(t, projection.Fields, "labels").IcebergType)
	require.Equal(t, "map", findProjection(t, projection.Fields, "counters").IcebergType)

	var sawIcebergUInt64Diagnostic bool
	for _, diagnostic := range diagnostics {
		if diagnostic.GetFieldPath() == "uint64_value" &&
			strings.Contains(diagnostic.GetMessage(), "Iceberg decimal(20, 0)") {
			sawIcebergUInt64Diagnostic = true
			break
		}
	}
	require.True(t, sawIcebergUInt64Diagnostic)

	timestamp := findProjection(t, projection.Fields, "created_at")
	require.Equal(t,
		"toUnixTimestamp64Nano(assumeNotNull({value}))",
		timestamp.ValueExpression,
	)

	required, err := data.CompileMessage((&greetpb.Proto2Record{}).ProtoReflect().Descriptor(), nil)
	require.NoError(t, err)
	_, _, err = ProjectToIceberg(required)
	require.ErrorContains(t, err, "required protobuf field")
}

func TestSchemaEvolutionPreservesCompatibleColumnsAndRejectsTypeReuse(t *testing.T) {
	first := compileEvolution(t, nil,
		evolutionField("id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT32),
	)
	second := compileEvolution(t, first,
		evolutionField("id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT32),
		evolutionField("label", 2, descriptorpb.FieldDescriptorProto_TYPE_STRING),
	)

	firstSchema, _, err := Schema(first.GetDatasets()[0])
	require.NoError(t, err)
	secondSchema, _, err := Schema(second.GetDatasets()[0])
	require.NoError(t, err)
	require.Equal(t, firstSchema.Columns[0], secondSchema.Columns[0])
	require.Equal(t, "id", secondSchema.Columns[0].Name, "compatible unchanged field must retain its storage name")
	require.Equal(t, "label", secondSchema.Columns[1].Name, "additive field must append deterministically")

	_, err = compileEvolutionError(second,
		evolutionField("id", 1, descriptorpb.FieldDescriptorProto_TYPE_STRING),
		evolutionField("label", 2, descriptorpb.FieldDescriptorProto_TYPE_STRING),
	)
	require.ErrorContains(t, err, "logical shape changed")
}

func allTypesDataset(t *testing.T) *datav1.DatasetSchema {
	t.Helper()
	dataset, err := data.CompileMessage((&greetpb.CanonicalRecord{}).ProtoReflect().Descriptor(), nil)
	require.NoError(t, err)
	dataset = proto.Clone(dataset).(*datav1.DatasetSchema)
	dataset.Description = "Canonical ClickHouse projection."
	dataset.Fields = append(dataset.Fields,
		semanticField("amount", 101, &datav1.DataType{Kind: &datav1.DataType_Decimal{Decimal: &datav1.DecimalType{
			Precision: 18,
			Scale:     4,
		}}}),
		semanticField("record_id", 102, &datav1.DataType{Kind: &datav1.DataType_Uuid{Uuid: &datav1.UuidType{}}}),
		semanticField("digest", 103, &datav1.DataType{Kind: &datav1.DataType_FixedBytes{FixedBytes: &datav1.FixedBytesType{
			ByteLength: 24,
		}}}),
		semanticField("json_value", 104, &datav1.DataType{Kind: &datav1.DataType_Json{Json: &datav1.JsonType{
			Kind: datav1.JsonKind_JSON_KIND_VALUE,
		}}}),
		semanticField("json_list", 105, &datav1.DataType{Kind: &datav1.DataType_Json{Json: &datav1.JsonType{
			Kind: datav1.JsonKind_JSON_KIND_LIST_VALUE,
		}}}),
	)
	return dataset
}

func semanticField(name string, number uint32, dataType *datav1.DataType) *datav1.Field {
	return &datav1.Field{
		Name:              name,
		ProtoFullName:     "data.v1.CanonicalRecord." + name,
		ProtoNumberPath:   []uint32{number},
		StableId:          int32(number),
		Presence:          datav1.Presence_PRESENCE_EXPLICIT,
		Nullable:          true,
		Description:       "Semantic " + name + ".",
		Type:              dataType,
		SyntheticRole:     datav1.SyntheticRole_SYNTHETIC_ROLE_PROTO_FIELD,
		JsonName:          name,
		StorageNameSource: name,
	}
}

func primitiveField(name string, number uint32, kind datav1.PrimitiveKind) *datav1.Field {
	return &datav1.Field{
		Name:              name,
		ProtoFullName:     "example.Record." + name,
		ProtoNumberPath:   []uint32{number},
		StableId:          int32(number),
		Presence:          datav1.Presence_PRESENCE_IMPLICIT,
		Type:              &datav1.DataType{Kind: &datav1.DataType_Primitive{Primitive: &datav1.PrimitiveType{Kind: kind}}},
		SyntheticRole:     datav1.SyntheticRole_SYNTHETIC_ROLE_PROTO_FIELD,
		JsonName:          name,
		StorageNameSource: name,
	}
}

func oneofField(name string, number uint32, oneof string, kind datav1.PrimitiveKind) *datav1.Field {
	field := primitiveField(name, number, kind)
	field.Presence = datav1.Presence_PRESENCE_ONEOF
	field.Nullable = true
	field.Oneof = oneof
	return field
}

func nestedOneofType(stableID int32) *datav1.DataType {
	text := oneofField("text", 1, "choice", datav1.PrimitiveKind_PRIMITIVE_KIND_STRING)
	text.StableId = stableID
	count := oneofField("count", 2, "choice", datav1.PrimitiveKind_PRIMITIVE_KIND_INT32)
	count.StableId = stableID + 1
	return &datav1.DataType{Kind: &datav1.DataType_Struct{Struct: &datav1.StructType{
		Fields: []*datav1.Field{text, count},
	}}}
}

func findColumn(t *testing.T, schema *TableSchema, name string) Column {
	t.Helper()
	for _, column := range schema.Columns {
		if column.Name == name {
			return column
		}
	}
	require.FailNow(t, "missing ClickHouse column", "name %q", name)
	return Column{}
}

func datasetField(t *testing.T, dataset *datav1.DatasetSchema, name string) *datav1.Field {
	t.Helper()
	for _, field := range dataset.GetFields() {
		if field.GetName() == name {
			return field
		}
	}
	require.FailNow(t, "missing logical field", "name %q", name)
	return nil
}

func findDiagnostic(t *testing.T, diagnostics []*datav1.MappingDiagnostic, path string) *datav1.MappingDiagnostic {
	t.Helper()
	for _, diagnostic := range diagnostics {
		if diagnostic.GetFieldPath() == path {
			return diagnostic
		}
	}
	require.FailNow(t, "missing diagnostic", "path %q", path)
	return nil
}

func findProjection(
	t *testing.T,
	fields []IcebergFieldProjection,
	path string,
) IcebergFieldProjection {
	t.Helper()
	for _, field := range fields {
		if field.FieldPath == path {
			return field
		}
		if child := findProjectionOptional(field.Children, path); child != nil {
			return *child
		}
	}
	require.FailNow(t, "missing projection field", "path %q", path)
	return IcebergFieldProjection{}
}

func findProjectionOptional(fields []IcebergFieldProjection, path string) *IcebergFieldProjection {
	for index := range fields {
		if fields[index].FieldPath == path {
			return &fields[index]
		}
		if child := findProjectionOptional(fields[index].Children, path); child != nil {
			return child
		}
	}
	return nil
}

func evolutionField(
	name string,
	number int32,
	kind descriptorpb.FieldDescriptorProto_Type,
) *descriptorpb.FieldDescriptorProto {
	label := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	return &descriptorpb.FieldDescriptorProto{
		Name:     new(name),
		JsonName: new(name),
		Number:   new(number),
		Label:    &label,
		Type:     &kind,
	}
}

func compileEvolution(
	t *testing.T,
	previous *datav1.SchemaBundle,
	fields ...*descriptorpb.FieldDescriptorProto,
) *datav1.SchemaBundle {
	t.Helper()
	bundle, err := compileEvolutionError(previous, fields...)
	require.NoError(t, err)
	return bundle
}

func compileEvolutionError(
	previous *datav1.SchemaBundle,
	fields ...*descriptorpb.FieldDescriptorProto,
) (*datav1.SchemaBundle, error) {
	syntax := "proto3"
	set := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{{
		Name:    new("example/record.proto"),
		Package: new("example"),
		Syntax:  &syntax,
		MessageType: []*descriptorpb.DescriptorProto{{
			Name:  new("Record"),
			Field: fields,
		}},
	}}}
	encoded, err := proto.Marshal(set)
	if err != nil {
		return nil, err
	}
	return data.CompileDescriptorBytes(encoded, []string{"example.Record"}, previous)
}
