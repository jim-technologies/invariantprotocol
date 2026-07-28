package clickhouse

import (
	"encoding/json"
	"errors"
	"fmt"

	iceberglib "github.com/apache/iceberg-go"
	invarianticeberg "github.com/jim-technologies/invariantprotocol/go/data/iceberg"
	datav1 "github.com/jim-technologies/invariantprotocol/go/gen/invariant/data/v1"
)

// ProjectionVersion is the version of the ClickHouse-to-Iceberg projection
// model. It is independent of SchemaBundle's canonical IR version.
const ProjectionVersion = 2

// IcebergProjection describes how a publisher reads ClickHouse values into the
// existing Iceberg-compatible representation. It is a conversion plan, not an
// ingestion runtime or a promise of ClickHouse catalog interoperability.
type IcebergProjection struct {
	Version       uint32                   `json:"version"`
	Dataset       string                   `json:"dataset"`
	SourceMessage string                   `json:"source_message"`
	Fields        []IcebergFieldProjection `json:"fields"`
}

// IcebergFieldProjection is one node in a deterministic structural conversion
// plan. Expressions use {value} for the current physical value and {case} for
// a oneof's sibling discriminator. A publisher evaluates ValueExpression only
// when PresenceExpression is true, then recursively applies Children.
type IcebergFieldProjection struct {
	FieldPath           string                   `json:"field_path"`
	Name                string                   `json:"name"`
	StableID            int32                    `json:"stable_id"`
	ClickHouseType      string                   `json:"clickhouse_type"`
	IcebergType         string                   `json:"iceberg_type"`
	PresenceExpression  string                   `json:"presence_expression"`
	ValueExpression     string                   `json:"value_expression"`
	FixedLength         uint32                   `json:"fixed_length,omitempty"`
	Discriminator       string                   `json:"discriminator,omitempty"`
	ProtobufFieldNumber uint32                   `json:"protobuf_field_number,omitempty"`
	Children            []IcebergFieldProjection `json:"children,omitempty"`
}

// ProjectToIceberg returns a structural conversion plan from the native
// ClickHouse representation to the schema emitted by data/iceberg, together
// with diagnostics from both target mappings. In particular, UInt64 and
// fixed64 use accurateCast({value}, 'Decimal(20, 0)'); every UInt64 value is
// exactly representable by Iceberg decimal(20,0).
func ProjectToIceberg(
	dataset *datav1.DatasetSchema,
) (*IcebergProjection, []*datav1.MappingDiagnostic, error) {
	if dataset == nil {
		return nil, nil, errors.New("clickhouse: nil dataset schema")
	}
	_, clickhouseDiagnostics, err := Schema(dataset)
	if err != nil {
		return nil, clickhouseDiagnostics,
			fmt.Errorf("clickhouse: build native schema before Iceberg projection: %w", err)
	}
	icebergSchema, icebergDiagnostics, err := invarianticeberg.Schema(dataset)
	diagnostics := append(clickhouseDiagnostics, icebergDiagnostics...)
	if err != nil {
		return nil, diagnostics, fmt.Errorf("clickhouse: build Iceberg target schema: %w", err)
	}

	fields := make([]IcebergFieldProjection, 0, len(dataset.GetFields()))
	for _, field := range dataset.GetFields() {
		target, ok := icebergSchema.FindFieldByID(int(field.GetStableId()))
		if !ok {
			return nil, diagnostics, fmt.Errorf(
				"clickhouse: Iceberg target is missing stable_id %d for field %q",
				field.GetStableId(), field.GetName(),
			)
		}
		projected, err := projectField(field, field.GetName(), target.Type.Type(), icebergSchema)
		if err != nil {
			return nil, diagnostics, err
		}
		fields = append(fields, projected)
	}
	return &IcebergProjection{
		Version:       ProjectionVersion,
		Dataset:       dataset.GetName(),
		SourceMessage: dataset.GetSourceMessage(),
		Fields:        fields,
	}, diagnostics, nil
}

// ProjectionJSON returns a deterministic JSON representation of a conversion
// plan. Field order follows SchemaBundle order.
func ProjectionJSON(projection *IcebergProjection) ([]byte, error) {
	if projection == nil {
		return nil, errors.New("clickhouse: nil Iceberg projection")
	}
	encoded, err := json.Marshal(projection)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: encode Iceberg projection: %w", err)
	}
	return encoded, nil
}

