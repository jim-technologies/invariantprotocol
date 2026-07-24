package openapiimport

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	base "github.com/pb33f/libopenapi/datamodel/high/base"
	v3 "github.com/pb33f/libopenapi/datamodel/high/v3"
	"github.com/pb33f/libopenapi/orderedmap"
)

type importedParameter struct {
	parameter *v3.Parameter
	pointer   string
}

func (c *converter) buildRequest(
	spec operationSpec,
	methodName string,
) (string, string, string, error) {
	if !strings.HasPrefix(spec.path, "/") {
		return "", "", "", fmt.Errorf("%s: OpenAPI paths must start with /", spec.pointer)
	}
	literals := pathVariablePattern.ReplaceAllString(spec.path, "")
	if strings.ContainsAny(literals, "{}?#*") {
		return "", "", "", fmt.Errorf(
			"%s: path %q contains an unsupported template, wildcard, query, or fragment",
			spec.pointer,
			spec.path,
		)
	}
	for segment := range strings.SplitSeq(strings.Trim(spec.path, "/"), "/") {
		if strings.ContainsAny(segment, "{}") && !pathVariableSegment.MatchString(segment) {
			return "", "", "", fmt.Errorf(
				"%s: path variables must occupy a complete segment; got %q",
				spec.pointer,
				segment,
			)
		}
	}
	parameters, err := mergeParameters(spec)
	if err != nil {
		return "", "", "", err
	}

	var fields []protoField
	fieldOrigins := make(map[string]string)
	httpPath := spec.path
	pathParameters := make(map[string]importedParameter)
	queryParameters := make([]importedParameter, 0)
	for _, parameter := range parameters {
		switch parameter.parameter.In {
		case "path":
			pathParameters[parameter.parameter.Name] = parameter
		case "query":
			queryParameters = append(queryParameters, parameter)
		case "header", "cookie":
			return "", "", "", fmt.Errorf(
				"%s: %s parameters cannot be represented by the canonical protobuf request",
				parameter.pointer,
				parameter.parameter.In,
			)
		default:
			return "", "", "", fmt.Errorf(
				"%s/in: unsupported parameter location %q",
				parameter.pointer,
				parameter.parameter.In,
			)
		}
	}

	seenPathParameters := make(map[string]struct{})
	for _, match := range pathVariablePattern.FindAllStringSubmatch(spec.path, -1) {
		wireName := match[1]
		parameter, exists := pathParameters[wireName]
		if !exists {
			return "", "", "", fmt.Errorf(
				"%s: path placeholder %q has no matching path parameter",
				spec.pointer,
				wireName,
			)
		}
		if _, duplicate := seenPathParameters[wireName]; duplicate {
			continue
		}
		seenPathParameters[wireName] = struct{}{}

		field, err := c.parameterField(parameter, methodName+"Request", true)
		if err != nil {
			return "", "", "", err
		}
		if !wireScalar(field.fieldType) {
			return "", "", "", fmt.Errorf(
				"%s/schema: path parameters must be scalar",
				parameter.pointer,
			)
		}
		if previous, duplicate := fieldOrigins[field.name]; duplicate {
			return "", "", "", normalizedFieldCollision(
				methodName+"Request",
				field.name,
				previous,
				parameter.pointer,
			)
		}
		fieldOrigins[field.name] = parameter.pointer
		fields = append(fields, field)
		httpPath = strings.ReplaceAll(httpPath, "{"+wireName+"}", "{"+field.name+"}")
	}
	for wireName, parameter := range pathParameters {
		if _, found := seenPathParameters[wireName]; !found {
			return "", "", "", fmt.Errorf(
				"%s: path parameter %q is not present in the path template",
				parameter.pointer,
				wireName,
			)
		}
	}

	bodySelector := ""
	if spec.operation.RequestBody != nil {
		if spec.httpMethod == "get" || spec.httpMethod == "delete" {
			return "", "", "", fmt.Errorf(
				"%s/requestBody: HTTP %s request bodies are not imported",
				spec.pointer,
				strings.ToUpper(spec.httpMethod),
			)
		}
		bodyType, bodyDescription, bodyRequired, err := c.requestBody(
			spec.operation.RequestBody,
			methodName+"RequestBody",
			spec.pointer+"/requestBody",
		)
		if err != nil {
			return "", "", "", err
		}
		bodyField := protoField{
			name:      "body",
			jsonName:  "body",
			comment:   bodyDescription,
			fieldType: bodyType,
			required:  bodyRequired,
		}
		if previous, duplicate := fieldOrigins[bodyField.name]; duplicate {
			return "", "", "", normalizedFieldCollision(
				methodName+"Request",
				bodyField.name,
				previous,
				spec.pointer+"/requestBody",
			)
		}
		fieldOrigins[bodyField.name] = spec.pointer + "/requestBody"
		fields = append(fields, bodyField)
		bodySelector = bodyField.name
	}

	slices.SortFunc(queryParameters, func(left, right importedParameter) int {
		if left.parameter.Name != right.parameter.Name {
			return strings.Compare(left.parameter.Name, right.parameter.Name)
		}
		return strings.Compare(left.pointer, right.pointer)
	})
	for _, parameter := range queryParameters {
		field, err := c.parameterField(parameter, methodName+"Request", false)
		if err != nil {
			return "", "", "", err
		}
		if !wireQuery(field.fieldType) {
			return "", "", "", fmt.Errorf(
				"%s/schema: query parameters must be scalar or arrays of scalars",
				parameter.pointer,
			)
		}
		if previous, duplicate := fieldOrigins[field.name]; duplicate {
			return "", "", "", normalizedFieldCollision(
				methodName+"Request",
				field.name,
				previous,
				parameter.pointer,
			)
		}
		fieldOrigins[field.name] = parameter.pointer
		fields = append(fields, field)
	}

	c.file.imports["google/api/annotations.proto"] = struct{}{}
	if len(fields) == 0 {
		c.file.imports["google/protobuf/empty.proto"] = struct{}{}
		return "google.protobuf.Empty", httpPath, bodySelector, nil
	}
	for index := range fields {
		fields[index].number = protobufFieldNumber(index)
	}
	message := &protoMessage{
		name:    methodName + "Request",
		comment: fmt.Sprintf("Request for %s.", methodName),
		fields:  fields,
	}
	if err := c.addMessage(message, spec.pointer+" request"); err != nil {
		return "", "", "", err
	}
	return message.name, httpPath, bodySelector, nil
}

