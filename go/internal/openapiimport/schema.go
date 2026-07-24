package openapiimport

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	base "github.com/pb33f/libopenapi/datamodel/high/base"
	"go.yaml.in/yaml/v4"
)

const (
	schemaValidateImport = "buf/validate/validate.proto"
	timestampImport      = "google/protobuf/timestamp.proto"
)

var protoIdentifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// typeForSchema translates the deliberately small, high-confidence
// intersection of OpenAPI Schema Objects and protobuf. Ambiguous mappings are
// import errors so the generated bootstrap contract is explicit about its
// remaining review boundary.
func (c *converter) typeForSchema(
	proxy *base.SchemaProxy,
	suggestedName string,
	pointer string,
) (protoType, error) {
	if proxy == nil {
		return protoType{}, fmt.Errorf("%s: schema is required", pointer)
	}
	if proxy.IsReference() {
		if proxy.IsTransformedRefWithSiblings() ||
			(proxy.GetReferenceNode() != nil && len(proxy.GetReferenceNode().Content) > 2) {
			return protoType{}, fmt.Errorf(
				"%s: $ref siblings are not supported; move the constraints into the referenced component",
				pointer,
			)
		}
		name, err := internalComponentReference(proxy.GetReference(), "schemas")
		if err != nil {
			return protoType{}, fmt.Errorf("%s: %w", pointer, err)
		}
		return c.componentType(name, pointer)
	}

	schema, err := proxy.BuildSchema()
	if err != nil {
		return protoType{}, fmt.Errorf("%s: build schema: %w", pointer, err)
	}
	if schema == nil {
		return protoType{}, fmt.Errorf("%s: boolean schemas are not supported", pointer)
	}
	return c.typeForResolvedSchema(schema, suggestedName, pointer, "")
}

func (c *converter) componentType(name, pointer string) (protoType, error) {
	if value, ok := c.componentTypes[name]; ok {
		return value, nil
	}
	if c.building[name] {
		return protoType{}, fmt.Errorf(
			"%s: component %q has a recursive alias, array, or map; recursion requires an object message",
			pointer,
			name,
		)
	}
	if c.document.Components == nil || c.document.Components.Schemas == nil {
		return protoType{}, fmt.Errorf("%s: schema component %q does not exist", pointer, name)
	}
	proxy := c.document.Components.Schemas.GetOrZero(name)
	if proxy == nil {
		return protoType{}, fmt.Errorf("%s: schema component %q does not exist", pointer, name)
	}

	origin := "#/components/schemas/" + escapeJSONPointer(name)
	c.building[name] = true
	defer delete(c.building, name)

	var (
		value protoType
		err   error
	)
	if proxy.IsReference() {
		value, err = c.typeForSchema(proxy, protoTypeName(name), origin)
	} else {
		var schema *base.Schema
		schema, err = proxy.BuildSchema()
		if err == nil && schema == nil {
			err = errors.New("boolean schemas are not supported")
		}
		if err == nil {
			value, err = c.typeForResolvedSchema(
				schema,
				protoTypeName(name),
				origin,
				name,
			)
		}
	}
	if err != nil {
		return protoType{}, fmt.Errorf("%s: %w", origin, err)
	}
	c.componentTypes[name] = value
	return value, nil
}

func (c *converter) typeForResolvedSchema(
	schema *base.Schema,
	suggestedName string,
	pointer string,
	componentName string,
) (protoType, error) {
	if err := validateCommonSchema(schema, pointer); err != nil {
		return protoType{}, err
	}
	kind, err := schemaKind(schema, pointer)
	if err != nil {
		return protoType{}, err
	}

	switch kind {
	case "boolean":
		return c.booleanType(schema, pointer)
	case "string":
		return c.stringType(schema, pointer)
	case "integer":
		return c.integerType(schema, pointer)
	case "number":
		return c.numberType(schema, pointer)
	case "array":
		return c.arrayType(schema, suggestedName, pointer)
	case "object":
		return c.objectType(schema, suggestedName, pointer, componentName)
	default:
		return protoType{}, fmt.Errorf("%s: unsupported schema type %q", pointer, kind)
	}
}

