package data

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	datav1 "github.com/jim-technologies/invariantprotocol/go/gen/invariant/data/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

func TestCompileMessagePreservesProtobufSemantics(t *testing.T) {
	_, files := fixtureDescriptors(t)
	canonical := findMessage(t, files, "data.v1.CanonicalRecord")

	schema, err := CompileMessage(canonical, nil)
	require.NoError(t, err)
	require.Equal(t, "data.v1.CanonicalRecord", schema.GetSourceMessage())
	assert.Equal(t, "data_v1_canonical_record", schema.GetName())
	assert.Contains(t, schema.GetDescription(), "every protobuf shape")
	require.Len(t, schema.GetFields(), 28)
	assert.Equal(t, int32(1), schema.GetFields()[0].GetStableId())
	assert.Equal(t, int32(28), schema.GetFields()[27].GetStableId())

	primitiveKinds := []datav1.PrimitiveKind{
		datav1.PrimitiveKind_PRIMITIVE_KIND_DOUBLE,
		datav1.PrimitiveKind_PRIMITIVE_KIND_FLOAT,
		datav1.PrimitiveKind_PRIMITIVE_KIND_INT64,
		datav1.PrimitiveKind_PRIMITIVE_KIND_UINT64,
		datav1.PrimitiveKind_PRIMITIVE_KIND_INT32,
		datav1.PrimitiveKind_PRIMITIVE_KIND_FIXED64,
		datav1.PrimitiveKind_PRIMITIVE_KIND_FIXED32,
		datav1.PrimitiveKind_PRIMITIVE_KIND_BOOL,
		datav1.PrimitiveKind_PRIMITIVE_KIND_STRING,
		datav1.PrimitiveKind_PRIMITIVE_KIND_BYTES,
		datav1.PrimitiveKind_PRIMITIVE_KIND_UINT32,
		datav1.PrimitiveKind_PRIMITIVE_KIND_SFIXED32,
		datav1.PrimitiveKind_PRIMITIVE_KIND_SFIXED64,
		datav1.PrimitiveKind_PRIMITIVE_KIND_SINT32,
		datav1.PrimitiveKind_PRIMITIVE_KIND_SINT64,
	}
	for number, want := range primitiveKinds {
		field := schemaField(t, schema, protoreflect.FieldNumber(number+1))
		require.NotNil(t, field.GetType().GetPrimitive(), "field %d", number+1)
		assert.Equal(t, want, field.GetType().GetPrimitive().GetKind(), "field %d", number+1)
		assert.Equal(t, datav1.Presence_PRESENCE_IMPLICIT, field.GetPresence())
		assert.False(t, field.GetNullable())
		assert.Equal(t, datav1.SyntheticRole_SYNTHETIC_ROLE_PROTO_FIELD, field.GetSyntheticRole())
	}
	assert.Equal(t, "doubleValue", schemaField(t, schema, 1).GetJsonName())
	assert.Contains(t, schemaField(t, schema, 1).GetDescription(), "double-precision")

	enum := schemaField(t, schema, 16).GetType().GetEnum()
	require.NotNil(t, enum)
	assert.Equal(t, "data.v1.DataState", enum.GetFullName())
	assert.False(t, enum.GetClosed())
	require.Len(t, enum.GetValues(), 3)
	assert.Equal(t, int32(1), enum.GetValues()[1].GetNumber())
	assert.Equal(t, int32(1), enum.GetValues()[2].GetNumber())
	assert.Equal(t, "DATA_STATE_ACTIVE", enum.GetValues()[2].GetName())
	assert.Contains(t, enum.GetValues()[2].GetDescription(), "alias")

	optional := schemaField(t, schema, 17)
	assert.Equal(t, datav1.Presence_PRESENCE_EXPLICIT, optional.GetPresence())
	assert.True(t, optional.GetNullable())
	assert.Empty(t, optional.GetOneof())

	nested := schemaField(t, schema, 18)
	assert.Equal(t, datav1.Presence_PRESENCE_EXPLICIT, nested.GetPresence())
	assert.True(t, nested.GetNullable())
	assert.Equal(t, "data.v1.NestedRecord", nested.GetType().GetProtobufType())
	require.Len(t, nested.GetType().GetStruct().GetFields(), 2)
	assert.Equal(t, []uint32{18, 1}, nested.GetType().GetStruct().GetFields()[0].GetProtoNumberPath())

	list := schemaField(t, schema, 19)
	assert.Equal(t, datav1.Presence_PRESENCE_REPEATED, list.GetPresence())
	assert.False(t, list.GetNullable())
	element := list.GetType().GetList().GetElement()
	require.NotNil(t, element)
	assert.Equal(t, datav1.Presence_PRESENCE_NOT_APPLICABLE, element.GetPresence())
	assert.Equal(t, datav1.SyntheticRole_SYNTHETIC_ROLE_LIST_ELEMENT, element.GetSyntheticRole())
	assert.False(t, element.GetNullable())
	assert.Empty(t, element.GetJsonName())
	assert.Equal(t, []uint32{19}, element.GetProtoNumberPath())

	messageList := schemaField(t, schema, 20).GetType().GetList().GetElement()
	assert.Equal(t, "data.v1.NestedRecord", messageList.GetType().GetProtobufType())
	require.Len(t, messageList.GetType().GetStruct().GetFields(), 2)

	protobufMap := schemaField(t, schema, 21)
	assert.Equal(t, datav1.Presence_PRESENCE_MAP, protobufMap.GetPresence())
	mapType := protobufMap.GetType().GetMap()
	require.NotNil(t, mapType)
	assert.Equal(t, datav1.SyntheticRole_SYNTHETIC_ROLE_MAP_KEY, mapType.GetKey().GetSyntheticRole())
	assert.Equal(t, datav1.PrimitiveKind_PRIMITIVE_KIND_STRING, mapType.GetKey().GetType().GetPrimitive().GetKind())
	assert.Equal(t, datav1.SyntheticRole_SYNTHETIC_ROLE_MAP_VALUE, mapType.GetValue().GetSyntheticRole())
	assert.Equal(t, datav1.PrimitiveKind_PRIMITIVE_KIND_UINT64, mapType.GetValue().GetType().GetPrimitive().GetKind())

	for _, number := range []protoreflect.FieldNumber{22, 23} {
		choice := schemaField(t, schema, number)
		assert.Equal(t, datav1.Presence_PRESENCE_ONEOF, choice.GetPresence())
		assert.True(t, choice.GetNullable())
		assert.Equal(t, "choice", choice.GetOneof())
	}

	timestamp := schemaField(t, schema, 24).GetType()
	assert.Equal(t, "google.protobuf.Timestamp", timestamp.GetProtobufType())
	assert.Equal(t, datav1.TimeUnit_TIME_UNIT_NANOSECOND, timestamp.GetTimestamp().GetUnit())
	assert.Equal(t, "UTC", timestamp.GetTimestamp().GetTimezone())
	duration := schemaField(t, schema, 25).GetType()
	assert.Equal(t, "google.protobuf.Duration", duration.GetProtobufType())
	assert.Equal(t, datav1.TimeUnit_TIME_UNIT_NANOSECOND, duration.GetDuration().GetUnit())
	wrapper := schemaField(t, schema, 26)
	assert.True(t, wrapper.GetNullable())
	assert.Equal(t, "google.protobuf.Int32Value", wrapper.GetType().GetProtobufType())
	assert.Equal(t, datav1.PrimitiveKind_PRIMITIVE_KIND_INT32, wrapper.GetType().GetPrimitive().GetKind())
	assert.Equal(t, datav1.JsonKind_JSON_KIND_STRUCT, schemaField(t, schema, 27).GetType().GetJson().GetKind())
	assert.Equal(t, datav1.JsonKind_JSON_KIND_ANY, schemaField(t, schema, 28).GetType().GetJson().GetKind())

	ids := collectStableIDs(schema.GetFields())
	assert.Len(t, ids, countFields(schema.GetFields()))
	for id := range ids {
		assert.GreaterOrEqual(t, id, int32(1))
		assert.LessOrEqual(t, id, maxStableID)
	}
	assert.Greater(t, schema.GetLastFieldId(), int32(28))
	assert.NotEqual(t, element.GetStableId(), mapType.GetKey().GetStableId())
	assert.NotEqual(t, mapType.GetKey().GetStableId(), mapType.GetValue().GetStableId())

	again, err := CompileMessage(canonical, nil)
	require.NoError(t, err)
	assert.True(t, proto.Equal(schema, again))
}

func TestCompileMessageProto2PresenceDefaultsAndClosedEnum(t *testing.T) {
	_, files := fixtureDescriptors(t)
	md := findMessage(t, files, "data.v1.Proto2Record")
	schema, err := CompileMessage(md, nil)
	require.NoError(t, err)

	requiredField := schemaField(t, schema, 1)
	assert.Equal(t, datav1.Presence_PRESENCE_REQUIRED, requiredField.GetPresence())
	assert.False(t, requiredField.GetNullable())
	assert.False(t, requiredField.GetHasDefault())
	assert.Equal(t, "id", requiredField.GetJsonName())

	optionalField := schemaField(t, schema, 2)
	assert.Equal(t, datav1.Presence_PRESENCE_EXPLICIT, optionalField.GetPresence())
	assert.True(t, optionalField.GetNullable())
	assert.True(t, optionalField.GetHasDefault())
	assert.Equal(t, "unknown", optionalField.GetProtobufDefault())
	assert.Equal(t, "label", optionalField.GetJsonName())

	closedEnum := proto2EnumMessage(t)
	closedSchema, err := CompileMessage(closedEnum, nil)
	require.NoError(t, err)
	assert.True(t, schemaField(t, closedSchema, 1).GetType().GetEnum().GetClosed())
}

