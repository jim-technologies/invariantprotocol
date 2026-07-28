// Package clickhouse projects Invariant's canonical protobuf data schema into
// ClickHouse column and constraint declarations. It deliberately does not
// choose a table engine, sorting key, partitioning, or other physical policy.
package clickhouse

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	datav1 "github.com/jim-technologies/invariantprotocol/go/gen/invariant/data/v1"
)

const (
	maxFixedStringBytes        = 256
	reservedNamePrefix         = "__invariant_"
	constraintValuePlaceholder = "\x00invariant-value\x00"
	projectionValuePlaceholder = "{value}"
)

// Column is one top-level ClickHouse column declaration.
type Column struct {
	Name              string
	Type              string
	DefaultExpression string
	Comment           string
	FieldPath         string
	StableID          int32
	Synthetic         bool
}

// Constraint is one ClickHouse CHECK constraint required by the logical
// schema. Callers must apply Constraints together with Columns.
type Constraint struct {
	Name       string
	Expression string
	FieldPath  string
}

// TableSchema is the ClickHouse projection for one canonical dataset.
// ColumnDeclarations returns a table-body fragment; this type does not select
// a database, table engine, ORDER BY key, or any other physical layout.
type TableSchema struct {
	Dataset       string
	SourceMessage string
	Description   string
	Columns       []Column
	Constraints   []Constraint
}

// ColumnDeclarations returns deterministic ClickHouse column and constraint
// declarations without surrounding parentheses or a CREATE TABLE statement.
func (schema *TableSchema) ColumnDeclarations() string {
	if schema == nil {
		return ""
	}
	declarations := make([]string, 0, len(schema.Columns)+len(schema.Constraints))
	for _, column := range schema.Columns {
		declaration := "  " + quoteIdentifier(column.Name) + " " + column.Type
		if column.DefaultExpression != "" {
			declaration += " DEFAULT " + column.DefaultExpression
		}
		if column.Comment != "" {
			declaration += " COMMENT " + quoteLiteral(column.Comment)
		}
		declarations = append(declarations, declaration)
	}
	for _, constraint := range schema.Constraints {
		declarations = append(declarations,
			"  CONSTRAINT "+quoteIdentifier(constraint.Name)+" CHECK "+constraint.Expression,
		)
	}
	return strings.Join(declarations, ",\n")
}

type fieldDeclaration struct {
	name              string
	sqlType           string
	defaultExpression string
	comment           string
	fieldPath         string
	stableID          int32
	synthetic         bool
}

type validation struct {
	fieldPath  string
	kind       string
	expression string
}

type mappedType struct {
	sqlType       string
	compatibility datav1.MappingCompatibility
	message       string
	diagnostics   []*datav1.MappingDiagnostic
	validations   []validation
	composite     bool
}

// Schema maps one canonical dataset to ClickHouse column declarations. Every
// logical field, including collection children, produces a diagnostic.
func Schema(dataset *datav1.DatasetSchema) (*TableSchema, []*datav1.MappingDiagnostic, error) {
	if dataset == nil {
		return nil, nil, errors.New("clickhouse: nil dataset schema")
	}
	if err := validateIdentifier(dataset.GetName()); err != nil {
		return nil, nil, fmt.Errorf("clickhouse: dataset name: %w", err)
	}
	if err := validateText(dataset.GetDescription()); err != nil {
		return nil, nil, fmt.Errorf("clickhouse: dataset description: %w", err)
	}

	fields, validations, diagnostics, err := mapScope(dataset.GetFields(), "", true)
	if err != nil {
		return nil, diagnostics, err
	}
	if len(fields) == 0 {
		fields = append(fields, fieldDeclaration{
			name:              "__invariant_unit",
			sqlType:           "Bool",
			defaultExpression: "false",
			fieldPath:         "<dataset>",
			synthetic:         true,
		})
		validations = append(validations, validation{
			fieldPath:  "<dataset>",
			kind:       "unit",
			expression: quoteIdentifier("__invariant_unit") + " = false",
		})
	}

	columns := make([]Column, len(fields))
	for index, field := range fields {
		columns[index] = Column{
			Name:              field.name,
			Type:              field.sqlType,
			DefaultExpression: field.defaultExpression,
			Comment:           field.comment,
			FieldPath:         field.fieldPath,
			StableID:          field.stableID,
			Synthetic:         field.synthetic,
		}
	}
	constraints := make([]Constraint, len(validations))
	seenConstraints := make(map[string]string, len(validations))
	for index, check := range validations {
		name := "invariant." + check.fieldPath + "." + check.kind
		if previous, duplicate := seenConstraints[name]; duplicate {
			return nil, diagnostics, fmt.Errorf(
				"clickhouse: constraints for %q and %q have the same name %q",
				previous, check.fieldPath, name,
			)
		}
		seenConstraints[name] = check.fieldPath
		constraints[index] = Constraint{
			Name:       name,
			Expression: check.expression,
			FieldPath:  check.fieldPath,
		}
	}
	return &TableSchema{
		Dataset:       dataset.GetName(),
		SourceMessage: dataset.GetSourceMessage(),
		Description:   dataset.GetDescription(),
		Columns:       columns,
		Constraints:   constraints,
	}, diagnostics, nil
}

