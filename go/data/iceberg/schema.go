// Package iceberg projects Invariant's canonical protobuf data schema into an
// official Apache Iceberg schema.
package iceberg

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"

	iceberglib "github.com/apache/iceberg-go"
	datav1 "github.com/jim-technologies/invariantprotocol/go/gen/invariant/data/v1"
)

// Iceberg reserves the highest 200 signed 32-bit IDs.
const maxFieldID = math.MaxInt32 - 200

// Schema maps a canonical dataset to an Iceberg schema. Timestamp nanoseconds
// and non-null protobuf defaults require callers to create a format-v3 table.
func Schema(dataset *datav1.DatasetSchema) (*iceberglib.Schema, []*datav1.MappingDiagnostic, error) {
	if dataset == nil {
		return nil, nil, errors.New("iceberg: nil dataset schema")
	}
	if err := validateFieldIDs(dataset); err != nil {
		return nil, nil, err
	}

	fields := make([]iceberglib.NestedField, 0, len(dataset.GetFields()))
	diagnostics := make([]*datav1.MappingDiagnostic, 0, len(dataset.GetFields()))
	for _, field := range dataset.GetFields() {
		mapped, fieldDiagnostics, err := mapField(field, field.GetName())
		if err != nil {
			return nil, diagnostics, err
		}
		fields = append(fields, mapped)
		diagnostics = append(diagnostics, fieldDiagnostics...)
	}
	return iceberglib.NewSchema(0, fields...), diagnostics, nil
}

// JSON returns Iceberg's canonical JSON representation for a schema.
func JSON(schema *iceberglib.Schema) ([]byte, error) {
	if schema == nil {
		return nil, errors.New("iceberg: nil schema")
	}
	encoded, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("iceberg: encode schema JSON: %w", err)
	}
	return encoded, nil
}

func mapField(field *datav1.Field, path string) (iceberglib.NestedField, []*datav1.MappingDiagnostic, error) {
	if field == nil || field.GetType() == nil {
		return iceberglib.NestedField{}, nil, fmt.Errorf("iceberg: field %q has no logical type", path)
	}

	mappedType, children, compatibility, message, err := mapType(field.GetType(), path)
	if err != nil {
		return iceberglib.NestedField{}, children, err
	}
	diagnostic := &datav1.MappingDiagnostic{
		FieldPath:     path,
		Compatibility: compatibility,
		Message:       message,
	}
	if field.GetOneof() != "" {
		diagnostic.Message += fmt.Sprintf("; Iceberg does not enforce mutual exclusivity for oneof %q", field.GetOneof())
		if diagnostic.Compatibility == datav1.MappingCompatibility_MAPPING_COMPATIBILITY_LOSSLESS {
			diagnostic.Compatibility = datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_WIDENED
		}
	}

	initialDefault, writeDefault, err := fieldDefaults(field, path)
	if err != nil {
		return iceberglib.NestedField{}, children, err
	}
	return iceberglib.NestedField{
		ID:             int(field.GetStableId()),
		Name:           field.GetName(),
		Required:       !field.GetNullable(),
		Doc:            field.GetDescription(),
		Type:           mappedType,
		InitialDefault: initialDefault,
		WriteDefault:   writeDefault,
	}, append([]*datav1.MappingDiagnostic{diagnostic}, children...), nil
}

// fieldDefaults returns Iceberg's format-v3 defaults only where protobuf has a
// canonical value for an absent field. Defaults attach to struct fields, not
// to synthetic list elements or map key/value fields.
func fieldDefaults(field *datav1.Field, path string) (any, any, error) {
	switch field.GetPresence() {
	case datav1.Presence_PRESENCE_IMPLICIT:
		if field.GetNullable() {
			return nil, nil, fmt.Errorf("iceberg: implicit protobuf field %q cannot be nullable", path)
		}
		value, err := implicitDefault(field.GetType(), path)
		if err != nil {
			return nil, nil, err
		}
		return value, value, nil
	case datav1.Presence_PRESENCE_REPEATED:
		if field.GetNullable() || field.GetType().GetList() == nil {
			return nil, nil, fmt.Errorf("iceberg: repeated protobuf field %q must be a non-null list", path)
		}
		initial := []any{}
		return initial, []any{}, nil
	case datav1.Presence_PRESENCE_MAP:
		if field.GetNullable() || field.GetType().GetMap() == nil {
			return nil, nil, fmt.Errorf("iceberg: protobuf map field %q must be a non-null map", path)
		}
		initial := map[string]any{"keys": []any{}, "values": []any{}}
		return initial, map[string]any{"keys": []any{}, "values": []any{}}, nil
	case datav1.Presence_PRESENCE_REQUIRED:
		return nil, nil, fmt.Errorf(
			"iceberg: required protobuf field %q has no safe canonical value for historical rows; use an implicit scalar/enum, repeated/map, or presence-bearing optional field",
			path,
		)
	case datav1.Presence_PRESENCE_EXPLICIT, datav1.Presence_PRESENCE_ONEOF:
		if !field.GetNullable() {
			return nil, nil, fmt.Errorf("iceberg: presence-bearing protobuf field %q must be nullable", path)
		}
		return nil, nil, nil
	case datav1.Presence_PRESENCE_NOT_APPLICABLE:
		if field.GetSyntheticRole() == datav1.SyntheticRole_SYNTHETIC_ROLE_PROTO_FIELD {
			return nil, nil, fmt.Errorf("iceberg: protobuf field %q has no presence semantics", path)
		}
		return nil, nil, nil
	default:
		return nil, nil, fmt.Errorf("iceberg: field %q has unspecified protobuf presence", path)
	}
}