func TestCompileDescriptorBytesUsesExplicitDeterministicRoots(t *testing.T) {
	descriptor, _ := fixtureDescriptors(t)
	bundle, err := CompileDescriptorBytes(descriptor, []string{
		"data.v1.Proto2Record",
		".data.v1.CanonicalRecord",
		"data.v1.CanonicalRecord",
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, IRVersion, bundle.GetIrVersion())
	assert.Equal(t, MappingVersion, bundle.GetMappingVersion())
	digest := sha256.Sum256(descriptor)
	assert.Equal(t, digest[:], bundle.GetSourceDescriptorSha256())
	require.Len(t, bundle.GetDatasets(), 2)
	assert.Equal(t, "data.v1.CanonicalRecord", bundle.GetDatasets()[0].GetSourceMessage())
	assert.Equal(t, "data.v1.Proto2Record", bundle.GetDatasets()[1].GetSourceMessage())

	again, err := CompileDescriptorBytes(descriptor, []string{
		"data.v1.CanonicalRecord",
		"data.v1.Proto2Record",
	}, bundle)
	require.NoError(t, err)
	assert.True(t, proto.Equal(bundle.GetDatasets()[0], again.GetDatasets()[0]))
	assert.True(t, proto.Equal(bundle.GetDatasets()[1], again.GetDatasets()[1]))

	_, err = CompileDescriptorBytes(descriptor, nil, nil)
	require.ErrorContains(t, err, "at least one dataset message")
	_, err = CompileDescriptorBytes(descriptor, []string{""}, nil)
	require.ErrorContains(t, err, "must not be empty")
	_, err = CompileDescriptorBytes(descriptor, []string{"data.v1.DataState"}, nil)
	require.ErrorContains(t, err, "is not a protobuf message")
	wellKnownRoot, err := CompileDescriptorBytes(descriptor, []string{"google.protobuf.Timestamp"}, nil)
	require.NoError(t, err)
	require.Len(t, wellKnownRoot.GetDatasets(), 1)
	assert.Equal(t, "google.protobuf.Timestamp", wellKnownRoot.GetDatasets()[0].GetSourceMessage())
}

func TestCompileDescriptorBytesDiscoversAnnotatedDatasets(t *testing.T) {
	file := rowFile("annotated.proto", "annotated.v1")
	file.MessageType[0].Options = &descriptorpb.MessageOptions{}
	proto.SetExtension(file.MessageType[0].Options, datav1.E_Dataset, &datav1.DatasetOptions{})
	file.MessageType = append(file.MessageType, &descriptorpb.DescriptorProto{
		Name: new("NotADataset"),
		Field: []*descriptorpb.FieldDescriptorProto{{
			Name:   new("value"),
			Number: proto.Int32(1),
			Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
		}},
	})
	descriptor := descriptorSetBytes(t, file)

	bundle, err := CompileDescriptorBytes(descriptor, nil, nil)
	require.NoError(t, err)
	require.Len(t, bundle.GetDatasets(), 1)
	assert.Equal(t, "annotated.v1.Row", bundle.GetDatasets()[0].GetSourceMessage())

	// Explicit roots remain the authoritative selection when supplied, even
	// when another message carries the dataset annotation.
	explicit, err := CompileDescriptorBytes(descriptor, []string{"annotated.v1.NotADataset"}, nil)
	require.NoError(t, err)
	require.Len(t, explicit.GetDatasets(), 1)
	assert.Equal(t, "annotated.v1.NotADataset", explicit.GetDatasets()[0].GetSourceMessage())
}

func TestCompileDescriptorBytesDiscoversAnnotatedImportedDatasets(t *testing.T) {
	dependency := rowFile("records/dependency.proto", "records.dependency.v1")
	dependency.MessageType[0].Options = &descriptorpb.MessageOptions{}
	proto.SetExtension(dependency.MessageType[0].Options, datav1.E_Dataset, &datav1.DatasetOptions{})

	application := rowFile("records/application.proto", "records.application.v1")
	application.Dependency = []string{dependency.GetName()}
	application.MessageType[0].Options = &descriptorpb.MessageOptions{}
	proto.SetExtension(application.MessageType[0].Options, datav1.E_Dataset, &datav1.DatasetOptions{})

	descriptor := descriptorSetBytes(t, application, dependency)
	discovered, err := CompileDescriptorBytes(descriptor, nil, nil)
	require.NoError(t, err)
	require.Len(t, discovered.GetDatasets(), 2)
	assert.Equal(t, "records.application.v1.Row", discovered.GetDatasets()[0].GetSourceMessage())
	assert.Equal(t, "records.dependency.v1.Row", discovered.GetDatasets()[1].GetSourceMessage())

	explicit, err := CompileDescriptorBytes(descriptor, []string{"records.application.v1.Row"}, nil)
	require.NoError(t, err)
	require.Len(t, explicit.GetDatasets(), 1)
	assert.Equal(t, "records.application.v1.Row", explicit.GetDatasets()[0].GetSourceMessage())
}

func TestCompileDescriptorBytesRejectsAnnotationNumberCollisions(t *testing.T) {
	for _, extendee := range []string{
		".google.protobuf.MessageOptions",
		".google.protobuf.FieldOptions",
	} {
		t.Run(strings.TrimPrefix(extendee, ".google.protobuf."), func(t *testing.T) {
			file := rowFile("row.proto", "collision.v1")
			collision := &descriptorpb.FileDescriptorProto{
				Name:    new("foreign/options.proto"),
				Package: new("foreign.options"),
				Syntax:  new("proto2"),
				Extension: []*descriptorpb.FieldDescriptorProto{{
					Name:     new("conflict"),
					Number:   proto.Int32(51974),
					Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					Type:     descriptorpb.FieldDescriptorProto_TYPE_BOOL.Enum(),
					Extendee: new(extendee),
				}},
			}
			descriptor := descriptorSetBytes(t, file, collision)

			_, err := CompileDescriptorBytes(descriptor, []string{"collision.v1.Row"}, nil)
			require.ErrorContains(t, err, `custom option number 51974`)
			require.ErrorContains(t, err, `assigned to "invariant.data.v1.`)
			require.ErrorContains(t, err, `also declared by "foreign.options.conflict"`)
		})
	}
}

func TestCompileDescriptorBytesRejectsInvalidOrNewerAnnotationDeclarations(t *testing.T) {
	t.Run("declaration fingerprint", func(t *testing.T) {
		file := rowFile("row.proto", "collision.v1")
		invalid := &descriptorpb.FileDescriptorProto{
			Name:    new("invariant/data/v1/invalid_annotations.proto"),
			Package: new("invariant.data.v1"),
			Syntax:  new("proto2"),
			Extension: []*descriptorpb.FieldDescriptorProto{{
				Name:     new("field"),
				Number:   proto.Int32(51974),
				Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:     descriptorpb.FieldDescriptorProto_TYPE_BOOL.Enum(),
				Extendee: new(".google.protobuf.FieldOptions"),
			}},
		}

		_, err := CompileDescriptorBytes(
			descriptorSetBytes(t, file, invalid),
			[]string{"collision.v1.Row"},
			nil,
		)
		require.ErrorContains(t, err, `invariant custom option "invariant.data.v1.field" has an unexpected declaration`)
	})

	t.Run("newer dataset option", func(t *testing.T) {
		file := rowFile("annotated.proto", "annotated.v1")
		option := &datav1.DatasetOptions{}
		option.ProtoReflect().SetUnknown([]byte{0x98, 0x06, 0x01})
		file.MessageType[0].Options = &descriptorpb.MessageOptions{}
		proto.SetExtension(file.MessageType[0].Options, datav1.E_Dataset, option)

		_, err := CompileDescriptorBytes(descriptorSetBytes(t, file), nil, nil)
		require.ErrorContains(t, err, "dataset option contains fields unsupported by this compiler")
	})

	t.Run("newer field option", func(t *testing.T) {
		option := uuidOptions()
		option.ProtoReflect().SetUnknown([]byte{0x98, 0x06, 0x01})
		_, err := CompileMessage(refinedMessage(t, refinedMessageSpec{
			fields: []refinedFieldSpec{{
				name: "id", number: 1, kind: descriptorpb.FieldDescriptorProto_TYPE_STRING, options: option,
			}},
		}), nil)
		require.ErrorContains(t, err, "field option contains fields unsupported by this compiler")
	})

	t.Run("newer nested fixed-list option", func(t *testing.T) {
		option := fixedListOptions(8)
		option.GetFixedList().ProtoReflect().SetUnknown([]byte{0x98, 0x06, 0x01})
		_, err := CompileMessage(refinedMessage(t, refinedMessageSpec{
			fields: []refinedFieldSpec{{
				name: "vector", number: 1, kind: descriptorpb.FieldDescriptorProto_TYPE_FLOAT,
				repeated: true, options: option,
			}},
		}), nil)
		require.ErrorContains(t, err, "field option.fixed_list contains fields unsupported by this compiler")
	})
}

func TestRepositoryAnnotationDeclarationsMatchCompilerPolicy(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	encoded, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "..", "..", "proto", "descriptor.binpb"))
	require.NoError(t, err)
	var files descriptorpb.FileDescriptorSet
	require.NoError(t, proto.Unmarshal(encoded, &files))
	require.NoError(t, validateAnnotationOptionNumbers(&files))
	assert.EqualValues(t, 51974, datav1.E_Dataset.TypeDescriptor().Number())
	assert.EqualValues(t, 51974, datav1.E_Field.TypeDescriptor().Number())
}

