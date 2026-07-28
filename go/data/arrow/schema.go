// Package arrow projects Invariant's canonical protobuf data schema into an
// Apache Arrow schema. It describes columns only; it does not convert records.
package arrow

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	arrowlib "github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/extensions"
	"github.com/apache/arrow-go/v18/arrow/ipc"
	datav1 "github.com/jim-technologies/invariantprotocol/go/gen/invariant/data/v1"
)

const (
	metadataDescription     = "invariant.description"
	metadataEnumClosed      = "invariant.enum.closed"
	metadataEnumValues      = "invariant.enum.values"
	metadataLogicalType     = "invariant.logical_type"
	metadataOneof           = "invariant.oneof"
	metadataPresence        = "invariant.presence"
	metadataProtoDefault    = "invariant.proto.default"
	metadataProtoFullName   = "invariant.proto.full_name"
	metadataProtoHasDefault = "invariant.proto.has_default"
	metadataProtoJSONName   = "invariant.proto.json_name"
	metadataProtoNumberPath = "invariant.proto.number_path"
	metadataProtobufType    = "invariant.protobuf_type"
	metadataStableID        = "invariant.stable_id"
	parquetFieldIDMetadata  = "PARQUET:field_id"
)

type enumMetadataValue struct {
	Name        string `json:"name"`
	Number      int32  `json:"number"`
	Description string `json:"description,omitempty"`
}

// Schema maps a canonical dataset to an Apache Arrow schema. Every logical
// field, including list elements and map children, produces one diagnostic.
func Schema(dataset *datav1.DatasetSchema) (*arrowlib.Schema, []*datav1.MappingDiagnostic, error) {
	if dataset == nil {
		return nil, nil, errors.New("arrow: nil dataset schema")
	}

	fields := make([]arrowlib.Field, 0, len(dataset.GetFields()))
	diagnostics := make([]*datav1.MappingDiagnostic, 0, len(dataset.GetFields()))
	for _, field := range dataset.GetFields() {
		mapped, fieldDiagnostics, err := mapField(field, field.GetName())
		if err != nil {
			return nil, diagnostics, err
		}
		fields = append(fields, mapped)
		diagnostics = append(diagnostics, fieldDiagnostics...)
	}

	metadata := arrowlib.MetadataFrom(compactMetadata(map[string]string{
		metadataDescription:        dataset.GetDescription(),
		"invariant.dataset":        dataset.GetName(),
		"invariant.last_field_id":  strconv.FormatInt(int64(dataset.GetLastFieldId()), 10),
		"invariant.source_message": dataset.GetSourceMessage(),
	}))
	return arrowlib.NewSchema(fields, &metadata), diagnostics, nil
}

// WriteIPC writes a schema-only Arrow IPC stream. The stream contains zero
// record batches and can be read by ordinary Arrow IPC readers.
func WriteIPC(w io.Writer, schema *arrowlib.Schema) error {
	if w == nil {
		return errors.New("arrow: nil IPC writer")
	}
	if schema == nil {
		return errors.New("arrow: nil IPC schema")
	}
	return ipc.NewWriter(w, ipc.WithSchema(schema)).Close()
}