func validateCommonSchema(schema *base.Schema, pointer string) error {
	if schema == nil {
		return fmt.Errorf("%s: schema is required", pointer)
	}
	if len(schema.AllOf) > 0 {
		return unsupportedSchemaKeyword(pointer, "allOf")
	}
	if len(schema.OneOf) > 0 {
		return unsupportedSchemaKeyword(pointer, "oneOf")
	}
	if len(schema.AnyOf) > 0 {
		return unsupportedSchemaKeyword(pointer, "anyOf")
	}
	if schema.Not != nil {
		return unsupportedSchemaKeyword(pointer, "not")
	}
	if schema.Discriminator != nil {
		return unsupportedSchemaKeyword(pointer, "discriminator")
	}
	if len(schema.PrefixItems) > 0 {
		return unsupportedSchemaKeyword(pointer, "prefixItems")
	}
	if schema.Contains != nil || schema.MinContains != nil || schema.MaxContains != nil {
		return unsupportedSchemaKeyword(pointer, "contains")
	}
	if schema.If != nil || schema.Then != nil || schema.Else != nil {
		return unsupportedSchemaKeyword(pointer, "if/then/else")
	}
	if schema.DependentSchemas != nil && schema.DependentSchemas.Len() > 0 {
		return unsupportedSchemaKeyword(pointer, "dependentSchemas")
	}
	if schema.DependentRequired != nil && schema.DependentRequired.Len() > 0 {
		return unsupportedSchemaKeyword(pointer, "dependentRequired")
	}
	if schema.PatternProperties != nil && schema.PatternProperties.Len() > 0 {
		return unsupportedSchemaKeyword(pointer, "patternProperties")
	}
	if schema.Defs != nil && schema.Defs.Len() > 0 {
		return unsupportedSchemaKeyword(pointer, "$defs")
	}
	if schema.PropertyNames != nil {
		return unsupportedSchemaKeyword(pointer, "propertyNames")
	}
	if schema.UnevaluatedItems != nil {
		return unsupportedSchemaKeyword(pointer, "unevaluatedItems")
	}
	if schema.UnevaluatedProperties != nil {
		return unsupportedSchemaKeyword(pointer, "unevaluatedProperties")
	}
	if schema.MultipleOf != nil {
		return unsupportedSchemaKeyword(pointer, "multipleOf")
	}
	if schema.Const != nil {
		return unsupportedSchemaKeyword(pointer, "const")
	}
	if schema.Default != nil {
		return fmt.Errorf(
			"%s/default: OpenAPI defaults are not protobuf presence semantics and cannot be imported safely",
			pointer,
		)
	}
	if schema.Nullable != nil && *schema.Nullable {
		return fmt.Errorf(
			"%s/nullable: nullable values cannot be represented without changing the protobuf JSON shape",
			pointer,
		)
	}
	if schema.ReadOnly != nil && *schema.ReadOnly {
		return fmt.Errorf(
			"%s/readOnly: request/response-specific fields require separate protobuf messages",
			pointer,
		)
	}
	if schema.WriteOnly != nil && *schema.WriteOnly {
		return fmt.Errorf(
			"%s/writeOnly: request/response-specific fields require separate protobuf messages",
			pointer,
		)
	}
	if schema.SchemaTypeRef != "" || schema.Id != "" || schema.Anchor != "" ||
		schema.DynamicAnchor != "" || schema.DynamicRef != "" ||
		(schema.Vocabulary != nil && schema.Vocabulary.Len() > 0) {
		return fmt.Errorf(
			"%s: custom JSON Schema dialect, identity, anchor, and dynamic-reference keywords are not supported",
			pointer,
		)
	}
	if schema.ContentEncoding != "" || schema.ContentMediaType != "" ||
		schema.ContentSchema != nil {
		return fmt.Errorf(
			"%s: contentEncoding, contentMediaType, and contentSchema are not supported",
			pointer,
		)
	}
	if schema.XML != nil {
		return fmt.Errorf("%s/xml: XML serialization metadata cannot be represented in protobuf", pointer)
	}
	if schema.Enum != nil && len(schema.Enum) == 0 {
		return fmt.Errorf("%s/enum: enum must contain at least one value", pointer)
	}
	if slices.Contains(schema.Type, "null") {
		return fmt.Errorf(
			"%s/type: nullable type unions cannot be represented without changing the protobuf JSON shape",
			pointer,
		)
	}
	return nil
}

