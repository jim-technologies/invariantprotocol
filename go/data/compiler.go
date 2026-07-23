// Package data compiles protobuf message descriptors into Invariant's
// language-neutral logical data schema.
package data

import (
	"cmp"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"

	datav1 "github.com/jim-technologies/invariantprotocol/go/gen/invariant/data/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

const (
	// IRVersion is the current SchemaBundle protobuf model version.
	IRVersion uint32 = 3

	// MappingVersion is the current protobuf-to-logical-type mapping version.
	MappingVersion uint32 = 2

	maxStableID int32 = 2147483447

	maxFixedBytesLength uint32 = 1<<31 - 1
)

// CompileDescriptorBytes compiles protobuf messages from a serialized
// FileDescriptorSet. Explicit message names take precedence; when none are
// supplied, messages marked with the dataset option are discovered. Ordinary
// RPC request and response messages are never implicitly storage datasets.
//
// Pass the last committed bundle as previous when evolving a schema. Stable
// field identities are retained by protobuf number path, and removed
// identities are permanently retired.
func CompileDescriptorBytes(
	descriptor []byte,
	messageNames []string,
	previous *datav1.SchemaBundle,
) (*datav1.SchemaBundle, error) {
	var fds descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(descriptor, &fds); err != nil {
		return nil, fmt.Errorf("compile descriptor: unmarshal FileDescriptorSet: %w", err)
	}
	if err := validateAnnotationOptionNumbers(&fds); err != nil {
		return nil, fmt.Errorf("compile descriptor: %w", err)
	}
	files, err := protodesc.NewFiles(&fds)
	if err != nil {
		return nil, fmt.Errorf("compile descriptor: resolve FileDescriptorSet: %w", err)
	}

	selectedNames := messageNames
	if len(selectedNames) == 0 {
		selectedNames, err = annotatedDatasetNames(files)
		if err != nil {
			return nil, fmt.Errorf("compile descriptor: discover datasets: %w", err)
		}
	}

	names := make([]string, 0, len(selectedNames))
	seenNames := make(map[string]struct{}, len(selectedNames))
	for _, rawName := range selectedNames {
		name := strings.TrimPrefix(strings.TrimSpace(rawName), ".")
		if name == "" {
			return nil, errors.New("compile descriptor: dataset message name must not be empty")
		}
		if _, ok := seenNames[name]; ok {
			continue
		}
		seenNames[name] = struct{}{}
		names = append(names, name)
	}
	if len(names) == 0 {
		return nil, errors.New("compile descriptor: at least one dataset message must be selected or annotated")
	}
	slices.Sort(names)

	previousByMessage, err := indexPreviousBundle(previous)
	if err != nil {
		return nil, fmt.Errorf("compile descriptor: %w", err)
	}
	removedRoots := make([]string, 0)
	for previousName := range previousByMessage {
		if _, selected := seenNames[previousName]; !selected {
			removedRoots = append(removedRoots, previousName)
		}
	}
	if len(removedRoots) > 0 {
		slices.Sort(removedRoots)
		quoted := make([]string, len(removedRoots))
		for i, name := range removedRoots {
			quoted[i] = strconv.Quote(name)
		}
		return nil, fmt.Errorf(
			"compile descriptor: selected dataset roots omit previous datasets %s; dataset roots are append-only",
			strings.Join(quoted, ", "),
		)
	}

	datasets := make([]*datav1.DatasetSchema, 0, len(names))
	datasetStorageNames := make(map[string]string, len(names))
	for _, name := range names {
		descriptor, err := files.FindDescriptorByName(protoreflect.FullName(name))
		if err != nil {
			return nil, fmt.Errorf("compile descriptor: resolve dataset %q: %w", name, err)
		}
		md, ok := descriptor.(protoreflect.MessageDescriptor)
		if !ok {
			return nil, fmt.Errorf("compile descriptor: %q is not a protobuf message", name)
		}
		if md.IsMapEntry() {
			return nil, fmt.Errorf("compile descriptor: %q is a synthetic protobuf map entry", name)
		}
		dataset, err := CompileMessage(md, previousByMessage[name])
		if err != nil {
			return nil, fmt.Errorf("compile descriptor: dataset %q: %w", name, err)
		}
		if owner, duplicate := datasetStorageNames[dataset.GetName()]; duplicate {
			return nil, fmt.Errorf(
				"compile descriptor: dataset storage name %q collides for protobuf messages %q and %q",
				dataset.GetName(), owner, name,
			)
		}
		datasetStorageNames[dataset.GetName()] = name
		datasets = append(datasets, dataset)
	}

	digest := sha256.Sum256(descriptor)
	return &datav1.SchemaBundle{
		IrVersion:              IRVersion,
		MappingVersion:         MappingVersion,
		SourceDescriptorSha256: digest[:],
		Datasets:               datasets,
	}, nil
}

func validateAnnotationOptionNumbers(files *descriptorpb.FileDescriptorSet) error {
	const optionNumber int32 = 51974
	type declaration struct {
		name     string
		typeName string
	}
	allowed := map[string]declaration{
		".google.protobuf.MessageOptions": {
			name: "invariant.data.v1.dataset", typeName: ".invariant.data.v1.DatasetOptions",
		},
		".google.protobuf.FieldOptions": {
			name: "invariant.data.v1.field", typeName: ".invariant.data.v1.FieldOptions",
		},
	}

	var visitExtensions func(string, []*descriptorpb.DescriptorProto, []*descriptorpb.FieldDescriptorProto) error
	visitExtensions = func(scope string, messages []*descriptorpb.DescriptorProto, extensions []*descriptorpb.FieldDescriptorProto) error {
		for _, extension := range extensions {
			if extension == nil || extension.GetNumber() != optionNumber {
				continue
			}
			expected, protected := allowed[extension.GetExtendee()]
			if !protected {
				continue
			}
			name := strings.TrimPrefix(scope+"."+extension.GetName(), ".")
			if name != expected.name {
				return fmt.Errorf(
					"custom option number %d for %s is assigned to %q by Invariant but also declared by %q",
					optionNumber, strings.TrimPrefix(extension.GetExtendee(), "."), expected.name, name,
				)
			}
			if extension.GetType() != descriptorpb.FieldDescriptorProto_TYPE_MESSAGE ||
				extension.GetTypeName() != expected.typeName ||
				extension.GetLabel() != descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL {
				return fmt.Errorf("invariant custom option %q has an unexpected declaration", expected.name)
			}
		}
		for _, message := range messages {
			if message == nil {
				continue
			}
			messageScope := strings.TrimPrefix(scope+"."+message.GetName(), ".")
			if err := visitExtensions(messageScope, message.GetNestedType(), message.GetExtension()); err != nil {
				return err
			}
		}
		return nil
	}

	for _, file := range files.GetFile() {
		if file == nil {
			continue
		}
		if err := visitExtensions(file.GetPackage(), file.GetMessageType(), file.GetExtension()); err != nil {
			return err
		}
	}
	return nil
}

func annotatedDatasetNames(files *protoregistry.Files) ([]string, error) {
	var names []string
	var discoveryErr error

	var visitMessages func(protoreflect.MessageDescriptors)
	visitMessages = func(messages protoreflect.MessageDescriptors) {
		for i := range messages.Len() {
			md := messages.Get(i)
			annotated, err := hasDatasetOption(md)
			if err != nil {
				discoveryErr = fmt.Errorf("message %q: %w", md.FullName(), err)
				return
			}
			if annotated {
				names = append(names, string(md.FullName()))
			}
			visitMessages(md.Messages())
			if discoveryErr != nil {
				return
			}
		}
	}

	files.RangeFiles(func(file protoreflect.FileDescriptor) bool {
		visitMessages(file.Messages())
		return discoveryErr == nil
	})
	if discoveryErr != nil {
		return nil, discoveryErr
	}
	slices.Sort(names)
	return names, nil
}

func hasDatasetOption(md protoreflect.MessageDescriptor) (bool, error) {
	options, ok := md.Options().(*descriptorpb.MessageOptions)
	if !ok || options == nil || !proto.HasExtension(options, datav1.E_Dataset) {
		return false, nil
	}
	value := proto.GetExtension(options, datav1.E_Dataset)
	datasetOptions, ok := value.(*datav1.DatasetOptions)
	if !ok || datasetOptions == nil {
		return false, fmt.Errorf("dataset option has unexpected type %T", value)
	}
	if len(datasetOptions.ProtoReflect().GetUnknown()) != 0 {
		return false, errors.New("dataset option contains fields unsupported by this compiler")
	}
	return true, nil
}

func dataFieldOptions(fd protoreflect.FieldDescriptor) (*datav1.FieldOptions, error) {
	options, ok := fd.Options().(*descriptorpb.FieldOptions)
	if !ok || options == nil || !proto.HasExtension(options, datav1.E_Field) {
		return nil, nil
	}
	value := proto.GetExtension(options, datav1.E_Field)
	fieldOptions, ok := value.(*datav1.FieldOptions)
	if !ok || fieldOptions == nil {
		return nil, fmt.Errorf("field option has unexpected type %T", value)
	}
	if len(fieldOptions.ProtoReflect().GetUnknown()) != 0 {
		return nil, errors.New("field option contains fields unsupported by this compiler")
	}
	return fieldOptions, nil
}

// CompileMessage compiles one protobuf message descriptor into a logical
// dataset schema. The previous schema, when supplied, must be the prior state
// for the same fully-qualified protobuf message.
func CompileMessage(
	md protoreflect.MessageDescriptor,
	previous *datav1.DatasetSchema,
) (*datav1.DatasetSchema, error) {
	if md == nil {
		return nil, errors.New("compile message: descriptor is nil")
	}
	if md.IsMapEntry() {
		return nil, fmt.Errorf("compile message %q: synthetic map entries are not datasets", md.FullName())
	}
	datasetName := storageName(string(md.FullName()))
	if datasetName == "" {
		return nil, fmt.Errorf(
			"compile message %q: protobuf message name normalizes to an empty storage name",
			md.FullName(),
		)
	}
	if err := rejectExtensionRanges(md); err != nil {
		return nil, fmt.Errorf("compile message %q: %w", md.FullName(), err)
	}

	c, err := newDatasetCompiler(md, previous)
	if err != nil {
		return nil, fmt.Errorf("compile message %q: %w", md.FullName(), err)
	}

	fields := sortedFields(md.Fields())
	if previous == nil {
		// On the first compilation, top-level protobuf field numbers are the
		// storage identities. Reserve all of them before allocating any nested
		// or collection-child identities.
		for _, fd := range fields {
			id := int32(fd.Number())
			if err := validateStableID(id); err != nil {
				return nil, fmt.Errorf("compile message %q: field %q: %w", md.FullName(), fd.FullName(), err)
			}
			identity := appendIdentity("", fieldIdentitySegment(fd.Number()))
			c.firstTopLevelIDs[identity] = id
			c.reservedIDs[id] = identity
			if id > c.lastID {
				c.lastID = id
			}
		}
	}

	stack := map[protoreflect.FullName]bool{md.FullName(): true}
	compiled := make([]*datav1.Field, 0, len(fields))
	for _, fd := range fields {
		field, err := c.compileProtoField(fd, nil, "", stack)
		if err != nil {
			return nil, fmt.Errorf("compile message %q: %w", md.FullName(), err)
		}
		compiled = append(compiled, field)
	}
	if err := validateStorageNameCollisions(string(md.FullName()), compiled); err != nil {
		return nil, fmt.Errorf("compile message %q: %w", md.FullName(), err)
	}

	retired := make(map[string]*datav1.RetiredField, len(c.retired)+len(c.previousActive))
	for identity, field := range c.retired {
		retired[identity] = proto.Clone(field).(*datav1.RetiredField)
	}
	for identity := range c.previousActive {
		if _, active := c.currentActive[identity]; !active {
			previousField := c.previousFields[identity]
			retired[identity] = &datav1.RetiredField{
				Identity:          identity,
				StableId:          previousField.GetStableId(),
				ProtoFullName:     previousField.GetProtoFullName(),
				Name:              previousField.GetName(),
				StorageNameSource: previousField.GetStorageNameSource(),
			}
		}
	}
	retiredFields := make([]*datav1.RetiredField, 0, len(retired))
	for _, field := range retired {
		retiredFields = append(retiredFields, field)
	}
	slices.SortFunc(retiredFields, func(left, right *datav1.RetiredField) int {
		if byID := cmp.Compare(left.GetStableId(), right.GetStableId()); byID != 0 {
			return byID
		}
		return cmp.Compare(left.GetIdentity(), right.GetIdentity())
	})
	for _, field := range retiredFields {
		if err := c.validateRetiredReservation(field.GetIdentity(), field); err != nil {
			return nil, fmt.Errorf("compile message %q: %w", md.FullName(), err)
		}
	}

	return &datav1.DatasetSchema{
		SourceMessage: string(md.FullName()),
		Name:          datasetName,
		Description:   descriptorComment(md),
		Fields:        compiled,
		LastFieldId:   c.lastID,
		RetiredFields: retiredFields,
	}, nil
}

type datasetCompiler struct {
	previousActive    map[string]int32
	previousFields    map[string]*datav1.Field
	retired           map[string]*datav1.RetiredField
	currentActive     map[string]int32
	currentContainers map[string]protoreflect.MessageDescriptor
	historicalNames   map[string]map[string]string
	reservedIDs       map[int32]string
	firstTopLevelIDs  map[string]int32
	lastID            int32
}

func newDatasetCompiler(
	md protoreflect.MessageDescriptor,
	previous *datav1.DatasetSchema,
) (*datasetCompiler, error) {
	c := &datasetCompiler{
		previousActive:    make(map[string]int32),
		previousFields:    make(map[string]*datav1.Field),
		retired:           make(map[string]*datav1.RetiredField),
		currentActive:     make(map[string]int32),
		currentContainers: map[string]protoreflect.MessageDescriptor{"": md},
		historicalNames:   make(map[string]map[string]string),
		reservedIDs:       make(map[int32]string),
		firstTopLevelIDs:  make(map[string]int32),
	}
	if previous == nil {
		return c, nil
	}
	if previous.GetSourceMessage() != string(md.FullName()) {
		return nil, fmt.Errorf(
			"previous schema is for %q, not %q",
			previous.GetSourceMessage(),
			md.FullName(),
		)
	}
	canonicalDatasetName := storageName(previous.GetSourceMessage())
	if previous.GetName() != canonicalDatasetName {
		return nil, fmt.Errorf(
			"previous dataset storage name %q is not the canonical name %q derived from source_message %q; "+
				"generated SchemaBundle names are compiler-owned",
			previous.GetName(), canonicalDatasetName, previous.GetSourceMessage(),
		)
	}
	if previous.GetLastFieldId() < 0 || previous.GetLastFieldId() > maxStableID {
		return nil, fmt.Errorf("previous last_field_id %d is outside 0..%d", previous.GetLastFieldId(), maxStableID)
	}
	c.lastID = previous.GetLastFieldId()

	for _, retired := range previous.GetRetiredFields() {
		if retired == nil {
			return nil, errors.New("previous schema contains a nil retired field")
		}
		if retired.GetIdentity() == "" {
			return nil, errors.New("previous retired field has an empty identity")
		}
		if err := validateStableID(retired.GetStableId()); err != nil {
			return nil, fmt.Errorf("previous retired field %q: %w", retired.GetIdentity(), err)
		}
		if _, duplicate := c.retired[retired.GetIdentity()]; duplicate {
			return nil, fmt.Errorf("previous retired identity %q is duplicated", retired.GetIdentity())
		}
		if owner, duplicate := c.reservedIDs[retired.GetStableId()]; duplicate {
			return nil, fmt.Errorf(
				"previous stable_id %d is shared by %q and %q",
				retired.GetStableId(), owner, retired.GetIdentity(),
			)
		}
		if err := validateCompilerOwnedName(
			retired.GetIdentity(),
			retired.GetProtoFullName(),
			retired.GetName(),
			retired.GetStorageNameSource(),
		); err != nil {
			return nil, fmt.Errorf("previous retired field %q: %w", retired.GetIdentity(), err)
		}
		if err := c.registerHistoricalName(retired.GetIdentity(), retired.GetName()); err != nil {
			return nil, err
		}
		c.retired[retired.GetIdentity()] = proto.Clone(retired).(*datav1.RetiredField)
		c.reservedIDs[retired.GetStableId()] = retired.GetIdentity()
	}
	for _, field := range previous.GetFields() {
		if err := c.indexPreviousField(field, nil, "", datav1.SyntheticRole_SYNTHETIC_ROLE_PROTO_FIELD); err != nil {
			return nil, err
		}
	}

	var highest int32
	for id := range c.reservedIDs {
		if id > highest {
			highest = id
		}
	}
	if c.lastID != highest {
		return nil, fmt.Errorf(
			"previous last_field_id is %d, but highest allocated stable_id is %d",
			c.lastID, highest,
		)
	}
	return c, nil
}

func (c *datasetCompiler) indexPreviousField(
	field *datav1.Field,
	parentPath []uint32,
	parentIdentity string,
	expectedRole datav1.SyntheticRole,
) error {
	if field == nil {
		return errors.New("previous schema contains a nil field")
	}
	if field.GetSyntheticRole() != expectedRole {
		return fmt.Errorf(
			"previous field %q has role %s, expected %s",
			field.GetProtoFullName(), field.GetSyntheticRole(), expectedRole,
		)
	}

	path := field.GetProtoNumberPath()
	var identity string
	switch expectedRole {
	case datav1.SyntheticRole_SYNTHETIC_ROLE_PROTO_FIELD:
		if len(path) != len(parentPath)+1 || !equalNumberPath(path[:len(parentPath)], parentPath) {
			return fmt.Errorf("previous field %q has invalid protobuf number path %v", field.GetProtoFullName(), path)
		}
		identity = appendIdentity(parentIdentity, fieldIdentitySegment(protoreflect.FieldNumber(path[len(path)-1])))
	case datav1.SyntheticRole_SYNTHETIC_ROLE_LIST_ELEMENT:
		if !equalNumberPath(path, parentPath) {
			return fmt.Errorf("previous list element %q has invalid protobuf number path %v", field.GetProtoFullName(), path)
		}
		identity = appendIdentity(parentIdentity, "list:element")
	case datav1.SyntheticRole_SYNTHETIC_ROLE_MAP_KEY:
		if !equalNumberPath(path, parentPath) {
			return fmt.Errorf("previous map key %q has invalid protobuf number path %v", field.GetProtoFullName(), path)
		}
		identity = appendIdentity(parentIdentity, "map:key")
	case datav1.SyntheticRole_SYNTHETIC_ROLE_MAP_VALUE:
		if !equalNumberPath(path, parentPath) {
			return fmt.Errorf("previous map value %q has invalid protobuf number path %v", field.GetProtoFullName(), path)
		}
		identity = appendIdentity(parentIdentity, "map:value")
	default:
		return fmt.Errorf("previous field %q has an invalid synthetic role", field.GetProtoFullName())
	}

	if err := validateStableID(field.GetStableId()); err != nil {
		return fmt.Errorf("previous field %q: %w", field.GetProtoFullName(), err)
	}
	if _, retired := c.retired[identity]; retired {
		return fmt.Errorf("previous identity %q is both active and retired", identity)
	}
	if _, duplicate := c.previousActive[identity]; duplicate {
		return fmt.Errorf("previous active identity %q is duplicated", identity)
	}
	if owner, duplicate := c.reservedIDs[field.GetStableId()]; duplicate {
		return fmt.Errorf(
			"previous stable_id %d is shared by %q and %q",
			field.GetStableId(), owner, identity,
		)
	}
	if err := validateCompilerOwnedName(
		identity,
		field.GetProtoFullName(),
		field.GetName(),
		field.GetStorageNameSource(),
	); err != nil {
		return fmt.Errorf("previous field %q: %w", field.GetProtoFullName(), err)
	}
	if err := c.registerHistoricalName(identity, field.GetName()); err != nil {
		return err
	}
	c.previousActive[identity] = field.GetStableId()
	c.previousFields[identity] = field
	c.reservedIDs[field.GetStableId()] = identity

	switch typ := field.GetType(); {
	case typ == nil:
		return fmt.Errorf("previous field %q has no logical type", field.GetProtoFullName())
	case typ.GetStruct() != nil:
		for _, child := range typ.GetStruct().GetFields() {
			if err := c.indexPreviousField(child, path, identity, datav1.SyntheticRole_SYNTHETIC_ROLE_PROTO_FIELD); err != nil {
				return err
			}
		}
	case typ.GetList() != nil:
		if typ.GetList().GetElement() == nil {
			return fmt.Errorf("previous list field %q has no element", field.GetProtoFullName())
		}
		if err := c.indexPreviousField(
			typ.GetList().GetElement(), path, identity,
			datav1.SyntheticRole_SYNTHETIC_ROLE_LIST_ELEMENT,
		); err != nil {
			return err
		}
	case typ.GetMap() != nil:
		if typ.GetMap().GetKey() == nil || typ.GetMap().GetValue() == nil {
			return fmt.Errorf("previous map field %q must have a key and value", field.GetProtoFullName())
		}
		if err := c.indexPreviousField(
			typ.GetMap().GetKey(), path, identity,
			datav1.SyntheticRole_SYNTHETIC_ROLE_MAP_KEY,
		); err != nil {
			return err
		}
		if err := c.indexPreviousField(
			typ.GetMap().GetValue(), path, identity,
			datav1.SyntheticRole_SYNTHETIC_ROLE_MAP_VALUE,
		); err != nil {
			return err
		}
	}
	return nil
}

func (c *datasetCompiler) compileProtoField(
	fd protoreflect.FieldDescriptor,
	parentPath []uint32,
	parentIdentity string,
	stack map[protoreflect.FullName]bool,
) (*datav1.Field, error) {
	path := appendNumber(parentPath, uint32(fd.Number()))
	identity := appendIdentity(parentIdentity, fieldIdentitySegment(fd.Number()))
	id, err := c.allocate(identity)
	if err != nil {
		return nil, fmt.Errorf("field %q: %w", fd.FullName(), err)
	}
	fieldName, storageNameSource, err := c.storageFieldName(identity, parentIdentity, fd)
	if err != nil {
		return nil, fmt.Errorf("field %q: %w", fd.FullName(), err)
	}

	presence, nullable, oneof := fieldPresence(fd)
	field := &datav1.Field{
		ProtoFullName:     string(fd.FullName()),
		ProtoNumberPath:   path,
		Name:              fieldName,
		StableId:          id,
		Presence:          presence,
		Nullable:          nullable,
		Oneof:             oneof,
		Description:       descriptorComment(fd),
		SyntheticRole:     datav1.SyntheticRole_SYNTHETIC_ROLE_PROTO_FIELD,
		JsonName:          fd.JSONName(),
		StorageNameSource: storageNameSource,
	}
	if fd.HasDefault() {
		field.HasDefault = true
		field.ProtobufDefault = protodesc.ToFieldDescriptorProto(fd).GetDefaultValue()
	}

	options, err := dataFieldOptions(fd)
	if err != nil {
		return nil, fmt.Errorf("field %q: %w", fd.FullName(), err)
	}
	if options != nil && !fd.IsList() && !fd.IsMap() {
		if presence != datav1.Presence_PRESENCE_EXPLICIT && presence != datav1.Presence_PRESENCE_ONEOF {
			return nil, fmt.Errorf(
				"field %q: a refined singular field must have explicit or oneof presence, got %s",
				fd.FullName(), presence,
			)
		}
		if fd.HasDefault() {
			return nil, fmt.Errorf("field %q: a refined singular field must not declare a protobuf default", fd.FullName())
		}
	}

	switch {
	case fd.IsMap():
		if options != nil {
			return nil, fmt.Errorf("field %q: semantic type refinements are not supported on protobuf maps", fd.FullName())
		}
		field.Type, err = c.compileMapType(fd, path, identity, stack)
	case fd.IsList():
		field.Type, err = c.compileListType(fd, path, identity, stack, options)
	default:
		field.Type, err = c.compileRefinedValueType(fd, path, identity, stack, options)
	}
	if err != nil {
		return nil, fmt.Errorf("field %q: %w", fd.FullName(), err)
	}
	if err := c.validateEvolution(identity, field); err != nil {
		return nil, fmt.Errorf("field %q: %w", fd.FullName(), err)
	}
	return field, nil
}

func (c *datasetCompiler) compileSyntheticField(
	fd protoreflect.FieldDescriptor,
	name string,
	protoFullName string,
	path []uint32,
	identity string,
	role datav1.SyntheticRole,
	stack map[protoreflect.FullName]bool,
	options *datav1.FieldOptions,
) (*datav1.Field, error) {
	id, err := c.allocate(identity)
	if err != nil {
		return nil, err
	}
	typ, err := c.compileRefinedValueType(fd, path, identity, stack, options)
	if err != nil {
		return nil, err
	}
	field := &datav1.Field{
		ProtoFullName:   protoFullName,
		ProtoNumberPath: append([]uint32(nil), path...),
		Name:            name,
		StableId:        id,
		Presence:        datav1.Presence_PRESENCE_NOT_APPLICABLE,
		Nullable:        false,
		Type:            typ,
		SyntheticRole:   role,
	}
	if err := c.validateEvolution(identity, field); err != nil {
		return nil, err
	}
	return field, nil
}

func (c *datasetCompiler) compileListType(
	fd protoreflect.FieldDescriptor,
	path []uint32,
	identity string,
	stack map[protoreflect.FullName]bool,
	options *datav1.FieldOptions,
) (*datav1.DataType, error) {
	elementIdentity := appendIdentity(identity, "list:element")
	element, err := c.compileSyntheticField(
		fd,
		"element",
		string(fd.FullName())+"[]",
		path,
		elementIdentity,
		datav1.SyntheticRole_SYNTHETIC_ROLE_LIST_ELEMENT,
		stack,
		options,
	)
	if err != nil {
		return nil, fmt.Errorf("list element: %w", err)
	}
	return &datav1.DataType{
		Kind: &datav1.DataType_List{List: &datav1.ListType{Element: element}},
	}, nil
}

func (c *datasetCompiler) compileMapType(
	fd protoreflect.FieldDescriptor,
	path []uint32,
	identity string,
	stack map[protoreflect.FullName]bool,
) (*datav1.DataType, error) {
	key, err := c.compileSyntheticField(
		fd.MapKey(),
		"key",
		string(fd.FullName())+".key",
		path,
		appendIdentity(identity, "map:key"),
		datav1.SyntheticRole_SYNTHETIC_ROLE_MAP_KEY,
		stack,
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("map key: %w", err)
	}
	value, err := c.compileSyntheticField(
		fd.MapValue(),
		"value",
		string(fd.FullName())+".value",
		path,
		appendIdentity(identity, "map:value"),
		datav1.SyntheticRole_SYNTHETIC_ROLE_MAP_VALUE,
		stack,
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("map value: %w", err)
	}
	return &datav1.DataType{
		Kind: &datav1.DataType_Map{Map: &datav1.MapType{Key: key, Value: value}},
	}, nil
}

func (c *datasetCompiler) compileRefinedValueType(
	fd protoreflect.FieldDescriptor,
	path []uint32,
	identity string,
	stack map[protoreflect.FullName]bool,
	options *datav1.FieldOptions,
) (*datav1.DataType, error) {
	if options == nil {
		return c.compileValueType(fd, path, identity, stack)
	}

	switch semanticType := options.GetSemanticType().(type) {
	case *datav1.FieldOptions_Decimal:
		if fd.Kind() != protoreflect.StringKind {
			return nil, fmt.Errorf("decimal refinement requires a protobuf string carrier, got %s", fd.Kind())
		}
		decimal := semanticType.Decimal
		if decimal == nil {
			return nil, errors.New("decimal refinement is missing its value")
		}
		if decimal.GetPrecision() < 1 || decimal.GetPrecision() > 38 {
			return nil, fmt.Errorf("decimal precision must be between 1 and 38, got %d", decimal.GetPrecision())
		}
		if decimal.GetScale() > decimal.GetPrecision() {
			return nil, fmt.Errorf(
				"decimal scale %d exceeds precision %d",
				decimal.GetScale(), decimal.GetPrecision(),
			)
		}
		return &datav1.DataType{
			Kind: &datav1.DataType_Decimal{Decimal: &datav1.DecimalType{
				Precision: decimal.GetPrecision(),
				Scale:     decimal.GetScale(),
			}},
		}, nil

	case *datav1.FieldOptions_Uuid:
		if fd.Kind() != protoreflect.StringKind {
			return nil, fmt.Errorf("uuid refinement requires a protobuf string carrier, got %s", fd.Kind())
		}
		if semanticType.Uuid == nil {
			return nil, errors.New("uuid refinement is missing its value")
		}
		return &datav1.DataType{
			Kind: &datav1.DataType_Uuid{Uuid: &datav1.UuidType{}},
		}, nil

	case *datav1.FieldOptions_FixedBytes:
		if fd.Kind() != protoreflect.BytesKind {
			return nil, fmt.Errorf("fixed_bytes refinement requires a protobuf bytes carrier, got %s", fd.Kind())
		}
		fixedBytes := semanticType.FixedBytes
		if fixedBytes == nil {
			return nil, errors.New("fixed_bytes refinement is missing its value")
		}
		if fixedBytes.GetByteLength() == 0 {
			return nil, errors.New("fixed_bytes byte_length must be greater than zero")
		}
		if fixedBytes.GetByteLength() > maxFixedBytesLength {
			return nil, fmt.Errorf(
				"fixed_bytes byte_length must not exceed %d, got %d",
				maxFixedBytesLength, fixedBytes.GetByteLength(),
			)
		}
		return &datav1.DataType{
			Kind: &datav1.DataType_FixedBytes{
				FixedBytes: &datav1.FixedBytesType{ByteLength: fixedBytes.GetByteLength()},
			},
		}, nil

	case nil:
		return nil, errors.New("field option must select exactly one semantic type")

	default:
		return nil, errors.New("field option contains an unsupported semantic type")
	}
}

func (c *datasetCompiler) storageFieldName(
	identity string,
	parentIdentity string,
	fd protoreflect.FieldDescriptor,
) (string, string, error) {
	currentSource := string(fd.Name())
	storageSource := currentSource
	previous, ok := c.previousFields[identity]
	if ok {
		previousSource, err := exactProtoFieldName(previous.GetProtoFullName())
		if err != nil {
			return "", "", fmt.Errorf("previous proto_full_name: %w", err)
		}
		expectedPreviousFullName := fd.ContainingMessage().FullName().Append(previousSource)
		if previous.GetProtoFullName() != string(expectedPreviousFullName) {
			return "", "", fmt.Errorf(
				"previous proto_full_name %q is not a field of current containing message %q",
				previous.GetProtoFullName(), fd.ContainingMessage().FullName(),
			)
		}
		if previousSource != fd.Name() && !fd.ContainingMessage().ReservedNames().Has(previousSource) {
			return "", "", fmt.Errorf(
				"same-number rename from %q to %q must reserve the exact previous protobuf field name %q",
				previousSource, fd.Name(), previousSource,
			)
		}

		storageSource = previous.GetStorageNameSource()
		sourceName := protoreflect.Name(storageSource)
		if sourceName != fd.Name() && !fd.ContainingMessage().ReservedNames().Has(sourceName) {
			return "", "", fmt.Errorf(
				"original storage-name source %q must remain an exact reserved protobuf field name",
				storageSource,
			)
		}
	}

	name := storageName(storageSource)
	if owner := c.historicalNameOwner(parentIdentity, name); owner != "" && owner != identity {
		return "", "", fmt.Errorf(
			"storage name %q in this logical scope is permanently owned by compiler identity %q, not new identity %q",
			name, owner, identity,
		)
	}
	return name, storageSource, nil
}

func validateCompilerOwnedName(identity, protoFullName, name, storageSource string) error {
	role, err := compilerIdentityRole(identity)
	if err != nil {
		return err
	}
	if protoFullName == "" {
		return errors.New("proto_full_name must not be empty")
	}
	if name == "" {
		return errors.New("storage name must not be empty")
	}

	switch role {
	case datav1.SyntheticRole_SYNTHETIC_ROLE_PROTO_FIELD:
		if _, err := exactProtoFieldName(protoFullName); err != nil {
			return err
		}
		sourceName := protoreflect.Name(storageSource)
		if storageSource == "" || !sourceName.IsValid() {
			return fmt.Errorf("storage_name_source %q is not an exact protobuf field name", storageSource)
		}
		if expected := storageName(storageSource); name != expected {
			return fmt.Errorf(
				"storage name %q is not the canonical name %q derived from storage_name_source %q; generated SchemaBundle names are compiler-owned",
				name, expected, storageSource,
			)
		}
	case datav1.SyntheticRole_SYNTHETIC_ROLE_LIST_ELEMENT:
		if name != "element" || storageSource != "" {
			return errors.New("synthetic list element must use storage name \"element\" and an empty storage_name_source")
		}
	case datav1.SyntheticRole_SYNTHETIC_ROLE_MAP_KEY:
		if name != "key" || storageSource != "" {
			return errors.New("synthetic map key must use storage name \"key\" and an empty storage_name_source")
		}
	case datav1.SyntheticRole_SYNTHETIC_ROLE_MAP_VALUE:
		if name != "value" || storageSource != "" {
			return errors.New("synthetic map value must use storage name \"value\" and an empty storage_name_source")
		}
	default:
		return fmt.Errorf("compiler identity %q has an invalid field role", identity)
	}
	return nil
}

func exactProtoFieldName(fullName string) (protoreflect.Name, error) {
	name := protoreflect.FullName(fullName)
	if !name.IsValid() || !name.Name().IsValid() {
		return "", fmt.Errorf("proto_full_name %q is not a valid fully-qualified protobuf field name", fullName)
	}
	return name.Name(), nil
}

func compilerIdentityRole(identity string) (datav1.SyntheticRole, error) {
	var previous, role datav1.SyntheticRole
	for index, segment := range strings.Split(identity, "/") {
		switch segment {
		case "list:element":
			role = datav1.SyntheticRole_SYNTHETIC_ROLE_LIST_ELEMENT
		case "map:key":
			role = datav1.SyntheticRole_SYNTHETIC_ROLE_MAP_KEY
		case "map:value":
			role = datav1.SyntheticRole_SYNTHETIC_ROLE_MAP_VALUE
		default:
			number, err := strconv.ParseInt(strings.TrimPrefix(segment, "field:"), 10, 32)
			if !strings.HasPrefix(segment, "field:") || err != nil || number < 1 || number > 1<<29-1 ||
				fieldIdentitySegment(protoreflect.FieldNumber(number)) != segment {
				return datav1.SyntheticRole_SYNTHETIC_ROLE_UNSPECIFIED, fmt.Errorf(
					"compiler identity %q has invalid segment %q",
					identity, segment,
				)
			}
			role = datav1.SyntheticRole_SYNTHETIC_ROLE_PROTO_FIELD
		}

		if index == 0 && role != datav1.SyntheticRole_SYNTHETIC_ROLE_PROTO_FIELD {
			return datav1.SyntheticRole_SYNTHETIC_ROLE_UNSPECIFIED, fmt.Errorf(
				"compiler identity %q must begin with a protobuf field segment",
				identity,
			)
		}
		if role != datav1.SyntheticRole_SYNTHETIC_ROLE_PROTO_FIELD &&
			previous != datav1.SyntheticRole_SYNTHETIC_ROLE_PROTO_FIELD {
			return datav1.SyntheticRole_SYNTHETIC_ROLE_UNSPECIFIED, fmt.Errorf(
				"compiler identity %q has a synthetic segment outside a protobuf collection field",
				identity,
			)
		}
		if previous == datav1.SyntheticRole_SYNTHETIC_ROLE_MAP_KEY {
			return datav1.SyntheticRole_SYNTHETIC_ROLE_UNSPECIFIED, fmt.Errorf(
				"compiler identity %q descends through a protobuf map key",
				identity,
			)
		}
		previous = role
	}
	return role, nil
}

func (c *datasetCompiler) registerHistoricalName(identity, name string) error {
	scope := parentCompilerIdentity(identity)
	owners := c.historicalNames[scope]
	if owners == nil {
		owners = make(map[string]string)
		c.historicalNames[scope] = owners
	}
	if owner, exists := owners[name]; exists && owner != identity {
		return fmt.Errorf(
			"previous storage name %q in logical scope %q is shared by compiler identities %q and %q",
			name, scope, owner, identity,
		)
	}
	owners[name] = identity
	return nil
}

func (c *datasetCompiler) historicalNameOwner(scope, name string) string {
	return c.historicalNames[scope][name]
}

func (c *datasetCompiler) validateRetiredReservation(
	identity string,
	field *datav1.RetiredField,
) error {
	role, err := compilerIdentityRole(identity)
	if err != nil {
		return fmt.Errorf("retired field %q: %w", identity, err)
	}
	if role != datav1.SyntheticRole_SYNTHETIC_ROLE_PROTO_FIELD {
		return nil
	}

	container, reachable := c.currentContainers[parentCompilerIdentity(identity)]
	if !reachable {
		return nil
	}
	previousName, err := exactProtoFieldName(field.GetProtoFullName())
	if err != nil {
		return fmt.Errorf("retired field %q: %w", identity, err)
	}
	if expected := container.FullName().Append(previousName); field.GetProtoFullName() != string(expected) {
		return fmt.Errorf(
			"retired field %q has proto_full_name %q outside current containing message %q",
			identity, field.GetProtoFullName(), container.FullName(),
		)
	}

	segment := identity[strings.LastIndexByte(identity, '/')+1:]
	number, _ := strconv.ParseInt(strings.TrimPrefix(segment, "field:"), 10, 32)
	fieldNumber := protoreflect.FieldNumber(number)
	if !container.ReservedRanges().Has(fieldNumber) {
		return fmt.Errorf(
			"removed protobuf field %q number %d must remain reserved while containing message %q remains reachable",
			field.GetProtoFullName(), fieldNumber, container.FullName(),
		)
	}
	if !container.ReservedNames().Has(previousName) {
		return fmt.Errorf(
			"removed protobuf field %q exact name %q must remain reserved while containing message %q remains reachable",
			field.GetProtoFullName(), previousName, container.FullName(),
		)
	}
	storageSource := protoreflect.Name(field.GetStorageNameSource())
	if storageSource != previousName && !container.ReservedNames().Has(storageSource) {
		return fmt.Errorf(
			"removed protobuf field %q original storage-name source %q must remain reserved while containing message %q remains reachable",
			field.GetProtoFullName(), storageSource, container.FullName(),
		)
	}
	return nil
}

func parentCompilerIdentity(identity string) string {
	if slash := strings.LastIndexByte(identity, '/'); slash >= 0 {
		return identity[:slash]
	}
	return ""
}

func (c *datasetCompiler) compileValueType(
	fd protoreflect.FieldDescriptor,
	path []uint32,
	identity string,
	stack map[protoreflect.FullName]bool,
) (*datav1.DataType, error) {
	if primitive, ok := primitiveKind(fd.Kind()); ok {
		return primitiveType(primitive), nil
	}

	switch fd.Kind() {
	case protoreflect.EnumKind:
		ed := fd.Enum()
		valueDescriptors := ed.Values()
		values := make([]*datav1.EnumValue, 0, valueDescriptors.Len())
		for i := range valueDescriptors.Len() {
			value := valueDescriptors.Get(i)
			values = append(values, &datav1.EnumValue{
				Name:        string(value.Name()),
				Number:      int32(value.Number()),
				Description: descriptorComment(value),
			})
		}
		fullName := string(ed.FullName())
		return &datav1.DataType{
			ProtobufType: fullName,
			Kind: &datav1.DataType_Enum{Enum: &datav1.EnumType{
				FullName: fullName,
				Values:   values,
				Closed:   ed.IsClosed(),
			}},
		}, nil

	case protoreflect.MessageKind, protoreflect.GroupKind:
		md := fd.Message()
		if md == nil {
			return nil, errors.New("message descriptor is missing")
		}
		if err := rejectExtensionRanges(md); err != nil {
			return nil, err
		}
		if typ, ok := wellKnownType(md.FullName()); ok {
			return typ, nil
		}
		c.currentContainers[identity] = md
		if stack[md.FullName()] {
			return nil, fmt.Errorf("recursive protobuf message %q is not a finite row schema", md.FullName())
		}
		stack[md.FullName()] = true
		defer delete(stack, md.FullName())

		fields := sortedFields(md.Fields())
		children := make([]*datav1.Field, 0, len(fields))
		for _, childDescriptor := range fields {
			child, err := c.compileProtoField(childDescriptor, path, identity, stack)
			if err != nil {
				return nil, err
			}
			children = append(children, child)
		}
		if err := validateStorageNameCollisions(string(md.FullName()), children); err != nil {
			return nil, err
		}
		return &datav1.DataType{
			ProtobufType: string(md.FullName()),
			Kind: &datav1.DataType_Struct{Struct: &datav1.StructType{
				Fields: children,
			}},
		}, nil

	default:
		return nil, fmt.Errorf("protobuf kind %s is unsupported", fd.Kind())
	}
}

func (c *datasetCompiler) allocate(identity string) (int32, error) {
	if _, duplicate := c.currentActive[identity]; duplicate {
		return 0, fmt.Errorf("compiler identity %q occurs more than once", identity)
	}
	if retired, exists := c.retired[identity]; exists {
		return 0, fmt.Errorf("compiler identity %q reuses retired stable_id %d", identity, retired.GetStableId())
	}
	if id, ok := c.previousActive[identity]; ok {
		c.currentActive[identity] = id
		return id, nil
	}
	if id, ok := c.firstTopLevelIDs[identity]; ok {
		c.currentActive[identity] = id
		return id, nil
	}
	if c.lastID == maxStableID {
		return 0, fmt.Errorf("cannot allocate stable_id beyond %d", maxStableID)
	}
	c.lastID++
	id := c.lastID
	if owner, reserved := c.reservedIDs[id]; reserved {
		return 0, fmt.Errorf("stable_id %d is already reserved by %q", id, owner)
	}
	c.reservedIDs[id] = identity
	c.currentActive[identity] = id
	return id, nil
}

func (c *datasetCompiler) validateEvolution(identity string, current *datav1.Field) error {
	previous, exists := c.previousFields[identity]
	if !exists {
		return nil
	}
	if previous.GetPresence() != current.GetPresence() || previous.GetNullable() != current.GetNullable() {
		return fmt.Errorf(
			"protobuf presence changed for stable_id %d; use a new protobuf field number",
			current.GetStableId(),
		)
	}
	if previous.GetOneof() != current.GetOneof() {
		return fmt.Errorf(
			"protobuf oneof changed for stable_id %d; use a new protobuf field number",
			current.GetStableId(),
		)
	}
	if previous.GetHasDefault() != current.GetHasDefault() ||
		previous.GetProtobufDefault() != current.GetProtobufDefault() {
		return fmt.Errorf(
			"protobuf default changed for stable_id %d; use a new protobuf field number",
			current.GetStableId(),
		)
	}
	if !sameLogicalShape(previous.GetType(), current.GetType()) {
		return fmt.Errorf(
			"protobuf logical shape changed for stable_id %d; use a new protobuf field number",
			current.GetStableId(),
		)
	}
	return nil
}

func sameLogicalShape(previous, current *datav1.DataType) bool {
	if previous == nil || current == nil {
		return previous == current
	}
	switch previousKind := previous.GetKind().(type) {
	case *datav1.DataType_Primitive:
		currentKind, ok := current.GetKind().(*datav1.DataType_Primitive)
		return ok &&
			previous.GetProtobufType() == current.GetProtobufType() &&
			previousKind.Primitive.GetKind() == currentKind.Primitive.GetKind()
	case *datav1.DataType_Enum:
		currentKind, ok := current.GetKind().(*datav1.DataType_Enum)
		return ok && compatibleEnumEvolution(previousKind.Enum, currentKind.Enum)
	case *datav1.DataType_Struct:
		_, ok := current.GetKind().(*datav1.DataType_Struct)
		return ok && previous.GetProtobufType() == current.GetProtobufType()
	case *datav1.DataType_List:
		_, ok := current.GetKind().(*datav1.DataType_List)
		return ok
	case *datav1.DataType_Map:
		_, ok := current.GetKind().(*datav1.DataType_Map)
		return ok
	case *datav1.DataType_Timestamp:
		currentKind, ok := current.GetKind().(*datav1.DataType_Timestamp)
		return ok &&
			previous.GetProtobufType() == current.GetProtobufType() &&
			previousKind.Timestamp.GetUnit() == currentKind.Timestamp.GetUnit() &&
			previousKind.Timestamp.GetTimezone() == currentKind.Timestamp.GetTimezone()
	case *datav1.DataType_Duration:
		currentKind, ok := current.GetKind().(*datav1.DataType_Duration)
		return ok &&
			previous.GetProtobufType() == current.GetProtobufType() &&
			previousKind.Duration.GetUnit() == currentKind.Duration.GetUnit()
	case *datav1.DataType_Json:
		currentKind, ok := current.GetKind().(*datav1.DataType_Json)
		return ok &&
			previous.GetProtobufType() == current.GetProtobufType() &&
			previousKind.Json.GetKind() == currentKind.Json.GetKind()
	case *datav1.DataType_Decimal:
		currentKind, ok := current.GetKind().(*datav1.DataType_Decimal)
		return ok && previousKind.Decimal != nil && currentKind.Decimal != nil &&
			previousKind.Decimal.GetPrecision() == currentKind.Decimal.GetPrecision() &&
			previousKind.Decimal.GetScale() == currentKind.Decimal.GetScale()
	case *datav1.DataType_Uuid:
		currentKind, ok := current.GetKind().(*datav1.DataType_Uuid)
		return ok && previousKind.Uuid != nil && currentKind.Uuid != nil
	case *datav1.DataType_FixedBytes:
		currentKind, ok := current.GetKind().(*datav1.DataType_FixedBytes)
		return ok && previousKind.FixedBytes != nil && currentKind.FixedBytes != nil &&
			previousKind.FixedBytes.GetByteLength() == currentKind.FixedBytes.GetByteLength()
	default:
		return false
	}
}

func compatibleEnumEvolution(previous, current *datav1.EnumType) bool {
	if previous == nil || current == nil {
		return previous == current
	}
	if previous.GetFullName() != current.GetFullName() || previous.GetClosed() != current.GetClosed() {
		return false
	}

	type enumValueIdentity struct {
		name   string
		number int32
	}
	currentValues := make(map[enumValueIdentity]struct{}, len(current.GetValues()))
	for _, value := range current.GetValues() {
		currentValues[enumValueIdentity{name: value.GetName(), number: value.GetNumber()}] = struct{}{}
	}
	for _, value := range previous.GetValues() {
		identity := enumValueIdentity{name: value.GetName(), number: value.GetNumber()}
		if _, retained := currentValues[identity]; !retained {
			return false
		}
	}
	return true
}

func indexPreviousBundle(previous *datav1.SchemaBundle) (map[string]*datav1.DatasetSchema, error) {
	indexed := make(map[string]*datav1.DatasetSchema)
	if previous == nil {
		return indexed, nil
	}
	if previous.GetIrVersion() != IRVersion {
		return nil, fmt.Errorf("previous ir_version is %d, want %d", previous.GetIrVersion(), IRVersion)
	}
	if previous.GetMappingVersion() != MappingVersion {
		return nil, fmt.Errorf(
			"previous mapping_version is %d, want %d",
			previous.GetMappingVersion(), MappingVersion,
		)
	}
	for _, dataset := range previous.GetDatasets() {
		if dataset == nil || dataset.GetSourceMessage() == "" {
			return nil, errors.New("previous bundle contains a dataset without source_message")
		}
		if _, duplicate := indexed[dataset.GetSourceMessage()]; duplicate {
			return nil, fmt.Errorf("previous dataset %q is duplicated", dataset.GetSourceMessage())
		}
		indexed[dataset.GetSourceMessage()] = dataset
	}
	return indexed, nil
}

func fieldPresence(fd protoreflect.FieldDescriptor) (datav1.Presence, bool, string) {
	if fd.IsMap() {
		return datav1.Presence_PRESENCE_MAP, false, ""
	}
	if fd.IsList() {
		return datav1.Presence_PRESENCE_REPEATED, false, ""
	}
	if fd.Cardinality() == protoreflect.Required {
		return datav1.Presence_PRESENCE_REQUIRED, false, ""
	}
	if oneof := fd.ContainingOneof(); oneof != nil && !oneof.IsSynthetic() {
		return datav1.Presence_PRESENCE_ONEOF, true, string(oneof.Name())
	}
	if fd.HasPresence() {
		return datav1.Presence_PRESENCE_EXPLICIT, true, ""
	}
	return datav1.Presence_PRESENCE_IMPLICIT, false, ""
}

func primitiveKind(kind protoreflect.Kind) (datav1.PrimitiveKind, bool) {
	switch kind {
	case protoreflect.DoubleKind:
		return datav1.PrimitiveKind_PRIMITIVE_KIND_DOUBLE, true
	case protoreflect.FloatKind:
		return datav1.PrimitiveKind_PRIMITIVE_KIND_FLOAT, true
	case protoreflect.Int64Kind:
		return datav1.PrimitiveKind_PRIMITIVE_KIND_INT64, true
	case protoreflect.Uint64Kind:
		return datav1.PrimitiveKind_PRIMITIVE_KIND_UINT64, true
	case protoreflect.Int32Kind:
		return datav1.PrimitiveKind_PRIMITIVE_KIND_INT32, true
	case protoreflect.Fixed64Kind:
		return datav1.PrimitiveKind_PRIMITIVE_KIND_FIXED64, true
	case protoreflect.Fixed32Kind:
		return datav1.PrimitiveKind_PRIMITIVE_KIND_FIXED32, true
	case protoreflect.BoolKind:
		return datav1.PrimitiveKind_PRIMITIVE_KIND_BOOL, true
	case protoreflect.StringKind:
		return datav1.PrimitiveKind_PRIMITIVE_KIND_STRING, true
	case protoreflect.BytesKind:
		return datav1.PrimitiveKind_PRIMITIVE_KIND_BYTES, true
	case protoreflect.Uint32Kind:
		return datav1.PrimitiveKind_PRIMITIVE_KIND_UINT32, true
	case protoreflect.Sfixed32Kind:
		return datav1.PrimitiveKind_PRIMITIVE_KIND_SFIXED32, true
	case protoreflect.Sfixed64Kind:
		return datav1.PrimitiveKind_PRIMITIVE_KIND_SFIXED64, true
	case protoreflect.Sint32Kind:
		return datav1.PrimitiveKind_PRIMITIVE_KIND_SINT32, true
	case protoreflect.Sint64Kind:
		return datav1.PrimitiveKind_PRIMITIVE_KIND_SINT64, true
	default:
		return datav1.PrimitiveKind_PRIMITIVE_KIND_UNSPECIFIED, false
	}
}

func primitiveType(kind datav1.PrimitiveKind) *datav1.DataType {
	return &datav1.DataType{
		Kind: &datav1.DataType_Primitive{Primitive: &datav1.PrimitiveType{Kind: kind}},
	}
}

func wellKnownType(name protoreflect.FullName) (*datav1.DataType, bool) {
	switch name {
	case "google.protobuf.DoubleValue":
		return wrapperType(name, datav1.PrimitiveKind_PRIMITIVE_KIND_DOUBLE), true
	case "google.protobuf.FloatValue":
		return wrapperType(name, datav1.PrimitiveKind_PRIMITIVE_KIND_FLOAT), true
	case "google.protobuf.Int64Value":
		return wrapperType(name, datav1.PrimitiveKind_PRIMITIVE_KIND_INT64), true
	case "google.protobuf.UInt64Value":
		return wrapperType(name, datav1.PrimitiveKind_PRIMITIVE_KIND_UINT64), true
	case "google.protobuf.Int32Value":
		return wrapperType(name, datav1.PrimitiveKind_PRIMITIVE_KIND_INT32), true
	case "google.protobuf.UInt32Value":
		return wrapperType(name, datav1.PrimitiveKind_PRIMITIVE_KIND_UINT32), true
	case "google.protobuf.BoolValue":
		return wrapperType(name, datav1.PrimitiveKind_PRIMITIVE_KIND_BOOL), true
	case "google.protobuf.StringValue":
		return wrapperType(name, datav1.PrimitiveKind_PRIMITIVE_KIND_STRING), true
	case "google.protobuf.BytesValue":
		return wrapperType(name, datav1.PrimitiveKind_PRIMITIVE_KIND_BYTES), true
	case "google.protobuf.Timestamp":
		return &datav1.DataType{
			ProtobufType: string(name),
			Kind: &datav1.DataType_Timestamp{Timestamp: &datav1.TimestampType{
				Unit:     datav1.TimeUnit_TIME_UNIT_NANOSECOND,
				Timezone: "UTC",
			}},
		}, true
	case "google.protobuf.Duration":
		return &datav1.DataType{
			ProtobufType: string(name),
			Kind: &datav1.DataType_Duration{Duration: &datav1.DurationType{
				Unit: datav1.TimeUnit_TIME_UNIT_NANOSECOND,
			}},
		}, true
	case "google.protobuf.Any":
		return jsonType(name, datav1.JsonKind_JSON_KIND_ANY), true
	case "google.protobuf.Struct":
		return jsonType(name, datav1.JsonKind_JSON_KIND_STRUCT), true
	case "google.protobuf.Value":
		return jsonType(name, datav1.JsonKind_JSON_KIND_VALUE), true
	case "google.protobuf.ListValue":
		return jsonType(name, datav1.JsonKind_JSON_KIND_LIST_VALUE), true
	default:
		return nil, false
	}
}

func wrapperType(name protoreflect.FullName, kind datav1.PrimitiveKind) *datav1.DataType {
	typ := primitiveType(kind)
	typ.ProtobufType = string(name)
	return typ
}

func jsonType(name protoreflect.FullName, kind datav1.JsonKind) *datav1.DataType {
	return &datav1.DataType{
		ProtobufType: string(name),
		Kind: &datav1.DataType_Json{Json: &datav1.JsonType{
			Kind: kind,
		}},
	}
}

func sortedFields(fields protoreflect.FieldDescriptors) []protoreflect.FieldDescriptor {
	sorted := make([]protoreflect.FieldDescriptor, fields.Len())
	for i := range fields.Len() {
		sorted[i] = fields.Get(i)
	}
	slices.SortFunc(sorted, func(left, right protoreflect.FieldDescriptor) int {
		return cmp.Compare(left.Number(), right.Number())
	})
	return sorted
}

func descriptorComment(descriptor protoreflect.Descriptor) string {
	location := descriptor.ParentFile().SourceLocations().ByDescriptor(descriptor)
	if comment := strings.TrimSpace(location.LeadingComments); comment != "" {
		return comment
	}
	return strings.TrimSpace(location.TrailingComments)
}

func rejectExtensionRanges(md protoreflect.MessageDescriptor) error {
	if md.ExtensionRanges().Len() == 0 {
		return nil
	}
	return fmt.Errorf(
		"protobuf message %q declares extension ranges; proto2 extensions cannot be represented in a finite data schema",
		md.FullName(),
	)
}

func validateStorageNameCollisions(scope string, fields []*datav1.Field) error {
	owners := make(map[string]string, len(fields))
	for _, field := range fields {
		name := field.GetName()
		if name == "" {
			return fmt.Errorf(
				"protobuf field %q normalizes to an empty storage name within protobuf message %q",
				field.GetProtoFullName(), scope,
			)
		}
		if owner, duplicate := owners[name]; duplicate {
			return fmt.Errorf(
				"storage name %q collides within protobuf message %q for fields %q and %q",
				name, scope, owner, field.GetProtoFullName(),
			)
		}
		owners[name] = field.GetProtoFullName()
	}
	return nil
}

func storageName(name string) string {
	runes := []rune(name)
	var b strings.Builder
	for i, r := range runes {
		if r == '.' || r == '-' {
			if b.Len() > 0 && !strings.HasSuffix(b.String(), "_") {
				b.WriteByte('_')
			}
			continue
		}
		if unicode.IsUpper(r) {
			previousIsLowerOrDigit := i > 0 && (unicode.IsLower(runes[i-1]) || unicode.IsDigit(runes[i-1]))
			nextIsLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
			previousIsUpper := i > 0 && unicode.IsUpper(runes[i-1])
			if b.Len() > 0 && (previousIsLowerOrDigit || previousIsUpper && nextIsLower) && !strings.HasSuffix(b.String(), "_") {
				b.WriteByte('_')
			}
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return strings.Trim(b.String(), "_")
}

func validateStableID(id int32) error {
	if id < 1 || id > maxStableID {
		return fmt.Errorf("stable_id %d is outside 1..%d", id, maxStableID)
	}
	return nil
}

func fieldIdentitySegment(number protoreflect.FieldNumber) string {
	return "field:" + strconv.FormatInt(int64(number), 10)
}

func appendIdentity(parent, segment string) string {
	if parent == "" {
		return segment
	}
	return parent + "/" + segment
}

func appendNumber(path []uint32, number uint32) []uint32 {
	result := make([]uint32, len(path)+1)
	copy(result, path)
	result[len(path)] = number
	return result
}

func equalNumberPath(left, right []uint32) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
