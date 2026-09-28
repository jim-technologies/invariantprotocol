// Package sqlite renders the canonical data schema as SQLite desired-state DDL.
package sqlite

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	datav1 "github.com/jim-technologies/invariantprotocol/go/gen/invariant/data/v1"
)

// DDL emits one table, including presence, storage-class and value-domain checks.
// It does not infer keys, indexes, migrations, or row conversion functions.
func DDL(dataset *datav1.DatasetSchema) (string, []*datav1.MappingDiagnostic, error) {
	if dataset == nil || len(dataset.GetFields()) == 0 {
		return "", nil, errors.New("sqlite: a dataset with at least one field is required")
	}
	if err := validText(dataset.GetName()); err != nil || dataset.GetName() == "" || strings.HasPrefix(asciiLower(dataset.GetName()), "sqlite_") {
		return "", nil, errors.New("sqlite: invalid or reserved table name")
	}
	if err := validText(dataset.GetDescription()); err != nil {
		return "", nil, fmt.Errorf("sqlite: table description: %w", err)
	}
	var definitions []string
	var diagnostics []*datav1.MappingDiagnostic
	seen := map[string]bool{}
	oneofs := map[string][]string{}
	var oneofOrder []string
	for _, field := range dataset.GetFields() {
		if field == nil || field.GetType() == nil || field.GetName() == "" {
			return "", diagnostics, errors.New("sqlite: field requires a name and logical type")
		}
		if err := validText(field.GetName()); err != nil {
			return "", diagnostics, fmt.Errorf("sqlite: field name: %w", err)
		}
		if err := validText(field.GetDescription()); err != nil {
			return "", diagnostics, fmt.Errorf("sqlite: field description: %w", err)
		}
		folded := asciiLower(field.GetName())
		if seen[folded] {
			return "", diagnostics, fmt.Errorf("sqlite: case-insensitive column collision %q", field.GetName())
		}
		seen[folded] = true
		name := quote(field.GetName())
		typeName, check, compatibility, message, err := mapping(field.GetType(), name)
		if err != nil {
			return "", diagnostics, fmt.Errorf("sqlite: field %q: %w", field.GetName(), err)
		}
		defaultSQL, err := defaultExpression(field)
		if err != nil {
			return "", diagnostics, fmt.Errorf("sqlite: field %q: %w", field.GetName(), err)
		}
		definition := comment(field.GetDescription(), "  ") + "  " + name + " " + typeName
		if !field.GetNullable() {
			definition += " NOT NULL"
		}
		if defaultSQL != "" {
			definition += " DEFAULT " + defaultSQL
		}
		definition += " CHECK (" + name + " IS NULL OR (" + check + "))"
		definitions = append(definitions, definition)
		diagnostics = append(diagnostics, &datav1.MappingDiagnostic{
			FieldPath: field.GetName(), Compatibility: compatibility, Message: message,
		})
		nested, err := nestedDiagnostics(field.GetType(), field.GetName())
		if err != nil {
			return "", diagnostics, err
		}
		diagnostics = append(diagnostics, nested...)
		if group := field.GetOneof(); group != "" {
			if _, ok := oneofs[group]; !ok {
				oneofOrder = append(oneofOrder, group)
			}
			oneofs[group] = append(oneofs[group], "("+name+" IS NOT NULL)")
		}
	}
	for _, group := range oneofOrder {
		definitions = append(definitions, "  CHECK (("+strings.Join(oneofs[group], " + ")+") <= 1)")
	}
	return comment(dataset.GetDescription(), "") + "CREATE TABLE " + quote(dataset.GetName()) + " (\n" + strings.Join(definitions, ",\n") + "\n);\n", diagnostics, nil
}

