// Package parquet projects Invariant's canonical protobuf data schema into an
// Apache Parquet schema through Arrow's official pqarrow bridge.
package parquet

import (
	"errors"
	"fmt"

	arrowlib "github.com/apache/arrow-go/v18/arrow"
	parquetlib "github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"
	parquetschema "github.com/apache/arrow-go/v18/parquet/schema"
	invariantarrow "github.com/jim-technologies/invariantprotocol/go/data/arrow"
	datav1 "github.com/jim-technologies/invariantprotocol/go/gen/invariant/data/v1"
)

// Schema maps a canonical dataset to an Apache Parquet schema. Nanosecond UTC
// timestamps remain nanosecond UTC timestamps. Duration has no Parquet logical
// type and is therefore represented as an exact signed int64 nanosecond count.
func Schema(dataset *datav1.DatasetSchema) (*parquetschema.Schema, []*datav1.MappingDiagnostic, error) {
	if dataset == nil {
		return nil, nil, errors.New("parquet: nil dataset schema")
	}

	arrowSchema, _, err := invariantarrow.Schema(dataset)
	if err != nil {
		return nil, nil, fmt.Errorf("parquet: build Arrow bridge schema: %w", err)
	}
	compatible, err := compatibleArrowSchema(arrowSchema, dataset)
	if err != nil {
		return nil, nil, err
	}

	properties := parquetlib.NewWriterProperties(
		parquetlib.WithRootName(dataset.GetName()),
		parquetlib.WithVersion(parquetlib.V2_LATEST),
	)
	mapped, err := pqarrow.ToParquet(compatible, properties, pqarrow.NewArrowWriterProperties())
	if err != nil {
		return nil, nil, fmt.Errorf("parquet: map dataset %q: %w", dataset.GetSourceMessage(), err)
	}
	return mapped, diagnostics(dataset), nil
}

func compatibleArrowSchema(schema *arrowlib.Schema, dataset *datav1.DatasetSchema) (*arrowlib.Schema, error) {
	if schema.NumFields() != len(dataset.GetFields()) {
		return nil, errors.New("parquet: Arrow bridge field count does not match dataset")
	}
	fields := make([]arrowlib.Field, schema.NumFields())
	for i, field := range schema.Fields() {
		mapped, err := compatibleArrowField(field, dataset.GetFields()[i], dataset.GetFields()[i].GetName())
		if err != nil {
			return nil, err
		}
		fields[i] = mapped
	}
	metadata := schema.Metadata()
	return arrowlib.NewSchema(fields, &metadata), nil
}

func compatibleArrowField(field arrowlib.Field, logical *datav1.Field, path string) (arrowlib.Field, error) {
	if logical == nil || logical.GetType() == nil {
		return arrowlib.Field{}, fmt.Errorf("parquet: field %q has no logical type", path)
	}

	switch kind := logical.GetType().GetKind().(type) {
	case *datav1.DataType_Duration:
		field.Type = arrowlib.PrimitiveTypes.Int64
	case *datav1.DataType_Struct:
		structType, ok := field.Type.(*arrowlib.StructType)
		if !ok || structType.NumFields() != len(kind.Struct.GetFields()) {
			return arrowlib.Field{}, fmt.Errorf("parquet: field %q has an invalid Arrow struct bridge", path)
		}
		children := make([]arrowlib.Field, structType.NumFields())
		for i, child := range structType.Fields() {
			mapped, err := compatibleArrowField(child, kind.Struct.GetFields()[i], joinPath(path, kind.Struct.GetFields()[i].GetName()))
			if err != nil {
				return arrowlib.Field{}, err
			}
			children[i] = mapped
		}
		field.Type = arrowlib.StructOf(children...)
	case *datav1.DataType_List:
		listType, ok := field.Type.(*arrowlib.ListType)
		if !ok {
			return arrowlib.Field{}, fmt.Errorf("parquet: field %q has an invalid Arrow list bridge", path)
		}
		element, err := compatibleArrowField(listType.ElemField(), kind.List.GetElement(), path+"[]")
		if err != nil {
			return arrowlib.Field{}, err
		}
		field.Type = arrowlib.ListOfField(element)
	case *datav1.DataType_Map:
		mapType, ok := field.Type.(*arrowlib.MapType)
		if !ok {
			return arrowlib.Field{}, fmt.Errorf("parquet: field %q has an invalid Arrow map bridge", path)
		}
		key, err := compatibleArrowField(mapType.KeyField(), kind.Map.GetKey(), path+".key")
		if err != nil {
			return arrowlib.Field{}, err
		}
		value, err := compatibleArrowField(mapType.ItemField(), kind.Map.GetValue(), path+".value")
		if err != nil {
			return arrowlib.Field{}, err
		}
		key.Nullable = false
		field.Type = arrowlib.MapOfFields(key, value)
	}
	return field, nil
}