// implicitDefault uses JSON-compatible Go values so Iceberg's official schema
// JSON round-trips without changing numeric or binary representations.
func implicitDefault(dataType *datav1.DataType, path string) (any, error) {
	if dataType == nil {
		return nil, fmt.Errorf("iceberg: implicit protobuf field %q has no logical type", path)
	}
	switch kind := dataType.GetKind().(type) {
	case *datav1.DataType_Primitive:
		switch kind.Primitive.GetKind() {
		case datav1.PrimitiveKind_PRIMITIVE_KIND_DOUBLE,
			datav1.PrimitiveKind_PRIMITIVE_KIND_FLOAT,
			datav1.PrimitiveKind_PRIMITIVE_KIND_INT64,
			datav1.PrimitiveKind_PRIMITIVE_KIND_INT32,
			datav1.PrimitiveKind_PRIMITIVE_KIND_FIXED32,
			datav1.PrimitiveKind_PRIMITIVE_KIND_UINT32,
			datav1.PrimitiveKind_PRIMITIVE_KIND_SFIXED32,
			datav1.PrimitiveKind_PRIMITIVE_KIND_SFIXED64,
			datav1.PrimitiveKind_PRIMITIVE_KIND_SINT32,
			datav1.PrimitiveKind_PRIMITIVE_KIND_SINT64:
			return float64(0), nil
		case datav1.PrimitiveKind_PRIMITIVE_KIND_UINT64,
			datav1.PrimitiveKind_PRIMITIVE_KIND_FIXED64:
			// These protobuf types map to decimal(20, 0), whose Iceberg JSON
			// single-value representation is a decimal string.
			return "0", nil
		case datav1.PrimitiveKind_PRIMITIVE_KIND_BOOL:
			return false, nil
		case datav1.PrimitiveKind_PRIMITIVE_KIND_STRING,
			datav1.PrimitiveKind_PRIMITIVE_KIND_BYTES:
			// Iceberg's empty hex string is also protobuf's empty bytes value.
			return "", nil
		default:
			return nil, fmt.Errorf("iceberg: implicit protobuf field %q has unsupported primitive kind %s", path, kind.Primitive.GetKind())
		}
	case *datav1.DataType_Enum:
		if len(kind.Enum.GetValues()) == 0 {
			return nil, fmt.Errorf("iceberg: implicit protobuf enum field %q has no declared values", path)
		}
		return float64(kind.Enum.GetValues()[0].GetNumber()), nil
	default:
		return nil, fmt.Errorf("iceberg: implicit protobuf field %q is not a scalar or enum", path)
	}
}