func mapping(typ *datav1.DataType, name string) (string, string, datav1.MappingCompatibility, string, error) {
	lossless := datav1.MappingCompatibility_MAPPING_COMPATIBILITY_LOSSLESS
	representation := datav1.MappingCompatibility_MAPPING_COMPATIBILITY_REPRESENTATION_CHANGED
	integer := "typeof(" + name + ") = 'integer'"
	text := "typeof(" + name + ") = 'text'"
	blob := "typeof(" + name + ") = 'blob'"
	switch kind := typ.GetKind().(type) {
	case *datav1.DataType_Primitive:
		switch kind.Primitive.GetKind() {
		case datav1.PrimitiveKind_PRIMITIVE_KIND_DOUBLE, datav1.PrimitiveKind_PRIMITIVE_KIND_FLOAT:
			return "REAL", "typeof(" + name + ") = 'real'", datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_REDUCED,
				"SQLite REAL stores binary64 values; NaN becomes NULL and cannot preserve a present protobuf NaN; float32 writers must enforce their source precision", nil
		case datav1.PrimitiveKind_PRIMITIVE_KIND_INT64, datav1.PrimitiveKind_PRIMITIVE_KIND_SINT64, datav1.PrimitiveKind_PRIMITIVE_KIND_SFIXED64:
			return "INTEGER", integer, lossless, "signed 64-bit integer with a SQLite storage-class check", nil
		case datav1.PrimitiveKind_PRIMITIVE_KIND_INT32, datav1.PrimitiveKind_PRIMITIVE_KIND_SINT32, datav1.PrimitiveKind_PRIMITIVE_KIND_SFIXED32:
			return "INTEGER", integer + " AND " + name + " BETWEEN -2147483648 AND 2147483647", lossless, "signed 32-bit integer with exact range checks", nil
		case datav1.PrimitiveKind_PRIMITIVE_KIND_UINT32, datav1.PrimitiveKind_PRIMITIVE_KIND_FIXED32:
			return "INTEGER", integer + " AND " + name + " BETWEEN 0 AND 4294967295", lossless, "unsigned 32-bit integer with exact range checks", nil
		case datav1.PrimitiveKind_PRIMITIVE_KIND_UINT64, datav1.PrimitiveKind_PRIMITIVE_KIND_FIXED64:
			check := text + " AND " + unsignedText(name) + " AND (length(" + name + ") < 20 OR (length(" + name + ") = 20 AND " + name + " <= '18446744073709551615' COLLATE BINARY))"
			return "TEXT", check, representation, "uint64 uses canonical decimal TEXT with exact bounds; bind text without an intermediate REAL or signed integer; lexical order is not numeric order", nil
		case datav1.PrimitiveKind_PRIMITIVE_KIND_BOOL:
			return "INTEGER", integer + " AND " + name + " IN (0, 1)", representation, "boolean represented by INTEGER 0 or 1", nil
		case datav1.PrimitiveKind_PRIMITIVE_KIND_STRING:
			return "TEXT", text, lossless, "protobuf string stored as TEXT, including embedded U+0000", nil
		case datav1.PrimitiveKind_PRIMITIVE_KIND_BYTES:
			return "BLOB", blob, lossless, "protobuf bytes stored as BLOB", nil
		}
	case *datav1.DataType_Enum:
		if kind.Enum == nil || len(kind.Enum.GetValues()) == 0 {
			break
		}
		check := integer + " AND " + name + " BETWEEN -2147483648 AND 2147483647"
		if kind.Enum.GetClosed() {
			var values []string
			for _, value := range kind.Enum.GetValues() {
				values = append(values, strconv.Itoa(int(value.GetNumber())))
			}
			check += " AND " + name + " IN (" + strings.Join(values, ", ") + ")"
		}
		return "INTEGER", check, lossless, "enum numbers retain their signed 32-bit domain; closed enums also check declared values", nil
	case *datav1.DataType_Timestamp:
		if kind.Timestamp.GetUnit() != datav1.TimeUnit_TIME_UNIT_NANOSECOND || kind.Timestamp.GetTimezone() != "UTC" {
			break
		}
		return "INTEGER", integer + " AND " + name + " BETWEEN -62135596800000000 AND 253402300799999999", datav1.MappingCompatibility_MAPPING_COMPATIBILITY_PRECISION_REDUCED,
			"Timestamp uses signed Unix microseconds with the complete protobuf year range; sub-microsecond precision requires explicit writer conversion", nil
	case *datav1.DataType_Duration:
		if kind.Duration.GetUnit() != datav1.TimeUnit_TIME_UNIT_NANOSECOND {
			break
		}
		return "INTEGER", integer, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_REDUCED,
			"Duration uses exact signed nanoseconds; SQLite int64 cannot represent the complete protobuf Duration range", nil
	case *datav1.DataType_Decimal:
		p, s := kind.Decimal.GetPrecision(), kind.Decimal.GetScale()
		if p == 0 || p > 38 || s > p {
			break
		}
		magnitude := "(CASE WHEN substr(" + name + ", 1, 1) = '-' THEN substr(" + name + ", 2) ELSE " + name + " END)"
		whole := magnitude
		check := text + " AND instr(" + name + ", char(0)) = 0"
		zero := "0"
		if s > 0 {
			whole = "substr(" + magnitude + ", 1, length(" + magnitude + ") - " + strconv.Itoa(int(s+1)) + ")"
			fraction := "substr(" + magnitude + ", -" + strconv.Itoa(int(s)) + ")"
			check += " AND substr(" + magnitude + ", -" + strconv.Itoa(int(s+1)) + ", 1) = '.' AND length(" + fraction + ") = " + strconv.Itoa(int(s)) + " AND " + fraction + " NOT GLOB '*[^0-9]*'"
			zero += "." + strings.Repeat("0", int(s))
		}
		check += " AND " + unsignedText(whole)
		if p == s {
			check += " AND " + whole + " = '0'"
		} else {
			check += " AND length(" + whole + ") <= " + strconv.Itoa(int(p-s))
		}
		check += " AND " + name + " != '-" + zero + "'"
		return "TEXT", check, lossless, "canonical fixed-scale decimal text with exact precision, scale, sign and zero checks; bind text, arithmetic is application-owned", nil
	case *datav1.DataType_Uuid:
		if kind.Uuid == nil {
			break
		}
		check := text + " AND length(" + name + ") = 36 AND instr(" + name + ", char(0)) = 0 AND substr(" + name + ", 9, 1) = '-' AND substr(" + name + ", 14, 1) = '-' AND substr(" + name + ", 19, 1) = '-' AND substr(" + name + ", 24, 1) = '-' AND length(replace(" + name + ", '-', '')) = 32 AND replace(" + name + ", '-', '') NOT GLOB '*[^0-9a-f]*'"
		return "TEXT", check, lossless, "canonical lowercase UUID text with exact hexadecimal and hyphen checks", nil
	case *datav1.DataType_FixedBytes:
		width := kind.FixedBytes.GetByteLength()
		if width == 0 || width > math.MaxInt32 {
			break
		}
		return "BLOB", blob + " AND length(" + name + ") = " + strconv.Itoa(int(width)), lossless, "fixed-width bytes with a BLOB length check", nil
	case *datav1.DataType_Struct, *datav1.DataType_List, *datav1.DataType_Map, *datav1.DataType_Json:
		shape := "object"
		fixed := uint32(0)
		switch value := typ.GetKind().(type) {
		case *datav1.DataType_Struct:
			if value.Struct == nil {
				return "", "", 0, "", errors.New("invalid struct type")
			}
		case *datav1.DataType_List:
			shape, fixed = "array", value.List.GetFixedLength()
			if value.List == nil || fixed > math.MaxInt32 {
				return "", "", 0, "", errors.New("invalid list type")
			}
		case *datav1.DataType_Map:
			if value.Map == nil {
				return "", "", 0, "", errors.New("invalid map type")
			}
		case *datav1.DataType_Json:
			switch value.Json.GetKind() {
			case datav1.JsonKind_JSON_KIND_VALUE:
				shape = ""
			case datav1.JsonKind_JSON_KIND_LIST_VALUE:
				shape = "array"
			case datav1.JsonKind_JSON_KIND_ANY, datav1.JsonKind_JSON_KIND_STRUCT:
			default:
				return "", "", 0, "", errors.New("unsupported dynamic JSON kind")
			}
		}
		valid := "1"
		if shape != "" {
			valid = "json_type(" + name + ") = '" + shape + "'"
		}
		if fixed > 0 {
			valid += " AND json_array_length(" + name + ") = " + strconv.Itoa(int(fixed))
		}
		check := text + " AND instr(" + name + ", char(0)) = 0 AND CASE WHEN json_valid(" + name + ") THEN " + valid + " ELSE 0 END"
		return "TEXT", check, datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_WIDENED,
			"ProtoJSON TEXT checks JSON syntax, top-level shape and any top-level fixed length; nested types, presence, refinements and Any resolution remain writer-owned; dynamic JSON numbers must be finite", nil
	}
	return "", "", datav1.MappingCompatibility_MAPPING_COMPATIBILITY_UNSUPPORTED, "", errors.New("unsupported or invalid logical type")
}