func diagnostics(dataset *datav1.DatasetSchema) []*datav1.MappingDiagnostic {
	var mapped []*datav1.MappingDiagnostic
	for _, field := range dataset.GetFields() {
		mapped = append(mapped, fieldDiagnostics(field, field.GetName())...)
	}
	return mapped
}

func fieldDiagnostics(field *datav1.Field, path string) []*datav1.MappingDiagnostic {
	if field == nil || field.GetType() == nil {
		return nil
	}
	compatibility := datav1.MappingCompatibility_MAPPING_COMPATIBILITY_LOSSLESS
	message := "protobuf value domain maps losslessly to a Parquet logical or physical type"
	var children []*datav1.MappingDiagnostic

	switch kind := field.GetType().GetKind().(type) {
	case *datav1.DataType_Struct:
		message = "protobuf message maps to a Parquet group"
		for _, child := range kind.Struct.GetFields() {
			children = append(children, fieldDiagnostics(child, joinPath(path, child.GetName()))...)
		}
	case *datav1.DataType_List:
		message = "protobuf repeated field maps to Parquet's canonical LIST shape"
		children = append(children, fieldDiagnostics(kind.List.GetElement(), path+"[]")...)
	case *datav1.DataType_Map:
		message = "protobuf map maps to Parquet's canonical MAP shape"
		children = append(children, fieldDiagnostics(kind.Map.GetKey(), path+".key")...)
		children = append(children, fieldDiagnostics(kind.Map.GetValue(), path+".value")...)
	case *datav1.DataType_Timestamp:
		compatibility = datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_REDUCED
		message = "Parquet TIMESTAMP(NANOS, adjustedToUTC=true) preserves nanosecond precision but its int64 range is narrower than protobuf Timestamp"
	case *datav1.DataType_Duration:
		compatibility = datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_REDUCED
		message = "Parquet has no duration logical type; exact nanoseconds use INT64, whose range is narrower than protobuf Duration"
	case *datav1.DataType_Json:
		compatibility = datav1.MappingCompatibility_MAPPING_COMPATIBILITY_REPRESENTATION_CHANGED
		message = "dynamic protobuf JSON is encoded as RFC 8259 text with Parquet's JSON logical annotation"
	case *datav1.DataType_Enum:
		if kind.Enum.GetClosed() {
			compatibility = datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_WIDENED
			message = "closed protobuf enum numbers map to unconstrained Parquet INT32; the physical type admits undeclared values"
		} else {
			message = "open protobuf enum numbers map losslessly to Parquet INT32"
		}
	}
	if field.GetOneof() != "" {
		message += fmt.Sprintf("; Parquet does not enforce mutual exclusivity for oneof %q", field.GetOneof())
		if compatibility == datav1.MappingCompatibility_MAPPING_COMPATIBILITY_LOSSLESS {
			compatibility = datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_WIDENED
		}
	}

	return append([]*datav1.MappingDiagnostic{{
		FieldPath:     path,
		Compatibility: compatibility,
		Message:       message,
	}}, children...)
}

func joinPath(parent, child string) string {
	if parent == "" {
		return child
	}
	return parent + "." + child
}