func mapScope(
	fields []*datav1.Field,
	path string,
	topLevel bool,
) ([]fieldDeclaration, []validation, []*datav1.MappingDiagnostic, error) {
	seenNames := make(map[string]string, len(fields))
	for _, field := range fields {
		if field == nil {
			diagnostic := unsupportedDiagnostic(path, "logical scope contains a nil field")
			return nil, nil, []*datav1.MappingDiagnostic{diagnostic}, errors.New("clickhouse: logical scope contains a nil field")
		}
		name := field.GetName()
		fieldPath := joinPath(path, name)
		if err := validateIdentifier(name); err != nil {
			diagnostic := unsupportedDiagnostic(fieldPath, err.Error())
			return nil, nil, []*datav1.MappingDiagnostic{diagnostic}, fmt.Errorf("clickhouse: field %q: %w", fieldPath, err)
		}
		if strings.HasPrefix(name, reservedNamePrefix) {
			message := fmt.Sprintf(
				"storage name %q uses the ClickHouse renderer's reserved %q namespace",
				name, reservedNamePrefix,
			)
			diagnostic := unsupportedDiagnostic(fieldPath, message)
			return nil, nil, []*datav1.MappingDiagnostic{diagnostic},
				fmt.Errorf("clickhouse: field %q: %s", fieldPath, message)
		}
		if !topLevel && name == "null" {
			message := "ClickHouse reserves the Tuple element name \"null\""
			diagnostic := unsupportedDiagnostic(fieldPath, message)
			return nil, nil, []*datav1.MappingDiagnostic{diagnostic}, fmt.Errorf("clickhouse: field %q: %s", fieldPath, message)
		}
		if previous, duplicate := seenNames[name]; duplicate {
			message := fmt.Sprintf("storage names %q and %q collide as ClickHouse identifier %q", previous, fieldPath, name)
			diagnostic := unsupportedDiagnostic(fieldPath, message)
			return nil, nil, []*datav1.MappingDiagnostic{diagnostic}, fmt.Errorf("clickhouse: %s", message)
		}
		seenNames[name] = fieldPath
	}

	type oneofGroup struct {
		name       string
		caseName   string
		fieldPath  string
		members    []*datav1.Field
		numbers    []uint32
		firstIndex int
	}
	groups := make(map[string]*oneofGroup)
	for index, field := range fields {
		fieldPath := joinPath(path, field.GetName())
		if field.GetOneof() == "" {
			if field.GetPresence() == datav1.Presence_PRESENCE_ONEOF {
				message := "oneof presence is missing its oneof name"
				return nil, nil, []*datav1.MappingDiagnostic{unsupportedDiagnostic(fieldPath, message)},
					fmt.Errorf("clickhouse: field %q: %s", fieldPath, message)
			}
			continue
		}
		if field.GetPresence() != datav1.Presence_PRESENCE_ONEOF || !field.GetNullable() {
			message := fmt.Sprintf("oneof member must have oneof presence and nullable=true, got %s and nullable=%t",
				field.GetPresence(), field.GetNullable())
			return nil, nil, []*datav1.MappingDiagnostic{unsupportedDiagnostic(fieldPath, message)},
				fmt.Errorf("clickhouse: field %q: %s", fieldPath, message)
		}
		group := groups[field.GetOneof()]
		if group == nil {
			caseName := oneofCaseName(field.GetOneof())
			if err := validateIdentifier(caseName); err != nil {
				message := "derived oneof discriminator: " + err.Error()
				return nil, nil, []*datav1.MappingDiagnostic{unsupportedDiagnostic(fieldPath, message)},
					fmt.Errorf("clickhouse: field %q: %s", fieldPath, message)
			}
			group = &oneofGroup{
				name:       field.GetOneof(),
				caseName:   caseName,
				fieldPath:  joinPath(path, field.GetOneof()),
				firstIndex: index,
			}
			groups[field.GetOneof()] = group
		}
		if previous, collision := seenNames[group.caseName]; collision {
			message := fmt.Sprintf(
				"oneof %q discriminator %q collides with storage field %q",
				group.name, group.caseName, previous,
			)
			return nil, nil, []*datav1.MappingDiagnostic{unsupportedDiagnostic(group.fieldPath, message)},
				fmt.Errorf("clickhouse: %s", message)
		}
		numberPath := field.GetProtoNumberPath()
		if len(numberPath) == 0 || numberPath[len(numberPath)-1] == 0 {
			message := "oneof member has no protobuf field number"
			return nil, nil, []*datav1.MappingDiagnostic{unsupportedDiagnostic(fieldPath, message)},
				fmt.Errorf("clickhouse: field %q: %s", fieldPath, message)
		}
		number := numberPath[len(numberPath)-1]
		if slices.Contains(group.numbers, number) {
			message := fmt.Sprintf("oneof %q repeats protobuf field number %d", group.name, number)
			return nil, nil, []*datav1.MappingDiagnostic{unsupportedDiagnostic(fieldPath, message)},
				fmt.Errorf("clickhouse: field %q: %s", fieldPath, message)
		}
		group.members = append(group.members, field)
		group.numbers = append(group.numbers, number)
	}

	declarations := make([]fieldDeclaration, 0, len(fields)+len(groups))
	var validations []validation
	var diagnostics []*datav1.MappingDiagnostic
	emittedGroups := make(map[string]bool, len(groups))
	for index, field := range fields {
		fieldPath := joinPath(path, field.GetName())
		if group := groups[field.GetOneof()]; group != nil && !emittedGroups[group.name] && group.firstIndex == index {
			emittedGroups[group.name] = true
			members := make([]string, len(group.members))
			numbers := slices.Clone(group.numbers)
			slices.Sort(numbers)
			for memberIndex, member := range group.members {
				numberPath := member.GetProtoNumberPath()
				members[memberIndex] = fmt.Sprintf("%d=%s", numberPath[len(numberPath)-1], member.GetName())
			}
			declarations = append(declarations, fieldDeclaration{
				name:              group.caseName,
				sqlType:           "Int32",
				defaultExpression: "0",
				comment: "Invariant oneof discriminator for " + group.name +
					": 0=unset, " + strings.Join(members, ", "),
				fieldPath: group.fieldPath,
				synthetic: true,
			})
			caseExpression := scopeElementExpression(group.caseName, topLevel)
			allowed := make([]string, 0, len(numbers)+1)
			allowed = append(allowed, "0")
			for _, number := range numbers {
				allowed = append(allowed, strconv.FormatUint(uint64(number), 10))
			}
			validations = append(validations, validation{
				fieldPath:  group.fieldPath,
				kind:       "oneof_case",
				expression: caseExpression + " IN (" + strings.Join(allowed, ", ") + ")",
			})
		}

		mapped, fieldDiagnostics, err := mapField(field, fieldPath)
		diagnostics = append(diagnostics, fieldDiagnostics...)
		if err != nil {
			return nil, nil, diagnostics, err
		}
		if group := groups[field.GetOneof()]; group != nil {
			numberPath := field.GetProtoNumberPath()
			caseExpression := scopeElementExpression(group.caseName, topLevel)
			memberExpression := scopeElementExpression(field.GetName(), topLevel)
			presentExpression := tupleElementExpression(memberExpression, "present")
			selected := caseExpression + " = " + strconv.FormatUint(uint64(numberPath[len(numberPath)-1]), 10)
			for checkIndex := range mapped.validations {
				mapped.validations[checkIndex].expression = replaceValue(
					mapped.validations[checkIndex].expression,
					memberExpression,
				)
			}
			validations = append(validations, validation{
				fieldPath: fieldPath,
				kind:      "oneof_presence",
				expression: "(" + selected + ") = " +
					presentExpression,
			})
		} else {
			for checkIndex := range mapped.validations {
				mapped.validations[checkIndex].expression = replaceValue(
					mapped.validations[checkIndex].expression,
					scopeElementExpression(field.GetName(), topLevel),
				)
			}
		}
		declarations = append(declarations, fieldDeclaration{
			name:              field.GetName(),
			sqlType:           mapped.sqlType,
			defaultExpression: mapped.defaultExpression,
			comment:           field.GetDescription(),
			fieldPath:         fieldPath,
			stableID:          field.GetStableId(),
		})
		validations = append(validations, mapped.validations...)
	}

	return declarations, validations, diagnostics, nil
}