func mapType(dataType *datav1.DataType, path string) (iceberglib.Type, []*datav1.MappingDiagnostic, datav1.MappingCompatibility, string, error) {
	switch kind := dataType.GetKind().(type) {
	case *datav1.DataType_Primitive:
		mapped, compatibility, message, err := mapPrimitive(kind.Primitive.GetKind())
		if err != nil {
			return nil, nil, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_UNSUPPORTED, "", fmt.Errorf("iceberg: field %q: %w", path, err)
		}
		return mapped, nil, compatibility, message, nil
	case *datav1.DataType_Enum:
		if kind.Enum.GetClosed() {
			return iceberglib.PrimitiveTypes.Int32, nil,
				datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_WIDENED,
				"closed protobuf enum numbers map to unconstrained Iceberg int; the column admits undeclared values", nil
		}
		return iceberglib.PrimitiveTypes.Int32, nil,
			datav1.MappingCompatibility_MAPPING_COMPATIBILITY_LOSSLESS,
			"open protobuf enum numbers map losslessly to Iceberg int", nil
	case *datav1.DataType_Struct:
		fields := make([]iceberglib.NestedField, 0, len(kind.Struct.GetFields()))
		var diagnostics []*datav1.MappingDiagnostic
		for _, child := range kind.Struct.GetFields() {
			mapped, childDiagnostics, err := mapField(child, joinPath(path, child.GetName()))
			if err != nil {
				return nil, diagnostics, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_UNSUPPORTED, "", err
			}
			fields = append(fields, mapped)
			diagnostics = append(diagnostics, childDiagnostics...)
		}
		return &iceberglib.StructType{FieldList: fields}, diagnostics,
			datav1.MappingCompatibility_MAPPING_COMPATIBILITY_LOSSLESS,
			"protobuf message maps to an Iceberg struct", nil
	case *datav1.DataType_List:
		element := kind.List.GetElement()
		mapped, diagnostics, err := mapField(element, path+"[]")
		if err != nil {
			return nil, diagnostics, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_UNSUPPORTED, "", err
		}
		return &iceberglib.ListType{
				ElementID:       mapped.ID,
				Element:         mapped.Type,
				ElementRequired: mapped.Required,
			}, diagnostics, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_LOSSLESS,
			"protobuf repeated field maps to an Iceberg list with a distinct element ID", nil
	case *datav1.DataType_Map:
		key, keyDiagnostics, err := mapField(kind.Map.GetKey(), path+".key")
		if err != nil {
			return nil, keyDiagnostics, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_UNSUPPORTED, "", err
		}
		value, valueDiagnostics, err := mapField(kind.Map.GetValue(), path+".value")
		if err != nil {
			return nil, append(keyDiagnostics, valueDiagnostics...), datav1.MappingCompatibility_MAPPING_COMPATIBILITY_UNSUPPORTED, "", err
		}
		return &iceberglib.MapType{
				KeyID:         key.ID,
				KeyType:       key.Type,
				ValueID:       value.ID,
				ValueType:     value.Type,
				ValueRequired: value.Required,
			}, append(keyDiagnostics, valueDiagnostics...),
			datav1.MappingCompatibility_MAPPING_COMPATIBILITY_LOSSLESS,
			"protobuf map maps to an Iceberg map with distinct key and value IDs", nil
	case *datav1.DataType_Timestamp:
		if kind.Timestamp.GetUnit() != datav1.TimeUnit_TIME_UNIT_NANOSECOND || kind.Timestamp.GetTimezone() != "UTC" {
			return nil, nil, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_UNSUPPORTED, "", fmt.Errorf("iceberg: field %q has unsupported timestamp unit or timezone", path)
		}
		return iceberglib.PrimitiveTypes.TimestampTzNs, nil,
			datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_REDUCED,
			"Iceberg timestamptz_ns preserves UTC nanoseconds but its int64 range is narrower than protobuf Timestamp; it requires Iceberg format v3", nil
	case *datav1.DataType_Duration:
		if kind.Duration.GetUnit() != datav1.TimeUnit_TIME_UNIT_NANOSECOND {
			return nil, nil, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_UNSUPPORTED, "", fmt.Errorf("iceberg: field %q has unsupported duration unit", path)
		}
		return iceberglib.PrimitiveTypes.Int64, nil,
			datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_REDUCED,
			"Iceberg has no duration type; exact nanoseconds use long, whose int64 range is narrower than protobuf Duration", nil
	case *datav1.DataType_Json:
		return iceberglib.PrimitiveTypes.String, nil,
			datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_REDUCED,
			fmt.Sprintf(
				"protobuf %s is encoded as RFC 8259 JSON text in an Iceberg string; %s",
				kind.Json.GetKind(), jsonRangeReduction(kind.Json.GetKind()),
			), nil
	default:
		return nil, nil, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_UNSUPPORTED, "", fmt.Errorf("iceberg: field %q has an unspecified logical type", path)
	}
}

func jsonRangeReduction(kind datav1.JsonKind) string {
	switch kind {
	case datav1.JsonKind_JSON_KIND_ANY:
		return "standard protobuf JSON requires each populated Any type URL to resolve to a known message descriptor; embedded Struct, Value, and ListValue numbers must also be finite"
	case datav1.JsonKind_JSON_KIND_STRUCT,
		datav1.JsonKind_JSON_KIND_VALUE,
		datav1.JsonKind_JSON_KIND_LIST_VALUE:
		return "standard protobuf JSON requires Struct, Value, and ListValue numbers to be finite; NaN and infinities are not representable"
	default:
		return "standard protobuf JSON requires an explicitly supported dynamic JSON kind"
	}
}

