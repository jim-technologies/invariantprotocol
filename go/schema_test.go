package invariant

import (
	"testing"

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

// -- Basic structure --

func TestSchemaTypeIsObject(t *testing.T) {
	s := schemaGen(t).MessageToSchema("greet.v1.GreetRequest")
	assert.Equal(t, "object", s["type"])
}

func TestSchemaHasProperties(t *testing.T) {
	s := schemaGen(t).MessageToSchema("greet.v1.GreetRequest")
	p := props(t, s)
	assert.Contains(t, p, "name")
}

func TestSchemaAdditionalPropertiesFalse(t *testing.T) {
	s := schemaGen(t).MessageToSchema("greet.v1.GreetRequest")
	assert.Equal(t, false, s["additionalProperties"])
}

// -- Required fields --

func TestRequiredIncludesNonOptional(t *testing.T) {
	s := schemaGen(t).MessageToSchema("greet.v1.GreetRequest")
	assert.Contains(t, toStringSlice(s["required"]), "name")
}

func TestRequiredExcludesOptional(t *testing.T) {
	s := schemaGen(t).MessageToSchema("greet.v1.GreetRequest")
	assert.NotContains(t, toStringSlice(s["required"]), "mood")
}

func TestRequiredExcludesMap(t *testing.T) {
	s := schemaGen(t).MessageToSchema("greet.v1.GreetRequest")
	assert.NotContains(t, toStringSlice(s["required"]), "tags")
}

func TestRequiredExcludesRepeated(t *testing.T) {
	s := schemaGen(t).MessageToSchema("greet.v1.GreetGroupRequest")
	assert.NotContains(t, toStringSlice(s["required"]), "people")
}

// -- String fields --

func TestStringFieldSchema(t *testing.T) {
	p := props(t, schemaGen(t).MessageToSchema("greet.v1.GreetRequest"))
	assert.Equal(t, "string", p["name"].(map[string]any)["type"])
}

// -- Enum fields --

func TestEnumFieldSchema(t *testing.T) {
	p := props(t, schemaGen(t).MessageToSchema("greet.v1.GreetRequest"))
	mood := p["mood"].(map[string]any)
	assert.Equal(t, "string", mood["type"])
	enumVals := toStringSlice(mood["enum"])
	assert.Contains(t, enumVals, "MOOD_UNSPECIFIED")
	assert.Contains(t, enumVals, "MOOD_HAPPY")
	assert.Contains(t, enumVals, "MOOD_SAD")
}

// -- Map fields --

func TestMapFieldSchema(t *testing.T) {
	p := props(t, schemaGen(t).MessageToSchema("greet.v1.GreetRequest"))
	tags := p["tags"].(map[string]any)
	assert.Equal(t, "object", tags["type"])
	assert.Equal(t, "string", tags["additionalProperties"].(map[string]any)["type"])
}

// -- Repeated fields --

func TestRepeatedMessageFieldSchema(t *testing.T) {
	p := props(t, schemaGen(t).MessageToSchema("greet.v1.GreetGroupRequest"))
	people := p["people"].(map[string]any)
	assert.Equal(t, "array", people["type"])
	items := people["items"].(map[string]any)
	assert.Equal(t, "object", items["type"])
	assert.Contains(t, items["properties"].(map[string]any), "name")
}

func TestRepeatedScalarFieldSchema(t *testing.T) {
	p := props(t, schemaGen(t).MessageToSchema("greet.v1.GreetGroupResponse"))
	msgs := p["messages"].(map[string]any)
	assert.Equal(t, "array", msgs["type"])
	assert.Equal(t, "string", msgs["items"].(map[string]any)["type"])
}

// -- Integer fields --

func TestIntegerFieldSchema(t *testing.T) {
	p := props(t, schemaGen(t).MessageToSchema("greet.v1.GreetGroupResponse"))
	assert.Equal(t, "integer", p["count"].(map[string]any)["type"])
}

// -- Nested message fields --

func TestNestedMessageSchema(t *testing.T) {
	p := props(t, schemaGen(t).MessageToSchema("greet.v1.GreetGroupRequest"))
	person := p["people"].(map[string]any)["items"].(map[string]any)
	assert.Equal(t, "object", person["type"])
	personProps := person["properties"].(map[string]any)
	assert.Contains(t, personProps, "name")
	assert.Contains(t, personProps, "mood")
	assert.Equal(t, "string", personProps["mood"].(map[string]any)["type"])
	assert.Contains(t, personProps["mood"].(map[string]any), "enum")
}

// -- Field descriptions --

func TestFieldDescriptionInSchema(t *testing.T) {
	p := props(t, schemaGen(t).MessageToSchema("greet.v1.GreetRequest"))
	desc := p["name"].(map[string]any)["description"].(string)
	assert.Contains(t, desc, "Name of the person")
}

func TestEnumFieldDescriptionInSchema(t *testing.T) {
	p := props(t, schemaGen(t).MessageToSchema("greet.v1.GreetRequest"))
	desc := p["mood"].(map[string]any)["description"].(string)
	assert.Contains(t, desc, "Optional mood")
}

// -- Unknown message --

func TestUnknownMessageReturnsGenericObject(t *testing.T) {
	s := schemaGen(t).MessageToSchema("does.not.Exist")
	assert.Equal(t, map[string]any{"type": "object"}, s)
}

// -- Person message (non-optional enum) --

func TestPersonMoodRequired(t *testing.T) {
	s := schemaGen(t).MessageToSchema("greet.v1.Person")
	required := toStringSlice(s["required"])
	assert.Contains(t, required, "name")
	assert.Contains(t, required, "mood")
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