type mappedField struct {
	sqlType           string
	defaultExpression string
	validations       []validation
}

func mapField(field *datav1.Field, path string) (mappedField, []*datav1.MappingDiagnostic, error) {
	if field.GetType() == nil {
		message := "field has no logical type"
		return mappedField{}, []*datav1.MappingDiagnostic{unsupportedDiagnostic(path, message)},
			fmt.Errorf("clickhouse: field %q: %s", path, message)
	}
	if err := validateText(field.GetDescription()); err != nil {
		message := "description: " + err.Error()
		return mappedField{}, []*datav1.MappingDiagnostic{unsupportedDiagnostic(path, message)},
			fmt.Errorf("clickhouse: field %q %s", path, message)
	}

	mapped, err := mapType(field.GetType(), path)
	if err != nil {
		diagnostics := append([]*datav1.MappingDiagnostic{
			unsupportedDiagnostic(path, err.Error()),
		}, mapped.diagnostics...)
		return mappedField{}, diagnostics, fmt.Errorf("clickhouse: field %q: %w", path, err)
	}
	compatibility := mapped.compatibility
	message := mapped.message
	result := mappedField{sqlType: mapped.sqlType, validations: mapped.validations}

	switch field.GetPresence() {
	case datav1.Presence_PRESENCE_IMPLICIT:
		if field.GetNullable() || !implicitCapable(field.GetType()) {
			err := errors.New("implicit presence requires a non-null primitive or enum")
			return mappedField{}, append([]*datav1.MappingDiagnostic{unsupportedDiagnostic(path, err.Error())}, mapped.diagnostics...),
				fmt.Errorf("clickhouse: field %q: %w", path, err)
		}
		result.defaultExpression = implicitDefault(field.GetType())

	case datav1.Presence_PRESENCE_EXPLICIT:
		if !field.GetNullable() {
			err := errors.New("explicit presence requires nullable=true")
			return mappedField{}, append([]*datav1.MappingDiagnostic{unsupportedDiagnostic(path, err.Error())}, mapped.diagnostics...),
				fmt.Errorf("clickhouse: field %q: %w", path, err)
		}
		compatibility = preserveStrongerCompatibility(compatibility)
		if mapped.composite {
			result.sqlType = "Tuple(`present` Bool, `value` " + mapped.sqlType + ")"
			for index := range result.validations {
				inner := replaceValue(
					result.validations[index].expression,
					tupleElementExpression(constraintValuePlaceholder, "value"),
				)
				result.validations[index].expression = "NOT " +
					tupleElementExpression(constraintValuePlaceholder, "present") + " OR (" + inner + ")"
			}
			message += "; explicit presence uses Tuple(present Bool, value T) because stable ClickHouse does not support Nullable(Array/Map) and Nullable(Tuple) is beta"
		} else {
			result.sqlType = "Nullable(" + mapped.sqlType + ")"
			result.defaultExpression = "NULL"
			for index := range result.validations {
				inner := replaceValue(result.validations[index].expression, "assumeNotNull("+constraintValuePlaceholder+")")
				result.validations[index].expression = "isNull(" + constraintValuePlaceholder + ") OR (" + inner + ")"
			}
			message += "; explicit presence maps to Nullable(T), preserving absent versus present-default"
		}

	case datav1.Presence_PRESENCE_REQUIRED:
		if field.GetNullable() {
			err := errors.New("required presence requires nullable=false")
			return mappedField{}, append([]*datav1.MappingDiagnostic{unsupportedDiagnostic(path, err.Error())}, mapped.diagnostics...),
				fmt.Errorf("clickhouse: field %q: %w", path, err)
		}
		compatibility = preserveStrongerCompatibility(compatibility)
		result.sqlType = "Tuple(`present` Bool, `value` " + mapped.sqlType + ")"
		for index := range result.validations {
			inner := replaceValue(
				result.validations[index].expression,
				tupleElementExpression(constraintValuePlaceholder, "value"),
			)
			result.validations[index].expression = "NOT " +
				tupleElementExpression(constraintValuePlaceholder, "present") + " OR (" + inner + ")"
		}
		result.validations = append([]validation{{
			fieldPath:  path,
			kind:       "required",
			expression: tupleElementExpression(constraintValuePlaceholder, "present"),
		}}, result.validations...)
		message += "; required presence uses Tuple(present Bool, value T) plus a CHECK so an omitted ClickHouse column cannot become an implicit type default"

	case datav1.Presence_PRESENCE_ONEOF:
		if field.GetOneof() == "" || !field.GetNullable() {
			err := errors.New("oneof presence requires a oneof name and nullable=true")
			return mappedField{}, append([]*datav1.MappingDiagnostic{unsupportedDiagnostic(path, err.Error())}, mapped.diagnostics...),
				fmt.Errorf("clickhouse: field %q: %w", path, err)
		}
		compatibility = preserveStrongerCompatibility(compatibility)
		result.sqlType = "Tuple(`present` Bool, `value` " + mapped.sqlType + ")"
		for index := range result.validations {
			inner := replaceValue(
				result.validations[index].expression,
				tupleElementExpression(constraintValuePlaceholder, "value"),
			)
			result.validations[index].expression = "NOT " +
				tupleElementExpression(constraintValuePlaceholder, "present") + " OR (" + inner + ")"
		}
		message += fmt.Sprintf(
			"; oneof %q uses a member Tuple(present Bool, value T) and sibling discriminator %q (0=unset, protobuf field number=selected member), preserving selected default values without making the synthetic discriminator the sole source of presence",
			field.GetOneof(), oneofCaseName(field.GetOneof()),
		)

	case datav1.Presence_PRESENCE_REPEATED:
		if field.GetNullable() || field.GetType().GetList() == nil {
			err := errors.New("repeated presence requires a non-null list")
			return mappedField{}, append([]*datav1.MappingDiagnostic{unsupportedDiagnostic(path, err.Error())}, mapped.diagnostics...),
				fmt.Errorf("clickhouse: field %q: %w", path, err)
		}
		if field.GetType().GetList().GetFixedLength() == 0 {
			result.defaultExpression = "[]"
		}

	case datav1.Presence_PRESENCE_MAP:
		if field.GetNullable() || field.GetType().GetMap() == nil {
			err := errors.New("map presence requires a non-null map")
			return mappedField{}, append([]*datav1.MappingDiagnostic{unsupportedDiagnostic(path, err.Error())}, mapped.diagnostics...),
				fmt.Errorf("clickhouse: field %q: %w", path, err)
		}
		result.defaultExpression = "map()"

	case datav1.Presence_PRESENCE_NOT_APPLICABLE:
		if field.GetSyntheticRole() == datav1.SyntheticRole_SYNTHETIC_ROLE_PROTO_FIELD || field.GetNullable() {
			err := errors.New("not-applicable presence is only valid for non-null synthetic collection children")
			return mappedField{}, append([]*datav1.MappingDiagnostic{unsupportedDiagnostic(path, err.Error())}, mapped.diagnostics...),
				fmt.Errorf("clickhouse: field %q: %w", path, err)
		}

	default:
		err := errors.New("protobuf presence is unspecified")
		return mappedField{}, append([]*datav1.MappingDiagnostic{unsupportedDiagnostic(path, err.Error())}, mapped.diagnostics...),
			fmt.Errorf("clickhouse: field %q: %w", path, err)
	}

	diagnostic := &datav1.MappingDiagnostic{
		FieldPath:     path,
		Compatibility: compatibility,
		Message:       message,
	}
	return result, append([]*datav1.MappingDiagnostic{diagnostic}, mapped.diagnostics...), nil
}