func mergeParameters(spec operationSpec) ([]importedParameter, error) {
	merged := make(map[string]importedParameter)
	order := make([]string, 0, len(spec.pathItem.Parameters)+len(spec.operation.Parameters))
	add := func(parameters []*v3.Parameter, basePointer string, override bool) error {
		seen := make(map[string]struct{})
		for index, parameter := range parameters {
			pointer := fmt.Sprintf("%s/%d", basePointer, index)
			if parameter == nil {
				return fmt.Errorf("%s: parameter is empty", pointer)
			}
			if parameter.Name == "" || parameter.In == "" {
				return fmt.Errorf("%s: parameter name and in are required", pointer)
			}
			key := parameter.In + "\x00" + parameter.Name
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf(
					"%s: duplicate %s parameter %q",
					pointer,
					parameter.In,
					parameter.Name,
				)
			}
			seen[key] = struct{}{}
			if _, exists := merged[key]; !exists {
				order = append(order, key)
			} else if !override {
				return fmt.Errorf(
					"%s: duplicate %s parameter %q",
					pointer,
					parameter.In,
					parameter.Name,
				)
			}
			merged[key] = importedParameter{parameter: parameter, pointer: pointer}
		}
		return nil
	}
	pathPointer := "#/paths/" + escapeJSONPointer(spec.path) + "/parameters"
	if err := add(spec.pathItem.Parameters, pathPointer, false); err != nil {
		return nil, err
	}
	if err := add(spec.operation.Parameters, spec.pointer+"/parameters", true); err != nil {
		return nil, err
	}
	result := make([]importedParameter, 0, len(order))
	for _, key := range order {
		result = append(result, merged[key])
	}
	return result, nil
}