func mapPrimitive(kind datav1.PrimitiveKind) (iceberglib.Type, datav1.MappingCompatibility, string, error) {
	lossless := datav1.MappingCompatibility_MAPPING_COMPATIBILITY_LOSSLESS
	switch kind {
	case datav1.PrimitiveKind_PRIMITIVE_KIND_DOUBLE:
		return iceberglib.PrimitiveTypes.Float64, lossless, "protobuf double maps losslessly to Iceberg double", nil
	case datav1.PrimitiveKind_PRIMITIVE_KIND_FLOAT:
		return iceberglib.PrimitiveTypes.Float32, lossless, "protobuf float maps losslessly to Iceberg float", nil
	case datav1.PrimitiveKind_PRIMITIVE_KIND_INT64,
		datav1.PrimitiveKind_PRIMITIVE_KIND_SFIXED64,
		datav1.PrimitiveKind_PRIMITIVE_KIND_SINT64:
		return iceberglib.PrimitiveTypes.Int64, lossless, "protobuf signed 64-bit integer maps losslessly to Iceberg long", nil
	case datav1.PrimitiveKind_PRIMITIVE_KIND_UINT64,
		datav1.PrimitiveKind_PRIMITIVE_KIND_FIXED64:
		return iceberglib.DecimalTypeOf(20, 0), datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_WIDENED,
			"protobuf unsigned 64-bit integer maps to Iceberg decimal(20, 0), which contains its full domain", nil
	case datav1.PrimitiveKind_PRIMITIVE_KIND_INT32,
		datav1.PrimitiveKind_PRIMITIVE_KIND_SFIXED32,
		datav1.PrimitiveKind_PRIMITIVE_KIND_SINT32:
		return iceberglib.PrimitiveTypes.Int32, lossless, "protobuf signed 32-bit integer maps losslessly to Iceberg int", nil
	case datav1.PrimitiveKind_PRIMITIVE_KIND_FIXED32,
		datav1.PrimitiveKind_PRIMITIVE_KIND_UINT32:
		return iceberglib.PrimitiveTypes.Int64, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_WIDENED,
			"protobuf unsigned 32-bit integer widens to Iceberg long", nil
	case datav1.PrimitiveKind_PRIMITIVE_KIND_BOOL:
		return iceberglib.PrimitiveTypes.Bool, lossless, "protobuf bool maps losslessly to Iceberg boolean", nil
	case datav1.PrimitiveKind_PRIMITIVE_KIND_STRING:
		return iceberglib.PrimitiveTypes.String, lossless, "protobuf string maps losslessly to Iceberg string", nil
	case datav1.PrimitiveKind_PRIMITIVE_KIND_BYTES:
		return iceberglib.PrimitiveTypes.Binary, lossless, "protobuf bytes maps losslessly to Iceberg binary", nil
	default:
		return nil, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_UNSUPPORTED, "", fmt.Errorf("unsupported primitive kind %s", kind)
	}
}

func validateFieldIDs(dataset *datav1.DatasetSchema) error {
	seen := make(map[int32]string)
	var visit func(*datav1.Field, string) error
	visit = func(field *datav1.Field, path string) error {
		if field == nil {
			return fmt.Errorf("iceberg: nil field at %q", path)
		}
		if err := claimFieldID(seen, field.GetStableId(), path); err != nil {
			return err
		}
		if field.GetType() == nil {
			return fmt.Errorf("iceberg: field %q has no logical type", path)
		}
		switch kind := field.GetType().GetKind().(type) {
		case *datav1.DataType_Struct:
			for _, child := range kind.Struct.GetFields() {
				if err := visit(child, joinPath(path, child.GetName())); err != nil {
					return err
				}
			}
		case *datav1.DataType_List:
			if err := visit(kind.List.GetElement(), path+"[]"); err != nil {
				return err
			}
		case *datav1.DataType_Map:
			if err := visit(kind.Map.GetKey(), path+".key"); err != nil {
				return err
			}
			if err := visit(kind.Map.GetValue(), path+".value"); err != nil {
				return err
			}
		}
		return nil
	}
	for _, field := range dataset.GetFields() {
		if err := visit(field, field.GetName()); err != nil {
			return err
		}
	}
	for _, retired := range dataset.GetRetiredFields() {
		if retired == nil {
			return errors.New("iceberg: nil retired field")
		}
		if err := claimFieldID(seen, retired.GetStableId(), "retired "+retired.GetIdentity()); err != nil {
			return err
		}
	}
	return nil
}

func claimFieldID(seen map[int32]string, id int32, path string) error {
	if id <= 0 || id > maxFieldID {
		return fmt.Errorf("iceberg: field %q has invalid stable ID %d; IDs must be between 1 and %d", path, id, maxFieldID)
	}
	if previous, ok := seen[id]; ok {
		return fmt.Errorf("iceberg: stable ID %d is shared by %q and %q", id, previous, path)
	}
	seen[id] = path
	return nil
}

func joinPath(parent, child string) string {
	if parent == "" {
		return child
	}
	return parent + "." + child
}