func mapType(dataType *datav1.DataType, path string) (mappedType, error) {
	switch kind := dataType.GetKind().(type) {
	case *datav1.DataType_Primitive:
		sqlType, message, err := mapPrimitive(kind.Primitive.GetKind())
		mapped := mappedType{
			sqlType:       sqlType,
			compatibility: datav1.MappingCompatibility_MAPPING_COMPATIBILITY_LOSSLESS,
			message:       message,
		}
		if kind.Primitive.GetKind() == datav1.PrimitiveKind_PRIMITIVE_KIND_STRING {
			mapped.validations = append(mapped.validations, validation{
				fieldPath:  path,
				kind:       "utf8",
				expression: "isValidUTF8(" + constraintValuePlaceholder + ")",
			})
		}
		return mapped, err

	case *datav1.DataType_Enum:
		if kind.Enum == nil || len(kind.Enum.GetValues()) == 0 {
			return mappedType{}, errors.New("enum has no declared values")
		}
		mapped := mappedType{
			sqlType:       "Int32",
			compatibility: datav1.MappingCompatibility_MAPPING_COMPATIBILITY_LOSSLESS,
			message:       "protobuf enum numeric values map losslessly to ClickHouse Int32; symbols and aliases remain canonical in SchemaBundle",
		}
		if kind.Enum.GetClosed() {
			numbers := make([]int32, 0, len(kind.Enum.GetValues()))
			for _, value := range kind.Enum.GetValues() {
				if value == nil {
					return mappedType{}, errors.New("enum contains a nil value")
				}
				if !slices.Contains(numbers, value.GetNumber()) {
					numbers = append(numbers, value.GetNumber())
				}
			}
			slices.Sort(numbers)
			allowed := make([]string, len(numbers))
			for index, number := range numbers {
				allowed[index] = strconv.FormatInt(int64(number), 10)
			}
			mapped.validations = append(mapped.validations, validation{
				fieldPath:  path,
				kind:       "closed_enum",
				expression: constraintValuePlaceholder + " IN (" + strings.Join(allowed, ", ") + ")",
			})
			mapped.message += "; a CHECK retains the closed numeric domain"
		}
		return mapped, nil

	case *datav1.DataType_Struct:
		if kind.Struct == nil {
			return mappedType{}, errors.New("invalid struct logical type")
		}
		fields, validations, diagnostics, err := mapScope(kind.Struct.GetFields(), path, false)
		if err != nil {
			return mappedType{diagnostics: diagnostics}, err
		}
		compatibility := datav1.MappingCompatibility_MAPPING_COMPATIBILITY_LOSSLESS
		message := "protobuf message maps to a ClickHouse named Tuple"
		if len(fields) == 0 {
			fields = append(fields, fieldDeclaration{
				name:      "__invariant_unit",
				sqlType:   "Bool",
				fieldPath: path,
				synthetic: true,
			})
			validations = append(validations, validation{
				fieldPath:  path,
				kind:       "unit",
				expression: tupleElementExpression(constraintValuePlaceholder, "__invariant_unit") + " = false",
			})
			compatibility = datav1.MappingCompatibility_MAPPING_COMPATIBILITY_REPRESENTATION_CHANGED
			message = "empty protobuf message uses a constrained synthetic Bool because ClickHouse has no empty Tuple"
		}
		elements := make([]string, len(fields))
		for index, field := range fields {
			elements[index] = quoteIdentifier(field.name) + " " + field.sqlType
		}
		return mappedType{
			sqlType:       "Tuple(" + strings.Join(elements, ", ") + ")",
			compatibility: compatibility,
			message:       message,
			diagnostics:   diagnostics,
			validations:   validations,
			composite:     true,
		}, nil

	case *datav1.DataType_List:
		if kind.List == nil || kind.List.GetElement() == nil {
			return mappedType{}, errors.New("list is missing its element")
		}
		element, diagnostics, err := mapField(kind.List.GetElement(), path+"[]")
		if err != nil {
			return mappedType{diagnostics: diagnostics}, err
		}
		validations := make([]validation, 0, len(element.validations)+1)
		message := "protobuf repeated field maps losslessly to ClickHouse Array(T)"
		if kind.List.GetFixedLength() != 0 {
			validations = append(validations, validation{
				fieldPath:  path,
				kind:       "fixed_list",
				expression: "length(" + constraintValuePlaceholder + ") = " + strconv.FormatUint(uint64(kind.List.GetFixedLength()), 10),
			})
			message = fmt.Sprintf(
				"fixed-cardinality protobuf repeated field maps losslessly to ClickHouse Array(T) with an exact length CHECK of %d",
				kind.List.GetFixedLength(),
			)
		}
		for _, check := range element.validations {
			validations = append(validations, check)
			validationIndex := len(validations) - 1
			validations[validationIndex].expression = "arrayAll(element -> (" +
				replaceValue(check.expression, "element") + "), " + constraintValuePlaceholder + ")"
		}
		return mappedType{
			sqlType:       "Array(" + element.sqlType + ")",
			compatibility: datav1.MappingCompatibility_MAPPING_COMPATIBILITY_LOSSLESS,
			message:       message,
			diagnostics:   diagnostics,
			validations:   validations,
			composite:     true,
		}, nil

	case *datav1.DataType_Map:
		if kind.Map == nil || kind.Map.GetKey() == nil || kind.Map.GetValue() == nil {
			return mappedType{}, errors.New("map is missing its key or value")
		}
		key, keyDiagnostics, err := mapField(kind.Map.GetKey(), path+".key")
		if err != nil {
			return mappedType{diagnostics: keyDiagnostics}, err
		}
		value, valueDiagnostics, err := mapField(kind.Map.GetValue(), path+".value")
		diagnostics := append(keyDiagnostics, valueDiagnostics...)
		if err != nil {
			return mappedType{diagnostics: diagnostics}, err
		}
		if strings.HasPrefix(key.sqlType, "Nullable(") {
			return mappedType{diagnostics: diagnostics}, errors.New("ClickHouse Map keys cannot be nullable")
		}
		validations := []validation{{
			fieldPath: path,
			kind:      "map_keys",
			expression: "length(mapKeys(" + constraintValuePlaceholder + ")) = " +
				"length(arrayDistinct(mapKeys(" + constraintValuePlaceholder + ")))",
		}}
		for _, check := range key.validations {
			validations = append(validations, validation{
				fieldPath: check.fieldPath,
				kind:      check.kind,
				expression: "arrayAll(key -> (" + replaceValue(check.expression, "key") +
					"), mapKeys(" + constraintValuePlaceholder + "))",
			})
		}
		for _, check := range value.validations {
			validations = append(validations, validation{
				fieldPath: check.fieldPath,
				kind:      check.kind,
				expression: "arrayAll(value -> (" + replaceValue(check.expression, "value") +
					"), mapValues(" + constraintValuePlaceholder + "))",
			})
		}
		return mappedType{
			sqlType:       "Map(" + key.sqlType + ", " + value.sqlType + ")",
			compatibility: datav1.MappingCompatibility_MAPPING_COMPATIBILITY_REPRESENTATION_CHANGED,
			message:       "protobuf map maps to ClickHouse Map(K, V); a recursive CHECK rejects ClickHouse's otherwise-permitted duplicate keys",
			diagnostics:   diagnostics,
			validations:   validations,
			composite:     true,
		}, nil

	case *datav1.DataType_Timestamp:
		if kind.Timestamp == nil ||
			kind.Timestamp.GetUnit() != datav1.TimeUnit_TIME_UNIT_NANOSECOND ||
			kind.Timestamp.GetTimezone() != "UTC" {
			return mappedType{}, errors.New("timestamp must use nanoseconds and UTC")
		}
		return mappedType{
			sqlType:       "DateTime64(9, 'UTC')",
			compatibility: datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_REDUCED,
			message:       "ClickHouse DateTime64(9, 'UTC') preserves UTC nanoseconds but only covers approximately 1677-09-21 through 2262-04-11, narrower than protobuf Timestamp",
		}, nil

	case *datav1.DataType_Duration:
		if kind.Duration == nil || kind.Duration.GetUnit() != datav1.TimeUnit_TIME_UNIT_NANOSECOND {
			return mappedType{}, errors.New("duration must use nanoseconds")
		}
		return mappedType{
			sqlType:       "Int64",
			compatibility: datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_REDUCED,
			message:       "ClickHouse has no duration type; exact nanoseconds use Int64, whose range is narrower than protobuf Duration",
		}, nil

	case *datav1.DataType_Json:
		if kind.Json == nil || kind.Json.GetKind() == datav1.JsonKind_JSON_KIND_UNSPECIFIED {
			return mappedType{}, errors.New("invalid protobuf JSON logical type")
		}
		return mappedType{
			sqlType:       "String",
			compatibility: datav1.MappingCompatibility_MAPPING_COMPATIBILITY_RANGE_REDUCED,
			message: fmt.Sprintf(
				"protobuf %s is encoded as canonical ProtoJSON text in ClickHouse String; %s",
				kind.Json.GetKind(), jsonRangeReduction(kind.Json.GetKind()),
			),
			validations: []validation{{
				fieldPath:  path,
				kind:       "json",
				expression: "isValidJSON(" + constraintValuePlaceholder + ")",
			}},
		}, nil

	case *datav1.DataType_Decimal:
		precision, scale, err := decimalParameters(kind.Decimal)
		if err != nil {
			return mappedType{}, err
		}
		return mappedType{
			sqlType:       fmt.Sprintf("Decimal(%d, %d)", precision, scale),
			compatibility: datav1.MappingCompatibility_MAPPING_COMPATIBILITY_REPRESENTATION_CHANGED,
			message:       fmt.Sprintf("canonical decimal text is decoded into ClickHouse Decimal(%d, %d); precision and scale are preserved", precision, scale),
		}, nil

	case *datav1.DataType_Uuid:
		if kind.Uuid == nil {
			return mappedType{}, errors.New("invalid UUID logical type")
		}
		return mappedType{
			sqlType:       "UUID",
			compatibility: datav1.MappingCompatibility_MAPPING_COMPATIBILITY_REPRESENTATION_CHANGED,
			message:       "canonical UUID text is decoded into ClickHouse UUID",
		}, nil

	case *datav1.DataType_FixedBytes:
		width, err := fixedByteWidth(kind.FixedBytes)
		if err != nil {
			return mappedType{}, err
		}
		return mappedType{
			sqlType:       fmt.Sprintf("FixedString(%d)", width),
			compatibility: datav1.MappingCompatibility_MAPPING_COMPATIBILITY_REPRESENTATION_CHANGED,
			message: fmt.Sprintf(
				"exact-width protobuf bytes map to ClickHouse FixedString(%d); publishers must validate exactly %d bytes before insertion because ClickHouse pads shorter input with NUL bytes",
				width, width,
			),
		}, nil

	default:
		return mappedType{}, errors.New("unspecified logical type")
	}
}