func TestCompileMessageAppliesPortableTypeRefinements(t *testing.T) {
	md := refinedMessage(t, refinedMessageSpec{
		fields: []refinedFieldSpec{
			{name: "amount", number: 1, kind: descriptorpb.FieldDescriptorProto_TYPE_STRING, options: decimalOptions(38, 9)},
			{name: "id", number: 2, kind: descriptorpb.FieldDescriptorProto_TYPE_STRING, options: uuidOptions()},
			{name: "digest", number: 3, kind: descriptorpb.FieldDescriptorProto_TYPE_BYTES, options: fixedBytesOptions(32)},
			{name: "amounts", number: 4, kind: descriptorpb.FieldDescriptorProto_TYPE_STRING, repeated: true, options: decimalOptions(12, 2)},
			// scale is deliberately omitted from the serialized proto3 option;
			// its contractual default is zero.
			{name: "whole_amount", number: 5, kind: descriptorpb.FieldDescriptorProto_TYPE_STRING, options: decimalOptions(18, 0)},
			{name: "vector", number: 6, kind: descriptorpb.FieldDescriptorProto_TYPE_FLOAT, repeated: true, options: fixedListOptions(8)},
			{name: "vector64", number: 7, kind: descriptorpb.FieldDescriptorProto_TYPE_DOUBLE, repeated: true, options: fixedListOptions(4)},
		},
	})

	schema, err := CompileMessage(md, nil)
	require.NoError(t, err)
	assert.Equal(t, uint32(38), schemaField(t, schema, 1).GetType().GetDecimal().GetPrecision())
	assert.Equal(t, uint32(9), schemaField(t, schema, 1).GetType().GetDecimal().GetScale())
	assert.NotNil(t, schemaField(t, schema, 2).GetType().GetUuid())
	assert.Equal(t, uint32(32), schemaField(t, schema, 3).GetType().GetFixedBytes().GetByteLength())

	repeated := schemaField(t, schema, 4)
	require.NotNil(t, repeated.GetType().GetList())
	assert.Nil(t, repeated.GetType().GetDecimal())
	element := repeated.GetType().GetList().GetElement()
	require.NotNil(t, element)
	assert.Equal(t, uint32(12), element.GetType().GetDecimal().GetPrecision())
	assert.Equal(t, uint32(2), element.GetType().GetDecimal().GetScale())
	wholeAmount := schemaField(t, schema, 5).GetType().GetDecimal()
	require.NotNil(t, wholeAmount)
	assert.Equal(t, uint32(18), wholeAmount.GetPrecision())
	assert.Zero(t, wholeAmount.GetScale())
	vector := schemaField(t, schema, 6).GetType().GetList()
	require.NotNil(t, vector)
	assert.EqualValues(t, 8, vector.GetFixedLength())
	assert.Equal(t, datav1.PrimitiveKind_PRIMITIVE_KIND_FLOAT, vector.GetElement().GetType().GetPrimitive().GetKind())
	vector64 := schemaField(t, schema, 7).GetType().GetList()
	require.NotNil(t, vector64)
	assert.EqualValues(t, 4, vector64.GetFixedLength())
	assert.Equal(t, datav1.PrimitiveKind_PRIMITIVE_KIND_DOUBLE, vector64.GetElement().GetType().GetPrimitive().GetKind())

	serialized := descriptorSetBytes(t, protodesc.ToFileDescriptorProto(md.ParentFile()))
	bundle, err := CompileDescriptorBytes(serialized, []string{"shape.v1.ScalarRow"}, nil)
	require.NoError(t, err)
	assert.True(t, proto.Equal(schema, bundle.GetDatasets()[0]), "serialized field options must compile identically")
}

func TestCompileMessageValidatesPortableTypeRefinements(t *testing.T) {
	tests := []struct {
		name      string
		syntax    string
		field     refinedFieldSpec
		wantError string
	}{
		{
			name:      "decimal carrier",
			field:     refinedFieldSpec{name: "value", number: 1, kind: descriptorpb.FieldDescriptorProto_TYPE_BYTES, options: decimalOptions(10, 2)},
			wantError: "decimal refinement requires a protobuf string carrier",
		},
		{
			name:      "zero decimal precision",
			field:     refinedFieldSpec{name: "value", number: 1, kind: descriptorpb.FieldDescriptorProto_TYPE_STRING, options: decimalOptions(0, 0)},
			wantError: "decimal precision must be between 1 and 38",
		},
		{
			name:      "excess decimal precision",
			field:     refinedFieldSpec{name: "value", number: 1, kind: descriptorpb.FieldDescriptorProto_TYPE_STRING, options: decimalOptions(39, 0)},
			wantError: "decimal precision must be between 1 and 38",
		},
		{
			name:      "decimal scale",
			field:     refinedFieldSpec{name: "value", number: 1, kind: descriptorpb.FieldDescriptorProto_TYPE_STRING, options: decimalOptions(10, 11)},
			wantError: "decimal scale 11 exceeds precision 10",
		},
		{
			name:      "uuid carrier",
			field:     refinedFieldSpec{name: "value", number: 1, kind: descriptorpb.FieldDescriptorProto_TYPE_BYTES, options: uuidOptions()},
			wantError: "uuid refinement requires a protobuf string carrier",
		},
		{
			name:      "fixed bytes carrier",
			field:     refinedFieldSpec{name: "value", number: 1, kind: descriptorpb.FieldDescriptorProto_TYPE_STRING, options: fixedBytesOptions(16)},
			wantError: "fixed_bytes refinement requires a protobuf bytes carrier",
		},
		{
			name:      "zero fixed bytes length",
			field:     refinedFieldSpec{name: "value", number: 1, kind: descriptorpb.FieldDescriptorProto_TYPE_BYTES, options: fixedBytesOptions(0)},
			wantError: "fixed_bytes byte_length must be greater than zero",
		},
		{
			name:      "excess fixed bytes length",
			field:     refinedFieldSpec{name: "value", number: 1, kind: descriptorpb.FieldDescriptorProto_TYPE_BYTES, options: fixedBytesOptions(1 << 31)},
			wantError: "fixed_bytes byte_length must not exceed 2147483647",
		},
		{
			name:      "empty option",
			field:     refinedFieldSpec{name: "value", number: 1, kind: descriptorpb.FieldDescriptorProto_TYPE_STRING, options: &datav1.FieldOptions{}},
			wantError: "field option must select exactly one semantic type",
		},
		{
			name:      "fixed list singular",
			field:     refinedFieldSpec{name: "value", number: 1, kind: descriptorpb.FieldDescriptorProto_TYPE_FLOAT, options: fixedListOptions(8)},
			wantError: "fixed_list refinement requires a non-map repeated field",
		},
		{
			name:      "fixed list carrier",
			field:     refinedFieldSpec{name: "value", number: 1, kind: descriptorpb.FieldDescriptorProto_TYPE_INT32, repeated: true, options: fixedListOptions(8)},
			wantError: "fixed_list refinement requires a protobuf float or double carrier",
		},
		{
			name:      "zero fixed list length",
			field:     refinedFieldSpec{name: "value", number: 1, kind: descriptorpb.FieldDescriptorProto_TYPE_FLOAT, repeated: true, options: fixedListOptions(0)},
			wantError: "fixed_list length must be greater than zero",
		},
		{
			name:      "excess fixed list length",
			field:     refinedFieldSpec{name: "value", number: 1, kind: descriptorpb.FieldDescriptorProto_TYPE_FLOAT, repeated: true, options: fixedListOptions(1 << 31)},
			wantError: "fixed_list length must not exceed 2147483647",
		},
		{
			name: "fixed list semantic conflict",
			field: refinedFieldSpec{name: "value", number: 1, kind: descriptorpb.FieldDescriptorProto_TYPE_FLOAT, repeated: true, options: func() *datav1.FieldOptions {
				options := fixedListOptions(8)
				options.SemanticType = &datav1.FieldOptions_Uuid{Uuid: &datav1.UuidOptions{}}
				return options
			}()},
			wantError: "fixed_list cannot be combined with a semantic type refinement",
		},
		{
			name:      "implicit singular presence",
			syntax:    "proto3",
			field:     refinedFieldSpec{name: "value", number: 1, kind: descriptorpb.FieldDescriptorProto_TYPE_STRING, options: uuidOptions()},
			wantError: "refined singular field must have explicit or oneof presence",
		},
		{
			name:      "required singular presence",
			field:     refinedFieldSpec{name: "value", number: 1, kind: descriptorpb.FieldDescriptorProto_TYPE_STRING, required: true, options: uuidOptions()},
			wantError: "refined singular field must have explicit or oneof presence",
		},
		{
			name:      "protobuf default",
			field:     refinedFieldSpec{name: "value", number: 1, kind: descriptorpb.FieldDescriptorProto_TYPE_STRING, defaultValue: "0.00", options: decimalOptions(10, 2)},
			wantError: "refined singular field must not declare a protobuf default",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := CompileMessage(refinedMessage(t, refinedMessageSpec{
				syntax: tt.syntax,
				fields: []refinedFieldSpec{tt.field},
			}), nil)
			require.ErrorContains(t, err, tt.wantError)
		})
	}
}

func TestCompileMessageRejectsMapRefinement(t *testing.T) {
	md := refinedMapMessage(t, decimalOptions(10, 2))
	_, err := CompileMessage(md, nil)
	require.ErrorContains(t, err, "semantic type refinements are not supported on protobuf maps")

	md = refinedMapMessage(t, fixedListOptions(8))
	_, err = CompileMessage(md, nil)
	require.ErrorContains(t, err, "fixed_list refinement requires a non-map repeated field")
}