func schemaKind(schema *base.Schema, pointer string) (string, error) {
	if len(schema.Type) > 1 {
		return "", fmt.Errorf(
			"%s/type: type unions are not supported; use one non-null type",
			pointer,
		)
	}
	if len(schema.Type) == 1 {
		return schema.Type[0], nil
	}

	switch {
	case schema.Properties != nil || schema.AdditionalProperties != nil:
		return "object", nil
	case schema.Items != nil:
		return "array", nil
	default:
		return "", fmt.Errorf("%s/type: an explicit OpenAPI schema type is required", pointer)
	}
}

func (c *converter) booleanType(schema *base.Schema, pointer string) (protoType, error) {
	if err := rejectNonBooleanKeywords(schema, pointer); err != nil {
		return protoType{}, err
	}
	return protoType{name: "bool", kind: typeScalar}, nil
}

func (c *converter) stringType(schema *base.Schema, pointer string) (protoType, error) {
	if err := rejectNonStringKeywords(schema, pointer); err != nil {
		return protoType{}, err
	}

	switch schema.Format {
	case "byte":
		if hasStringConstraints(schema) {
			return protoType{}, fmt.Errorf(
				"%s: minLength, maxLength, pattern, and enum constrain encoded text and cannot be transferred to protobuf bytes",
				pointer,
			)
		}
		return protoType{name: "bytes", kind: typeScalar}, nil
	case "date-time":
		if hasStringConstraints(schema) {
			return protoType{}, fmt.Errorf(
				"%s: string constraints cannot be transferred to google.protobuf.Timestamp",
				pointer,
			)
		}
		c.file.imports[timestampImport] = struct{}{}
		return protoType{name: "google.protobuf.Timestamp", kind: typeMessage}, nil
	case "", "email", "hostname", "ip", "ipv4", "ipv6", "uri", "uri-reference", "uuid":
	default:
		return protoType{}, fmt.Errorf(
			"%s/format: string format %q has no canonical protobuf representation",
			pointer,
			schema.Format,
		)
	}

	rules := make([]constraint, 0, 4+len(schema.Enum))
	if schema.MinLength != nil {
		if *schema.MinLength < 0 {
			return protoType{}, fmt.Errorf("%s/minLength: must be non-negative", pointer)
		}
		rules = append(rules, constraint{
			path:  "string.min_len",
			value: strconv.FormatInt(*schema.MinLength, 10),
		})
	}
	if schema.MaxLength != nil {
		if *schema.MaxLength < 0 {
			return protoType{}, fmt.Errorf("%s/maxLength: must be non-negative", pointer)
		}
		rules = append(rules, constraint{
			path:  "string.max_len",
			value: strconv.FormatInt(*schema.MaxLength, 10),
		})
	}
	if schema.MinLength != nil && schema.MaxLength != nil &&
		*schema.MinLength > *schema.MaxLength {
		return protoType{}, fmt.Errorf("%s: minLength exceeds maxLength", pointer)
	}
	if schema.Pattern != "" {
		if _, err := regexp.Compile(schema.Pattern); err != nil {
			return protoType{}, fmt.Errorf(
				"%s/pattern: pattern is not compatible with Protovalidate's RE2 syntax: %w",
				pointer,
				err,
			)
		}
		rules = append(rules, constraint{path: "string.pattern", value: quoted(schema.Pattern)})
	}
	if schema.Format != "" {
		formatRule := strings.ReplaceAll(schema.Format, "-", "_")
		if schema.Format == "uri-reference" {
			formatRule = "uri_ref"
		}
		rules = append(rules, constraint{path: "string." + formatRule, value: "true"})
	}

	enumValues, err := stringEnum(schema.Enum, pointer)
	if err != nil {
		return protoType{}, err
	}
	for _, value := range enumValues {
		rules = append(rules, constraint{path: "string.in", value: quoted(value)})
	}
	if len(rules) > 0 {
		c.file.imports[schemaValidateImport] = struct{}{}
	}
	return protoType{name: "string", kind: typeScalar, constraints: rules}, nil
}

func (c *converter) integerType(schema *base.Schema, pointer string) (protoType, error) {
	if err := rejectNonNumericKeywords(schema, pointer); err != nil {
		return protoType{}, err
	}
	if schema.Format != "int32" {
		return protoType{}, fmt.Errorf(
			"%s/format: integers require format \"int32\"; unformatted and 64-bit OpenAPI integers do not preserve ProtoJSON semantics",
			pointer,
		)
	}
	if len(schema.Enum) > 0 {
		return protoType{}, fmt.Errorf(
			"%s/enum: only string enums have a stable OpenAPI-to-Protobuf JSON mapping",
			pointer,
		)
	}

	rules, err := numericRules(schema, "int32", true, pointer)
	if err != nil {
		return protoType{}, err
	}
	if len(rules) > 0 {
		c.file.imports[schemaValidateImport] = struct{}{}
	}
	return protoType{name: "int32", kind: typeScalar, constraints: rules}, nil
}