func projectField(
	field *datav1.Field,
	path string,
	icebergType string,
	finder *iceberglib.Schema,
) (IcebergFieldProjection, error) {
	if field == nil || field.GetType() == nil {
		return IcebergFieldProjection{}, fmt.Errorf("clickhouse: projection field %q has no logical type", path)
	}
	physical, _, err := mapField(field, path)
	if err != nil {
		return IcebergFieldProjection{}, err
	}
	mapped, err := mapType(field.GetType(), path)
	if err != nil {
		return IcebergFieldProjection{}, fmt.Errorf("clickhouse: projection field %q: %w", path, err)
	}

	presence := "true"
	value := projectionValuePlaceholder
	discriminator := ""
	var fieldNumber uint32
	switch field.GetPresence() {
	case datav1.Presence_PRESENCE_EXPLICIT:
		if mapped.composite {
			presence = tupleElementExpression(projectionValuePlaceholder, "present")
			value = tupleElementExpression(projectionValuePlaceholder, "value")
		} else {
			presence = "isNotNull(" + projectionValuePlaceholder + ")"
			value = "assumeNotNull(" + projectionValuePlaceholder + ")"
		}
	case datav1.Presence_PRESENCE_REQUIRED:
		presence = tupleElementExpression(projectionValuePlaceholder, "present")
		value = tupleElementExpression(projectionValuePlaceholder, "value")
	case datav1.Presence_PRESENCE_ONEOF:
		numberPath := field.GetProtoNumberPath()
		if len(numberPath) == 0 {
			return IcebergFieldProjection{}, fmt.Errorf(
				"clickhouse: projection oneof field %q has no protobuf field number",
				path,
			)
		}
		fieldNumber = numberPath[len(numberPath)-1]
		discriminator = oneofCaseName(field.GetOneof())
		presence = fmt.Sprintf(
			"%s AND {case} = %d",
			tupleElementExpression(projectionValuePlaceholder, "present"),
			fieldNumber,
		)
		value = tupleElementExpression(projectionValuePlaceholder, "value")
	case datav1.Presence_PRESENCE_IMPLICIT,
		datav1.Presence_PRESENCE_REPEATED,
		datav1.Presence_PRESENCE_MAP,
		datav1.Presence_PRESENCE_NOT_APPLICABLE:
	default:
		return IcebergFieldProjection{}, fmt.Errorf(
			"clickhouse: projection field %q has unsupported presence %s",
			path, field.GetPresence(),
		)
	}

	projected := IcebergFieldProjection{
		FieldPath:           path,
		Name:                field.GetName(),
		StableID:            field.GetStableId(),
		ClickHouseType:      physical.sqlType,
		IcebergType:         icebergType,
		PresenceExpression:  presence,
		ValueExpression:     conversionExpression(field.GetType(), value),
		Discriminator:       discriminator,
		ProtobufFieldNumber: fieldNumber,
	}
	if list := field.GetType().GetList(); list != nil {
		projected.FixedLength = list.GetFixedLength()
	}

	appendChild := func(child *datav1.Field, childPath string) error {
		if child == nil {
			return fmt.Errorf("clickhouse: projection field %q contains a nil child", path)
		}
		target, ok := finder.FindFieldByID(int(child.GetStableId()))
		if !ok {
			return fmt.Errorf(
				"clickhouse: Iceberg target is missing stable_id %d for field %q",
				child.GetStableId(), childPath,
			)
		}
		childProjection, err := projectField(child, childPath, target.Type.Type(), finder)
		if err != nil {
			return err
		}
		projected.Children = append(projected.Children, childProjection)
		return nil
	}

	switch kind := field.GetType().GetKind().(type) {
	case *datav1.DataType_Struct:
		for _, child := range kind.Struct.GetFields() {
			if err := appendChild(child, joinPath(path, child.GetName())); err != nil {
				return IcebergFieldProjection{}, err
			}
		}
	case *datav1.DataType_List:
		if err := appendChild(kind.List.GetElement(), path+"[]"); err != nil {
			return IcebergFieldProjection{}, err
		}
	case *datav1.DataType_Map:
		if err := appendChild(kind.Map.GetKey(), path+".key"); err != nil {
			return IcebergFieldProjection{}, err
		}
		if err := appendChild(kind.Map.GetValue(), path+".value"); err != nil {
			return IcebergFieldProjection{}, err
		}
	}
	return projected, nil
}

func conversionExpression(dataType *datav1.DataType, value string) string {
	switch kind := dataType.GetKind().(type) {
	case *datav1.DataType_Primitive:
		switch kind.Primitive.GetKind() {
		case datav1.PrimitiveKind_PRIMITIVE_KIND_UINT64,
			datav1.PrimitiveKind_PRIMITIVE_KIND_FIXED64:
			return "accurateCast(" + value + ", 'Decimal(20, 0)')"
		case datav1.PrimitiveKind_PRIMITIVE_KIND_UINT32,
			datav1.PrimitiveKind_PRIMITIVE_KIND_FIXED32:
			return "toInt64(" + value + ")"
		}
	case *datav1.DataType_Timestamp:
		return "toUnixTimestamp64Nano(" + value + ")"
	}
	return value
}
