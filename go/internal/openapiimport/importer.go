package openapiimport

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"slices"
	"strings"

	"github.com/pb33f/libopenapi"
	"github.com/pb33f/libopenapi/datamodel"
	base "github.com/pb33f/libopenapi/datamodel/high/base"
	v3 "github.com/pb33f/libopenapi/datamodel/high/v3"
)

var (
	openAPIVersionPattern = regexp.MustCompile(`^3\.(0|1)\.\d+([+-][0-9A-Za-z.-]+)?$`)
	pathVariablePattern   = regexp.MustCompile(`\{([^{}]+)\}`)
	pathVariableSegment   = regexp.MustCompile(`^\{[^{}]+\}$`)
)

const openAPI31BaseDialect = "https://spec.openapis.org/oas/3.1/dialect/base"

type converter struct {
	document        *v3.Document
	file            protoFile
	messageOrigins  map[string]string
	componentTypes  map[string]protoType
	building        map[string]bool
	warnings        []string
	warningSet      map[string]struct{}
	operationNames  map[string]string
	serviceOverride string
}

type operationSpec struct {
	httpMethod string
	path       string
	pathItem   *v3.PathItem
	operation  *v3.Operation
	pointer    string
}

// Convert translates a bundled OpenAPI 3.0/3.1 document into reviewable
// protobuf source. The source becomes canonical after import; this function is
// deliberately not a round-trip synchronization engine.
func Convert(spec []byte, options Options) (*Result, error) {
	if len(spec) == 0 {
		return nil, errors.New("OpenAPI document is empty")
	}
	if len(spec) > MaxDocumentBytes {
		return nil, fmt.Errorf(
			"OpenAPI document is %d bytes; maximum is %d",
			len(spec),
			MaxDocumentBytes,
		)
	}
	if err := validatePackage(options.Package); err != nil {
		return nil, err
	}
	if err := validateGoPackage(options.GoPackage); err != nil {
		return nil, err
	}

	config := datamodel.NewDocumentConfiguration()
	config.AllowFileReferences = false
	config.AllowRemoteReferences = false
	config.SkipExternalRefResolution = true
	config.ExcludeExtensionRefs = true
	config.ExtractRefsSequentially = true
	config.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	document, err := libopenapi.NewDocumentWithConfiguration(spec, config)
	if err != nil {
		return nil, fmt.Errorf("parse OpenAPI document: %w", err)
	}
	if !openAPIVersionPattern.MatchString(document.GetVersion()) {
		return nil, fmt.Errorf(
			"unsupported OpenAPI version %q; expected 3.0.x or 3.1.x",
			document.GetVersion(),
		)
	}

	model, err := document.BuildV3Model()
	if err != nil {
		return nil, fmt.Errorf("build OpenAPI model: %w", err)
	}
	for _, reference := range model.Index.GetAllSequencedReferences() {
		if strings.HasPrefix(reference.RawRef, "#") {
			continue
		}
		location := "$ref"
		if reference.KeyNode != nil {
			location = fmt.Sprintf("$ref at line %d, column %d", reference.KeyNode.Line, reference.KeyNode.Column)
		}
		return nil, fmt.Errorf(
			"%s points outside the input document (%q); bundle the OpenAPI document before importing",
			location,
			reference.RawRef,
		)
	}

	c := &converter{
		document: &model.Model,
		file: protoFile{
			packageName: options.Package,
			goPackage:   options.GoPackage,
			messages:    make(map[string]*protoMessage),
			imports:     make(map[string]struct{}),
		},
		messageOrigins:  make(map[string]string),
		componentTypes:  make(map[string]protoType),
		building:        make(map[string]bool),
		warningSet:      make(map[string]struct{}),
		operationNames:  make(map[string]string),
		serviceOverride: options.Service,
	}
	if err := c.convert(); err != nil {
		return nil, err
	}
	source, err := renderProto(c.file)
	if err != nil {
		return nil, err
	}
	slices.Sort(c.warnings)
	return &Result{Source: source, Warnings: c.warnings}, nil
}

