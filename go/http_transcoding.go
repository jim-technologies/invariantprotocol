package invariant

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	annotationspb "google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/proto"
)

type httpBinding struct {
	method   string
	pattern  string
	body     string
	tool     *Tool
	template *pathTemplate
}

type pathTemplate struct {
	segments []pathSegment
}

type pathSegment struct {
	literal string
	field   string
	multi   bool
}

func (s *Server) buildHTTPBindings() ([]*httpBinding, error) {
	var bindings []*httpBinding

	if s.fds != nil {
		annotated, err := s.buildAnnotatedHTTPBindings()
		if err != nil {
			return nil, err
		}
		bindings = append(bindings, annotated...)
	}

	// Always keep canonical RPC-style routes for compatibility.
	names := make([]string, 0, len(s.tools))
	for name := range s.tools {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		tool := s.tools[name]
		pattern := fmt.Sprintf("/%s/%s", tool.ServiceFullName, tool.MethodName)
		binding, err := newHTTPBinding(http.MethodPost, pattern, "*", tool)
		if err != nil {
			return nil, err
		}
		bindings = append(bindings, binding)
	}

	return bindings, nil
}

func (s *Server) buildAnnotatedHTTPBindings() ([]*httpBinding, error) {
	toolByMethod := make(map[string]*Tool, len(s.tools))
	for _, tool := range s.tools {
		key := fmt.Sprintf("/%s/%s", tool.ServiceFullName, tool.MethodName)
		toolByMethod[key] = tool
	}

	var bindings []*httpBinding
	for _, file := range s.fds.GetFile() {
		pkg := file.GetPackage()
		for _, svc := range file.GetService() {
			svcFullName := qualifiedName(pkg, svc.GetName())
			for _, method := range svc.GetMethod() {
				key := fmt.Sprintf("/%s/%s", svcFullName, method.GetName())
				tool := toolByMethod[key]
				if tool == nil {
					continue
				}

				opts := method.GetOptions()
				if opts == nil || !proto.HasExtension(opts, annotationspb.E_Http) {
					continue
				}

				ext := proto.GetExtension(opts, annotationspb.E_Http)
				rule, ok := ext.(*annotationspb.HttpRule)
				if !ok || rule == nil {
					return nil, fmt.Errorf("invalid google.api.http extension for %s", key)
				}

				ruleBindings, err := httpRuleBindings(rule, tool)
				if err != nil {
					return nil, fmt.Errorf("parse google.api.http for %s: %w", key, err)
				}
				bindings = append(bindings, ruleBindings...)
			}
		}
	}

	return bindings, nil
}

func httpRuleBindings(rule *annotationspb.HttpRule, tool *Tool) ([]*httpBinding, error) {
	method, pattern, err := httpMethodAndPattern(rule)
	if err != nil {
		return nil, err
	}

	binding, err := newHTTPBinding(method, pattern, rule.GetBody(), tool)
	if err != nil {
		return nil, err
	}

	out := []*httpBinding{binding}
	for _, additional := range rule.GetAdditionalBindings() {
		if additional == nil {
			continue
		}
		nested, err := httpRuleBindings(additional, tool)
		if err != nil {
			return nil, err
		}
		out = append(out, nested...)
	}

	return out, nil
}

func httpMethodAndPattern(rule *annotationspb.HttpRule) (string, string, error) {
	switch pattern := rule.Pattern.(type) {
	case *annotationspb.HttpRule_Get:
		return http.MethodGet, pattern.Get, nil
	case *annotationspb.HttpRule_Post:
		return http.MethodPost, pattern.Post, nil
	case *annotationspb.HttpRule_Put:
		return http.MethodPut, pattern.Put, nil
	case *annotationspb.HttpRule_Delete:
		return http.MethodDelete, pattern.Delete, nil
	case *annotationspb.HttpRule_Patch:
		return http.MethodPatch, pattern.Patch, nil
	case *annotationspb.HttpRule_Custom:
		if pattern.Custom == nil {
			return "", "", errors.New("custom pattern is nil")
		}
		return strings.ToUpper(pattern.Custom.Kind), pattern.Custom.Path, nil
	default:
		return "", "", errors.New("http rule missing pattern")
	}
}

