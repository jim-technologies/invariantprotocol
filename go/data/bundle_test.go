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

func TestSchemaBundleValidatedRoundTrip(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	require.True(t, ok)
	encoded, err := os.ReadFile(filepath.Join(filepath.Dir(filename), "..", "..", "testdata", "data.schema.binpb"))
	require.NoError(t, err)

	bundle, err := ParseSchemaBundle(encoded)
	require.NoError(t, err)
	assert.Equal(t, "data.v1.CanonicalRecord", bundle.GetDatasets()[0].GetSourceMessage())
	assert.NotNil(t, FindDataset(bundle, "data.v1.Proto2Record"))
	assert.Nil(t, FindDataset(bundle, "data.v1.Missing"))

	roundTrip, err := MarshalSchemaBundle(bundle)
	require.NoError(t, err)
	assert.Equal(t, encoded, roundTrip)
}

func TestSchemaBundleRejectsUnsupportedVersions(t *testing.T) {
	for _, test := range []struct {
		name      string
		bundle    *datav1.SchemaBundle
		wantError string
	}{
		{
			name:      "IR",
			bundle:    &datav1.SchemaBundle{IrVersion: IRVersion + 1, MappingVersion: MappingVersion},
			wantError: "unsupported SchemaBundle ir_version",
		},
		{
			name:      "mapping",
			bundle:    &datav1.SchemaBundle{IrVersion: IRVersion, MappingVersion: MappingVersion + 1},
			wantError: "unsupported SchemaBundle mapping_version",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := proto.Marshal(test.bundle)
			require.NoError(t, err)
			_, err = ParseSchemaBundle(encoded)
			require.ErrorContains(t, err, test.wantError)
		})
	}

	require.ErrorContains(t, ValidateSchemaBundle(nil), "nil")
}