func TestCompileMessageTreatsFixedListLengthAsEvolutionaryShape(t *testing.T) {
	firstDescriptor := refinedMessage(t, refinedMessageSpec{fields: []refinedFieldSpec{{
		name: "vector", number: 1, kind: descriptorpb.FieldDescriptorProto_TYPE_FLOAT,
		repeated: true, options: fixedListOptions(8),
	}}})
	first, err := CompileMessage(firstDescriptor, nil)
	require.NoError(t, err)
	assert.EqualValues(t, 8, schemaField(t, first, 1).GetType().GetList().GetFixedLength())

	for name, options := range map[string]*datav1.FieldOptions{
		"changed": fixedListOptions(16),
		"removed": nil,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := CompileMessage(refinedMessage(t, refinedMessageSpec{fields: []refinedFieldSpec{{
				name: "vector", number: 1, kind: descriptorpb.FieldDescriptorProto_TYPE_FLOAT,
				repeated: true, options: options,
			}}}), first)
			require.ErrorContains(t, err, "logical shape changed")
			require.ErrorContains(t, err, "use a new protobuf field number")
		})
	}

	replacement, err := CompileMessage(refinedMessage(t, refinedMessageSpec{
		reservedNames:   []string{"vector"},
		reservedNumbers: []int32{1},
		fields: []refinedFieldSpec{{
			name: "vector_v2", number: 2, kind: descriptorpb.FieldDescriptorProto_TYPE_FLOAT,
			repeated: true, options: fixedListOptions(16),
		}},
	}), first)
	require.NoError(t, err)
	assert.EqualValues(t, 16, schemaField(t, replacement, 2).GetType().GetList().GetFixedLength())
	require.Len(t, replacement.GetRetiredFields(), 2, "the retired list and its element identities are tombstoned")
	assert.Equal(t, []string{"field:1", "field:1/list:element"}, []string{
		replacement.GetRetiredFields()[0].GetIdentity(),
		replacement.GetRetiredFields()[1].GetIdentity(),
	})
}

func TestCompileMessageOwnsStorageNamesAndPreservesReservedRenames(t *testing.T) {
	firstDescriptor := refinedMessage(t, refinedMessageSpec{
		fields: []refinedFieldSpec{{
			name: "amount", number: 1, kind: descriptorpb.FieldDescriptorProto_TYPE_STRING, options: decimalOptions(10, 2),
		}},
	})
	first, err := CompileMessage(firstDescriptor, nil)
	require.NoError(t, err)
	assert.Equal(t, "shape_v1_scalar_row", first.GetName())
	assert.Equal(t, "amount", first.GetFields()[0].GetName())
	assert.Equal(t, "amount", first.GetFields()[0].GetStorageNameSource())

	tamperedDataset := proto.Clone(first).(*datav1.DatasetSchema)
	tamperedDataset.Name = "ledger_entries"
	_, err = CompileMessage(firstDescriptor, tamperedDataset)
	require.ErrorContains(t, err, "generated SchemaBundle names are compiler-owned")

	tamperedField := proto.Clone(first).(*datav1.DatasetSchema)
	tamperedField.Fields[0].Name = "ledger_amount"
	_, err = CompileMessage(firstDescriptor, tamperedField)
	require.ErrorContains(t, err, "generated SchemaBundle names are compiler-owned")

	unreservedRename := refinedMessage(t, refinedMessageSpec{
		fields: []refinedFieldSpec{{
			name: "renamed_amount", number: 1, kind: descriptorpb.FieldDescriptorProto_TYPE_STRING, options: decimalOptions(10, 2),
		}},
	})
	_, err = CompileMessage(unreservedRename, first)
	require.ErrorContains(t, err, "must reserve the exact previous protobuf field name")

	coordinatedTamper := proto.Clone(first).(*datav1.DatasetSchema)
	coordinatedTamper.Fields[0].Name = "renamed_amount"
	_, err = CompileMessage(unreservedRename, coordinatedTamper)
	require.ErrorContains(t, err, "storage_name_source \"amount\"")

	unrelatedReserved := refinedMessage(t, refinedMessageSpec{
		reservedNames: []string{"ledger_amount"},
		fields: []refinedFieldSpec{{
			name: "renamed_amount", number: 1, kind: descriptorpb.FieldDescriptorProto_TYPE_STRING, options: decimalOptions(10, 2),
		}},
	})
	unrelatedReservedBundle := proto.Clone(first).(*datav1.DatasetSchema)
	unrelatedReservedBundle.Fields[0].Name = "ledger_amount"
	_, err = CompileMessage(unrelatedReserved, unrelatedReservedBundle)
	require.ErrorContains(t, err, "storage_name_source \"amount\"")

	renamedDescriptor := refinedMessage(t, refinedMessageSpec{
		reservedNames: []string{"amount"},
		fields: []refinedFieldSpec{{
			name: "renamed_amount", number: 1, kind: descriptorpb.FieldDescriptorProto_TYPE_STRING, options: decimalOptions(10, 2),
		}},
	})
	renamed, err := CompileMessage(renamedDescriptor, first)
	require.NoError(t, err)
	assert.Equal(t, "shape_v1_scalar_row", renamed.GetName())
	assert.Equal(t, "amount", schemaField(t, renamed, 1).GetName())
	assert.Equal(t, "shape.v1.ScalarRow.renamed_amount", schemaField(t, renamed, 1).GetProtoFullName())
	assert.Equal(t, "amount", schemaField(t, renamed, 1).GetStorageNameSource())

	recompiled, err := CompileMessage(renamedDescriptor, renamed)
	require.NoError(t, err)
	assert.Equal(t, "amount", schemaField(t, recompiled, 1).GetName())
	assert.True(t, proto.Equal(renamed, recompiled))

	renamedAgainDescriptor := refinedMessage(t, refinedMessageSpec{
		reservedNames: []string{"amount", "renamed_amount"},
		fields: []refinedFieldSpec{{
			name: "final_amount", number: 1, kind: descriptorpb.FieldDescriptorProto_TYPE_STRING, options: decimalOptions(10, 2),
		}},
	})
	renamedAgain, err := CompileMessage(renamedAgainDescriptor, renamed)
	require.NoError(t, err)
	assert.Equal(t, "amount", schemaField(t, renamedAgain, 1).GetName())
	assert.Equal(t, "amount", schemaField(t, renamedAgain, 1).GetStorageNameSource())

	droppedIntermediateReservation := refinedMessage(t, refinedMessageSpec{
		reservedNames: []string{"amount"},
		fields: []refinedFieldSpec{{
			name: "final_amount", number: 1, kind: descriptorpb.FieldDescriptorProto_TYPE_STRING, options: decimalOptions(10, 2),
		}},
	})
	_, err = CompileMessage(droppedIntermediateReservation, renamed)
	require.ErrorContains(t, err, "exact previous protobuf field name \"renamed_amount\"")

	changedDescriptor := refinedMessage(t, refinedMessageSpec{
		reservedNames: []string{"amount"},
		fields: []refinedFieldSpec{{
			name: "renamed_amount", number: 1, kind: descriptorpb.FieldDescriptorProto_TYPE_STRING, options: decimalOptions(11, 2),
		}},
	})
	_, err = CompileMessage(changedDescriptor, renamed)
	require.ErrorContains(t, err, "logical shape changed")
	require.ErrorContains(t, err, "use a new protobuf field number")
}

func TestCompileDescriptorBytesKeepsDatasetRootsAppendOnly(t *testing.T) {
	descriptor, _ := fixtureDescriptors(t)
	first, err := CompileDescriptorBytes(descriptor, []string{
		"data.v1.CanonicalRecord",
	}, nil)
	require.NoError(t, err)

	expanded, err := CompileDescriptorBytes(descriptor, []string{
		"data.v1.Proto2Record",
		"data.v1.CanonicalRecord",
	}, first)
	require.NoError(t, err)
	require.Len(t, expanded.GetDatasets(), 2)
	assert.True(t, proto.Equal(first.GetDatasets()[0], expanded.GetDatasets()[0]))

	_, err = CompileDescriptorBytes(descriptor, []string{
		"data.v1.Proto2Record",
	}, expanded)
	require.EqualError(t, err,
		"compile descriptor: selected dataset roots omit previous datasets \"data.v1.CanonicalRecord\"; dataset roots are append-only",
	)

	// A failed removal cannot erase history. Restoring the complete root set
	// produces the same bundle deterministically.
	readded, err := CompileDescriptorBytes(descriptor, []string{
		"data.v1.CanonicalRecord",
		"data.v1.Proto2Record",
	}, expanded)
	require.NoError(t, err)
	assert.True(t, proto.Equal(expanded, readded))

	// Diagnostics remain deterministic when more than one old root is omitted.
	_, err = CompileDescriptorBytes(descriptor, []string{
		"data.v1.NestedRecord",
	}, expanded)
	require.EqualError(t, err,
		"compile descriptor: selected dataset roots omit previous datasets \"data.v1.CanonicalRecord\", \"data.v1.Proto2Record\"; dataset roots are append-only",
	)
}

func TestCompileDescriptorBytesRejectsDatasetStorageNameCollisions(t *testing.T) {
	descriptor := descriptorSetBytes(t,
		rowFile("alpha_beta.proto", "alpha.beta"),
		rowFile("alpha_beta_flat.proto", "alpha_beta"),
	)
	_, err := CompileDescriptorBytes(descriptor, []string{
		"alpha_beta.Row",
		"alpha.beta.Row",
	}, nil)
	require.EqualError(t, err,
		"compile descriptor: dataset storage name \"alpha_beta_row\" collides for protobuf messages \"alpha.beta.Row\" and \"alpha_beta.Row\"",
	)
}

