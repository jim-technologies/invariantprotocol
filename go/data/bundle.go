package data

import (
	"errors"
	"fmt"

	datav1 "github.com/jim-technologies/invariantprotocol/go/gen/invariant/data/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// ParseSchemaBundle decodes a bundle and rejects IR or mapping rules this
// package cannot interpret.
func ParseSchemaBundle(encoded []byte) (*datav1.SchemaBundle, error) {
	bundle := new(datav1.SchemaBundle)
	if err := proto.Unmarshal(encoded, bundle); err != nil {
		return nil, fmt.Errorf("decode SchemaBundle: %w", err)
	}
	migrated, err := MigrateSchemaBundle(bundle)
	if err != nil {
		return nil, err
	}
	return migrated, nil
}

// MigrateSchemaBundle upgrades a supported historical mapping in memory. The
// IR-v3/mapping-v2 to IR-v4/mapping-v3 migration preserves every stable
// identity and tombstone. The legacy mapping cannot contain fixed-cardinality
// lists, so ordinary lists retain fixed_length=0.
func MigrateSchemaBundle(bundle *datav1.SchemaBundle) (*datav1.SchemaBundle, error) {
	if bundle == nil {
		return nil, errors.New("SchemaBundle is nil")
	}
	switch {
	case bundle.GetIrVersion() == IRVersion && bundle.GetMappingVersion() == MappingVersion:
		return bundle, nil
	case bundle.GetIrVersion() == 3 && bundle.GetMappingVersion() == 2:
		if err := rejectUnknownFields(bundle.ProtoReflect(), "SchemaBundle", "unknown to this migrator"); err != nil {
			return nil, fmt.Errorf("migrate SchemaBundle: %w", err)
		}
		for _, dataset := range bundle.GetDatasets() {
			if dataset == nil {
				continue
			}
			for _, field := range dataset.GetFields() {
				if err := validateLegacyListShape(field, field.GetName()); err != nil {
					return nil, err
				}
			}
		}
		migrated := proto.Clone(bundle).(*datav1.SchemaBundle)
		migrated.IrVersion = IRVersion
		migrated.MappingVersion = MappingVersion
		return migrated, nil
	default:
		return nil, fmt.Errorf(
			"unsupported SchemaBundle version pair ir_version=%d mapping_version=%d; expected 3/2 or %d/%d",
			bundle.GetIrVersion(), bundle.GetMappingVersion(), IRVersion, MappingVersion,
		)
	}
}

// MarshalSchemaBundle validates and deterministically serializes a bundle.
func MarshalSchemaBundle(bundle *datav1.SchemaBundle) ([]byte, error) {
	if err := ValidateSchemaBundle(bundle); err != nil {
		return nil, err
	}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(bundle)
	if err != nil {
		return nil, fmt.Errorf("encode SchemaBundle: %w", err)
	}
	return encoded, nil
}

// ValidateSchemaBundle rejects IR or mapping rules this package cannot
// interpret. It validates the artifact version, not every compiler invariant.
func ValidateSchemaBundle(bundle *datav1.SchemaBundle) error {
	if bundle == nil {
		return errors.New("SchemaBundle is nil")
	}
	if bundle.GetIrVersion() != IRVersion {
		return fmt.Errorf("unsupported SchemaBundle ir_version %d; expected %d", bundle.GetIrVersion(), IRVersion)
	}
	if bundle.GetMappingVersion() != MappingVersion {
		return fmt.Errorf(
			"unsupported SchemaBundle mapping_version %d; expected %d",
			bundle.GetMappingVersion(), MappingVersion,
		)
	}
	return nil
}

func validateLegacyListShape(field *datav1.Field, path string) error {
	if field == nil || field.GetType() == nil {
		return nil
	}
	switch kind := field.GetType().GetKind().(type) {
	case *datav1.DataType_Struct:
		for _, child := range kind.Struct.GetFields() {
			if err := validateLegacyListShape(child, path+"."+child.GetName()); err != nil {
				return err
			}
		}
	case *datav1.DataType_List:
		if kind.List.GetFixedLength() != 0 {
			return fmt.Errorf(
				"SchemaBundle mapping_version 2 field %q contains fixed_length %d, which was introduced in mapping_version %d",
				path, kind.List.GetFixedLength(), MappingVersion,
			)
		}
		return validateLegacyListShape(kind.List.GetElement(), path+"[]")
	case *datav1.DataType_Map:
		if err := validateLegacyListShape(kind.Map.GetKey(), path+".key"); err != nil {
			return err
		}
		return validateLegacyListShape(kind.Map.GetValue(), path+".value")
	}
	return nil
}

func rejectUnknownFields(message protoreflect.Message, path, unsupported string) error {
	if len(message.GetUnknown()) != 0 {
		return fmt.Errorf("%s contains fields %s", path, unsupported)
	}
	var validationErr error
	message.Range(func(descriptor protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		if descriptor.IsMap() {
			if descriptor.MapValue().Kind() != protoreflect.MessageKind {
				return true
			}
			value.Map().Range(func(key protoreflect.MapKey, item protoreflect.Value) bool {
				validationErr = rejectUnknownFields(
					item.Message(),
					fmt.Sprintf("%s.%s[%v]", path, descriptor.Name(), key.Interface()),
					unsupported,
				)
				return validationErr == nil
			})
			return validationErr == nil
		}
		if descriptor.IsList() {
			if descriptor.Kind() != protoreflect.MessageKind {
				return true
			}
			list := value.List()
			for index := range list.Len() {
				validationErr = rejectUnknownFields(
					list.Get(index).Message(),
					fmt.Sprintf("%s.%s[%d]", path, descriptor.Name(), index),
					unsupported,
				)
				if validationErr != nil {
					return false
				}
			}
			return true
		}
		if descriptor.Kind() == protoreflect.MessageKind {
			validationErr = rejectUnknownFields(
				value.Message(),
				path+"."+string(descriptor.Name()),
				unsupported,
			)
		}
		return validationErr == nil
	})
	return validationErr
}

// FindDataset returns the dataset compiled from sourceMessage, if present.
func FindDataset(bundle *datav1.SchemaBundle, sourceMessage string) *datav1.DatasetSchema {
	if bundle == nil {
		return nil
	}
	for _, dataset := range bundle.GetDatasets() {
		if dataset != nil && dataset.GetSourceMessage() == sourceMessage {
			return dataset
		}
	}
	return nil
}