func (c *converter) convert() error {
	if c.document.JsonSchemaDialect != "" &&
		c.document.JsonSchemaDialect != openAPI31BaseDialect {
		return fmt.Errorf(
			"#/jsonSchemaDialect: custom JSON Schema dialect %q is not supported",
			c.document.JsonSchemaDialect,
		)
	}
	if c.document.Info == nil {
		return errors.New("OpenAPI info object is required")
	}
	if strings.TrimSpace(c.document.Info.Title) == "" {
		return errors.New("#/info/title: OpenAPI info.title is required")
	}
	serviceName := c.serviceOverride
	if serviceName == "" {
		serviceName = protoTypeName(c.document.Info.Title)
	} else {
		serviceName = protoTypeName(serviceName)
	}
	if !strings.HasSuffix(serviceName, "Service") {
		serviceName += "Service"
	}
	if !protoIdentifierPattern.MatchString(serviceName) {
		source := "#/info/title"
		if c.serviceOverride != "" {
			source = "--service"
		}
		return fmt.Errorf(
			"%s: generated service name %q is not a portable protobuf identifier",
			source,
			serviceName,
		)
	}
	c.file.service = protoService{
		name:    serviceName,
		comment: comment(c.document.Info.Title, c.document.Info.Description),
	}

	if c.document.Webhooks != nil && c.document.Webhooks.Len() > 0 {
		return errors.New("#/webhooks: OpenAPI webhooks cannot be represented by unary gRPC methods")
	}
	if len(c.document.Servers) > 0 {
		c.warn("#/servers were not encoded; service endpoints remain deployment configuration")
	}
	if len(c.document.Security) > 0 {
		c.warn("OpenAPI security requirements remain caller-owned HTTP authentication policy and were not encoded in protobuf")
	}
	if c.document.Paths == nil || c.document.Paths.PathItems == nil ||
		c.document.Paths.PathItems.Len() == 0 {
		return errors.New("#/paths: at least one OpenAPI operation is required")
	}

	operations, err := c.operations()
	if err != nil {
		return err
	}
	for _, operation := range operations {
		method, err := c.convertOperation(operation)
		if err != nil {
			return err
		}
		c.file.service.methods = append(c.file.service.methods, method)
	}
	return nil
}

func (c *converter) operations() ([]operationSpec, error) {
	var operations []operationSpec
	for path, item := range c.document.Paths.PathItems.FromOldest() {
		if item == nil {
			return nil, fmt.Errorf("#/paths/%s: path item is empty", escapeJSONPointer(path))
		}
		if item.IsReference() {
			return nil, fmt.Errorf(
				"#/paths/%s: path-item references are not supported; inline the path item before importing",
				escapeJSONPointer(path),
			)
		}
		if item.AdditionalOperations != nil && item.AdditionalOperations.Len() > 0 {
			return nil, fmt.Errorf(
				"#/paths/%s: OpenAPI 3.2 additional operations are not supported",
				escapeJSONPointer(path),
			)
		}
		if len(item.Servers) > 0 {
			c.warn(fmt.Sprintf(
				"#/paths/%s/servers were not encoded; service endpoints remain deployment configuration",
				escapeJSONPointer(path),
			))
		}
		candidates := []struct {
			name      string
			operation *v3.Operation
		}{
			{"get", item.Get},
			{"post", item.Post},
			{"put", item.Put},
			{"patch", item.Patch},
			{"delete", item.Delete},
			{"head", item.Head},
			{"options", item.Options},
			{"trace", item.Trace},
			{"query", item.Query},
		}
		for _, candidate := range candidates {
			if candidate.operation == nil {
				continue
			}
			pointer := "#/paths/" + escapeJSONPointer(path) + "/" + candidate.name
			switch candidate.name {
			case "get", "post", "put", "patch", "delete":
			default:
				return nil, fmt.Errorf(
					"%s: HTTP %s is not supported by google.api.http import",
					pointer,
					strings.ToUpper(candidate.name),
				)
			}
			operations = append(operations, operationSpec{
				httpMethod: candidate.name,
				path:       path,
				pathItem:   item,
				operation:  candidate.operation,
				pointer:    pointer,
			})
		}
	}
	slices.SortFunc(operations, func(left, right operationSpec) int {
		if left.path != right.path {
			return strings.Compare(left.path, right.path)
		}
		return strings.Compare(left.httpMethod, right.httpMethod)
	})
	if len(operations) == 0 {
		return nil, errors.New("#/paths: at least one supported HTTP operation is required")
	}
	return operations, nil
}

