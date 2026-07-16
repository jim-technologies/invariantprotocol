// Package postgres projects Invariant's canonical protobuf data schema into
// PostgreSQL DDL. Atlas can consume the emitted SQL directly; HCL is not an
// intermediate source of truth.
package postgres

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	datav1 "github.com/jim-technologies/invariantprotocol/go/gen/invariant/data/v1"
)

const maxIdentifierBytes = 63

type column struct {
	name        string
	description string
	sqlType     string
	defaultSQL  string
	notNull     bool
	oneof       string
}

// DDL returns complete, semicolon-terminated PostgreSQL DDL for one dataset.
// Nested messages, lists, maps, and dynamic protobuf JSON values are JSONB.
// Relational keys and indexes are deliberately not inferred from protobuf.
func DDL(dataset *datav1.DatasetSchema) (string, []*datav1.MappingDiagnostic, error) {
	if dataset == nil {
		return "", nil, errors.New("postgres: nil dataset schema")
	}
	tableName, err := identifier(dataset.GetName())
	if err != nil {
		return "", nil, fmt.Errorf("postgres: dataset name: %w", err)
	}
	if err := validateText(dataset.GetDescription()); err != nil {
		return "", nil, fmt.Errorf("postgres: dataset description: %w", err)
	}

	columns := make([]column, 0, len(dataset.GetFields()))
	diagnostics := make([]*datav1.MappingDiagnostic, 0, len(dataset.GetFields()))
	seenNames := make(map[string]string, len(dataset.GetFields()))
	for _, field := range dataset.GetFields() {
		if field == nil || field.GetType() == nil {
			return "", diagnostics, errors.New("postgres: dataset contains a field without a logical type")
		}
		name, err := identifier(field.GetName())
		if err != nil {
			return "", diagnostics, fmt.Errorf("postgres: field %q: %w", field.GetProtoFullName(), err)
		}
		if previous, ok := seenNames[name]; ok {
			return "", diagnostics, fmt.Errorf("postgres: fields %q and %q map to the same identifier %q", previous, field.GetName(), name)
		}
		seenNames[name] = field.GetName()
		if err := validateText(field.GetDescription()); err != nil {
			return "", diagnostics, fmt.Errorf("postgres: field %q description: %w", field.GetName(), err)
		}

		typeName, compatibility, message, err := mapType(field.GetType())
		if err != nil {
			return "", diagnostics, fmt.Errorf("postgres: field %q: %w", field.GetName(), err)
		}
		columns = append(columns, column{
			name:        name,
			description: field.GetDescription(),
			sqlType:     typeName,
			defaultSQL:  defaultExpression(field),
			notNull:     !field.GetNullable(),
			oneof:       field.GetOneof(),
		})
		diagnostics = append(diagnostics, &datav1.MappingDiagnostic{
			FieldPath:     field.GetName(),
			Compatibility: compatibility,
			Message:       message,
		})
		diagnostics = append(diagnostics, nestedDiagnostics(field.GetType(), field.GetName())...)
	}

	checks, err := oneofChecks(tableName, columns)
	if err != nil {
		return "", diagnostics, err
	}

	var ddl strings.Builder
	fmt.Fprintf(&ddl, "CREATE TABLE %s (\n", quoteIdentifier(tableName))
	definitions := make([]string, 0, len(columns)+len(checks))
	for _, col := range columns {
		definition := "  " + quoteIdentifier(col.name) + " " + col.sqlType
		if col.notNull {
			definition += " NOT NULL"
		}
		if col.defaultSQL != "" {
			definition += " DEFAULT " + col.defaultSQL
		}
		definitions = append(definitions, definition)
	}
	definitions = append(definitions, checks...)
	ddl.WriteString(strings.Join(definitions, ",\n"))
	ddl.WriteString("\n);\n")
	if dataset.GetDescription() != "" {
		fmt.Fprintf(&ddl, "COMMENT ON TABLE %s IS %s;\n", quoteIdentifier(tableName), quoteLiteral(dataset.GetDescription()))
	}
	for _, col := range columns {
		if col.description != "" {
			fmt.Fprintf(&ddl, "COMMENT ON COLUMN %s.%s IS %s;\n",
				quoteIdentifier(tableName), quoteIdentifier(col.name), quoteLiteral(col.description))
		}
	}
	return ddl.String(), diagnostics, nil
}