func (c *converter) parameterField(
	imported importedParameter,
	parentName string,
	path bool,
) (protoField, error) {
	parameter := imported.parameter
	if parameter.AllowEmptyValue {
		return protoField{}, fmt.Errorf(
			"%s/allowEmptyValue: empty-value parameter semantics are not supported",
			imported.pointer,
		)
	}
	if parameter.AllowReserved {
		return protoField{}, fmt.Errorf(
			"%s/allowReserved: reserved-character query semantics are not supported",
			imported.pointer,
		)
	}
	if path {
		if parameter.Style != "" && parameter.Style != "simple" {
			return protoField{}, fmt.Errorf(
				"%s/style: path style %q is not supported",
				imported.pointer,
				parameter.Style,
			)
		}
		if parameter.Explode != nil && *parameter.Explode {
			return protoField{}, fmt.Errorf(
				"%s/explode: exploded path parameters are not supported",
				imported.pointer,
			)
		}
		if parameter.Required == nil || !*parameter.Required {
			return protoField{}, fmt.Errorf(
				"%s/required: path parameters must be required",
				imported.pointer,
			)
		}
	} else {
		if parameter.Style != "" && parameter.Style != "form" {
			return protoField{}, fmt.Errorf(
				"%s/style: query style %q is not supported",
				imported.pointer,
				parameter.Style,
			)
		}
		if parameter.Explode != nil && !*parameter.Explode {
			return protoField{}, fmt.Errorf(
				"%s/explode: non-exploded query parameters are not supported",
				imported.pointer,
			)
		}
	}

	schema, err := schemaFromParameter(parameter)
	if err != nil {
		return protoField{}, fmt.Errorf("%s: %w", imported.pointer, err)
	}
	fieldName := protoFieldName(parameter.Name)
	if !protoIdentifierPattern.MatchString(fieldName) {
		return protoField{}, fmt.Errorf(
			"%s/name: generated field name %q is not a portable protobuf identifier",
			imported.pointer,
			fieldName,
		)
	}
	fieldType, err := c.typeForSchema(
		schema,
		parentName+protoTypeName(parameter.Name),
		imported.pointer+"/schema",
	)
	if err != nil {
		return protoField{}, err
	}
	required := path || (parameter.Required != nil && *parameter.Required)
	if required && (fieldType.kind == typeList || fieldType.kind == typeMap) {
		return protoField{}, fmt.Errorf(
			"%s/required: required arrays and maps cannot preserve parameter presence in protobuf",
			imported.pointer,
		)
	}
	description := strings.TrimSpace(parameter.Description)
	if description == "" {
		description = fmt.Sprintf("OpenAPI %s parameter %q.", parameter.In, parameter.Name)
	}
	return protoField{
		name:       fieldName,
		jsonName:   parameter.Name,
		comment:    description,
		fieldType:  fieldType,
		required:   required,
		deprecated: parameter.Deprecated,
	}, nil
}

func (c *converter) requestBody(
	requestBody *v3.RequestBody,
	suggestedName string,
	pointer string,
) (protoType, string, bool, error) {
	schema, err := jsonMediaSchema(requestBody.Content, pointer+"/content")
	if err != nil {
		return protoType{}, "", false, err
	}
	fieldType, err := c.typeForSchema(schema, suggestedName, pointer+"/content/application~1json/schema")
	if err != nil {
		return protoType{}, "", false, err
	}
	required := requestBody.Required != nil && *requestBody.Required
	if required && (fieldType.kind == typeList || fieldType.kind == typeMap) {
		return protoType{}, "", false, fmt.Errorf(
			"%s/required: required arrays and maps cannot preserve request-body presence in protobuf",
			pointer,
		)
	}
	description := strings.TrimSpace(requestBody.Description)
	if description == "" {
		description = "JSON request body."
	}
	return fieldType, description, required, nil
}