func mapPrimitive(kind datav1.PrimitiveKind) (string, string, error) {
	switch kind {
	case datav1.PrimitiveKind_PRIMITIVE_KIND_DOUBLE:
		return "Float64", "protobuf double maps losslessly to ClickHouse Float64", nil
	case datav1.PrimitiveKind_PRIMITIVE_KIND_FLOAT:
		return "Float32", "protobuf float maps losslessly to ClickHouse Float32", nil
	case datav1.PrimitiveKind_PRIMITIVE_KIND_INT64,
		datav1.PrimitiveKind_PRIMITIVE_KIND_SFIXED64,
		datav1.PrimitiveKind_PRIMITIVE_KIND_SINT64:
		return "Int64", "protobuf signed 64-bit integer maps losslessly to ClickHouse Int64", nil
	case datav1.PrimitiveKind_PRIMITIVE_KIND_UINT64,
		datav1.PrimitiveKind_PRIMITIVE_KIND_FIXED64:
		return "UInt64", "protobuf unsigned 64-bit integer maps losslessly to ClickHouse UInt64", nil
	case datav1.PrimitiveKind_PRIMITIVE_KIND_INT32,
		datav1.PrimitiveKind_PRIMITIVE_KIND_SFIXED32,
		datav1.PrimitiveKind_PRIMITIVE_KIND_SINT32:
		return "Int32", "protobuf signed 32-bit integer maps losslessly to ClickHouse Int32", nil
	case datav1.PrimitiveKind_PRIMITIVE_KIND_FIXED32,
		datav1.PrimitiveKind_PRIMITIVE_KIND_UINT32:
		return "UInt32", "protobuf unsigned 32-bit integer maps losslessly to ClickHouse UInt32", nil
	case datav1.PrimitiveKind_PRIMITIVE_KIND_BOOL:
		return "Bool", "protobuf bool maps losslessly to ClickHouse Bool", nil
	case datav1.PrimitiveKind_PRIMITIVE_KIND_STRING:
		return "String", "protobuf UTF-8 string maps to ClickHouse String with a generated UTF-8 CHECK", nil
	case datav1.PrimitiveKind_PRIMITIVE_KIND_BYTES:
		return "String", "protobuf bytes map losslessly to binary-safe ClickHouse String", nil
	default:
		return "", "", fmt.Errorf("unsupported primitive kind %s", kind)
	}
}