func mapType(dataType *datav1.DataType) (string, datav1.MappingCompatibility, string, error) {
	switch kind := dataType.GetKind().(type) {
	case *datav1.DataType_Primitive:
		return mapPrimitive(kind.Primitive.GetKind())
	case *datav1.DataType_Enum:
		if kind.Enum.GetClosed() {
			return "INTEGER", datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_WIDENED,
				"closed protobuf enum numbers map to unconstrained PostgreSQL INTEGER; the column admits undeclared values", nil
		}
		return "INTEGER", datav1.MappingCompatibility_MAPPING_COMPATIBILITY_LOSSLESS,
			"open protobuf enum numbers map losslessly to PostgreSQL INTEGER", nil
	case *datav1.DataType_Timestamp:
		return "TIMESTAMPTZ", datav1.MappingCompatibility_MAPPING_COMPATIBILITY_PRECISION_REDUCED,
			"PostgreSQL TIMESTAMPTZ retains the protobuf Timestamp range and UTC instant semantics but reduces nanoseconds to microseconds", nil
	case *datav1.DataType_Duration:
		return "BIGINT", datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_REDUCED,
			"PostgreSQL has no exact protobuf Duration type; exact nanoseconds use BIGINT, whose range is narrower than protobuf Duration", nil
	case *datav1.DataType_Struct:
		if canContainNUL(dataType) {
			return "JSONB", datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_REDUCED,
				"nested protobuf message is stored as one JSONB column; PostgreSQL JSONB cannot represent the protobuf string value U+0000", nil
		}
		return "JSONB", datav1.MappingCompatibility_MAPPING_COMPATIBILITY_REPRESENTATION_CHANGED,
			"nested protobuf message is stored as one JSONB column", nil
	case *datav1.DataType_List:
		if canContainNUL(dataType) {
			return "JSONB", datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_REDUCED,
				"protobuf repeated field is stored as one JSONB array column; PostgreSQL JSONB cannot represent the protobuf string value U+0000", nil
		}
		return "JSONB", datav1.MappingCompatibility_MAPPING_COMPATIBILITY_REPRESENTATION_CHANGED,
			"protobuf repeated field is stored as one JSONB array column", nil
	case *datav1.DataType_Map:
		if canContainNUL(dataType) {
			return "JSONB", datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_REDUCED,
				"protobuf map is stored as one JSONB object column; PostgreSQL JSONB cannot represent the protobuf string value U+0000", nil
		}
		return "JSONB", datav1.MappingCompatibility_MAPPING_COMPATIBILITY_REPRESENTATION_CHANGED,
			"protobuf map is stored as one JSONB object column", nil
	case *datav1.DataType_Json:
		return "JSONB", datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_REDUCED,
			fmt.Sprintf(
				"protobuf %s is encoded with protobuf JSON semantics in PostgreSQL JSONB, which cannot represent the protobuf string value U+0000; %s",
				kind.Json.GetKind(), jsonRangeReduction(kind.Json.GetKind()),
			), nil
	default:
		return "", datav1.MappingCompatibility_MAPPING_COMPATIBILITY_UNSUPPORTED, "", errors.New("unspecified logical type")
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

func mapPrimitive(kind datav1.PrimitiveKind) (string, datav1.MappingCompatibility, string, error) {
	lossless := datav1.MappingCompatibility_MAPPING_COMPATIBILITY_LOSSLESS
	switch kind {
	case datav1.PrimitiveKind_PRIMITIVE_KIND_DOUBLE:
		return "DOUBLE PRECISION", lossless, "protobuf double maps losslessly to PostgreSQL DOUBLE PRECISION", nil
	case datav1.PrimitiveKind_PRIMITIVE_KIND_FLOAT:
		return "REAL", lossless, "protobuf float maps losslessly to PostgreSQL REAL", nil
	case datav1.PrimitiveKind_PRIMITIVE_KIND_INT64,
		datav1.PrimitiveKind_PRIMITIVE_KIND_SFIXED64,
		datav1.PrimitiveKind_PRIMITIVE_KIND_SINT64:
		return "BIGINT", lossless, "protobuf signed 64-bit integer maps losslessly to PostgreSQL BIGINT", nil
	case datav1.PrimitiveKind_PRIMITIVE_KIND_UINT64,
		datav1.PrimitiveKind_PRIMITIVE_KIND_FIXED64:
		return "NUMERIC(20,0)", datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_WIDENED,
			"protobuf unsigned 64-bit integer maps to PostgreSQL NUMERIC(20,0), which contains its full domain", nil
	case datav1.PrimitiveKind_PRIMITIVE_KIND_INT32,
		datav1.PrimitiveKind_PRIMITIVE_KIND_SFIXED32,
		datav1.PrimitiveKind_PRIMITIVE_KIND_SINT32:
		return "INTEGER", lossless, "protobuf signed 32-bit integer maps losslessly to PostgreSQL INTEGER", nil
	case datav1.PrimitiveKind_PRIMITIVE_KIND_FIXED32,
		datav1.PrimitiveKind_PRIMITIVE_KIND_UINT32:
		return "BIGINT", datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_WIDENED,
			"protobuf unsigned 32-bit integer widens to PostgreSQL BIGINT", nil
	case datav1.PrimitiveKind_PRIMITIVE_KIND_BOOL:
		return "BOOLEAN", lossless, "protobuf bool maps losslessly to PostgreSQL BOOLEAN", nil
	case datav1.PrimitiveKind_PRIMITIVE_KIND_STRING:
		return "TEXT", datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_REDUCED,
			"protobuf string maps to PostgreSQL TEXT, which cannot represent U+0000", nil
	case datav1.PrimitiveKind_PRIMITIVE_KIND_BYTES:
		return "BYTEA", lossless, "protobuf bytes maps losslessly to PostgreSQL BYTEA", nil
	default:
		return "", datav1.MappingCompatibility_MAPPING_COMPATIBILITY_UNSUPPORTED, "", fmt.Errorf("unsupported primitive kind %s", kind)
	}
}

func defaultExpression(field *datav1.Field) string {
	switch field.GetPresence() {
	case datav1.Presence_PRESENCE_REPEATED:
		return "'[]'::jsonb"
	case datav1.Presence_PRESENCE_MAP:
		return "'{}'::jsonb"
	case datav1.Presence_PRESENCE_IMPLICIT:
		return implicitDefault(field.GetType())
	default:
		return ""
	}
}

func implicitDefault(dataType *datav1.DataType) string {
	if dataType.GetEnum() != nil {
		return "0"
	}
	primitive := dataType.GetPrimitive()
	if primitive == nil {
		return ""
	}
	switch primitive.GetKind() {
	case datav1.PrimitiveKind_PRIMITIVE_KIND_BOOL:
		return "FALSE"
	case datav1.PrimitiveKind_PRIMITIVE_KIND_STRING:
		return "''"
	case datav1.PrimitiveKind_PRIMITIVE_KIND_BYTES:
		return "'\\x'::bytea"
	default:
		return "0"
	}
}

func nestedDiagnostics(dataType *datav1.DataType, path string) []*datav1.MappingDiagnostic {
	var diagnostics []*datav1.MappingDiagnostic
	var addEmbedded func(*datav1.Field, string)
	addEmbedded = func(field *datav1.Field, fieldPath string) {
		if field == nil || field.GetType() == nil {
			return
		}
		compatibility := datav1.MappingCompatibility_MAPPING_COMPATIBILITY_REPRESENTATION_CHANGED
		message := "logical child is embedded in its parent PostgreSQL JSONB column rather than projected as a relational column"
		if canContainNUL(field.GetType()) {
			compatibility = datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_REDUCED
			message += "; PostgreSQL JSONB cannot represent the protobuf string value U+0000"
		}
		diagnostics = append(diagnostics, &datav1.MappingDiagnostic{
			FieldPath:     fieldPath,
			Compatibility: compatibility,
			Message:       message,
		})
		switch kind := field.GetType().GetKind().(type) {
		case *datav1.DataType_Struct:
			for _, child := range kind.Struct.GetFields() {
				addEmbedded(child, joinPath(fieldPath, child.GetName()))
			}
		case *datav1.DataType_List:
			addEmbedded(kind.List.GetElement(), fieldPath+"[]")
		case *datav1.DataType_Map:
			addEmbedded(kind.Map.GetKey(), fieldPath+".key")
			addEmbedded(kind.Map.GetValue(), fieldPath+".value")
		}
	}
	switch kind := dataType.GetKind().(type) {
	case *datav1.DataType_Struct:
		for _, child := range kind.Struct.GetFields() {
			addEmbedded(child, joinPath(path, child.GetName()))
		}
	case *datav1.DataType_List:
		addEmbedded(kind.List.GetElement(), path+"[]")
	case *datav1.DataType_Map:
		addEmbedded(kind.Map.GetKey(), path+".key")
		addEmbedded(kind.Map.GetValue(), path+".value")
	}
	return diagnostics
}

func canContainNUL(dataType *datav1.DataType) bool {
	if dataType == nil {
		return false
	}
	switch kind := dataType.GetKind().(type) {
	case *datav1.DataType_Primitive:
		return kind.Primitive.GetKind() == datav1.PrimitiveKind_PRIMITIVE_KIND_STRING
	case *datav1.DataType_Struct:
		for _, field := range kind.Struct.GetFields() {
			if canContainNUL(field.GetType()) {
				return true
			}
		}
	case *datav1.DataType_List:
		return canContainNUL(kind.List.GetElement().GetType())
	case *datav1.DataType_Map:
		return canContainNUL(kind.Map.GetKey().GetType()) || canContainNUL(kind.Map.GetValue().GetType())
	case *datav1.DataType_Json:
		return true
	}
	return false
}

func oneofChecks(tableName string, columns []column) ([]string, error) {
	groups := make(map[string][]string)
	var order []string
	for _, col := range columns {
		if col.oneof == "" {
			continue
		}
		if _, exists := groups[col.oneof]; !exists {
			order = append(order, col.oneof)
		}
		groups[col.oneof] = append(groups[col.oneof], col.name)
	}
	checks := make([]string, 0, len(order))
	for _, oneof := range order {
		members := groups[oneof]
		if len(members) < 2 {
			continue
		}
		constraint, err := identifier(tableName + "_" + oneof + "_oneof_check")
		if err != nil {
			return nil, fmt.Errorf("postgres: oneof %q constraint: %w", oneof, err)
		}
		quoted := make([]string, len(members))
		for i, member := range members {
			quoted[i] = quoteIdentifier(member)
		}
		checks = append(checks, fmt.Sprintf("  CONSTRAINT %s CHECK (num_nonnulls(%s) <= 1)",
			quoteIdentifier(constraint), strings.Join(quoted, ", ")))
	}
	return checks, nil
}

func identifier(value string) (string, error) {
	if value == "" {
		return "", errors.New("empty identifier")
	}
	if err := validateText(value); err != nil {
		return "", err
	}
	if len(value) <= maxIdentifierBytes {
		return value, nil
	}
	digest := sha256.Sum256([]byte(value))
	suffix := "_" + hex.EncodeToString(digest[:6])
	limit := maxIdentifierBytes - len(suffix)
	var prefix strings.Builder
	for _, r := range value {
		if prefix.Len()+utf8.RuneLen(r) > limit {
			break
		}
		prefix.WriteRune(r)
	}
	return prefix.String() + suffix, nil
}

func quoteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func quoteLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func validateText(value string) error {
	if !utf8.ValidString(value) {
		return errors.New("invalid UTF-8")
	}
	if strings.IndexByte(value, 0) >= 0 {
		return errors.New("NUL byte is not allowed")
	}
	return nil
}

func joinPath(parent, child string) string {
	if parent == "" {
		return child
	}
	return parent + "." + child
}