func mapField(field *datav1.Field, path string) (arrowlib.Field, []*datav1.MappingDiagnostic, error) {
	if field == nil {
		return arrowlib.Field{}, nil, fmt.Errorf("arrow: nil field at %q", path)
	}
	if field.GetType() == nil {
		return arrowlib.Field{}, nil, fmt.Errorf("arrow: field %q has no logical type", path)
	}

	mappedType, children, compatibility, message, err := mapType(field.GetType(), path)
	if err != nil {
		return arrowlib.Field{}, children, err
	}

	metadata := compactMetadata(map[string]string{
		metadataDescription:     field.GetDescription(),
		metadataLogicalType:     logicalTypeName(field.GetType()),
		metadataOneof:           field.GetOneof(),
		metadataPresence:        field.GetPresence().String(),
		metadataProtoFullName:   field.GetProtoFullName(),
		metadataProtoHasDefault: strconv.FormatBool(field.GetHasDefault()),
		metadataProtoJSONName:   field.GetJsonName(),
		metadataProtoNumberPath: numberPath(field.GetProtoNumberPath()),
		metadataProtobufType:    field.GetType().GetProtobufType(),
		metadataStableID:        strconv.FormatInt(int64(field.GetStableId()), 10),
		parquetFieldIDMetadata:  strconv.FormatInt(int64(field.GetStableId()), 10),
	})
	if field.GetHasDefault() {
		metadata[metadataProtoDefault] = field.GetProtobufDefault()
	}
	if enumType := field.GetType().GetEnum(); enumType != nil {
		metadata[metadataEnumClosed] = strconv.FormatBool(enumType.GetClosed())
		enumValues := make([]enumMetadataValue, len(enumType.GetValues()))
		for i, value := range enumType.GetValues() {
			enumValues[i] = enumMetadataValue{
				Name:        value.GetName(),
				Number:      value.GetNumber(),
				Description: value.GetDescription(),
			}
		}
		values, marshalErr := json.Marshal(enumValues)
		if marshalErr != nil {
			return arrowlib.Field{}, children, fmt.Errorf("arrow: encode enum metadata for %q: %w", path, marshalErr)
		}
		metadata[metadataEnumValues] = string(values)
	}

	diagnostic := &datav1.MappingDiagnostic{
		FieldPath:     path,
		Compatibility: compatibility,
		Message:       message,
	}
	if field.GetOneof() != "" {
		diagnostic.Message += fmt.Sprintf("; Arrow records membership in oneof %q as metadata but does not enforce mutual exclusivity", field.GetOneof())
		if diagnostic.Compatibility == datav1.MappingCompatibility_MAPPING_COMPATIBILITY_LOSSLESS {
			diagnostic.Compatibility = datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_WIDENED
		}
	}
	return arrowlib.Field{
		Name:     field.GetName(),
		Type:     mappedType,
		Nullable: field.GetNullable(),
		Metadata: arrowlib.MetadataFrom(metadata),
	}, append([]*datav1.MappingDiagnostic{diagnostic}, children...), nil
}