func (c *converter) numberType(schema *base.Schema, pointer string) (protoType, error) {
	if err := rejectNonNumericKeywords(schema, pointer); err != nil {
		return protoType{}, err
	}
	if len(schema.Enum) > 0 {
		return protoType{}, fmt.Errorf(
			"%s/enum: only string enums have a stable OpenAPI-to-Protobuf JSON mapping",
			pointer,
		)
	}

	protoName := "double"
	switch schema.Format {
	case "double":
	case "float":
		protoName = "float"
	case "":
		return protoType{}, fmt.Errorf(
			"%s/format: numbers require format \"float\" or \"double\" so protobuf precision is explicit",
			pointer,
		)
	default:
		return protoType{}, fmt.Errorf(
			"%s/format: number format %q is not supported",
			pointer,
			schema.Format,
		)
	}
	rules, err := numericRules(schema, protoName, false, pointer)
	if err != nil {
		return protoType{}, err
	}
	rules = append(rules, constraint{path: protoName + ".finite", value: "true"})
	c.file.imports[schemaValidateImport] = struct{}{}
	return protoType{name: protoName, kind: typeScalar, constraints: rules}, nil
}

func (c *converter) arrayType(
	schema *base.Schema,
	suggestedName string,
	pointer string,
) (protoType, error) {
	if err := rejectNonArrayKeywords(schema, pointer); err != nil {
		return protoType{}, err
	}
	if schema.Items == nil {
		return protoType{}, fmt.Errorf("%s/items: array items must have one schema", pointer)
	}
	if schema.Items.IsB() {
		return protoType{}, fmt.Errorf(
			"%s/items: boolean and tuple item schemas are not supported",
			pointer,
		)
	}
	if schema.Items.A == nil {
		return protoType{}, fmt.Errorf("%s/items: array items must have one schema", pointer)
	}
	element, err := c.typeForSchema(
		schema.Items.A,
		protoTypeName(suggestedName)+"Item",
		pointer+"/items",
	)
	if err != nil {
		return protoType{}, err
	}
	if element.kind == typeList || element.kind == typeMap {
		return protoType{}, fmt.Errorf(
			"%s/items: nested arrays and maps cannot be represented as a protobuf repeated field",
			pointer,
		)
	}

	rules := make([]constraint, 0, 3)
	if schema.MinItems != nil {
		if *schema.MinItems < 0 {
			return protoType{}, fmt.Errorf("%s/minItems: must be non-negative", pointer)
		}
		if *schema.MinItems > 0 {
			return protoType{}, fmt.Errorf(
				"%s/minItems: a positive lower bound cannot preserve absent versus empty collection semantics in protobuf",
				pointer,
			)
		}
	}
	if schema.MaxItems != nil {
		if *schema.MaxItems < 0 {
			return protoType{}, fmt.Errorf("%s/maxItems: must be non-negative", pointer)
		}
		rules = append(rules, constraint{
			path:  "repeated.max_items",
			value: strconv.FormatInt(*schema.MaxItems, 10),
		})
	}
	if schema.MinItems != nil && schema.MaxItems != nil && *schema.MinItems > *schema.MaxItems {
		return protoType{}, fmt.Errorf("%s: minItems exceeds maxItems", pointer)
	}
	if schema.UniqueItems != nil && *schema.UniqueItems {
		if element.kind == typeMessage {
			return protoType{}, fmt.Errorf(
				"%s/uniqueItems: Protovalidate uniqueness is not defined for repeated message values",
				pointer,
			)
		}
		rules = append(rules, constraint{path: "repeated.unique", value: "true"})
	}
	if len(rules) > 0 || len(element.constraints) > 0 {
		c.file.imports[schemaValidateImport] = struct{}{}
	}
	return protoType{
		kind:        typeList,
		constraints: rules,
		element:     &element,
	}, nil
}