func TestCompileMessageRejectsEmptyStorageNames(t *testing.T) {
	t.Run("dataset", func(t *testing.T) {
		md := messageFromFile(t, &descriptorpb.FileDescriptorProto{
			Name:    new("empty_dataset.proto"),
			Syntax:  new("proto3"),
			Package: new(""),
			MessageType: []*descriptorpb.DescriptorProto{{
				Name: new("_"),
				Field: []*descriptorpb.FieldDescriptorProto{{
					Name:   new("id"),
					Number: proto.Int32(1),
					Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
				}},
			}},
		}, "_")

		_, err := CompileMessage(md, nil)
		require.EqualError(t, err,
			`compile message "_": protobuf message name normalizes to an empty storage name`,
		)
	})

	t.Run("top-level field", func(t *testing.T) {
		md := messageFromFile(t, &descriptorpb.FileDescriptorProto{
			Name:    new("empty_top_level_field.proto"),
			Package: new("empty.v1"),
			Syntax:  new("proto3"),
			MessageType: []*descriptorpb.DescriptorProto{{
				Name: new("Row"),
				Field: []*descriptorpb.FieldDescriptorProto{{
					Name:   new("_"),
					Number: proto.Int32(1),
					Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
				}},
			}},
		}, "Row")

		_, err := CompileMessage(md, nil)
		require.EqualError(t, err,
			`compile message "empty.v1.Row": protobuf field "empty.v1.Row._" normalizes to an empty storage name within protobuf message "empty.v1.Row"`,
		)
	})

	t.Run("nested field", func(t *testing.T) {
		md := messageFromFile(t, &descriptorpb.FileDescriptorProto{
			Name:    new("empty_nested_field.proto"),
			Package: new("empty.v1"),
			Syntax:  new("proto3"),
			MessageType: []*descriptorpb.DescriptorProto{
				{
					Name: new("Nested"),
					Field: []*descriptorpb.FieldDescriptorProto{{
						Name:   new("_"),
						Number: proto.Int32(1),
						Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
					}},
				},
				{
					Name: new("Row"),
					Field: []*descriptorpb.FieldDescriptorProto{{
						Name:     new("nested"),
						Number:   proto.Int32(1),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
						TypeName: new(".empty.v1.Nested"),
					}},
				},
			},
		}, "Row")

		_, err := CompileMessage(md, nil)
		require.EqualError(t, err,
			`compile message "empty.v1.Row": field "empty.v1.Row.nested": protobuf field "empty.v1.Nested._" normalizes to an empty storage name within protobuf message "empty.v1.Nested"`,
		)
	})
}

func TestCompileMessageRejectsRecursion(t *testing.T) {
	_, files := fixtureDescriptors(t)
	_, err := CompileMessage(findMessage(t, files, "data.v1.RecursiveRecord"), nil)
	require.Error(t, err)
	require.ErrorContains(t, err, "recursive protobuf message")
	require.ErrorContains(t, err, "not a finite row schema")
}

func TestCompileMessageRejectsReachableProto2ExtensionRanges(t *testing.T) {
	tests := []struct {
		name   string
		nested bool
		root   protoreflect.Name
	}{
		{name: "dataset root", root: "Extensible"},
		{name: "nested message", nested: true, root: "Row"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			md := extensionRangeMessage(t, tt.nested, tt.root)
			_, err := CompileMessage(md, nil)
			require.ErrorContains(t, err, "protobuf message \"extensions.v1.Extensible\" declares extension ranges")
			require.ErrorContains(t, err, "cannot be represented in a finite data schema")
		})
	}
}

func TestCompileMessageRejectsStorageNameCollisionsRecursively(t *testing.T) {
	for _, nested := range []bool{false, true} {
		name := "root"
		if nested {
			name = "nested"
		}
		t.Run(name, func(t *testing.T) {
			md := storageCollisionMessage(t, nested)
			_, err := CompileMessage(md, nil)
			require.ErrorContains(t, err, "storage name \"http_status\" collides")
			require.ErrorContains(t, err, "fields \"collision.v1.Collision.HTTPStatus\" and \"collision.v1.Collision.http_status\"")
		})
	}
}

func TestCompileMessageRetainsAndRetiresStableIDs(t *testing.T) {
	v1 := evolutionMessage(t, evolutionVersion{
		rootFieldName:   "child",
		nestedFieldName: "value",
		includeRemoved:  true,
	})
	first, err := CompileMessage(v1, nil)
	require.NoError(t, err)
	child := schemaField(t, first, 1)
	nestedChild := child.GetType().GetStruct().GetFields()[0]
	assert.Equal(t, int32(1), child.GetStableId())
	assert.Equal(t, int32(2), schemaField(t, first, 2).GetStableId())
	assert.Equal(t, int32(3), nestedChild.GetStableId())
	assert.Equal(t, int32(3), first.GetLastFieldId())

	v2 := evolutionMessage(t, evolutionVersion{
		rootFieldName:       "renamed_child",
		nestedFieldName:     "renamed_value",
		rootReservedNames:   []string{"child", "removed"},
		rootReservedNumbers: []int32{2},
		nestedReservedNames: []string{"value"},
		includeAdded:        true,
	})
	second, err := CompileMessage(v2, first)
	require.NoError(t, err)
	secondChild := schemaField(t, second, 1)
	assert.Equal(t, int32(1), secondChild.GetStableId())
	assert.Equal(t, "child", secondChild.GetName())
	require.Len(t, secondChild.GetType().GetStruct().GetFields(), 1)
	assert.Equal(t, int32(3), secondChild.GetType().GetStruct().GetFields()[0].GetStableId())
	assert.Equal(t, "value", secondChild.GetType().GetStruct().GetFields()[0].GetName())
	assert.Equal(t, int32(4), schemaField(t, second, 100).GetStableId())
	assert.Equal(t, int32(4), second.GetLastFieldId())
	require.Len(t, second.GetRetiredFields(), 1)
	assert.Equal(t, int32(2), second.GetRetiredFields()[0].GetStableId())
	assert.Equal(t, "field:2", second.GetRetiredFields()[0].GetIdentity())
	assert.Equal(t, "evolution.v1.Row.removed", second.GetRetiredFields()[0].GetProtoFullName())
	assert.Equal(t, "removed", second.GetRetiredFields()[0].GetName())
	assert.Equal(t, "removed", second.GetRetiredFields()[0].GetStorageNameSource())

	secondAgain, err := CompileMessage(v2, first)
	require.NoError(t, err)
	assert.True(t, proto.Equal(second, secondAgain))

	third, err := CompileMessage(v2, second)
	require.NoError(t, err)
	assert.True(t, proto.Equal(second, third))

	v3 := evolutionMessage(t, evolutionVersion{
		rootFieldName:       "renamed_child",
		nestedFieldName:     "renamed_value",
		rootReservedNames:   []string{"child"},
		nestedReservedNames: []string{"value"},
		includeRemoved:      true,
		includeAdded:        true,
	})
	_, err = CompileMessage(v3, second)
	require.Error(t, err)
	require.ErrorContains(t, err, "reuses retired stable_id 2")
}

func TestCompileMessageTombstonesNestedAndCollectionIdentities(t *testing.T) {
	first, err := CompileMessage(collectionRetirementMessage(t, true), nil)
	require.NoError(t, err)

	child := schemaField(t, first, 1)
	labels := schemaField(t, first, 2)
	counters := schemaField(t, first, 3)
	expected := map[string]int32{
		"field:1/field:2":      child.GetType().GetStruct().GetFields()[1].GetStableId(),
		"field:2":              labels.GetStableId(),
		"field:2/list:element": labels.GetType().GetList().GetElement().GetStableId(),
		"field:3":              counters.GetStableId(),
		"field:3/map:key":      counters.GetType().GetMap().GetKey().GetStableId(),
		"field:3/map:value":    counters.GetType().GetMap().GetValue().GetStableId(),
	}

	second, err := CompileMessage(collectionRetirementMessage(t, false), first)
	require.NoError(t, err)
	require.Equal(t, first.GetLastFieldId(), second.GetLastFieldId())
	require.Len(t, second.GetRetiredFields(), len(expected))
	for _, retired := range second.GetRetiredFields() {
		require.Equal(t, expected[retired.GetIdentity()], retired.GetStableId(), retired.GetIdentity())
		delete(expected, retired.GetIdentity())
	}
	require.Empty(t, expected)
}