func newHTTPBinding(method, pattern, body string, tool *Tool) (*httpBinding, error) {
	template, err := parsePathTemplate(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid route pattern %q: %w", pattern, err)
	}

	return &httpBinding{
		method:   strings.ToUpper(method),
		pattern:  pattern,
		body:     body,
		tool:     tool,
		template: template,
	}, nil
}

func parsePathTemplate(pattern string) (*pathTemplate, error) {
	if !strings.HasPrefix(pattern, "/") {
		return nil, errors.New("path must start with '/'")
	}

	trimmed := strings.Trim(pattern, "/")
	if trimmed == "" {
		return &pathTemplate{}, nil
	}

	rawSegments := strings.Split(trimmed, "/")
	segments := make([]pathSegment, 0, len(rawSegments))

	for i, raw := range rawSegments {
		if strings.HasPrefix(raw, "{") && strings.HasSuffix(raw, "}") {
			inner := strings.TrimSuffix(strings.TrimPrefix(raw, "{"), "}")
			field := inner
			wildcard := "*"
			if strings.Contains(inner, "=") {
				parts := strings.SplitN(inner, "=", 2)
				field = parts[0]
				wildcard = parts[1]
			}

			if field == "" {
				return nil, errors.New("empty field in variable segment")
			}

			switch wildcard {
			case "*", "":
				segments = append(segments, pathSegment{field: field})
			case "**":
				if i != len(rawSegments)-1 {
					return nil, errors.New("** wildcard is only supported in the final segment")
				}
				segments = append(segments, pathSegment{field: field, multi: true})
			default:
				return nil, fmt.Errorf("unsupported wildcard pattern %q", wildcard)
			}
			continue
		}

		segments = append(segments, pathSegment{literal: raw})
	}

	return &pathTemplate{segments: segments}, nil
}

func (t *pathTemplate) match(path string) (map[string]string, bool) {
	trimmed := strings.Trim(path, "/")
	var parts []string
	if trimmed != "" {
		parts = strings.Split(trimmed, "/")
	}

	params := make(map[string]string)
	idx := 0

	for _, seg := range t.segments {
		if seg.multi {
			if idx > len(parts) {
				return nil, false
			}
			raw := strings.Join(parts[idx:], "/")
			val, err := url.PathUnescape(raw)
			if err != nil {
				val = raw
			}
			params[seg.field] = val
			idx = len(parts)
			continue
		}

		if idx >= len(parts) {
			return nil, false
		}

		part := parts[idx]
		if seg.field == "" {
			if part != seg.literal {
				return nil, false
			}
		} else {
			val, err := url.PathUnescape(part)
			if err != nil {
				val = part
			}
			params[seg.field] = val
		}
		idx++
	}

	if idx != len(parts) {
		return nil, false
	}
	return params, true
}

func findHTTPBinding(bindings []*httpBinding, method, path string) (*httpBinding, map[string]string, bool) {
	var methodMismatch bool

	for _, binding := range bindings {
		pathParams, ok := binding.template.match(path)
		if !ok {
			continue
		}
		if strings.EqualFold(binding.method, method) {
			return binding, pathParams, false
		}
		methodMismatch = true
	}

	return nil, nil, methodMismatch
}

func (b *httpBinding) requestArgs(r *http.Request, pathParams map[string]string) (json.RawMessage, error) {
	args := make(map[string]any)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, invalidArgumentError("read body: " + err.Error())
	}

	if err := b.applyBody(args, body); err != nil {
		return nil, err
	}

	for field, raw := range pathParams {
		coerced, err := b.coerceScalar(field, raw)
		if err != nil {
			return nil, err
		}
		if err := setNestedField(args, field, coerced); err != nil {
			return nil, err
		}
	}

	for key, values := range r.URL.Query() {
		coerced, err := b.coerceQueryValues(key, values)
		if err != nil {
			return nil, err
		}
		if err := setNestedField(args, key, coerced); err != nil {
			return nil, err
		}
	}

	if len(args) == 0 {
		return json.RawMessage("{}"), nil
	}

	raw, err := json.Marshal(args)
	if err != nil {
		return nil, invalidArgumentError("marshal request arguments: " + err.Error())
	}
	return raw, nil
}