func (c *converter) objectType(
	schema *base.Schema,
	suggestedName string,
	pointer string,
	componentName string,
) (protoType, error) {
	if err := rejectNonObjectKeywords(schema, pointer); err != nil {
		return protoType{}, err
	}
	propertyCount := 0
	if schema.Properties != nil {
		propertyCount = schema.Properties.Len()
	}
	if schema.AdditionalProperties == nil {
		return protoType{}, fmt.Errorf(
			"%s/additionalProperties: objects must be explicitly closed with false or be a typed map",
			pointer,
		)
	}
	if schema.AdditionalProperties.IsA() {
		if schema.AdditionalProperties.A == nil {
			return protoType{}, fmt.Errorf(
				"%s/additionalProperties: typed map values require a schema",
				pointer,
			)
		}
		if propertyCount > 0 {
			return protoType{}, fmt.Errorf(
				"%s: objects cannot mix named properties with additional map values",
				pointer,
			)
		}
		if len(schema.Required) > 0 {
			return protoType{}, fmt.Errorf(
				"%s/required: typed maps cannot express named required properties",
				pointer,
			)
		}
		return c.mapType(schema, suggestedName, pointer)
	}
	if schema.AdditionalProperties.B {
		return protoType{}, fmt.Errorf(
			"%s/additionalProperties: open objects cannot be represented by a protobuf message",
			pointer,
		)
	}
	if schema.MinProperties != nil || schema.MaxProperties != nil {
		return protoType{}, fmt.Errorf(
			"%s: minProperties and maxProperties cannot be represented for a structured protobuf message",
			pointer,
		)
	}

	messageName := protoTypeName(suggestedName)
	if !protoIdentifierPattern.MatchString(messageName) {
		return protoType{}, fmt.Errorf(
			"%s: generated message name %q is not a portable protobuf identifier",
			pointer,
			messageName,
		)
	}
	message := &protoMessage{
		name:    messageName,
		comment: comment(schema.Title, schema.Description),
	}
	if message.comment == "" {
		message.comment = fmt.Sprintf("OpenAPI schema for %s.", messageName)
	}
	if err := c.addMessage(message, pointer); err != nil {
		return protoType{}, err
	}
	value := protoType{name: messageName, kind: typeMessage}
	if componentName != "" {
		// Publish the message identity before walking fields so direct and
		// mutual object recursion resolve without constructing a second type.
		c.componentTypes[componentName] = value
	}
	if err := c.populateMessage(message, schema, pointer); err != nil {
		return protoType{}, err
	}
	return value, nil
}

func (c *converter) mapType(
	schema *base.Schema,
	suggestedName string,
	pointer string,
) (protoType, error) {
	value, err := c.typeForSchema(
		schema.AdditionalProperties.A,
		protoTypeName(suggestedName)+"Value",
		pointer+"/additionalProperties",
	)
	if err != nil {
		return protoType{}, err
	}
	if value.kind == typeList || value.kind == typeMap {
		return protoType{}, fmt.Errorf(
			"%s/additionalProperties: protobuf map values cannot be arrays or maps",
			pointer,
		)
	}

	rules := make([]constraint, 0, 2)
	if schema.MinProperties != nil {
		if *schema.MinProperties < 0 {
			return protoType{}, fmt.Errorf("%s/minProperties: must be non-negative", pointer)
		}
		if *schema.MinProperties > 0 {
			return protoType{}, fmt.Errorf(
				"%s/minProperties: a positive lower bound cannot preserve absent versus empty collection semantics in protobuf",
				pointer,
			)
		}
	}
	if schema.MaxProperties != nil {
		if *schema.MaxProperties < 0 {
			return protoType{}, fmt.Errorf("%s/maxProperties: must be non-negative", pointer)
		}
		rules = append(rules, constraint{
			path:  "map.max_pairs",
			value: strconv.FormatInt(*schema.MaxProperties, 10),
		})
	}
	if schema.MinProperties != nil && schema.MaxProperties != nil &&
		*schema.MinProperties > *schema.MaxProperties {
		return protoType{}, fmt.Errorf("%s: minProperties exceeds maxProperties", pointer)
	}
	if len(rules) > 0 || len(value.constraints) > 0 {
		c.file.imports[schemaValidateImport] = struct{}{}
	}
	return protoType{kind: typeMap, constraints: rules, element: &value}, nil
}

