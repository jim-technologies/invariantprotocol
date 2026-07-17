package data

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	datav1 "github.com/jim-technologies/invariantprotocol/go/gen/invariant/data/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestSharedCanonicalSchemaBundle(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	repositoryRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")
	descriptor, err := os.ReadFile(filepath.Join(repositoryRoot, "python", "tests", "proto", "descriptor.binpb"))
	require.NoError(t, err)
	encoded, err := os.ReadFile(filepath.Join(repositoryRoot, "testdata", "data.schema.binpb"))
	require.NoError(t, err)

	previous := new(datav1.SchemaBundle)
	require.NoError(t, proto.Unmarshal(encoded, previous))
	compiled, err := CompileDescriptorBytes(descriptor, []string{
		"data.v1.CanonicalRecord",
		"data.v1.Proto2Record",
	}, previous)
	require.NoError(t, err)
	regenerated, err := proto.MarshalOptions{Deterministic: true}.Marshal(compiled)
	require.NoError(t, err)
	assert.Equal(t, encoded, regenerated, "the committed cross-language artifact must be deterministic")

	require.Equal(t, IRVersion, previous.GetIrVersion())
	require.Equal(t, MappingVersion, previous.GetMappingVersion())
	require.Len(t, previous.GetDatasets(), 2)
	canonical := previous.GetDatasets()[0]
	proto2 := previous.GetDatasets()[1]
	assert.Equal(t, "data.v1.CanonicalRecord", canonical.GetSourceMessage())
	assert.Equal(t, "data.v1.Proto2Record", proto2.GetSourceMessage())

	optionalNote := schemaField(t, canonical, 17)
	assert.Equal(t, int32(17), optionalNote.GetStableId())
	assert.Equal(t, datav1.Presence_PRESENCE_EXPLICIT, optionalNote.GetPresence())
	assert.True(t, optionalNote.GetNullable())

	labels := schemaField(t, canonical, 19)
	assert.Equal(t, datav1.Presence_PRESENCE_REPEATED, labels.GetPresence())
	element := labels.GetType().GetList().GetElement()
	require.NotNil(t, element)
	assert.Equal(t, int32(31), element.GetStableId())
	assert.Equal(t, datav1.Presence_PRESENCE_NOT_APPLICABLE, element.GetPresence())
	assert.Equal(t, datav1.SyntheticRole_SYNTHETIC_ROLE_LIST_ELEMENT, element.GetSyntheticRole())

	choiceCount := schemaField(t, canonical, 22)
	assert.Equal(t, datav1.Presence_PRESENCE_ONEOF, choiceCount.GetPresence())
	assert.Equal(t, "choice", choiceCount.GetOneof())

	label := schemaField(t, proto2, 2)
	assert.Equal(t, datav1.Presence_PRESENCE_EXPLICIT, label.GetPresence())
	assert.True(t, label.GetHasDefault())
	assert.Equal(t, "unknown", label.GetProtobufDefault())
}
