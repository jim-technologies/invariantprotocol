package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow/ipc"
	"github.com/jim-technologies/invariantprotocol/go/data"
	"github.com/jim-technologies/invariantprotocol/go/data/clickhouse"
	datav1 "github.com/jim-technologies/invariantprotocol/go/gen/invariant/data/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

func TestCompileRetainsPreviousBundleAutomatically(t *testing.T) {
	directory := t.TempDir()
	descriptorPath := filepath.Join(directory, "descriptor.binpb")
	bundlePath := filepath.Join(directory, "schema.binpb")

	writeDescriptor(t, descriptorPath,
		field("id", 1),
		field("removed", 2),
	)
	var stdout, stderr bytes.Buffer
	require.NoError(t, run([]string{
		"compile",
		"--descriptor", descriptorPath,
		"--message", "example.v1.Record",
		"--output", bundlePath,
	}, &stdout, &stderr))
	assert.Empty(t, stdout.String())
	assert.Empty(t, stderr.String())

	firstBytes, err := os.ReadFile(bundlePath)
	require.NoError(t, err)
	first := decodeBundle(t, firstBytes)
	require.Len(t, first.GetDatasets(), 1)
	require.Len(t, first.GetDatasets()[0].GetFields(), 2)
	firstID := first.GetDatasets()[0].GetFields()[0].GetStableId()
	removedID := first.GetDatasets()[0].GetFields()[1].GetStableId()

	writeDescriptorWithReservations(t, descriptorPath, []string{"id", "removed"}, []int32{2},
		field("identifier", 1),
		field("created", 3),
	)
	require.NoError(t, run([]string{
		"compile",
		"--descriptor", descriptorPath,
		"--message", "example.v1.Record",
		"--output", bundlePath,
	}, &stdout, &stderr))

	secondBytes, err := os.ReadFile(bundlePath)
	require.NoError(t, err)
	second := decodeBundle(t, secondBytes)
	dataset := second.GetDatasets()[0]
	require.Len(t, dataset.GetFields(), 2)
	assert.Equal(t, "id", dataset.GetFields()[0].GetName(), "a source rename must not rename an existing storage column")
	assert.Equal(t, "example.v1.Record.identifier", dataset.GetFields()[0].GetProtoFullName())
	assert.Equal(t, firstID, dataset.GetFields()[0].GetStableId(), "a source rename must retain storage identity")
	assert.Greater(t, dataset.GetFields()[1].GetStableId(), removedID)
	require.Len(t, dataset.GetRetiredFields(), 1)
	assert.Equal(t, removedID, dataset.GetRetiredFields()[0].GetStableId())

	require.NoError(t, run([]string{
		"compile",
		"--descriptor", descriptorPath,
		"--message", "example.v1.Record",
		"--output", bundlePath,
	}, &stdout, &stderr))
	thirdBytes, err := os.ReadFile(bundlePath)
	require.NoError(t, err)
	assert.Equal(t, secondBytes, thirdBytes, "deterministic compilation must not churn a committed bundle")
}

func TestCompileDiscoversAnnotatedDatasetsWithoutMessageFlags(t *testing.T) {
	directory := t.TempDir()
	descriptorPath := filepath.Join(directory, "descriptor.binpb")
	bundlePath := filepath.Join(directory, "schema.binpb")
	writeAnnotatedDescriptor(t, descriptorPath)

	var stdout, stderr bytes.Buffer
	require.NoError(t, run([]string{
		"compile",
		"--descriptor", descriptorPath,
		"--output", bundlePath,
	}, &stdout, &stderr))
	assert.Empty(t, stdout.String())
	assert.Empty(t, stderr.String())

	encoded, err := os.ReadFile(bundlePath)
	require.NoError(t, err)
	bundle := decodeBundle(t, encoded)
	require.Len(t, bundle.GetDatasets(), 1)
	assert.Equal(t, "example.v1.Record", bundle.GetDatasets()[0].GetSourceMessage())
}