func unsignedText(value string) string {
	return "length(" + value + ") > 0 AND instr(" + value + ", char(0)) = 0 AND " + value + " NOT GLOB '*[^0-9]*' AND (" + value + " = '0' OR substr(" + value + ", 1, 1) BETWEEN '1' AND '9')"
}

func defaultExpression(field *datav1.Field) (string, error) {
	switch field.GetPresence() {
	case datav1.Presence_PRESENCE_REPEATED:
		if field.GetType().GetList().GetFixedLength() == 0 {
			return "'[]'", nil
		}
	case datav1.Presence_PRESENCE_MAP:
		return "'{}'", nil
	case datav1.Presence_PRESENCE_IMPLICIT:
		if field.GetType().GetEnum() != nil {
			return "0", nil
		}
		if field.GetType().GetPrimitive() == nil {
			return "", errors.New("implicit refined or composite field has no protobuf default")
		}
		switch field.GetType().GetPrimitive().GetKind() {
		case datav1.PrimitiveKind_PRIMITIVE_KIND_STRING:
			return "''", nil
		case datav1.PrimitiveKind_PRIMITIVE_KIND_BYTES:
			return "X''", nil
		case datav1.PrimitiveKind_PRIMITIVE_KIND_UINT64, datav1.PrimitiveKind_PRIMITIVE_KIND_FIXED64:
			return "'0'", nil
		default:
			return "0", nil
		}
	}
	return "", nil
}