func (b *httpBinding) applyBody(args map[string]any, body []byte) error {
	trimmed := bytes.TrimSpace(body)

	switch b.body {
	case "":
		if len(trimmed) > 0 {
			return invalidArgumentError("request body is not allowed for this route")
		}
		return nil
	case "*":
		if len(trimmed) == 0 {
			return nil
		}

		var decoded any
		if err := json.Unmarshal(trimmed, &decoded); err != nil {
			return invalidArgumentError("invalid JSON body: " + err.Error())
		}

		obj, ok := decoded.(map[string]any)
		if !ok {
			return invalidArgumentError("request body must be a JSON object for body:\"*\"")
		}
		maps.Copy(args, obj)
		return nil
	default:
		if len(trimmed) == 0 {
			return nil
		}

		var decoded any
		if err := json.Unmarshal(trimmed, &decoded); err != nil {
			return invalidArgumentError("invalid JSON body: " + err.Error())
		}
		return setNestedField(args, b.body, decoded)
	}
}

func (b *httpBinding) coerceQueryValues(field string, values []string) (any, error) {
	if len(values) == 0 {
		return nil, nil
	}

	schema := schemaForFieldPath(b.tool.InputSchema, field)
	if schema == nil {
		if len(values) == 1 {
			return values[0], nil
		}
		out := make([]any, 0, len(values))
		for _, v := range values {
			out = append(out, v)
		}
		return out, nil
	}

	if schemaType, _ := schema["type"].(string); schemaType == "array" {
		itemSchema, _ := schema["items"].(map[string]any)
		out := make([]any, 0, len(values))
		for _, raw := range values {
			coerced, err := coerceScalarValue(raw, itemSchema)
			if err != nil {
				return nil, invalidArgumentError(fmt.Sprintf("invalid query value for %q: %v", field, err))
			}
			out = append(out, coerced)
		}
		return out, nil
	}

	if len(values) > 1 {
		return nil, invalidArgumentError(fmt.Sprintf("multiple query values provided for non-repeated field %q", field))
	}

	coerced, err := coerceScalarValue(values[0], schema)
	if err != nil {
		return nil, invalidArgumentError(fmt.Sprintf("invalid query value for %q: %v", field, err))
	}
	return coerced, nil
}

func (b *httpBinding) coerceScalar(field, raw string) (any, error) {
	schema := schemaForFieldPath(b.tool.InputSchema, field)
	if schema == nil {
		return raw, nil
	}

	coerced, err := coerceScalarValue(raw, schema)
	if err != nil {
		return nil, invalidArgumentError(fmt.Sprintf("invalid path value for %q: %v", field, err))
	}
	return coerced, nil
}

func schemaForFieldPath(schema map[string]any, fieldPath string) map[string]any {
	current := schema
	for part := range strings.SplitSeq(fieldPath, ".") {
		propsAny, ok := current["properties"]
		if !ok {
			return nil
		}
		props, ok := propsAny.(map[string]any)
		if !ok {
			return nil
		}
		nextAny, ok := props[part]
		if !ok {
			return nil
		}
		next, ok := nextAny.(map[string]any)
		if !ok {
			return nil
		}
		current = next
	}

	return current
}

func coerceScalarValue(raw string, schema map[string]any) (any, error) {
	schemaType, _ := schema["type"].(string)
	switch schemaType {
	case "integer":
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, err
		}
		return n, nil
	case "number":
		n, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, err
		}
		return n, nil
	case "boolean":
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, err
		}
		return b, nil
	default:
		return raw, nil
	}
}

func setNestedField(root map[string]any, fieldPath string, value any) error {
	parts := strings.Split(fieldPath, ".")
	current := root

	for i, part := range parts {
		last := i == len(parts)-1
		if last {
			current[part] = value
			return nil
		}

		next, ok := current[part]
		if !ok {
			child := make(map[string]any)
			current[part] = child
			current = child
			continue
		}

		child, ok := next.(map[string]any)
		if !ok {
			return invalidArgumentError(fmt.Sprintf("field path conflict at %q", strings.Join(parts[:i+1], ".")))
		}
		current = child
	}

	return nil
}