func TestMigrateUpgradesLegacyBundleAndSupportsInPlaceWrites(t *testing.T) {
	legacy := oneFieldBundle("example.v1.Record")
	legacy.IrVersion = 3
	legacy.MappingVersion = 2
	legacy.Datasets[0].LastFieldId = 2
	legacy.Datasets[0].RetiredFields = []*datav1.RetiredField{{
		Identity:      "field:9",
		StableId:      2,
		ProtoFullName: "example.v1.Record.removed",
		Name:          "removed",
	}}

	inputPath := writeBundle(t, legacy)
	outputPath := filepath.Join(t.TempDir(), "migrated.binpb")
	var stdout, stderr bytes.Buffer
	require.NoError(t, run([]string{
		"migrate",
		"--bundle", inputPath,
		"--output", outputPath,
	}, &stdout, &stderr))
	assert.Empty(t, stdout.String())
	assert.Empty(t, stderr.String())

	encoded, err := os.ReadFile(outputPath)
	require.NoError(t, err)
	migrated := new(datav1.SchemaBundle)
	require.NoError(t, proto.Unmarshal(encoded, migrated))
	assert.Equal(t, data.IRVersion, migrated.GetIrVersion())
	assert.Equal(t, data.MappingVersion, migrated.GetMappingVersion())
	require.Len(t, migrated.GetDatasets(), 1)
	assert.True(t, proto.Equal(legacy.GetDatasets()[0], migrated.GetDatasets()[0]))

	require.NoError(t, run([]string{
		"migrate",
		"--bundle", outputPath,
		"--output", outputPath,
	}, &stdout, &stderr))
	inPlace, err := os.ReadFile(outputPath)
	require.NoError(t, err)
	assert.Equal(t, encoded, inPlace, "an already-current bundle must remain byte-for-byte stable")
}

func TestHelpDescribesMigrationAndLanceArrowBoundary(t *testing.T) {
	var stdout, stderr bytes.Buffer
	require.NoError(t, run([]string{"help"}, &stdout, &stderr))
	assert.Empty(t, stderr.String())
	assert.Contains(t, stdout.String(), "invariant-schema migrate --bundle FILE --output FILE")
	assert.Contains(t, stdout.String(), "Arrow emits schema-only IPC")
	assert.Contains(t, stdout.String(), "Lance/LanceDB consume that schema")
	assert.NotContains(t, stdout.String(), "invariant-schema lance")
}

func TestCompileRejectsEmptyStorageNamesBeforeWriting(t *testing.T) {
	directory := t.TempDir()
	descriptorPath := filepath.Join(directory, "descriptor.binpb")
	bundlePath := filepath.Join(directory, "schema.binpb")
	writeDescriptor(t, descriptorPath, field("_", 1))

	var stdout, stderr bytes.Buffer
	err := run([]string{
		"compile",
		"--descriptor", descriptorPath,
		"--message", "example.v1.Record",
		"--output", bundlePath,
	}, &stdout, &stderr)
	require.EqualError(t, err,
		`compile descriptor: dataset "example.v1.Record": compile message "example.v1.Record": protobuf field "example.v1.Record._" normalizes to an empty storage name within protobuf message "example.v1.Record"`,
	)
	assert.Empty(t, stdout.String())
	assert.Empty(t, stderr.String())
	_, statErr := os.Stat(bundlePath)
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestArrowWritesOnlyIPCToStdoutAndDiagnosticsToStderr(t *testing.T) {
	bundlePath := writeBundle(t, oneFieldBundle("example.v1.Record"))
	var stdout, stderr bytes.Buffer
	require.NoError(t, run([]string{"arrow", "--bundle", bundlePath}, &stdout, &stderr))

	reader, err := ipc.NewReader(bytes.NewReader(stdout.Bytes()))
	require.NoError(t, err, "stdout must contain a valid Arrow IPC stream with no diagnostic text")
	defer reader.Release()
	require.Len(t, reader.Schema().Fields(), 1)
	assert.Equal(t, "id", reader.Schema().Field(0).Name)
	assert.False(t, reader.Next(), "schema artifact must contain zero records")
	require.NoError(t, reader.Err())

	assert.Contains(t, stderr.String(), "arrow: MAPPING_COMPATIBILITY_LOSSLESS: id:")
	assert.Contains(t, stderr.String(), "maps losslessly")
	assert.NotContains(t, stdout.String(), "MAPPING_COMPATIBILITY")
}

func TestRenderersEmitOfficialArtifactsAndSelectMessage(t *testing.T) {
	bundle := oneFieldBundle("example.v1.Second")
	bundle.Datasets = append(bundle.Datasets, oneFieldBundle("example.v1.First").Datasets[0])
	bundlePath := writeBundle(t, bundle)

	for _, target := range []string{"arrow", "parquet", "iceberg", "clickhouse-iceberg"} {
		t.Run(target+" requires selection", func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := run([]string{target, "--bundle", bundlePath}, &stdout, &stderr)
			require.EqualError(t, err, target+": --message is required because bundle contains 2 datasets")
			assert.Empty(t, stdout.String())
		})
	}

	t.Run("postgres renders the complete bundle deterministically", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		require.NoError(t, run([]string{"postgres", "--bundle", bundlePath}, &stdout, &stderr))
		ddl := stdout.String()
		require.Equal(t, 2, strings.Count(ddl, "CREATE TABLE"))
		first := strings.Index(ddl, `CREATE TABLE "example_v1_first"`)
		second := strings.Index(ddl, `CREATE TABLE "example_v1_second"`)
		require.NotEqual(t, -1, first)
		require.NotEqual(t, -1, second)
		assert.Less(t, first, second, "bundle SQL must be independent of input dataset order")
		assert.Contains(t, ddl, ");\n\nCREATE TABLE", "dataset statements must have one readable separator")
		assert.Equal(t, 2, strings.Count(stderr.String(), "postgres: MAPPING_COMPATIBILITY_LOSSLESS: id:"))
		assert.NotContains(t, ddl, "MAPPING_COMPATIBILITY")
	})

	t.Run("clickhouse requires selection and emits only a table body", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		err := run([]string{"clickhouse", "--bundle", bundlePath}, &stdout, &stderr)
		require.EqualError(t, err, "clickhouse: --message is required because bundle contains 2 datasets")
		assert.Empty(t, stdout.String())

		require.NoError(t, run([]string{
			"clickhouse",
			"--bundle", bundlePath,
			"--message", "example.v1.Second",
		}, &stdout, &stderr))
		assert.Contains(t, stdout.String(), "(\n  `id` Int64 DEFAULT 0")
		assert.NotContains(t, stdout.String(), "CREATE TABLE")
		assert.NotContains(t, stdout.String(), "ENGINE")
		assert.True(t, strings.HasSuffix(stdout.String(), "\n)\n"))
		assert.Contains(t, stderr.String(), "clickhouse: MAPPING_COMPATIBILITY_LOSSLESS: id:")
	})

	var stdout, stderr bytes.Buffer
	for _, test := range []struct {
		target   string
		contains string
	}{
		{target: "parquet", contains: "group field_id=-1 example_v1_second"},
		{target: "iceberg", contains: `"type":"struct"`},
		{target: "clickhouse-iceberg", contains: fmt.Sprintf(`"version":%d`, clickhouse.ProjectionVersion)},
		{target: "postgres", contains: `CREATE TABLE "example_v1_second"`},
	} {
		t.Run(test.target, func(t *testing.T) {
			stdout.Reset()
			stderr.Reset()
			require.NoError(t, run([]string{
				test.target,
				"--bundle", bundlePath,
				"--message", "example.v1.Second",
			}, &stdout, &stderr))
			assert.Contains(t, stdout.String(), test.contains)
			assert.NotContains(t, stdout.String(), "example_v1_first")
			assert.True(t, strings.HasSuffix(stdout.String(), "\n"))
			assert.Contains(t, stderr.String(), test.target+": MAPPING_COMPATIBILITY_LOSSLESS: id:")
			assert.NotContains(t, stdout.String(), "MAPPING_COMPATIBILITY")
		})
	}
}