func implicitCapable(dataType *datav1.DataType) bool {
	if dataType == nil {
		return false
	}
	switch dataType.GetKind().(type) {
	case *datav1.DataType_Primitive, *datav1.DataType_Enum:
		return true
	default:
		return false
	}
}

func implicitDefault(dataType *datav1.DataType) string {
	if dataType.GetEnum() != nil {
		return "0"
	}
	switch dataType.GetPrimitive().GetKind() {
	case datav1.PrimitiveKind_PRIMITIVE_KIND_BOOL:
		return "false"
	case datav1.PrimitiveKind_PRIMITIVE_KIND_STRING,
		datav1.PrimitiveKind_PRIMITIVE_KIND_BYTES:
		return "''"
	default:
		return "0"
	}
}

func preserveStrongerCompatibility(compatibility datav1.MappingCompatibility) datav1.MappingCompatibility {
	if compatibility == datav1.MappingCompatibility_MAPPING_COMPATIBILITY_LOSSLESS {
		return datav1.MappingCompatibility_MAPPING_COMPATIBILITY_REPRESENTATION_CHANGED
	}
	return compatibility
}

func decimalParameters(decimal *datav1.DecimalType) (uint32, uint32, error) {
	if decimal == nil || decimal.GetPrecision() == 0 || decimal.GetPrecision() > 38 {
		return 0, 0, errors.New("decimal precision must be between 1 and 38")
	}
	if decimal.GetScale() > decimal.GetPrecision() {
		return 0, 0, errors.New("decimal scale must not exceed precision")
	}
	return decimal.GetPrecision(), decimal.GetScale(), nil
}