func TestCompileMessageEnforcesPermanentRemovalHistory(t *testing.T) {
	firstDescriptor := refinedMessage(t, refinedMessageSpec{
		fields: []refinedFieldSpec{
			{name: "old_value", number: 1, kind: descriptorpb.FieldDescriptorProto_TYPE_STRING},
			{name: "keep", number: 2, kind: descriptorpb.FieldDescriptorProto_TYPE_STRING},
		},
	})
	first, err := CompileMessage(firstDescriptor, nil)
	require.NoError(t, err)

	removedFields := []refinedFieldSpec{{
		name: "keep", number: 2, kind: descriptorpb.FieldDescriptorProto_TYPE_STRING,
	}}
	for _, test := range []struct {
		name            string
		reservedNames   []string
		reservedNumbers []int32
		want            string
	}{
		{name: "neither reserved", want: "number 1 must remain reserved"},
		{name: "only name reserved", reservedNames: []string{"old_value"}, want: "number 1 must remain reserved"},
		{name: "only number reserved", reservedNumbers: []int32{1}, want: `exact name "old_value" must remain reserved`},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := CompileMessage(refinedMessage(t, refinedMessageSpec{
				reservedNames:   test.reservedNames,
				reservedNumbers: test.reservedNumbers,
				fields:          removedFields,
			}), first)
			require.ErrorContains(t, err, test.want)
		})
	}

	removedDescriptor := refinedMessage(t, refinedMessageSpec{
		reservedNames:   []string{"old_value"},
		reservedNumbers: []int32{1},
		fields:          removedFields,
	})
	removed, err := CompileMessage(removedDescriptor, first)
	require.NoError(t, err)
	require.Len(t, removed.GetRetiredFields(), 1)
	retired := removed.GetRetiredFields()[0]
	assert.Equal(t, "field:1", retired.GetIdentity())
	assert.Equal(t, "shape.v1.ScalarRow.old_value", retired.GetProtoFullName())
	assert.Equal(t, "old_value", retired.GetName())
	assert.Equal(t, "old_value", retired.GetStorageNameSource())

	recompiled, err := CompileMessage(removedDescriptor, removed)
	require.NoError(t, err)
	assert.True(t, proto.Equal(removed, recompiled))

	_, err = CompileMessage(refinedMessage(t, refinedMessageSpec{
		reservedNames: []string{"old_value"},
		fields:        removedFields,
	}), removed)
	require.ErrorContains(t, err, "number 1 must remain reserved")

	_, err = CompileMessage(refinedMessage(t, refinedMessageSpec{
		reservedNumbers: []int32{1},
		fields:          removedFields,
	}), removed)
	require.ErrorContains(t, err, `exact name "old_value" must remain reserved`)

	_, err = CompileMessage(refinedMessage(t, refinedMessageSpec{
		fields: []refinedFieldSpec{
			{name: "replacement", number: 1, kind: descriptorpb.FieldDescriptorProto_TYPE_STRING},
			{name: "keep", number: 2, kind: descriptorpb.FieldDescriptorProto_TYPE_STRING},
		},
	}), removed)
	require.ErrorContains(t, err, "reuses retired stable_id")

	_, err = CompileMessage(refinedMessage(t, refinedMessageSpec{
		reservedNames:   []string{"old_value"},
		reservedNumbers: []int32{1},
		fields: []refinedFieldSpec{
			{name: "keep", number: 2, kind: descriptorpb.FieldDescriptorProto_TYPE_STRING},
			{name: "OldValue", number: 3, kind: descriptorpb.FieldDescriptorProto_TYPE_STRING},
		},
	}), removed)
	require.ErrorContains(t, err, `storage name "old_value" in this logical scope is permanently owned`)
}

func TestCompileMessageRequiresReservationsOnlyForDirectlyReachableRemovals(t *testing.T) {
	first, err := CompileMessage(nestedRemovalMessage(t, nestedRemovalVersion{
		includeParent: true,
		includeChild:  true,
	}), nil)
	require.NoError(t, err)

	_, err = CompileMessage(nestedRemovalMessage(t, nestedRemovalVersion{
		includeParent: true,
	}), first)
	require.ErrorContains(t, err, "number 1 must remain reserved")

	directlyReserved, err := CompileMessage(nestedRemovalMessage(t, nestedRemovalVersion{
		includeParent: true,
		reserveChild:  true,
	}), first)
	require.NoError(t, err)
	require.Len(t, directlyReserved.GetRetiredFields(), 1)
	assert.Equal(t, "field:1/field:1", directlyReserved.GetRetiredFields()[0].GetIdentity())

	ancestorRemoved, err := CompileMessage(nestedRemovalMessage(t, nestedRemovalVersion{
		reserveParent: true,
	}), first)
	require.NoError(t, err)
	require.Len(t, ancestorRemoved.GetRetiredFields(), 2)
	assert.Equal(t, "field:1", ancestorRemoved.GetRetiredFields()[0].GetIdentity())
	assert.Equal(t, "field:1/field:1", ancestorRemoved.GetRetiredFields()[1].GetIdentity())
}

func TestCompileMessageRejectsStorageShapeAndPresenceChanges(t *testing.T) {
	stringMessage := scalarMessage(t, descriptorpb.FieldDescriptorProto_TYPE_STRING, false)
	previous, err := CompileMessage(stringMessage, nil)
	require.NoError(t, err)

	bytesMessage := scalarMessage(t, descriptorpb.FieldDescriptorProto_TYPE_BYTES, false)
	_, err = CompileMessage(bytesMessage, previous)
	require.Error(t, err)
	require.ErrorContains(t, err, "logical shape changed")
	require.ErrorContains(t, err, "new protobuf field number")

	implicit := scalarMessage(t, descriptorpb.FieldDescriptorProto_TYPE_INT32, false)
	implicitSchema, err := CompileMessage(implicit, nil)
	require.NoError(t, err)
	explicit := scalarMessage(t, descriptorpb.FieldDescriptorProto_TYPE_INT32, true)
	_, err = CompileMessage(explicit, implicitSchema)
	require.Error(t, err)
	require.ErrorContains(t, err, "presence changed")

	initialEnum := evolvingEnumMessage(t, false)
	initialEnumSchema, err := CompileMessage(initialEnum, nil)
	require.NoError(t, err)
	extendedEnum := evolvingEnumMessage(t, true)
	extendedEnumSchema, err := CompileMessage(extendedEnum, initialEnumSchema)
	require.NoError(t, err)
	assert.Equal(t, int32(1), schemaField(t, extendedEnumSchema, 1).GetStableId())
	assert.Len(t, schemaField(t, extendedEnumSchema, 1).GetType().GetEnum().GetValues(), 3)
}

func TestCompileMessageRejectsEnumOneofAndDefaultSemanticChanges(t *testing.T) {
	baseValues := []*descriptorpb.EnumValueDescriptorProto{
		{Name: new("STATE_UNSPECIFIED"), Number: proto.Int32(0)},
		{Name: new("STATE_READY"), Number: proto.Int32(1)},
	}
	baseEnum := enumEvolutionMessage(t, baseValues, false)
	baseEnumSchema, err := CompileMessage(baseEnum, nil)
	require.NoError(t, err)

	for name, values := range map[string][]*descriptorpb.EnumValueDescriptorProto{
		"removed value": {
			{Name: new("STATE_UNSPECIFIED"), Number: proto.Int32(0)},
		},
		"renumbered value": {
			{Name: new("STATE_UNSPECIFIED"), Number: proto.Int32(0)},
			{Name: new("STATE_READY"), Number: proto.Int32(2)},
		},
		"renamed without alias": {
			{Name: new("STATE_UNSPECIFIED"), Number: proto.Int32(0)},
			{Name: new("STATE_AVAILABLE"), Number: proto.Int32(1)},
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := CompileMessage(enumEvolutionMessage(t, values, false), baseEnumSchema)
			require.ErrorContains(t, err, "logical shape changed")
		})
	}

	aliasedValues := append(slices.Clone(baseValues), &descriptorpb.EnumValueDescriptorProto{
		Name: new("STATE_AVAILABLE"), Number: proto.Int32(1),
	})
	aliased, err := CompileMessage(enumEvolutionMessage(t, aliasedValues, true), baseEnumSchema)
	require.NoError(t, err)
	assert.Len(t, schemaField(t, aliased, 1).GetType().GetEnum().GetValues(), 3)

	initialOneof, err := CompileMessage(oneofEvolutionMessage(t, "choice"), nil)
	require.NoError(t, err)
	_, err = CompileMessage(oneofEvolutionMessage(t, "selection"), initialOneof)
	require.ErrorContains(t, err, "oneof changed")

	initialDefault, err := CompileMessage(proto2DefaultMessage(t, "first"), nil)
	require.NoError(t, err)
	_, err = CompileMessage(proto2DefaultMessage(t, "second"), initialDefault)
	require.ErrorContains(t, err, "default changed")
}

func TestCompileMessageValidatesPreviousIdentityState(t *testing.T) {
	md := scalarMessage(t, descriptorpb.FieldDescriptorProto_TYPE_STRING, false)
	valid, err := CompileMessage(md, nil)
	require.NoError(t, err)

	duplicate := proto.Clone(valid).(*datav1.DatasetSchema)
	duplicate.Fields = append(duplicate.Fields, proto.Clone(duplicate.Fields[0]).(*datav1.Field))
	_, err = CompileMessage(md, duplicate)
	require.ErrorContains(t, err, "active identity \"field:1\" is duplicated")

	outOfRange := proto.Clone(valid).(*datav1.DatasetSchema)
	outOfRange.Fields[0].StableId = 0
	_, err = CompileMessage(md, outOfRange)
	require.ErrorContains(t, err, "outside 1..")

	badLast := proto.Clone(valid).(*datav1.DatasetSchema)
	badLast.LastFieldId = 2
	_, err = CompileMessage(md, badLast)
	require.ErrorContains(t, err, "highest allocated stable_id is 1")
}

type evolutionVersion struct {
	rootFieldName       string
	nestedFieldName     string
	rootReservedNames   []string
	rootReservedNumbers []int32
	nestedReservedNames []string
	includeRemoved      bool
	includeAdded        bool
}