func (c *converter) populateMessage(
	message *protoMessage,
	schema *base.Schema,
	pointer string,
) error {
	required := make(map[string]struct{}, len(schema.Required))
	for _, name := range schema.Required {
		if _, duplicate := required[name]; duplicate {
			return fmt.Errorf("%s/required: property %q appears more than once", pointer, name)
		}
		required[name] = struct{}{}
	}

	properties := make(map[string]*base.SchemaProxy)
	if schema.Properties != nil {
		maps.Insert(properties, schema.Properties.FromOldest())
	}
	for name := range required {
		if _, exists := properties[name]; !exists {
			return fmt.Errorf(
				"%s/required: property %q is required but is not declared in this closed object",
				pointer,
				name,
			)
		}
	}

	names := sortedKeys(properties)
	fieldOrigins := make(map[string]string, len(names))
	for index, wireName := range names {
		fieldPointer := pointer + "/properties/" + escapeJSONPointer(wireName)
		if wireName == "" {
			return fmt.Errorf(
				"%s: an empty JSON property name cannot be preserved as a protobuf json_name",
				fieldPointer,
			)
		}
		proxy := properties[wireName]
		if proxy == nil {
			return fmt.Errorf("%s: property schema is required", fieldPointer)
		}
		fieldName := protoFieldName(wireName)
		if !protoIdentifierPattern.MatchString(fieldName) {
			return fmt.Errorf(
				"%s: generated field name %q is not a portable protobuf identifier",
				fieldPointer,
				fieldName,
			)
		}
		if previous, exists := fieldOrigins[fieldName]; exists {
			return fmt.Errorf(
				"%s: protobuf field name %q collides with property %q after normalization",
				fieldPointer,
				fieldName,
				previous,
			)
		}
		fieldOrigins[fieldName] = wireName

		fieldType, err := c.typeForSchema(
			proxy,
			message.name+protoTypeName(wireName),
			fieldPointer,
		)
		if err != nil {
			return err
		}
		_, isRequired := required[wireName]
		if isRequired && (fieldType.kind == typeList || fieldType.kind == typeMap) {
			return fmt.Errorf(
				"%s: required arrays and maps cannot preserve property presence in protobuf",
				fieldPointer,
			)
		}

		fieldSchema, err := proxy.BuildSchema()
		if err != nil {
			return fmt.Errorf("%s: build property schema: %w", fieldPointer, err)
		}
		if fieldSchema == nil {
			return fmt.Errorf("%s: boolean schemas are not supported", fieldPointer)
		}
		fieldComment := comment(fieldSchema.Title, fieldSchema.Description)
		if fieldComment == "" {
			fieldComment = fmt.Sprintf("OpenAPI field %q.", wireName)
		}
		message.fields = append(message.fields, protoField{
			name:       fieldName,
			jsonName:   wireName,
			comment:    fieldComment,
			fieldType:  fieldType,
			number:     protobufFieldNumber(index),
			required:   isRequired,
			deprecated: fieldSchema.Deprecated != nil && *fieldSchema.Deprecated,
		})
		if isRequired {
			c.file.imports[schemaValidateImport] = struct{}{}
		}
	}
	return nil
}

func protobufFieldNumber(index int) int {
	number := index + 1
	if number >= 19000 {
		// Protobuf reserves 19000 through 19999 for its implementation.
		number += 1000
	}
	return number
}

// addMessage centralizes protobuf top-level name ownership so component and
// inline schema normalization can never silently merge unrelated contracts.
func (c *converter) addMessage(message *protoMessage, origin string) error {
	if message == nil || message.name == "" {
		return fmt.Errorf("%s: generated message name is empty", origin)
	}
	if !protoIdentifierPattern.MatchString(message.name) {
		return fmt.Errorf(
			"%s: generated message name %q is not a portable protobuf identifier",
			origin,
			message.name,
		)
	}
	if message.name == c.file.service.name {
		return fmt.Errorf(
			"%s: message name %q collides with the generated service name",
			origin,
			message.name,
		)
	}
	if existing, exists := c.file.messages[message.name]; exists {
		if existing == message {
			return nil
		}
		return fmt.Errorf(
			"%s: message name %q collides with %s after protobuf normalization",
			origin,
			message.name,
			c.messageOrigins[message.name],
		)
	}
	c.file.messages[message.name] = message
	c.messageOrigins[message.name] = origin
	return nil
}

