package invariant

import (
	"testing"

	invpb "github.com/jim-technologies/invariantprotocol/go/gen/invariant/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func schemaGen(t *testing.T) *schemaGenerator {
	t.Helper()
	return newSchemaGenerator(mustParse(t))
}

func props(t *testing.T, s map[string]any) map[string]any {
	t.Helper()
	p, ok := s["properties"].(map[string]any)
	require.True(t, ok, "properties missing or wrong type")
	return p
}

func TestSchemaBasicStructure(t *testing.T) {
	s := schemaGen(t).MessageToSchema("greet.v1.GreetRequest")
	assert.Equal(t, "object", s["type"])
	assert.Equal(t, false, s["additionalProperties"])
	assert.Contains(t, props(t, s), "name")
}

func TestSchemaRequiredFields(t *testing.T) {
	sg := schemaGen(t)

	s := sg.MessageToSchema("greet.v1.GreetRequest")
	required := toStringSlice(s["required"])
	assert.Contains(t, required, "name")
	assert.NotContains(t, required, "mood") // optional
	assert.NotContains(t, required, "tags") // map

	s = sg.MessageToSchema("greet.v1.GreetGroupRequest")
	assert.NotContains(t, toStringSlice(s["required"]), "people") // repeated

	// Person.mood is not proto3 optional — should be required
	s = sg.MessageToSchema("greet.v1.Person")
	personReq := toStringSlice(s["required"])
	assert.Contains(t, personReq, "name")
	assert.Contains(t, personReq, "mood")
}

func TestSchemaFieldTypes(t *testing.T) {
	sg := schemaGen(t)
	p := props(t, sg.MessageToSchema("greet.v1.GreetRequest"))

	// String field
	assert.Equal(t, "string", p["name"].(map[string]any)["type"])

	// Enum field
	mood := p["mood"].(map[string]any)
	assert.Equal(t, "string", mood["type"])
	enumVals := toStringSlice(mood["enum"])
	assert.Contains(t, enumVals, "MOOD_UNSPECIFIED")
	assert.Contains(t, enumVals, "MOOD_HAPPY")
	assert.Contains(t, enumVals, "MOOD_SAD")

	// Map field
	tags := p["tags"].(map[string]any)
	assert.Equal(t, "object", tags["type"])
	assert.Equal(t, "string", tags["additionalProperties"].(map[string]any)["type"])

	// Integer field
	p2 := props(t, sg.MessageToSchema("greet.v1.GreetGroupResponse"))
	assert.Equal(t, "integer", p2["count"].(map[string]any)["type"])

	// Repeated message field
	p3 := props(t, sg.MessageToSchema("greet.v1.GreetGroupRequest"))
	people := p3["people"].(map[string]any)
	assert.Equal(t, "array", people["type"])
	assert.Equal(t, "object", people["items"].(map[string]any)["type"])
	assert.Contains(t, people["items"].(map[string]any)["properties"].(map[string]any), "name")

	// Repeated scalar field
	msgs := p2["messages"].(map[string]any)
	assert.Equal(t, "array", msgs["type"])
	assert.Equal(t, "string", msgs["items"].(map[string]any)["type"])
}

func TestSchemaUsesProtoJSONNamesAnd64BitStrings(t *testing.T) {
	sg := schemaGen(t)

	greet := sg.MessageToSchema("greet.v1.GreetRequest")
	greetProps := props(t, greet)
	assert.NotContains(t, greetProps, "account_sequence")
	sequence := greetProps["wireSequenceId"].(map[string]any)
	assert.Equal(t, "string", sequence["type"])
	assert.Equal(t, `^(0|-?[1-9][0-9]*)$`, sequence["pattern"])
	assert.NotContains(t, toStringSlice(greet["required"]), "wireSequenceId")

	record := props(t, sg.MessageToSchema("data.v1.CanonicalRecord"))
	for _, name := range []string{"int64Value", "sfixed64Value", "sint64Value"} {
		field := record[name].(map[string]any)
		assert.Equal(t, "string", field["type"], name)
		assert.Equal(t, `^(0|-?[1-9][0-9]*)$`, field["pattern"], name)
	}
	for _, name := range []string{"uint64Value", "fixed64Value"} {
		field := record[name].(map[string]any)
		assert.Equal(t, "string", field["type"], name)
		assert.Equal(t, `^(0|[1-9][0-9]*)$`, field["pattern"], name)
	}
	assert.Equal(t, "integer", record["int32Value"].(map[string]any)["type"])
	assert.Equal(
		t,
		"string",
		record["counters"].(map[string]any)["additionalProperties"].(map[string]any)["type"],
	)
	assert.Equal(t, "string", props(t, record["nested"].(map[string]any))["id"].(map[string]any)["type"])

	proto2 := sg.MessageToSchema("data.v1.Proto2Record")
	assert.Equal(t, []string{"id"}, toStringSlice(proto2["required"]))
	assert.Equal(t, "string", props(t, proto2)["id"].(map[string]any)["type"])
}