func evolutionMessage(t *testing.T, version evolutionVersion) protoreflect.MessageDescriptor {
	t.Helper()
	nested := &descriptorpb.DescriptorProto{
		Name:         new("Nested"),
		ReservedName: version.nestedReservedNames,
		Field: []*descriptorpb.FieldDescriptorProto{{
			Name:   new(version.nestedFieldName),
			Number: proto.Int32(1),
			Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
		}},
	}
	row := &descriptorpb.DescriptorProto{
		Name:          new("Row"),
		ReservedName:  version.rootReservedNames,
		ReservedRange: reservedRanges(version.rootReservedNumbers...),
		Field: []*descriptorpb.FieldDescriptorProto{{
			Name:     new(version.rootFieldName),
			Number:   proto.Int32(1),
			Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
			TypeName: new(".evolution.v1.Nested"),
		}},
	}
	if version.includeRemoved {
		row.Field = append(row.Field, &descriptorpb.FieldDescriptorProto{
			Name:   new("removed"),
			Number: proto.Int32(2),
			Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
		})
	}
	if version.includeAdded {
		row.Field = append(row.Field, &descriptorpb.FieldDescriptorProto{
			Name:   new("added"),
			Number: proto.Int32(100),
			Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
		})
	}
	return messageFromFile(t, &descriptorpb.FileDescriptorProto{
		Name:        new("evolution.proto"),
		Package:     new("evolution.v1"),
		Syntax:      new("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{nested, row},
	}, "Row")
}

func collectionRetirementMessage(t *testing.T, includeRemoved bool) protoreflect.MessageDescriptor {
	t.Helper()
	child := &descriptorpb.DescriptorProto{
		Name: new("Child"),
		Field: []*descriptorpb.FieldDescriptorProto{{
			Name:   new("keep"),
			Number: proto.Int32(1),
			Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
		}},
	}
	if includeRemoved {
		child.Field = append(child.Field, &descriptorpb.FieldDescriptorProto{
			Name:   new("removed"),
			Number: proto.Int32(2),
			Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
		})
	}
	if !includeRemoved {
		child.ReservedName = []string{"removed"}
		child.ReservedRange = reservedRanges(2)
	}

	row := &descriptorpb.DescriptorProto{
		Name: new("Row"),
		Field: []*descriptorpb.FieldDescriptorProto{{
			Name:     new("child"),
			Number:   proto.Int32(1),
			Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
			TypeName: new(".retirement.v1.Child"),
		}},
	}
	if includeRemoved {
		row.NestedType = []*descriptorpb.DescriptorProto{{
			Name:    new("CountersEntry"),
			Options: &descriptorpb.MessageOptions{MapEntry: new(true)},
			Field: []*descriptorpb.FieldDescriptorProto{
				{
					Name:   new("key"),
					Number: proto.Int32(1),
					Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
				},
				{
					Name:   new("value"),
					Number: proto.Int32(2),
					Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					Type:   descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
				},
			},
		}}
		row.Field = append(row.Field,
			&descriptorpb.FieldDescriptorProto{
				Name:   new("labels"),
				Number: proto.Int32(2),
				Label:  descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(),
				Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
			},
			&descriptorpb.FieldDescriptorProto{
				Name:     new("counters"),
				Number:   proto.Int32(3),
				Label:    descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(),
				Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
				TypeName: new(".retirement.v1.Row.CountersEntry"),
			},
		)
	}
	if !includeRemoved {
		row.ReservedName = []string{"labels", "counters"}
		row.ReservedRange = reservedRanges(2, 3)
	}

	return messageFromFile(t, &descriptorpb.FileDescriptorProto{
		Name:        new("collection_retirement.proto"),
		Package:     new("retirement.v1"),
		Syntax:      new("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{child, row},
	}, "Row")
}

type nestedRemovalVersion struct {
	includeParent bool
	includeChild  bool
	reserveParent bool
	reserveChild  bool
}

func nestedRemovalMessage(t *testing.T, version nestedRemovalVersion) protoreflect.MessageDescriptor {
	t.Helper()
	child := &descriptorpb.DescriptorProto{Name: new("Child")}
	if version.includeChild {
		child.Field = []*descriptorpb.FieldDescriptorProto{{
			Name:   new("value"),
			Number: proto.Int32(1),
			Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
		}}
	}
	if version.reserveChild {
		child.ReservedName = []string{"value"}
		child.ReservedRange = reservedRanges(1)
	}

	row := &descriptorpb.DescriptorProto{Name: new("Row")}
	if version.includeParent {
		row.Field = []*descriptorpb.FieldDescriptorProto{{
			Name:     new("child"),
			Number:   proto.Int32(1),
			Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
			TypeName: new(".nested_removal.v1.Child"),
		}}
	}
	if version.reserveParent {
		row.ReservedName = []string{"child"}
		row.ReservedRange = reservedRanges(1)
	}

	return messageFromFile(t, &descriptorpb.FileDescriptorProto{
		Name:        new("nested_removal.proto"),
		Package:     new("nested_removal.v1"),
		Syntax:      new("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{child, row},
	}, "Row")
}

func scalarMessage(
	t *testing.T,
	kind descriptorpb.FieldDescriptorProto_Type,
	proto3Optional bool,
) protoreflect.MessageDescriptor {
	t.Helper()
	field := &descriptorpb.FieldDescriptorProto{
		Name:           new("value"),
		Number:         proto.Int32(1),
		Label:          descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		Type:           kind.Enum(),
		Proto3Optional: new(proto3Optional),
	}
	message := &descriptorpb.DescriptorProto{
		Name:  new("ScalarRow"),
		Field: []*descriptorpb.FieldDescriptorProto{field},
	}
	if proto3Optional {
		field.OneofIndex = proto.Int32(0)
		message.OneofDecl = []*descriptorpb.OneofDescriptorProto{{Name: new("_value")}}
	}
	return messageFromFile(t, &descriptorpb.FileDescriptorProto{
		Name:        new("scalar.proto"),
		Package:     new("shape.v1"),
		Syntax:      new("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{message},
	}, "ScalarRow")
}

func proto2EnumMessage(t *testing.T) protoreflect.MessageDescriptor {
	t.Helper()
	return messageFromFile(t, &descriptorpb.FileDescriptorProto{
		Name:    new("closed_enum.proto"),
		Package: new("closed.v1"),
		Syntax:  new("proto2"),
		EnumType: []*descriptorpb.EnumDescriptorProto{{
			Name: new("State"),
			Value: []*descriptorpb.EnumValueDescriptorProto{
				{Name: new("UNKNOWN"), Number: proto.Int32(0)},
				{Name: new("READY"), Number: proto.Int32(1)},
			},
		}},
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: new("EnumRow"),
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name:     new("state"),
				Number:   proto.Int32(1),
				Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:     descriptorpb.FieldDescriptorProto_TYPE_ENUM.Enum(),
				TypeName: new(".closed.v1.State"),
			}},
		}},
	}, "EnumRow")
}

func evolvingEnumMessage(t *testing.T, extended bool) protoreflect.MessageDescriptor {
	t.Helper()
	values := []*descriptorpb.EnumValueDescriptorProto{
		{Name: new("STATE_UNSPECIFIED"), Number: proto.Int32(0)},
		{Name: new("STATE_READY"), Number: proto.Int32(1)},
	}
	if extended {
		values = append(values, &descriptorpb.EnumValueDescriptorProto{
			Name: new("STATE_DONE"), Number: proto.Int32(2),
		})
	}
	return enumEvolutionMessage(t, values, false)
}

func enumEvolutionMessage(
	t *testing.T,
	values []*descriptorpb.EnumValueDescriptorProto,
	allowAlias bool,
) protoreflect.MessageDescriptor {
	t.Helper()
	return messageFromFile(t, &descriptorpb.FileDescriptorProto{
		Name:    new("evolving_enum.proto"),
		Package: new("enum_evolution.v1"),
		Syntax:  new("proto3"),
		EnumType: []*descriptorpb.EnumDescriptorProto{{
			Name:    new("State"),
			Value:   values,
			Options: &descriptorpb.EnumOptions{AllowAlias: new(allowAlias)},
		}},
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: new("EnumRow"),
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name:     new("state"),
				Number:   proto.Int32(1),
				Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:     descriptorpb.FieldDescriptorProto_TYPE_ENUM.Enum(),
				TypeName: new(".enum_evolution.v1.State"),
			}},
		}},
	}, "EnumRow")
}

func oneofEvolutionMessage(t *testing.T, oneofName string) protoreflect.MessageDescriptor {
	t.Helper()
	return messageFromFile(t, &descriptorpb.FileDescriptorProto{
		Name:    new("oneof_evolution.proto"),
		Package: new("oneof_evolution.v1"),
		Syntax:  new("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name:      new("OneofRow"),
			OneofDecl: []*descriptorpb.OneofDescriptorProto{{Name: new(oneofName)}},
			Field: []*descriptorpb.FieldDescriptorProto{
				{
					Name: new("text"), Number: proto.Int32(1),
					Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					Type:  descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(), OneofIndex: proto.Int32(0),
				},
				{
					Name: new("number"), Number: proto.Int32(2),
					Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					Type:  descriptorpb.FieldDescriptorProto_TYPE_INT32.Enum(), OneofIndex: proto.Int32(0),
				},
			},
		}},
	}, "OneofRow")
}

func proto2DefaultMessage(t *testing.T, defaultValue string) protoreflect.MessageDescriptor {
	t.Helper()
	return messageFromFile(t, &descriptorpb.FileDescriptorProto{
		Name:    new("default_evolution.proto"),
		Package: new("default_evolution.v1"),
		Syntax:  new("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: new("DefaultRow"),
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name: new("label"), Number: proto.Int32(1),
				Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:  descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(), DefaultValue: new(defaultValue),
			}},
		}},
	}, "DefaultRow")
}

func extensionRangeMessage(
	t *testing.T,
	nested bool,
	root protoreflect.Name,
) protoreflect.MessageDescriptor {
	t.Helper()
	extensible := &descriptorpb.DescriptorProto{
		Name: new("Extensible"),
		ExtensionRange: []*descriptorpb.DescriptorProto_ExtensionRange{{
			Start: proto.Int32(100),
			End:   proto.Int32(200),
		}},
	}
	messages := []*descriptorpb.DescriptorProto{extensible}
	if nested {
		messages = append(messages, &descriptorpb.DescriptorProto{
			Name: new("Row"),
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name:     new("payload"),
				Number:   proto.Int32(1),
				Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
				TypeName: new(".extensions.v1.Extensible"),
			}},
		})
	}
	return messageFromFile(t, &descriptorpb.FileDescriptorProto{
		Name:        new("extensions.proto"),
		Package:     new("extensions.v1"),
		Syntax:      new("proto2"),
		MessageType: messages,
	}, root)
}

