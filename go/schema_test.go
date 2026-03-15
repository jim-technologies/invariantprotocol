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