func TestPostgresRejectsAnEmptyBundle(t *testing.T) {
	bundlePath := writeBundle(t, &datav1.SchemaBundle{
		IrVersion:      data.IRVersion,
		MappingVersion: data.MappingVersion,
	})
	var stdout, stderr bytes.Buffer
	err := run([]string{"postgres", "--bundle", bundlePath}, &stdout, &stderr)
	require.EqualError(t, err, "postgres: bundle contains no datasets")
	assert.Empty(t, stdout.String())
	assert.Empty(t, stderr.String())
}

func TestRenderOutputFileDoesNotContainDiagnostics(t *testing.T) {
	bundlePath := writeBundle(t, oneFieldBundle("example.v1.Record"))
	outputPath := filepath.Join(t.TempDir(), "schema.sql")
	var stdout, stderr bytes.Buffer
	require.NoError(t, run([]string{
		"postgres",
		"--bundle", bundlePath,
		"--output", outputPath,
	}, &stdout, &stderr))
	assert.Empty(t, stdout.String())
	artifact, err := os.ReadFile(outputPath)
	require.NoError(t, err)
	assert.Contains(t, string(artifact), "CREATE TABLE")
	assert.NotContains(t, string(artifact), "MAPPING_COMPATIBILITY")
	assert.Contains(t, stderr.String(), "MAPPING_COMPATIBILITY_LOSSLESS")
}

