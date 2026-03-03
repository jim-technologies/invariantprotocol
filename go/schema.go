package invariant

import (
	"maps"

	invpb "github.com/jim-technologies/invariantprotocol/go/gen/invariant/v1"
)

// Proto field type constants (matching google.protobuf.FieldDescriptorProto.Type).
const (
	typeDouble   = 1
	typeFloat    = 2
	typeInt64    = 3
	typeUint64   = 4
	typeInt32    = 5
	typeFixed64  = 6
	typeFixed32  = 7
	typeBool     = 8
	typeString   = 9
	typeMessage  = 11
	typeBytes    = 12
	typeUint32   = 13
	typeEnum     = 14
	typeSfixed32 = 15
	typeSfixed64 = 16
	typeSint32   = 17
	typeSint64   = 18

	labelRepeated = 3
)

// Well-known type mappings.
var wkt = map[string]map[string]any{
	"google.protobuf.Timestamp":   {"type": "string", "format": "date-time"},
	"google.protobuf.Duration":    {"type": "string", "description": "Duration e.g. '300s', '1.5h'"},
	"google.protobuf.Any":         {"type": "object"},
	"google.protobuf.Struct":      {"type": "object"},
	"google.protobuf.Value":       {},
	"google.protobuf.DoubleValue": {"type": "number"},
	"google.protobuf.FloatValue":  {"type": "number"},
	"google.protobuf.Int64Value":  {"type": "integer"},
	"google.protobuf.UInt64Value": {"type": "integer", "minimum": 0},
	"google.protobuf.Int32Value":  {"type": "integer"},
	"google.protobuf.UInt32Value": {"type": "integer", "minimum": 0},
	"google.protobuf.BoolValue":   {"type": "boolean"},
	"google.protobuf.StringValue": {"type": "string"},
	"google.protobuf.BytesValue":  {"type": "string", "contentEncoding": "base64"},
}

// schemaGenerator converts parsed proto types to JSON Schema.
type schemaGenerator struct {
	parsed *invpb.ParsedDescriptor
}

// newSchemaGenerator creates a schemaGenerator from a ParsedDescriptor.
func newSchemaGenerator(parsed *invpb.ParsedDescriptor) *schemaGenerator {
	return &schemaGenerator{parsed: parsed}
}

// MessageToSchema converts a fully-qualified message name to a JSON Schema.
func (sg *schemaGenerator) MessageToSchema(fullName string) map[string]any {
	msg, ok := sg.parsed.Messages[fullName]
	if !ok {
		return map[string]any{"type": "object"}
	}
	return sg.messageSchema(msg)
}

func (sg *schemaGenerator) messageSchema(msg *invpb.MessageInfo) map[string]any {
	properties := make(map[string]any)
	var required []string

	oneofFields := make(map[string]bool)
	for _, oneof := range msg.Oneofs {
		for _, fname := range oneof.FieldNames {
			oneofFields[fname] = true
		}
	}

	for _, field := range msg.Fields {
		var prop map[string]any

		switch {
		case sg.isMapField(field):
			mapMsg, ok := sg.parsed.Messages[field.TypeName]
			if ok {
				prop = sg.mapSchema(mapMsg)
			} else {
				prop = map[string]any{"type": "object"}
			}
		case field.Label == labelRepeated:
			prop = map[string]any{"type": "array", "items": sg.fieldTypeSchema(field)}
		default:
			prop = sg.fieldTypeSchema(field)
		}

		if field.Comment != "" {
			prop["description"] = field.Comment
		}

		properties[field.Name] = prop

		if field.Label != labelRepeated &&
			!oneofFields[field.Name] &&
			field.OneofIndex == nil &&
			!field.Optional {
			required = append(required, field.Name)
		}
	}

	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func (sg *schemaGenerator) fieldTypeSchema(field *invpb.FieldInfo) map[string]any {
	switch field.Type {
	case typeDouble, typeFloat:
		return map[string]any{"type": "number"}
	case typeInt32, typeInt64, typeSint32, typeSint64, typeSfixed32, typeSfixed64:
		return map[string]any{"type": "integer"}
	case typeUint32, typeUint64, typeFixed32, typeFixed64:
		return map[string]any{"type": "integer", "minimum": 0}
	case typeBool:
		return map[string]any{"type": "boolean"}
	case typeString:
		return map[string]any{"type": "string"}
	case typeBytes:
		return map[string]any{"type": "string", "contentEncoding": "base64"}
	case typeEnum:
		return sg.enumSchema(field.TypeName)
	case typeMessage:
		return sg.messageTypeSchema(field.TypeName)
	}
	return map[string]any{}
}

func (sg *schemaGenerator) messageTypeSchema(typeName string) map[string]any {
	if w, ok := wkt[typeName]; ok {
		result := make(map[string]any, len(w))
		maps.Copy(result, w)
		return result
	}
	msg, ok := sg.parsed.Messages[typeName]
	if !ok {
		return map[string]any{"type": "object"}
	}
	return sg.messageSchema(msg)
}

func (sg *schemaGenerator) enumSchema(typeName string) map[string]any {
	enumInfo, ok := sg.parsed.Enums[typeName]
	if !ok {
		return map[string]any{"type": "string"}
	}
	names := make([]string, len(enumInfo.Values))
	for i, v := range enumInfo.Values {
		names[i] = v.Name
	}
	return map[string]any{"type": "string", "enum": names}
}

func (sg *schemaGenerator) isMapField(field *invpb.FieldInfo) bool {
	if field.Label != labelRepeated || field.Type != typeMessage {
		return false
	}
	msg, ok := sg.parsed.Messages[field.TypeName]
	return ok && msg.IsMapEntry
}

func (sg *schemaGenerator) mapSchema(mapEntryMsg *invpb.MessageInfo) map[string]any {
	var valueField *invpb.FieldInfo
	for _, f := range mapEntryMsg.Fields {
		if f.Name == "value" {
			valueField = f
			break
		}
	}
	if valueField == nil {
		return map[string]any{"type": "object"}
	}
	return map[string]any{
		"type":                 "object",
		"additionalProperties": sg.fieldTypeSchema(valueField),
	}
}
