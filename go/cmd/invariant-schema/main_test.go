package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow/ipc"
	"github.com/jim-technologies/invariantprotocol/go/data"
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

	writeDescriptor(t, descriptorPath,
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
	bundle := oneFieldBundle("example.v1.First")
	bundle.Datasets = append(bundle.Datasets, oneFieldBundle("example.v1.Second").Datasets[0])
	bundlePath := writeBundle(t, bundle)

	var stdout, stderr bytes.Buffer
	err := run([]string{"parquet", "--bundle", bundlePath}, &stdout, &stderr)
	require.EqualError(t, err, "parquet: --message is required because bundle contains 2 datasets")
	assert.Empty(t, stdout.String())

	for _, test := range []struct {
		target   string
		contains string
	}{
		{target: "parquet", contains: "group field_id=-1 example_v1_second"},
		{target: "iceberg", contains: `"type":"struct"`},
		{target: "sql", contains: `CREATE TABLE "example_v1_second"`},
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
			assert.True(t, strings.HasSuffix(stdout.String(), "\n"))
			assert.Contains(t, stderr.String(), test.target+": MAPPING_COMPATIBILITY_LOSSLESS: id:")
			assert.NotContains(t, stdout.String(), "MAPPING_COMPATIBILITY")
		})
	}
}

func TestRenderOutputFileDoesNotContainDiagnostics(t *testing.T) {
	bundlePath := writeBundle(t, oneFieldBundle("example.v1.Record"))
	outputPath := filepath.Join(t.TempDir(), "schema.sql")
	var stdout, stderr bytes.Buffer
	require.NoError(t, run([]string{
		"sql",
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
			wantError: "unsupported SchemaBundle ir_version 0; expected 2",
		},
		{
			name: "mapping",
			mutate: func(bundle *datav1.SchemaBundle) {
				bundle.MappingVersion = 1
			},
			wantError: "unsupported SchemaBundle mapping_version 1; expected 2",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			bundle := oneFieldBundle("example.v1.Record")
			test.mutate(bundle)
			bundlePath := writeBundle(t, bundle)
			var stdout, stderr bytes.Buffer
			err := run([]string{"sql", "--bundle", bundlePath}, &stdout, &stderr)
			require.ErrorContains(t, err, test.wantError)
			assert.Empty(t, stdout.String())
			assert.Empty(t, stderr.String())
		})
	}
}

func writeDescriptor(t *testing.T, path string, fields ...*descriptorpb.FieldDescriptorProto) {
	t.Helper()
	set := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{{
		Name:    new("example/v1/record.proto"),
		Package: new("example.v1"),
		Syntax:  new("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name:  new("Record"),
			Field: fields,
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