func TestRenderRejectsUnknownBundleVersions(t *testing.T) {
	for _, test := range []struct {
		name      string
		mutate    func(*datav1.SchemaBundle)
		wantError string
	}{
		{
			name: "IR",
			mutate: func(bundle *datav1.SchemaBundle) {
				bundle.IrVersion = 0
			},
			wantError: fmt.Sprintf(
				"unsupported SchemaBundle version pair ir_version=0 mapping_version=%d; expected 3/2 or %d/%d",
				data.MappingVersion, data.IRVersion, data.MappingVersion,
			),
		},
		{
			name: "mapping",
			mutate: func(bundle *datav1.SchemaBundle) {
				bundle.MappingVersion = 1
			},
			wantError: fmt.Sprintf(
				"unsupported SchemaBundle version pair ir_version=%d mapping_version=1; expected 3/2 or %d/%d",
				data.IRVersion, data.IRVersion, data.MappingVersion,
			),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			bundle := oneFieldBundle("example.v1.Record")
			test.mutate(bundle)
			bundlePath := writeBundle(t, bundle)
			var stdout, stderr bytes.Buffer
			err := run([]string{"postgres", "--bundle", bundlePath}, &stdout, &stderr)
			require.ErrorContains(t, err, test.wantError)
			assert.Empty(t, stdout.String())
			assert.Empty(t, stderr.String())
		})
	}
}

func writeDescriptor(t *testing.T, path string, fields ...*descriptorpb.FieldDescriptorProto) {
	t.Helper()
	writeDescriptorWithReservedNames(t, path, nil, fields...)
}

func writeDescriptorWithReservedNames(
	t *testing.T,
	path string,
	reservedNames []string,
	fields ...*descriptorpb.FieldDescriptorProto,
) {
	t.Helper()
	writeDescriptorWithReservations(t, path, reservedNames, nil, fields...)
}

func writeDescriptorWithReservations(
	t *testing.T,
	path string,
	reservedNames []string,
	reservedNumbers []int32,
	fields ...*descriptorpb.FieldDescriptorProto,
) {
	t.Helper()
	reservedRanges := make([]*descriptorpb.DescriptorProto_ReservedRange, 0, len(reservedNumbers))
	for _, number := range reservedNumbers {
		reservedRanges = append(reservedRanges, &descriptorpb.DescriptorProto_ReservedRange{
			Start: new(number),
			End:   new(number + 1),
		})
	}
	set := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{{
		Name:    new("example/v1/record.proto"),
		Package: new("example.v1"),
		Syntax:  new("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name:          new("Record"),
			ReservedName:  reservedNames,
			ReservedRange: reservedRanges,
			Field:         fields,
		}},
	}}}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(set)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, encoded, 0o644))
}

func writeAnnotatedDescriptor(t *testing.T, path string) {
	t.Helper()
	options := &descriptorpb.MessageOptions{}
	proto.SetExtension(options, datav1.E_Dataset, &datav1.DatasetOptions{})
	set := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{{
		Name:    new("example/v1/record.proto"),
		Package: new("example.v1"),
		Syntax:  new("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name:    new("Record"),
				Options: options,
				Field:   []*descriptorpb.FieldDescriptorProto{field("id", 1)},
			},
			{
				Name:  new("Request"),
				Field: []*descriptorpb.FieldDescriptorProto{field("query", 1)},
			},
		},
	}}}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(set)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, encoded, 0o644))
}

func field(name string, number int32) *descriptorpb.FieldDescriptorProto {
	return &descriptorpb.FieldDescriptorProto{
		Name:   new(name),
		Number: new(number),
		Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
	}
}

func oneFieldBundle(sourceMessage string) *datav1.SchemaBundle {
	return &datav1.SchemaBundle{
		IrVersion:      data.IRVersion,
		MappingVersion: data.MappingVersion,
		Datasets: []*datav1.DatasetSchema{{
			SourceMessage: sourceMessage,
			Name:          strings.ToLower(strings.ReplaceAll(sourceMessage, ".", "_")),
			LastFieldId:   1,
			Fields: []*datav1.Field{{
				ProtoFullName:   sourceMessage + ".id",
				ProtoNumberPath: []uint32{1},
				Name:            "id",
				StableId:        1,
				Presence:        datav1.Presence_PRESENCE_IMPLICIT,
				SyntheticRole:   datav1.SyntheticRole_SYNTHETIC_ROLE_PROTO_FIELD,
				Type: &datav1.DataType{
					ProtobufType: "int64",
					Kind: &datav1.DataType_Primitive{Primitive: &datav1.PrimitiveType{
						Kind: datav1.PrimitiveKind_PRIMITIVE_KIND_INT64,
					}},
				},
			}},
		}},
	}
}

func writeBundle(t *testing.T, bundle *datav1.SchemaBundle) string {
	t.Helper()
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(bundle)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "schema.binpb")
	require.NoError(t, os.WriteFile(path, encoded, 0o644))
	return path
}

func decodeBundle(t *testing.T, encoded []byte) *datav1.SchemaBundle {
	t.Helper()
	bundle, err := data.ParseSchemaBundle(encoded)
	require.NoError(t, err)
	return bundle
}