func nestedDiagnostics(typ *datav1.DataType, path string) ([]*datav1.MappingDiagnostic, error) {
	var fields []*datav1.Field
	var paths []string
	switch kind := typ.GetKind().(type) {
	case *datav1.DataType_Struct:
		fields = kind.Struct.GetFields()
		for _, field := range fields {
			paths = append(paths, path+"."+field.GetName())
		}
	case *datav1.DataType_List:
		fields, paths = []*datav1.Field{kind.List.GetElement()}, []string{path + "[]"}
	case *datav1.DataType_Map:
		fields, paths = []*datav1.Field{kind.Map.GetKey(), kind.Map.GetValue()}, []string{path + ".key", path + ".value"}
	}
	var result []*datav1.MappingDiagnostic
	for index, field := range fields {
		if field.GetType() == nil {
			return nil, fmt.Errorf("sqlite: field %q: missing nested logical type", paths[index])
		}
		if _, _, _, _, err := mapping(field.GetType(), quote(field.GetName())); err != nil {
			return nil, fmt.Errorf("sqlite: field %q: %w", paths[index], err)
		}
		result = append(result, &datav1.MappingDiagnostic{
			FieldPath: paths[index], Compatibility: datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_WIDENED,
			Message: "nested ProtoJSON value: SQLite does not enforce this node's logical domain, presence or refinements; writer validation is required",
		})
		nested, err := nestedDiagnostics(field.GetType(), paths[index])
		if err != nil {
			return nil, err
		}
		result = append(result, nested...)
	}
	return result, nil
}

func quote(value string) string { return `"` + strings.ReplaceAll(value, `"`, `""`) + `"` }

func asciiLower(value string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, value)
}

func validText(value string) error {
	if !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
		return errors.New("text must be valid UTF-8 without NUL")
	}
	return nil
}

func comment(value, indent string) string {
	if value == "" {
		return ""
	}
	value = strings.ReplaceAll(value, "\r", "\n")
	return indent + "-- " + strings.ReplaceAll(value, "\n", "\n"+indent+"-- ") + "\n"
}
