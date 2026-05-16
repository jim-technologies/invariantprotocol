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

func TestParseServices(t *testing.T) {
	pd := mustParse(t)
	svc := pd.Services["greet.v1.GreetService"]
	require.NotNil(t, svc)

	assert.Equal(t, "GreetService", svc.Name)
	assert.Equal(t, "greet.v1.GreetService", svc.FullName)
	assert.Contains(t, strings.ToLower(svc.Comment), "simple greeting service")
	assert.Len(t, svc.Methods, 3)
	assert.Contains(t, svc.Methods, "Greet")
	assert.Contains(t, svc.Methods, "GreetGroup")
	assert.Contains(t, svc.Methods, "StreamGreet")

	greet := svc.Methods["Greet"]
	assert.Equal(t, "greet.v1.GreetRequest", greet.InputType)
	assert.Equal(t, "greet.v1.GreetResponse", greet.OutputType)
	assert.False(t, greet.ClientStreaming)
	assert.False(t, greet.ServerStreaming)
	assert.Contains(t, strings.ToLower(greet.Comment), "greet a person")

	group := svc.Methods["GreetGroup"]
	assert.Equal(t, "greet.v1.GreetGroupRequest", group.InputType)
	assert.Equal(t, "greet.v1.GreetGroupResponse", group.OutputType)

	stream := svc.Methods["StreamGreet"]
	assert.Equal(t, "greet.v1.StreamGreetRequest", stream.InputType)
	assert.Equal(t, "greet.v1.GreetResponse", stream.OutputType)
	assert.True(t, stream.ServerStreaming)
	assert.False(t, stream.ClientStreaming)
}

func TestParseMessages(t *testing.T) {
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

	msg := pd.Messages["greet.v1.GreetRequest"]
	names := fieldNames(msg)
	assert.Contains(t, names, "name")
	assert.Contains(t, names, "mood")
	assert.Contains(t, names, "tags")

	nameField := findField(t, msg, "name")
	assert.Contains(t, strings.ToLower(nameField.Comment), "name of the person")
	assert.False(t, nameField.Optional)

	moodField := findField(t, msg, "mood")
	assert.True(t, moodField.Optional)
	assert.Equal(t, int32(typeEnum), moodField.Type)

	// Map entry messages
	mapEntries := 0
	for _, m := range pd.Messages {
		if m.IsMapEntry {
			mapEntries++
		}
	}
	assert.GreaterOrEqual(t, mapEntries, 1)

	// Repeated and nested message references
	people := findField(t, pd.Messages["greet.v1.GreetGroupRequest"], "people")
	assert.Equal(t, int32(labelRepeated), people.Label)
	assert.Equal(t, "greet.v1.Person", people.TypeName)
}

func TestParseEnums(t *testing.T) {
	pd := mustParse(t)
	e := pd.Enums["greet.v1.Mood"]
	require.NotNil(t, e)

	assert.Contains(t, strings.ToLower(e.Comment), "mood")

	var names []string
	for _, v := range e.Values {
		names = append(names, v.Name)
	}
	assert.Equal(t, []string{"MOOD_UNSPECIFIED", "MOOD_HAPPY", "MOOD_SAD"}, names)

	for _, v := range e.Values {
		if v.Name == "MOOD_HAPPY" {
			assert.Contains(t, strings.ToLower(v.Comment), "happy")
			return
		}
	}
	t.Fatal("MOOD_HAPPY value not found")
}

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