func (c *converter) buildResponse(
	spec operationSpec,
	methodName string,
) (string, string, error) {
	responses := spec.operation.Responses
	if responses == nil || responses.Codes == nil {
		return "", "", fmt.Errorf("%s/responses: at least one response is required", spec.pointer)
	}

	var successCodes []string
	for code := range responses.Codes.FromOldest() {
		value, err := strconv.Atoi(code)
		if err != nil {
			if strings.HasPrefix(code, "2") {
				return "", "", fmt.Errorf(
					"%s/responses/%s: wildcard success responses are ambiguous",
					spec.pointer,
					escapeJSONPointer(code),
				)
			}
			continue
		}
		if value >= 200 && value <= 299 {
			successCodes = append(successCodes, code)
		}
	}
	slices.Sort(successCodes)
	if len(successCodes) != 1 {
		return "", "", fmt.Errorf(
			"%s/responses: exactly one explicit 2xx response is required; found %d",
			spec.pointer,
			len(successCodes),
		)
	}
	if successCodes[0] != "200" {
		c.warn(fmt.Sprintf(
			"%s/responses/%s success status is not encoded; canonical gRPC has one ordinary success outcome",
			spec.pointer,
			escapeJSONPointer(successCodes[0]),
		))
	}

	for code, response := range responses.Codes.FromOldest() {
		if code == successCodes[0] {
			continue
		}
		if responseHasContract(response) {
			c.warn(fmt.Sprintf(
				"%s/responses/%s was not encoded; model HTTP errors as gRPC statuses in the canonical service",
				spec.pointer,
				escapeJSONPointer(code),
			))
		}
	}
	if responseHasContract(responses.Default) {
		c.warn(fmt.Sprintf(
			"%s/responses/default was not encoded; model HTTP errors as gRPC statuses in the canonical service",
			spec.pointer,
		))
	}

	response := responses.Codes.GetOrZero(successCodes[0])
	responsePointer := spec.pointer + "/responses/" + escapeJSONPointer(successCodes[0])
	if response == nil {
		return "", "", fmt.Errorf("%s: response is empty", responsePointer)
	}
	if response.Headers != nil && response.Headers.Len() > 0 {
		c.warn(responsePointer + "/headers were not encoded; use gRPC response metadata explicitly")
	}
	if response.Links != nil && response.Links.Len() > 0 {
		c.warn(responsePointer + "/links were not encoded")
	}
	if response.Content == nil || response.Content.Len() == 0 {
		c.file.imports["google/protobuf/empty.proto"] = struct{}{}
		return "google.protobuf.Empty", "", nil
	}

	schema, err := jsonMediaSchema(response.Content, responsePointer+"/content")
	if err != nil {
		return "", "", err
	}
	fieldType, err := c.typeForSchema(
		schema,
		methodName+"ResponseBody",
		responsePointer+"/content/application~1json/schema",
	)
	if err != nil {
		return "", "", err
	}

	fieldName := "value"
	switch fieldType.kind {
	case typeMessage:
		if !strings.HasPrefix(fieldType.name, "google.protobuf.") {
			fieldName = protoFieldName(fieldType.name)
		}
	case typeList:
		fieldName = "items"
	case typeMap:
		fieldName = "values"
	}
	responseComment := comment(response.Summary, response.Description)
	if responseComment == "" {
		responseComment = fmt.Sprintf("Response from %s.", methodName)
	}
	message := &protoMessage{
		name:    methodName + "Response",
		comment: responseComment,
		fields: []protoField{{
			name:      fieldName,
			jsonName:  fieldName,
			comment:   responseComment,
			fieldType: fieldType,
			number:    1,
		}},
	}
	if err := c.addMessage(message, responsePointer); err != nil {
		return "", "", err
	}
	return message.name, fieldName, nil
}

func jsonMediaSchema(
	content *orderedmap.Map[string, *v3.MediaType],
	pointer string,
) (*base.SchemaProxy, error) {
	if content == nil || content.Len() == 0 {
		return nil, fmt.Errorf("%s: application/json content is required", pointer)
	}
	if content.Len() != 1 || content.GetOrZero("application/json") == nil {
		var mediaTypes []string
		for mediaType := range content.FromOldest() {
			mediaTypes = append(mediaTypes, mediaType)
		}
		slices.Sort(mediaTypes)
		return nil, fmt.Errorf(
			"%s: exactly application/json is supported; found %s",
			pointer,
			strings.Join(mediaTypes, ", "),
		)
	}
	media := content.GetOrZero("application/json")
	if media.Encoding != nil && media.Encoding.Len() > 0 {
		return nil, fmt.Errorf(
			"%s/application~1json/encoding: per-property encodings are not supported",
			pointer,
		)
	}
	if media.ItemSchema != nil {
		return nil, fmt.Errorf(
			"%s/application~1json/itemSchema: OpenAPI 3.2 itemSchema is not supported",
			pointer,
		)
	}
	if media.Schema == nil {
		return nil, fmt.Errorf("%s/application~1json/schema: schema is required", pointer)
	}
	return media.Schema, nil
}

func responseHasContract(response *v3.Response) bool {
	if response == nil {
		return false
	}
	return (response.Content != nil && response.Content.Len() > 0) ||
		(response.Headers != nil && response.Headers.Len() > 0) ||
		(response.Links != nil && response.Links.Len() > 0)
}

func wireScalar(fieldType protoType) bool {
	return fieldType.kind == typeScalar
}

func wireQuery(fieldType protoType) bool {
	if wireScalar(fieldType) {
		return true
	}
	return fieldType.kind == typeList && fieldType.element != nil && wireScalar(*fieldType.element)
}

func normalizedFieldCollision(message, name, first, second string) error {
	return fmt.Errorf(
		"%s: field name %q collides between %s and %s after protobuf normalization",
		message,
		name,
		first,
		second,
	)
}
