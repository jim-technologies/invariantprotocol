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
			wantError: "unsupported SchemaBundle version pair",
		},
		{
			name:      "mapping",
			bundle:    &datav1.SchemaBundle{IrVersion: IRVersion, MappingVersion: MappingVersion + 1},
			wantError: "unsupported SchemaBundle version pair",
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

func TestSchemaBundleMigratesLegacyArtifactWithoutChangingIdentityState(t *testing.T) {
	legacy := &datav1.SchemaBundle{
		IrVersion:      3,
		MappingVersion: 2,
		Datasets: []*datav1.DatasetSchema{{
			SourceMessage: "example.v1.Record",
			Name:          "example_v1_record",
			LastFieldId:   3,
			Fields: []*datav1.Field{{
				Name:     "values",
				StableId: 1,
				Type: &datav1.DataType{Kind: &datav1.DataType_List{List: &datav1.ListType{
					Element: &datav1.Field{
						Name:     "element",
						StableId: 2,
						Type: &datav1.DataType{Kind: &datav1.DataType_Primitive{
							Primitive: &datav1.PrimitiveType{Kind: datav1.PrimitiveKind_PRIMITIVE_KIND_FLOAT},
						}},
					},
				}}},
			}},
			RetiredFields: []*datav1.RetiredField{{
				Identity: "field:9", StableId: 3, ProtoFullName: "example.v1.Record.old", Name: "old",
			}},
		}},
	}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(legacy)
	require.NoError(t, err)

	migrated, err := ParseSchemaBundle(encoded)
	require.NoError(t, err)
	assert.Equal(t, IRVersion, migrated.GetIrVersion())
	assert.Equal(t, MappingVersion, migrated.GetMappingVersion())
	require.Len(t, migrated.GetDatasets(), 1)
	assert.True(t, proto.Equal(legacy.GetDatasets()[0], migrated.GetDatasets()[0]))
	assert.Zero(t, migrated.GetDatasets()[0].GetFields()[0].GetType().GetList().GetFixedLength())

	again, err := MigrateSchemaBundle(migrated)
	require.NoError(t, err)
	assert.True(t, proto.Equal(migrated, again))
	_, err = MarshalSchemaBundle(migrated)
	require.NoError(t, err)
	require.ErrorContains(t, ValidateSchemaBundle(legacy), "unsupported SchemaBundle ir_version")
}

func TestSchemaBundleLegacyMigrationFailsClosed(t *testing.T) {
	fixed := &datav1.SchemaBundle{
		IrVersion: 3, MappingVersion: 2,
		Datasets: []*datav1.DatasetSchema{{
			Name: "example_v1_record",
			Fields: []*datav1.Field{{
				Name: "vector",
				Type: &datav1.DataType{Kind: &datav1.DataType_List{List: &datav1.ListType{
					FixedLength: 8,
				}}},
			}},
		}},
	}
	encoded, err := proto.Marshal(fixed)
	require.NoError(t, err)
	_, err = ParseSchemaBundle(encoded)
	require.ErrorContains(t, err, "mapping_version 2 field")
	require.ErrorContains(t, err, "fixed_length 8")

	unknown := proto.Clone(fixed).(*datav1.SchemaBundle)
	unknown.Datasets[0].Fields[0].Type.GetList().FixedLength = 0
	unknown.Datasets[0].ProtoReflect().SetUnknown([]byte{0x98, 0x06, 0x01})
	encoded, err = proto.Marshal(unknown)
	require.NoError(t, err)
	_, err = ParseSchemaBundle(encoded)
	require.ErrorContains(t, err, "fields unknown to this migrator")
}