func fixedByteWidth(fixed *datav1.FixedBytesType) (int, error) {
	if fixed == nil || fixed.GetByteLength() == 0 {
		return 0, errors.New("fixed byte length must be positive")
	}
	if fixed.GetByteLength() > maxFixedStringBytes {
		return 0, fmt.Errorf(
			"fixed byte length %d exceeds ClickHouse's conventional FixedString limit of %d; larger widths require allow_suspicious_fixed_string_types and are not emitted",
			fixed.GetByteLength(), maxFixedStringBytes,
		)
	}
	return int(fixed.GetByteLength()), nil
}

func jsonRangeReduction(kind datav1.JsonKind) string {
	switch kind {
	case datav1.JsonKind_JSON_KIND_ANY:
		return "standard ProtoJSON requires each populated Any type URL to resolve to a known message descriptor; embedded dynamic numbers must be finite"
	case datav1.JsonKind_JSON_KIND_STRUCT,
		datav1.JsonKind_JSON_KIND_VALUE,
		datav1.JsonKind_JSON_KIND_LIST_VALUE:
		return "standard ProtoJSON requires dynamic numbers to be finite; NaN and infinities are not representable"
	default:
		return "standard ProtoJSON requires an explicitly supported dynamic JSON kind"
	}
}

func unsupportedDiagnostic(path, message string) *datav1.MappingDiagnostic {
	if path == "" {
		path = "<dataset>"
	}
	return &datav1.MappingDiagnostic{
		FieldPath:     path,
		Compatibility: datav1.MappingCompatibility_MAPPING_COMPATIBILITY_UNSUPPORTED,
		Message:       message,
	}
}

