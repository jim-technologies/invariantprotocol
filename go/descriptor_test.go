package invariant

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	invpb "github.com/jim-technologies/invariantprotocol/go/gen/invariant/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func descriptorPath() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "python", "tests", "proto", "descriptor.binpb")
}

func mustParse(t *testing.T) *invpb.ParsedDescriptor {
	t.Helper()
	pd, err := parseDescriptorFile(descriptorPath())
	require.NoError(t, err)
	return pd
}

// -- Services --

func TestServiceFound(t *testing.T) {
	pd := mustParse(t)
	assert.Contains(t, pd.Services, "greet.v1.GreetService")
}

func TestServiceName(t *testing.T) {
	svc := mustParse(t).Services["greet.v1.GreetService"]
	assert.Equal(t, "GreetService", svc.Name)
	assert.Equal(t, "greet.v1.GreetService", svc.FullName)
}

func TestServiceComment(t *testing.T) {
	svc := mustParse(t).Services["greet.v1.GreetService"]
	assert.Contains(t, strings.ToLower(svc.Comment), "simple greeting service")
}

func TestServiceHasTwoMethods(t *testing.T) {
	svc := mustParse(t).Services["greet.v1.GreetService"]
	assert.Len(t, svc.Methods, 2)
	assert.Contains(t, svc.Methods, "Greet")
	assert.Contains(t, svc.Methods, "GreetGroup")
}

// -- Methods --

func TestMethodGreet(t *testing.T) {
	m := mustParse(t).Services["greet.v1.GreetService"].Methods["Greet"]
	assert.Equal(t, "greet.v1.GreetRequest", m.InputType)
	assert.Equal(t, "greet.v1.GreetResponse", m.OutputType)
	assert.False(t, m.ClientStreaming)
	assert.False(t, m.ServerStreaming)
}

func TestMethodGreetGroup(t *testing.T) {
	m := mustParse(t).Services["greet.v1.GreetService"].Methods["GreetGroup"]
	assert.Equal(t, "greet.v1.GreetGroupRequest", m.InputType)
	assert.Equal(t, "greet.v1.GreetGroupResponse", m.OutputType)
}

func TestMethodComment(t *testing.T) {
	m := mustParse(t).Services["greet.v1.GreetService"].Methods["Greet"]
	assert.Contains(t, strings.ToLower(m.Comment), "greet a person")
}

// -- Messages --

func TestMessagesParsed(t *testing.T) {
	pd := mustParse(t)
	for _, name := range []string{
		"greet.v1.GreetRequest",
		"greet.v1.GreetResponse",
		"greet.v1.Person",
		"greet.v1.GreetGroupRequest",
		"greet.v1.GreetGroupResponse",
	} {
		assert.Contains(t, pd.Messages, name)
	}
}

func TestGreetRequestFields(t *testing.T) {
	msg := mustParse(t).Messages["greet.v1.GreetRequest"]
	names := fieldNames(msg)
	assert.Contains(t, names, "name")
	assert.Contains(t, names, "mood")
	assert.Contains(t, names, "tags")
}

func TestFieldComment(t *testing.T) {
	f := findField(t, mustParse(t).Messages["greet.v1.GreetRequest"], "name")
	assert.Contains(t, strings.ToLower(f.Comment), "name of the person")
}

func TestOptionalField(t *testing.T) {
	f := findField(t, mustParse(t).Messages["greet.v1.GreetRequest"], "mood")
	assert.True(t, f.Optional)
}

func TestNonOptionalField(t *testing.T) {
	f := findField(t, mustParse(t).Messages["greet.v1.GreetRequest"], "name")
	assert.False(t, f.Optional)
}

func TestMapEntryMessage(t *testing.T) {
	pd := mustParse(t)
	count := 0
	for _, m := range pd.Messages {
		if m.IsMapEntry {
			count++
		}
	}
	assert.GreaterOrEqual(t, count, 1)
}

func TestRepeatedField(t *testing.T) {
	f := findField(t, mustParse(t).Messages["greet.v1.GreetGroupRequest"], "people")
	assert.Equal(t, int32(labelRepeated), f.Label)
}

func TestNestedMessageReference(t *testing.T) {
	f := findField(t, mustParse(t).Messages["greet.v1.GreetGroupRequest"], "people")
	assert.Equal(t, "greet.v1.Person", f.TypeName)
}

func TestEnumFieldType(t *testing.T) {
	f := findField(t, mustParse(t).Messages["greet.v1.GreetRequest"], "mood")
	assert.Equal(t, int32(typeEnum), f.Type)
}

// -- Enums --

func TestEnumParsed(t *testing.T) {
	assert.Contains(t, mustParse(t).Enums, "greet.v1.Mood")
}

func TestEnumValues(t *testing.T) {
	e := mustParse(t).Enums["greet.v1.Mood"]
	var names []string
	for _, v := range e.Values {
		names = append(names, v.Name)
	}
	assert.Equal(t, []string{"MOOD_UNSPECIFIED", "MOOD_HAPPY", "MOOD_SAD"}, names)
}

func TestEnumComment(t *testing.T) {
	e := mustParse(t).Enums["greet.v1.Mood"]
	assert.Contains(t, strings.ToLower(e.Comment), "mood")
}

func TestEnumValueComments(t *testing.T) {
	e := mustParse(t).Enums["greet.v1.Mood"]
	for _, v := range e.Values {
		if v.Name == "MOOD_HAPPY" {
			assert.Contains(t, strings.ToLower(v.Comment), "happy")
			return
		}
	}
	t.Fatal("MOOD_HAPPY value not found")
}

// -- Error cases --

func TestFromFileNotFound(t *testing.T) {
	_, err := parseDescriptorFile("/nonexistent/path.binpb")
	assert.Error(t, err)
}

// -- Helpers --

func fieldNames(msg *invpb.MessageInfo) []string {
	var names []string
	for _, f := range msg.Fields {
		names = append(names, f.Name)
	}
	return names
}

func findField(t *testing.T, msg *invpb.MessageInfo, name string) *invpb.FieldInfo {
	t.Helper()
	for _, f := range msg.Fields {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("field %q not found", name)
	return nil
}