func numericRules(
	schema *base.Schema,
	protoName string,
	integer bool,
	pointer string,
) ([]constraint, error) {
	lower, upper, err := numericBounds(schema, pointer)
	if err != nil {
		return nil, err
	}
	rules := make([]constraint, 0, 2)
	if lower != nil {
		value, err := numericLiteral(lower.value, protoName, integer)
		if err != nil {
			return nil, fmt.Errorf("%s: lower bound: %w", pointer, err)
		}
		operator := "gte"
		if lower.exclusive {
			operator = "gt"
		}
		rules = append(rules, constraint{path: protoName + "." + operator, value: value})
	}
	if upper != nil {
		value, err := numericLiteral(upper.value, protoName, integer)
		if err != nil {
			return nil, fmt.Errorf("%s: upper bound: %w", pointer, err)
		}
		operator := "lte"
		if upper.exclusive {
			operator = "lt"
		}
		rules = append(rules, constraint{path: protoName + "." + operator, value: value})
	}
	return rules, nil
}

type numericBound struct {
	value     float64
	exclusive bool
}

func numericBounds(schema *base.Schema, pointer string) (*numericBound, *numericBound, error) {
	var lower, upper *numericBound
	if schema.Minimum != nil {
		lower = &numericBound{value: *schema.Minimum}
	}
	if schema.Maximum != nil {
		upper = &numericBound{value: *schema.Maximum}
	}
	if schema.ExclusiveMinimum != nil {
		if schema.ExclusiveMinimum.IsA() {
			if schema.ExclusiveMinimum.A {
				if lower == nil {
					return nil, nil, fmt.Errorf(
						"%s/exclusiveMinimum: boolean true requires minimum",
						pointer,
					)
				}
				lower.exclusive = true
			}
		} else {
			lower = stricterLower(lower, &numericBound{
				value:     schema.ExclusiveMinimum.B,
				exclusive: true,
			})
		}
	}
	if schema.ExclusiveMaximum != nil {
		if schema.ExclusiveMaximum.IsA() {
			if schema.ExclusiveMaximum.A {
				if upper == nil {
					return nil, nil, fmt.Errorf(
						"%s/exclusiveMaximum: boolean true requires maximum",
						pointer,
					)
				}
				upper.exclusive = true
			}
		} else {
			upper = stricterUpper(upper, &numericBound{
				value:     schema.ExclusiveMaximum.B,
				exclusive: true,
			})
		}
	}
	for _, candidate := range []*numericBound{lower, upper} {
		if candidate != nil && (math.IsNaN(candidate.value) || math.IsInf(candidate.value, 0)) {
			return nil, nil, fmt.Errorf("%s: numeric bounds must be finite", pointer)
		}
	}
	if lower != nil && upper != nil {
		if lower.value > upper.value ||
			(lower.value == upper.value && (lower.exclusive || upper.exclusive)) {
			return nil, nil, fmt.Errorf("%s: numeric bounds do not admit any value", pointer)
		}
	}
	return lower, upper, nil
}

func stricterLower(left, right *numericBound) *numericBound {
	if left == nil || right.value > left.value ||
		(right.value == left.value && right.exclusive) {
		return right
	}
	return left
}

func stricterUpper(left, right *numericBound) *numericBound {
	if left == nil || right.value < left.value ||
		(right.value == left.value && right.exclusive) {
		return right
	}
	return left
}

func numericLiteral(value float64, protoName string, integer bool) (string, error) {
	if integer {
		if math.Trunc(value) != value {
			return "", fmt.Errorf("%v is not an integer", value)
		}
		if value < math.MinInt32 || value > math.MaxInt32 {
			return "", fmt.Errorf("%v is outside the int32 range", value)
		}
		return strconv.FormatInt(int64(value), 10), nil
	}
	if protoName == "float" {
		converted := float32(value)
		if math.IsInf(float64(converted), 0) {
			return "", fmt.Errorf("%v is outside the float range", value)
		}
		return strconv.FormatFloat(float64(converted), 'g', -1, 32), nil
	}
	return strconv.FormatFloat(value, 'g', -1, 64), nil
}