func storageCollisionMessage(t *testing.T, nested bool) protoreflect.MessageDescriptor {
	t.Helper()
	collision := &descriptorpb.DescriptorProto{
		Name: new("Collision"),
		Field: []*descriptorpb.FieldDescriptorProto{
			{
				Name:   new("HTTPStatus"),
				Number: proto.Int32(1),
				Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
			},
			{
				Name:   new("http_status"),
				Number: proto.Int32(2),
				Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
			},
		},
	}
	messages := []*descriptorpb.DescriptorProto{collision}
	root := protoreflect.Name("Collision")
	if nested {
		root = "Row"
		messages = append(messages, &descriptorpb.DescriptorProto{
			Name: new("Row"),
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name:     new("nested"),
				Number:   proto.Int32(1),
				Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
				TypeName: new(".collision.v1.Collision"),
			}},
		})
	}
	return messageFromFile(t, &descriptorpb.FileDescriptorProto{
		Name:        new("storage_collision.proto"),
		Package:     new("collision.v1"),
		Syntax:      new("proto3"),
		MessageType: messages,
	}, root)
}

type refinedMessageSpec struct {
	syntax          string
	reservedNames   []string
	reservedNumbers []int32
	fields          []refinedFieldSpec
}

type refinedFieldSpec struct {
	name         string
	number       int32
	kind         descriptorpb.FieldDescriptorProto_Type
	repeated     bool
	required     bool
	defaultValue string
	options      *datav1.FieldOptions
}

func refinedMessage(t *testing.T, spec refinedMessageSpec) protoreflect.MessageDescriptor {
	t.Helper()
	if spec.syntax == "" {
		spec.syntax = "proto2"
	}
	fields := make([]*descriptorpb.FieldDescriptorProto, 0, len(spec.fields))
	for _, fieldSpec := range spec.fields {
		label := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
		switch {
		case fieldSpec.repeated:
			label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED
		case fieldSpec.required:
			label = descriptorpb.FieldDescriptorProto_LABEL_REQUIRED
		}
		field := &descriptorpb.FieldDescriptorProto{
			Name:   new(fieldSpec.name),
			Number: new(fieldSpec.number),
			Label:  label.Enum(),
			Type:   fieldSpec.kind.Enum(),
		}
		if fieldSpec.defaultValue != "" {
			field.DefaultValue = new(fieldSpec.defaultValue)
		}
		if fieldSpec.options != nil {
			field.Options = &descriptorpb.FieldOptions{}
			proto.SetExtension(field.Options, datav1.E_Field, fieldSpec.options)
		}
		fields = append(fields, field)
	}
	return messageFromFile(t, &descriptorpb.FileDescriptorProto{
		Name:    new("refined.proto"),
		Package: new("shape.v1"),
		Syntax:  new(spec.syntax),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name:          new("ScalarRow"),
			ReservedName:  spec.reservedNames,
			ReservedRange: reservedRanges(spec.reservedNumbers...),
			Field:         fields,
		}},
	}, "ScalarRow")
}

func reservedRanges(numbers ...int32) []*descriptorpb.DescriptorProto_ReservedRange {
	ranges := make([]*descriptorpb.DescriptorProto_ReservedRange, 0, len(numbers))
	for _, number := range numbers {
		ranges = append(ranges, &descriptorpb.DescriptorProto_ReservedRange{
			Start: new(number),
			End:   new(number + 1),
		})
	}
	return ranges
}

func refinedMapMessage(t *testing.T, options *datav1.FieldOptions) protoreflect.MessageDescriptor {
	t.Helper()
	fieldOptions := &descriptorpb.FieldOptions{}
	proto.SetExtension(fieldOptions, datav1.E_Field, options)
	return messageFromFile(t, &descriptorpb.FileDescriptorProto{
		Name:    new("refined_map.proto"),
		Package: new("shape.v1"),
		Syntax:  new("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: new("MapRow"),
			NestedType: []*descriptorpb.DescriptorProto{{
				Name:    new("ValuesEntry"),
				Options: &descriptorpb.MessageOptions{MapEntry: new(true)},
				Field: []*descriptorpb.FieldDescriptorProto{
					{
						Name: new("key"), Number: proto.Int32(1),
						Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:  descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
					},
					{
						Name: new("value"), Number: proto.Int32(2),
						Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:  descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
					},
				},
			}},
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name: new("values"), Number: proto.Int32(1),
				Label:    descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(),
				Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
				TypeName: new(".shape.v1.MapRow.ValuesEntry"), Options: fieldOptions,
			}},
		}},
	}, "MapRow")
}

func decimalOptions(precision, scale uint32) *datav1.FieldOptions {
	return &datav1.FieldOptions{SemanticType: &datav1.FieldOptions_Decimal{
		Decimal: &datav1.DecimalOptions{Precision: precision, Scale: scale},
	}}
}

func uuidOptions() *datav1.FieldOptions {
	return &datav1.FieldOptions{SemanticType: &datav1.FieldOptions_Uuid{Uuid: &datav1.UuidOptions{}}}
}

func fixedBytesOptions(byteLength uint32) *datav1.FieldOptions {
	return &datav1.FieldOptions{SemanticType: &datav1.FieldOptions_FixedBytes{
		FixedBytes: &datav1.FixedBytesOptions{ByteLength: byteLength},
	}}
}

func fixedListOptions(length uint32) *datav1.FieldOptions {
	return &datav1.FieldOptions{FixedList: &datav1.FixedListOptions{Length: length}}
}

func rowFile(path, packageName string) *descriptorpb.FileDescriptorProto {
	return &descriptorpb.FileDescriptorProto{
		Name:    new(path),
		Package: new(packageName),
		Syntax:  new("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: new("Row"),
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name:   new("value"),
				Number: proto.Int32(1),
				Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
			}},
		}},
	}
}

func descriptorSetBytes(t *testing.T, files ...*descriptorpb.FileDescriptorProto) []byte {
	t.Helper()
	descriptor, err := proto.Marshal(&descriptorpb.FileDescriptorSet{File: files})
	require.NoError(t, err)
	return descriptor
}

func messageFromFile(
	t *testing.T,
	file *descriptorpb.FileDescriptorProto,
	messageName protoreflect.Name,
) protoreflect.MessageDescriptor {
	t.Helper()
	descriptor, err := protodesc.NewFile(file, protoregistry.GlobalFiles)
	require.NoError(t, err)
	message := descriptor.Messages().ByName(messageName)
	require.NotNil(t, message)
	return message
}

func fixtureDescriptors(t *testing.T) ([]byte, *protoregistry.Files) {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	path := filepath.Join(filepath.Dir(thisFile), "..", "..", "python", "tests", "proto", "descriptor.binpb")
	descriptor, err := os.ReadFile(path)
	require.NoError(t, err)
	var fds descriptorpb.FileDescriptorSet
	require.NoError(t, proto.Unmarshal(descriptor, &fds))
	files, err := protodesc.NewFiles(&fds)
	require.NoError(t, err)
	return descriptor, files
}

func findMessage(t *testing.T, files *protoregistry.Files, name protoreflect.FullName) protoreflect.MessageDescriptor {
	t.Helper()
	descriptor, err := files.FindDescriptorByName(name)
	require.NoError(t, err)
	message, ok := descriptor.(protoreflect.MessageDescriptor)
	require.True(t, ok)
	return message
}

func schemaField(t *testing.T, schema *datav1.DatasetSchema, number protoreflect.FieldNumber) *datav1.Field {
	t.Helper()
	for _, field := range schema.GetFields() {
		path := field.GetProtoNumberPath()
		if len(path) == 1 && path[0] == uint32(number) {
			return field
		}
	}
	require.FailNow(t, "schema field not found", "number %d", number)
	return nil
}

func collectStableIDs(fields []*datav1.Field) map[int32]struct{} {
	ids := make(map[int32]struct{})
	var collect func([]*datav1.Field)
	collect = func(current []*datav1.Field) {
		for _, field := range current {
			ids[field.GetStableId()] = struct{}{}
			typ := field.GetType()
			switch {
			case typ.GetStruct() != nil:
				collect(typ.GetStruct().GetFields())
			case typ.GetList() != nil:
				collect([]*datav1.Field{typ.GetList().GetElement()})
			case typ.GetMap() != nil:
				collect([]*datav1.Field{typ.GetMap().GetKey(), typ.GetMap().GetValue()})
			}
		}
	}
	collect(fields)
	return ids
}

func countFields(fields []*datav1.Field) int {
	count := 0
	var visit func([]*datav1.Field)
	visit = func(current []*datav1.Field) {
		for _, field := range current {
			count++
			typ := field.GetType()
			switch {
			case typ.GetStruct() != nil:
				visit(typ.GetStruct().GetFields())
			case typ.GetList() != nil:
				visit([]*datav1.Field{typ.GetList().GetElement()})
			case typ.GetMap() != nil:
				visit([]*datav1.Field{typ.GetMap().GetKey(), typ.GetMap().GetValue()})
			}
		}
	}
	visit(fields)
	return count
}

func TestStorageName(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"data.v1.CanonicalRecord": "data_v1_canonical_record",
		"HTTPRecord":              "http_record",
		"already_snake":           "already_snake",
	}
	names := make([]string, 0, len(cases))
	for name := range cases {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		assert.Equal(t, cases[name], storageName(name))
	}
	assert.NotContains(t, storageName("data.v1.Row"), ".")
}