func mapType(dataType *datav1.DataType, path string) (arrowlib.DataType, []*datav1.MappingDiagnostic, datav1.MappingCompatibility, string, error) {
	switch kind := dataType.GetKind().(type) {
	case *datav1.DataType_Primitive:
		mapped, err := mapPrimitive(kind.Primitive.GetKind())
		if err != nil {
			return nil, nil, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_UNSUPPORTED, "", fmt.Errorf("arrow: field %q: %w", path, err)
		}
		return mapped, nil, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_LOSSLESS,
			fmt.Sprintf("protobuf %s maps losslessly to Arrow %s", primitiveName(kind.Primitive.GetKind()), mapped), nil
	case *datav1.DataType_Enum:
		if kind.Enum.GetClosed() {
			return arrowlib.PrimitiveTypes.Int32, nil,
				datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_WIDENED,
				"closed protobuf enum numbers map to unconstrained Arrow int32; symbols, aliases, and the closed value set are field metadata", nil
		}
		return arrowlib.PrimitiveTypes.Int32, nil,
			datav1.MappingCompatibility_MAPPING_COMPATIBILITY_LOSSLESS,
			"open protobuf enum numbers map losslessly to Arrow int32; symbols and aliases are field metadata", nil
	case *datav1.DataType_Struct:
		fields := make([]arrowlib.Field, 0, len(kind.Struct.GetFields()))
		var diagnostics []*datav1.MappingDiagnostic
		for _, child := range kind.Struct.GetFields() {
			childPath := joinPath(path, child.GetName())
			mapped, childDiagnostics, err := mapField(child, childPath)
			if err != nil {
				return nil, diagnostics, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_UNSUPPORTED, "", err
			}
			fields = append(fields, mapped)
			diagnostics = append(diagnostics, childDiagnostics...)
		}
		return arrowlib.StructOf(fields...), diagnostics,
			datav1.MappingCompatibility_MAPPING_COMPATIBILITY_LOSSLESS,
			"protobuf message maps to an Arrow struct", nil
	case *datav1.DataType_List:
		element := kind.List.GetElement()
		mapped, diagnostics, err := mapField(element, path+"[]")
		if err != nil {
			return nil, diagnostics, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_UNSUPPORTED, "", err
		}
		mapped.Name = "item"
		if length := kind.List.GetFixedLength(); length != 0 {
			if length > math.MaxInt32 {
				return nil, diagnostics, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_UNSUPPORTED, "",
					fmt.Errorf("arrow: field %q fixed-list length %d exceeds Arrow's maximum of %d", path, length, int64(math.MaxInt32))
			}
			return arrowlib.FixedSizeListOfField(int32(length), mapped), diagnostics,
				datav1.MappingCompatibility_MAPPING_COMPATIBILITY_LOSSLESS,
				fmt.Sprintf("fixed-cardinality protobuf repeated field maps losslessly to Arrow fixed_size_list[%d]", length), nil
		}
		return arrowlib.ListOfField(mapped), diagnostics,
			datav1.MappingCompatibility_MAPPING_COMPATIBILITY_LOSSLESS,
			"protobuf repeated field maps to an Arrow list", nil
	case *datav1.DataType_Map:
		key, keyDiagnostics, err := mapField(kind.Map.GetKey(), path+".key")
		if err != nil {
			return nil, keyDiagnostics, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_UNSUPPORTED, "", err
		}
		value, valueDiagnostics, err := mapField(kind.Map.GetValue(), path+".value")
		if err != nil {
			return nil, append(keyDiagnostics, valueDiagnostics...), datav1.MappingCompatibility_MAPPING_COMPATIBILITY_UNSUPPORTED, "", err
		}
		key.Name = "key"
		key.Nullable = false
		value.Name = "value"
		return arrowlib.MapOfFields(key, value), append(keyDiagnostics, valueDiagnostics...),
			datav1.MappingCompatibility_MAPPING_COMPATIBILITY_LOSSLESS,
			"protobuf map maps to an Arrow map with typed key and value children", nil
	case *datav1.DataType_Timestamp:
		if kind.Timestamp.GetUnit() != datav1.TimeUnit_TIME_UNIT_NANOSECOND || kind.Timestamp.GetTimezone() != "UTC" {
			return nil, nil, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_UNSUPPORTED, "", fmt.Errorf("arrow: field %q has unsupported timestamp unit or timezone", path)
		}
		return &arrowlib.TimestampType{Unit: arrowlib.Nanosecond, TimeZone: "UTC"}, nil,
			datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_REDUCED,
			"Arrow timestamp[ns, tz=UTC] preserves nanosecond precision but its int64 range is narrower than protobuf Timestamp", nil
	case *datav1.DataType_Duration:
		if kind.Duration.GetUnit() != datav1.TimeUnit_TIME_UNIT_NANOSECOND {
			return nil, nil, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_UNSUPPORTED, "", fmt.Errorf("arrow: field %q has unsupported duration unit", path)
		}
		return arrowlib.FixedWidthTypes.Duration_ns, nil,
			datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_REDUCED,
			"Arrow duration[ns] preserves nanosecond precision but its int64 range is narrower than protobuf Duration", nil
	case *datav1.DataType_Json:
		jsonType, err := extensions.NewJSONType(arrowlib.BinaryTypes.String)
		if err != nil {
			return nil, nil, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_UNSUPPORTED, "", fmt.Errorf("arrow: field %q: %w", path, err)
		}
		return jsonType, nil,
			datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_REDUCED,
			fmt.Sprintf(
				"protobuf %s is encoded as RFC 8259 text in Arrow's canonical JSON extension type; %s",
				kind.Json.GetKind(), jsonRangeReduction(kind.Json.GetKind()),
			), nil
	case *datav1.DataType_Decimal:
		precision, scale, err := decimalParameters(kind.Decimal)
		if err != nil {
			return nil, nil, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_UNSUPPORTED, "", fmt.Errorf("arrow: field %q: %w", path, err)
		}
		return &arrowlib.Decimal128Type{Precision: precision, Scale: scale}, nil,
			datav1.MappingCompatibility_MAPPING_COMPATIBILITY_REPRESENTATION_CHANGED,
			fmt.Sprintf("canonical decimal text is decoded into Arrow decimal128(%d, %d); precision and scale are preserved but the physical representation changes", precision, scale), nil
	case *datav1.DataType_Uuid:
		if kind.Uuid == nil {
			return nil, nil, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_UNSUPPORTED, "", fmt.Errorf("arrow: field %q has an invalid UUID logical type", path)
		}
		return extensions.NewUUIDType(), nil,
			datav1.MappingCompatibility_MAPPING_COMPATIBILITY_REPRESENTATION_CHANGED,
			"canonical UUID text is decoded into Arrow's canonical arrow.uuid extension over fixed-size binary[16]", nil
	case *datav1.DataType_FixedBytes:
		width, err := fixedByteWidth(kind.FixedBytes)
		if err != nil {
			return nil, nil, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_UNSUPPORTED, "", fmt.Errorf("arrow: field %q: %w", path, err)
		}
		return &arrowlib.FixedSizeBinaryType{ByteWidth: width}, nil,
			datav1.MappingCompatibility_MAPPING_COMPATIBILITY_LOSSLESS,
			fmt.Sprintf("exact-width protobuf bytes map losslessly to Arrow fixed_size_binary[%d]", width), nil
	default:
		return nil, nil, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_UNSUPPORTED, "", fmt.Errorf("arrow: field %q has an unspecified logical type", path)
	}
}

