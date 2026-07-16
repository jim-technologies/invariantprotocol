package data

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"runtime"
	"slices"
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
	_, err = CompileDescriptorBytes(descriptor, []string{"google.protobuf.Timestamp"}, nil)
	require.ErrorContains(t, err, "dependency namespace")
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
		rootFieldName:   "renamed_child",
		nestedFieldName: "renamed_value",
		includeAdded:    true,
	})
	second, err := CompileMessage(v2, first)
	require.NoError(t, err)
	assert.Equal(t, int32(1), schemaField(t, second, 1).GetStableId())
	assert.Equal(t, int32(3), schemaField(t, second, 1).GetType().GetStruct().GetFields()[0].GetStableId())
	assert.Equal(t, int32(4), schemaField(t, second, 100).GetStableId())
	assert.Equal(t, int32(4), second.GetLastFieldId())
	require.Len(t, second.GetRetiredFields(), 1)
	assert.Equal(t, int32(2), second.GetRetiredFields()[0].GetStableId())
	assert.Equal(t, "field:2", second.GetRetiredFields()[0].GetIdentity())

	secondAgain, err := CompileMessage(v2, first)
	require.NoError(t, err)
	assert.True(t, proto.Equal(second, secondAgain))

	v3 := evolutionMessage(t, evolutionVersion{
		rootFieldName:   "renamed_child",
		nestedFieldName: "renamed_value",
		includeRemoved:  true,
		includeAdded:    true,
	})
	_, err = CompileMessage(v3, second)
	require.Error(t, err)
	require.ErrorContains(t, err, "reuses retired stable_id 2")
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
	rootFieldName   string
	nestedFieldName string
	includeRemoved  bool
	includeAdded    bool
}

func evolutionMessage(t *testing.T, version evolutionVersion) protoreflect.MessageDescriptor {
	t.Helper()
	nested := &descriptorpb.DescriptorProto{
		Name: new("Nested"),
		Field: []*descriptorpb.FieldDescriptorProto{{
			Name:   new(version.nestedFieldName),
			Number: proto.Int32(1),
			Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
		}},
	}
	row := &descriptorpb.DescriptorProto{
		Name: new("Row"),
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