func stringEnum(nodes []*yaml.Node, pointer string) ([]string, error) {
	if len(nodes) == 0 {
		return nil, nil
	}
	values := make([]string, 0, len(nodes))
	seen := make(map[string]struct{}, len(nodes))
	for _, node := range nodes {
		if node == nil {
			return nil, fmt.Errorf("%s/enum: enum values must be strings", pointer)
		}
		var decoded any
		if err := node.Decode(&decoded); err != nil {
			return nil, fmt.Errorf("%s/enum: decode value: %w", pointer, err)
		}
		value, ok := decoded.(string)
		if !ok {
			return nil, fmt.Errorf(
				"%s/enum: only string enum values have a stable OpenAPI-to-Protobuf JSON mapping",
				pointer,
			)
		}
		if _, duplicate := seen[value]; duplicate {
			return nil, fmt.Errorf("%s/enum: duplicate value %q", pointer, value)
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}
	slices.Sort(values)
	return values, nil
}

func hasStringConstraints(schema *base.Schema) bool {
	return schema.MinLength != nil || schema.MaxLength != nil ||
		schema.Pattern != "" || len(schema.Enum) > 0
}

func rejectNonBooleanKeywords(schema *base.Schema, pointer string) error {
	if len(schema.Enum) > 0 {
		return fmt.Errorf(
			"%s/enum: only string enums have a stable OpenAPI-to-Protobuf JSON mapping",
			pointer,
		)
	}
	return rejectKeywords(schema, pointer, false, false, false, false)
}

func rejectNonStringKeywords(schema *base.Schema, pointer string) error {
	return rejectKeywords(schema, pointer, true, false, false, false)
}

func rejectNonNumericKeywords(schema *base.Schema, pointer string) error {
	return rejectKeywords(schema, pointer, false, true, false, false)
}

func rejectNonArrayKeywords(schema *base.Schema, pointer string) error {
	if len(schema.Enum) > 0 {
		return unsupportedSchemaKeyword(pointer, "enum")
	}
	return rejectKeywords(schema, pointer, false, false, true, false)
}

func rejectNonObjectKeywords(schema *base.Schema, pointer string) error {
	if len(schema.Enum) > 0 {
		return unsupportedSchemaKeyword(pointer, "enum")
	}
	return rejectKeywords(schema, pointer, false, false, false, true)
}

func rejectKeywords(
	schema *base.Schema,
	pointer string,
	allowString bool,
	allowNumeric bool,
	allowArray bool,
	allowObject bool,
) error {
	if !allowString {
		switch {
		case schema.MinLength != nil:
			return unsupportedSchemaKeyword(pointer, "minLength")
		case schema.MaxLength != nil:
			return unsupportedSchemaKeyword(pointer, "maxLength")
		case schema.Pattern != "":
			return unsupportedSchemaKeyword(pointer, "pattern")
		}
	}
	if !allowString && !allowNumeric && schema.Format != "" {
		return unsupportedSchemaKeyword(pointer, "format")
	}
	if !allowNumeric {
		switch {
		case schema.Minimum != nil:
			return unsupportedSchemaKeyword(pointer, "minimum")
		case schema.Maximum != nil:
			return unsupportedSchemaKeyword(pointer, "maximum")
		case schema.ExclusiveMinimum != nil:
			return unsupportedSchemaKeyword(pointer, "exclusiveMinimum")
		case schema.ExclusiveMaximum != nil:
			return unsupportedSchemaKeyword(pointer, "exclusiveMaximum")
		}
	}
	if !allowArray {
		switch {
		case schema.Items != nil:
			return unsupportedSchemaKeyword(pointer, "items")
		case schema.MinItems != nil:
			return unsupportedSchemaKeyword(pointer, "minItems")
		case schema.MaxItems != nil:
			return unsupportedSchemaKeyword(pointer, "maxItems")
		case schema.UniqueItems != nil && *schema.UniqueItems:
			return unsupportedSchemaKeyword(pointer, "uniqueItems")
		}
	}
	if !allowObject {
		switch {
		case schema.Properties != nil && schema.Properties.Len() > 0:
			return unsupportedSchemaKeyword(pointer, "properties")
		case schema.AdditionalProperties != nil:
			return unsupportedSchemaKeyword(pointer, "additionalProperties")
		case len(schema.Required) > 0:
			return unsupportedSchemaKeyword(pointer, "required")
		case schema.MinProperties != nil:
			return unsupportedSchemaKeyword(pointer, "minProperties")
		case schema.MaxProperties != nil:
			return unsupportedSchemaKeyword(pointer, "maxProperties")
		}
	}
	return nil
}

func unsupportedSchemaKeyword(pointer, keyword string) error {
	return fmt.Errorf(
		"%s/%s: keyword has no supported lossless protobuf mapping",
		pointer,
		keyword,
	)
}
