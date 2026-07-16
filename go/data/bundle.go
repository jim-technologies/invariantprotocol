package data

import (
	"errors"
	"fmt"

	datav1 "github.com/jim-technologies/invariantprotocol/go/gen/invariant/data/v1"
	"google.golang.org/protobuf/proto"
)

// ParseSchemaBundle decodes a bundle and rejects IR or mapping rules this
// package cannot interpret.
func ParseSchemaBundle(encoded []byte) (*datav1.SchemaBundle, error) {
	bundle := new(datav1.SchemaBundle)
	if err := proto.Unmarshal(encoded, bundle); err != nil {
		return nil, fmt.Errorf("decode SchemaBundle: %w", err)
	}
	if err := ValidateSchemaBundle(bundle); err != nil {
		return nil, err
	}
	return bundle, nil
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