func scopeElementExpression(name string, topLevel bool) string {
	if topLevel {
		return quoteIdentifier(name)
	}
	return tupleElementExpression(constraintValuePlaceholder, name)
}

func tupleElementExpression(value, name string) string {
	return "tupleElement(" + value + ", " + quoteLiteral(name) + ")"
}

func oneofCaseName(name string) string {
	return reservedNamePrefix + "oneof_" + name + "_case"
}

func replaceValue(expression, replacement string) string {
	return strings.ReplaceAll(expression, constraintValuePlaceholder, replacement)
}

func joinPath(parent, child string) string {
	if parent == "" {
		return child
	}
	return parent + "." + child
}

func validateIdentifier(value string) error {
	if value == "" {
		return errors.New("identifier must not be empty")
	}
	if !utf8.ValidString(value) {
		return errors.New("identifier is not valid UTF-8")
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return errors.New("identifier contains a control character")
		}
	}
	return nil
}

func validateText(value string) error {
	if !utf8.ValidString(value) {
		return errors.New("text is not valid UTF-8")
	}
	for _, character := range value {
		if unicode.IsControl(character) && character != '\n' && character != '\r' && character != '\t' {
			return errors.New("text contains an unsupported control character")
		}
	}
	return nil
}

func quoteIdentifier(value string) string {
	return "`" + strings.NewReplacer(`\`, `\\`, "`", "\\`").Replace(value) + "`"
}

func quoteLiteral(value string) string {
	return "'" + strings.NewReplacer(
		`\`, `\\`,
		"'", "\\'",
		"\n", `\n`,
		"\r", `\r`,
		"\t", `\t`,
	).Replace(value) + "'"
}