func (c *converter) convertOperation(spec operationSpec) (protoMethod, error) {
	operation := spec.operation
	if operation.Callbacks != nil && operation.Callbacks.Len() > 0 {
		return protoMethod{}, fmt.Errorf(
			"%s/callbacks: callbacks cannot be represented by a unary gRPC method",
			spec.pointer,
		)
	}
	if len(operation.Security) > 0 {
		c.warn(fmt.Sprintf(
			"%s security requirements remain caller-owned HTTP authentication policy",
			spec.pointer,
		))
	}
	if len(operation.Servers) > 0 {
		c.warn(spec.pointer + "/servers were not encoded; service endpoints remain deployment configuration")
	}

	methodName := protoTypeName(operation.OperationId)
	if operation.OperationId == "" {
		methodName = fallbackMethodName(spec.httpMethod, spec.path)
	}
	if !protoIdentifierPattern.MatchString(methodName) {
		return protoMethod{}, fmt.Errorf(
			"%s/operationId: generated RPC name %q is not a portable protobuf identifier",
			spec.pointer,
			methodName,
		)
	}
	if previous, exists := c.operationNames[methodName]; exists {
		return protoMethod{}, fmt.Errorf(
			"%s: RPC name %q collides with %s after protobuf normalization",
			spec.pointer,
			methodName,
			previous,
		)
	}
	c.operationNames[methodName] = spec.pointer

	request, httpPath, body, err := c.buildRequest(spec, methodName)
	if err != nil {
		return protoMethod{}, err
	}
	response, responseBody, err := c.buildResponse(spec, methodName)
	if err != nil {
		return protoMethod{}, err
	}

	return protoMethod{
		name:         methodName,
		comment:      operationComment(spec),
		input:        request,
		output:       response,
		httpMethod:   spec.httpMethod,
		httpPath:     httpPath,
		body:         body,
		responseBody: responseBody,
		deprecated:   operation.Deprecated != nil && *operation.Deprecated,
	}, nil
}

func fallbackMethodName(method, path string) string {
	var words []string
	words = append(words, method)
	cursor := 0
	for _, match := range pathVariablePattern.FindAllStringSubmatchIndex(path, -1) {
		words = append(words, splitWords(path[cursor:match[0]])...)
		words = append(words, "by")
		words = append(words, splitWords(path[match[2]:match[3]])...)
		cursor = match[1]
	}
	words = append(words, splitWords(path[cursor:])...)
	return protoTypeName(strings.Join(words, " "))
}

func operationComment(spec operationSpec) string {
	value := comment(spec.operation.Summary, spec.operation.Description)
	if value != "" {
		return value
	}
	return fmt.Sprintf("%s %s.", strings.ToUpper(spec.httpMethod), spec.path)
}

func (c *converter) warn(message string) {
	if _, exists := c.warningSet[message]; exists {
		return
	}
	c.warningSet[message] = struct{}{}
	c.warnings = append(c.warnings, message)
}

func escapeJSONPointer(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}

func internalComponentReference(reference, section string) (string, error) {
	prefix := "#/components/" + section + "/"
	if !strings.HasPrefix(reference, prefix) {
		return "", fmt.Errorf("expected an internal %s component reference, got %q", section, reference)
	}
	name := strings.TrimPrefix(reference, prefix)
	if name == "" || strings.Contains(name, "/") {
		return "", fmt.Errorf("invalid internal component reference %q", reference)
	}
	name = strings.ReplaceAll(strings.ReplaceAll(name, "~1", "/"), "~0", "~")
	return name, nil
}

func schemaFromParameter(parameter *v3.Parameter) (*base.SchemaProxy, error) {
	if parameter.Content != nil && parameter.Content.Len() > 0 {
		return nil, errors.New("content-based parameters are not supported")
	}
	if parameter.Schema == nil {
		return nil, errors.New("parameter schema is required")
	}
	return parameter.Schema, nil
}