func TestSchemaCoversCanonicalProtoJSONSpecialValuesAndWellKnownTypes(t *testing.T) {
	sg := schemaGen(t)

	for _, schema := range []map[string]any{
		sg.fieldTypeSchema(findField(t, sg.parsed.Messages["data.v1.CanonicalRecord"], "double_value")),
		sg.fieldTypeSchema(findField(t, sg.parsed.Messages["data.v1.CanonicalRecord"], "float_value")),
		sg.messageTypeSchema("google.protobuf.DoubleValue"),
		sg.messageTypeSchema("google.protobuf.FloatValue"),
	} {
		choices := schema["oneOf"].([]any)
		require.Len(t, choices, 2)
		assert.Equal(t, "number", choices[0].(map[string]any)["type"])
		assert.Equal(
			t,
			[]string{"NaN", "Infinity", "-Infinity"},
			toStringSlice(choices[1].(map[string]any)["enum"]),
		)
	}

	assert.Equal(t, map[string]any{"type": "string"}, sg.messageTypeSchema("google.protobuf.FieldMask"))
	assert.Equal(
		t,
		map[string]any{"type": "array", "items": map[string]any{}},
		sg.messageTypeSchema("google.protobuf.ListValue"),
	)
	assert.Equal(
		t,
		map[string]any{"type": "object", "additionalProperties": false},
		sg.messageTypeSchema("google.protobuf.Empty"),
	)
	assert.Equal(t, map[string]any{"type": "null"}, sg.enumSchema("google.protobuf.NullValue"))
	assert.Equal(
		t,
		map[string]any{
			"type":    "string",
			"pattern": `^-?(?:0|[1-9][0-9]*)(?:\.[0-9]{1,9})?s$`,
		},
		sg.messageTypeSchema("google.protobuf.Duration"),
	)
}

func TestSchemaConstrainsCanonicalProtoJSONMapKeys(t *testing.T) {
	sg := schemaGen(t)
	for _, test := range []struct {
		name       string
		fieldType  int32
		constraint map[string]any
	}{
		{name: "bool", fieldType: typeBool, constraint: map[string]any{"enum": []string{"false", "true"}}},
		{name: "signed", fieldType: typeInt64, constraint: map[string]any{"pattern": `^(0|-?[1-9][0-9]*)$`}},
		{name: "unsigned", fieldType: typeUint64, constraint: map[string]any{"pattern": `^(0|[1-9][0-9]*)$`}},
	} {
		t.Run(test.name, func(t *testing.T) {
			schema := sg.mapSchema(&invpb.MessageInfo{Fields: []*invpb.FieldInfo{
				{Name: "key", Type: test.fieldType},
				{Name: "value", Type: typeString},
			}})
			assert.Equal(t, test.constraint, schema["propertyNames"])
		})
	}

	stringKey := sg.mapSchema(&invpb.MessageInfo{Fields: []*invpb.FieldInfo{
		{Name: "key", Type: typeString},
		{Name: "value", Type: typeString},
	}})
	assert.NotContains(t, stringKey, "propertyNames")
}

func TestSchemaNestedMessageAndDescriptions(t *testing.T) {
	sg := schemaGen(t)
	p := props(t, sg.MessageToSchema("greet.v1.GreetGroupRequest"))
	person := p["people"].(map[string]any)["items"].(map[string]any)
	assert.Equal(t, "object", person["type"])
	personProps := person["properties"].(map[string]any)
	assert.Contains(t, personProps, "name")
	assert.Contains(t, personProps, "mood")
	assert.Equal(t, "string", personProps["mood"].(map[string]any)["type"])
	assert.Contains(t, personProps["mood"].(map[string]any), "enum")

	// Field descriptions
	p2 := props(t, sg.MessageToSchema("greet.v1.GreetRequest"))
	assert.Contains(t, p2["name"].(map[string]any)["description"].(string), "Name of the person")
	assert.Contains(t, p2["mood"].(map[string]any)["description"].(string), "Optional mood")
}

func TestUnknownMessageReturnsGenericObject(t *testing.T) {
	s := schemaGen(t).MessageToSchema("does.not.Exist")
	assert.Equal(t, map[string]any{"type": "object"}, s)
}

// -- Helpers --

func toStringSlice(v any) []string {
	if v == nil {
		return nil
	}
	switch sl := v.(type) {
	case []string:
		return sl
	case []any:
		var result []string
		for _, item := range sl {
			if s, ok := item.(string); ok {
				result = append(result, s)
			}
		}
		return result
	}
	return nil
}