func decimalParameters(decimal *datav1.DecimalType) (int32, int32, error) {
	if decimal == nil || decimal.GetPrecision() == 0 || decimal.GetPrecision() > 38 {
		return 0, 0, errors.New("decimal precision must be between 1 and 38")
	}
	if decimal.GetScale() > decimal.GetPrecision() {
		return 0, 0, errors.New("decimal scale must not exceed precision")
	}
	return int32(decimal.GetPrecision()), int32(decimal.GetScale()), nil
}

func fixedByteWidth(fixed *datav1.FixedBytesType) (int, error) {
	if fixed == nil || fixed.GetByteLength() == 0 {
		return 0, errors.New("fixed byte length must be positive")
	}
	if fixed.GetByteLength() > math.MaxInt32 {
		return 0, fmt.Errorf("fixed byte length %d exceeds Arrow IPC's maximum of %d", fixed.GetByteLength(), int64(math.MaxInt32))
	}
	return int(fixed.GetByteLength()), nil
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

func mapPrimitive(kind datav1.PrimitiveKind) (arrowlib.DataType, error) {
	switch kind {
	case datav1.PrimitiveKind_PRIMITIVE_KIND_DOUBLE:
		return arrowlib.PrimitiveTypes.Float64, nil
	case datav1.PrimitiveKind_PRIMITIVE_KIND_FLOAT:
		return arrowlib.PrimitiveTypes.Float32, nil
	case datav1.PrimitiveKind_PRIMITIVE_KIND_INT64,
		datav1.PrimitiveKind_PRIMITIVE_KIND_SFIXED64,
		datav1.PrimitiveKind_PRIMITIVE_KIND_SINT64:
		return arrowlib.PrimitiveTypes.Int64, nil
	case datav1.PrimitiveKind_PRIMITIVE_KIND_UINT64,
		datav1.PrimitiveKind_PRIMITIVE_KIND_FIXED64:
		return arrowlib.PrimitiveTypes.Uint64, nil
	case datav1.PrimitiveKind_PRIMITIVE_KIND_INT32,
		datav1.PrimitiveKind_PRIMITIVE_KIND_SFIXED32,
		datav1.PrimitiveKind_PRIMITIVE_KIND_SINT32:
		return arrowlib.PrimitiveTypes.Int32, nil
	case datav1.PrimitiveKind_PRIMITIVE_KIND_FIXED32,
		datav1.PrimitiveKind_PRIMITIVE_KIND_UINT32:
		return arrowlib.PrimitiveTypes.Uint32, nil
	case datav1.PrimitiveKind_PRIMITIVE_KIND_BOOL:
		return arrowlib.FixedWidthTypes.Boolean, nil
	case datav1.PrimitiveKind_PRIMITIVE_KIND_STRING:
		return arrowlib.BinaryTypes.String, nil
	case datav1.PrimitiveKind_PRIMITIVE_KIND_BYTES:
		return arrowlib.BinaryTypes.Binary, nil
	default:
		return nil, fmt.Errorf("unsupported primitive kind %s", kind)
	}
}

func compactMetadata(values map[string]string) map[string]string {
	for key, value := range values {
		if value == "" {
			delete(values, key)
		}
	}
	return values
}

func joinPath(parent, child string) string {
	if parent == "" {
		return child
	}
	return parent + "." + child
}

func logicalTypeName(dataType *datav1.DataType) string {
	switch kind := dataType.GetKind().(type) {
	case *datav1.DataType_Primitive:
		return "primitive"
	case *datav1.DataType_Enum:
		return "enum"
	case *datav1.DataType_Struct:
		return "struct"
	case *datav1.DataType_List:
		if kind.List.GetFixedLength() != 0 {
			return "fixed_list"
		}
		return "list"
	case *datav1.DataType_Map:
		return "map"
	case *datav1.DataType_Timestamp:
		return "timestamp"
	case *datav1.DataType_Duration:
		return "duration"
	case *datav1.DataType_Json:
		return "json"
	case *datav1.DataType_Decimal:
		return "decimal"
	case *datav1.DataType_Uuid:
		return "uuid"
	case *datav1.DataType_FixedBytes:
		return "fixed_bytes"
	default:
		return "unspecified"
	}
}

func numberPath(numbers []uint32) string {
	parts := make([]string, len(numbers))
	for i, number := range numbers {
		parts[i] = strconv.FormatUint(uint64(number), 10)
	}
	return strings.Join(parts, ".")
}

func primitiveName(kind datav1.PrimitiveKind) string {
	return strings.ToLower(strings.TrimPrefix(kind.String(), "PRIMITIVE_KIND_"))
}
