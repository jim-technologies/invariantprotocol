// CLI projection — call tools from command-line arguments or request files.
//
// Format: ServiceName Method [-r request]
//
// Values for -r are auto-detected:
//   - Existing file path → load by extension (.yaml/.yml, .json, .binpb/.pb)
//   - Otherwise → parse as inline JSON
//
// Internally proto-first: input is deserialized directly into a proto.Message,
// passed through invoke() (proto in/out), then marshaled to JSON only at the
// terminal output boundary.
package invariant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
	"gopkg.in/yaml.v3"
)

func (s *Server) cli(ctx context.Context, args []string) (string, error) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		return s.cliHelp(), nil
	}

	serviceName, methodName, requestValue, err := splitCLIArgs(args)
	if err != nil {
		return "", err
	}

	toolName := s.resolveServiceMethod(serviceName, methodName)
	if toolName == "" {
		var available []string
		for k := range s.tools {
			available = append(available, k)
		}
		return "", fmt.Errorf("unknown service/method: %s %s. Available: %v", serviceName, methodName, available)
	}

	tool := s.tools[toolName]

	// Build proto request directly from input.
	req, err := s.newRequest(tool)
	if err != nil {
		return "", err
	}

	if requestValue != "" {
		if err := loadIntoProto(req, requestValue); err != nil {
			return "", fmt.Errorf("load request: %w", err)
		}
	}

	// Core dispatch (proto in / proto out).
	resp, err := s.invoke(ctx, tool, req)
	if err != nil {
		return "", err
	}

	// Boundary: proto → JSON (terminal output).
	if resp == nil {
		return "{}", nil
	}
	out, err := (protojson.MarshalOptions{UseProtoNames: true, Indent: "  "}).Marshal(resp)
	if err != nil {
		return "", fmt.Errorf("marshal response: %w", err)
	}
	return string(out), nil
}

// newRequest creates an empty proto.Message of the correct type for the tool's input.
func (s *Server) newRequest(tool *Tool) (proto.Message, error) {
	if provider, ok := tool.Handler.(interface {
		requestDescriptor() protoreflect.MessageDescriptor
	}); ok {
		return dynamicpb.NewMessage(provider.requestDescriptor()), nil
	}

	handlerVal := reflect.ValueOf(tool.Handler)
	handlerType := handlerVal.Type()
	if handlerType.NumIn() != 2 || handlerType.NumOut() != 2 {
		return nil, fmt.Errorf("handler has unexpected signature (in=%d, out=%d)", handlerType.NumIn(), handlerType.NumOut())
	}
	reqType := handlerType.In(1)
	reqMsg, ok := reflect.New(reqType.Elem()).Interface().(proto.Message)
	if !ok {
		return nil, fmt.Errorf("request type %s does not implement proto.Message", reqType)
	}
	return reqMsg, nil
}

// loadIntoProto populates a proto.Message from a file path or inline JSON string.
// File detection: if value is an existing file, load by extension.
// Inline: parse as JSON.
func loadIntoProto(msg proto.Message, value string) error {
	if _, err := os.Stat(value); err == nil {
		return loadFileIntoProto(msg, value)
	}

	// Inline JSON.
	if !json.Valid([]byte(value)) {
		return invalidArgumentError("cannot parse inline value as JSON")
	}
	if err := protojson.Unmarshal([]byte(value), msg); err != nil {
		return invalidArgumentFromJSONError(err)
	}
	return nil
}

// loadFileIntoProto reads a file and deserializes it into a proto.Message.
func loadFileIntoProto(msg proto.Message, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read file %s: %w", path, err)
	}

	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".binpb", ".pb":
		return proto.Unmarshal(data, msg)
	case ".json":
		if err := protojson.Unmarshal(data, msg); err != nil {
			return invalidArgumentFromJSONError(err)
		}
		return nil
	default: // .yaml, .yml
		var m any
		if err := yaml.Unmarshal(data, &m); err != nil {
			return fmt.Errorf("parse YAML file %s: %w", path, err)
		}
		jsonBytes, err := json.Marshal(convertYAMLToJSON(m))
		if err != nil {
			return fmt.Errorf("convert YAML to JSON for %s: %w", path, err)
		}
		if err := protojson.Unmarshal(jsonBytes, msg); err != nil {
			return invalidArgumentFromJSONError(err)
		}
		return nil
	}
}

// serveCLI reads args from os.Args and prints the result to stdout.
func (s *Server) serveCLI(ctx context.Context) error {
	args := os.Args[1:]
	for i, arg := range os.Args {
		if arg == "cli" {
			args = os.Args[i+1:]
			break
		}
	}

	result, err := s.cli(ctx, args)
	if err != nil {
		return err
	}
	fmt.Println(result)
	return nil
}

// splitCLIArgs parses: ServiceName Method [-r request].
func splitCLIArgs(args []string) (serviceName, methodName, requestValue string, err error) {
	i := 0

	if i >= len(args) || strings.HasPrefix(args[i], "-") {
		return "", "", "", errors.New("expected service name as first argument")
	}
	serviceName = args[i]
	i++

	if i >= len(args) || strings.HasPrefix(args[i], "-") {
		return "", "", "", errors.New("expected method name after service name")
	}
	methodName = args[i]
	i++

	if i < len(args) && args[i] == "-r" {
		i++
		if i >= len(args) {
			return "", "", "", errors.New("missing value after -r")
		}
		requestValue = args[i]
	}

	return serviceName, methodName, requestValue, nil
}

// cliHelp returns a help string listing all registered tools and their fields.
func (s *Server) cliHelp() string {
	var b strings.Builder
	b.WriteString("Usage: <binary> <ServiceName> <Method> [-r request.yaml|request.json|request.binpb|'{\"inline\":\"json\"}']\n\n")

	if len(s.tools) == 0 {
		b.WriteString("No tools registered.\n")
		return b.String()
	}

	// Group tools by service name for clean output.
	type entry struct {
		serviceName string
		tool        *Tool
	}
	var entries []entry
	for _, tool := range s.tools {
		parts := strings.Split(tool.ServiceFullName, ".")
		svcName := parts[len(parts)-1]
		entries = append(entries, entry{serviceName: svcName, tool: tool})
	}
	slices.SortFunc(entries, func(a, b entry) int {
		if a.serviceName != b.serviceName {
			return strings.Compare(a.serviceName, b.serviceName)
		}
		return strings.Compare(a.tool.MethodName, b.tool.MethodName)
	})

	b.WriteString("Available methods:\n\n")
	for _, e := range entries {
		fmt.Fprintf(&b, "  %s %s\n", e.serviceName, e.tool.MethodName)
		if e.tool.Description != "" && e.tool.Description != e.tool.Name {
			fmt.Fprintf(&b, "    %s\n", e.tool.Description)
		}

		props, _ := e.tool.InputSchema["properties"].(map[string]any)
		requiredSlice, _ := e.tool.InputSchema["required"].([]any)
		required := make(map[string]bool)
		for _, r := range requiredSlice {
			if s, ok := r.(string); ok {
				required[s] = true
			}
		}

		if len(props) > 0 {
			var fields []string
			for name := range props {
				fields = append(fields, name)
			}
			slices.Sort(fields)

			b.WriteString("    Fields:\n")
			for _, name := range fields {
				fieldSchema, _ := props[name].(map[string]any)
				typ := fieldType(fieldSchema)
				tag := ""
				if required[name] {
					tag = " (required)"
				}
				desc, _ := fieldSchema["description"].(string)
				line := fmt.Sprintf("      %-20s %-10s%s", name, typ, tag)
				if desc != "" {
					line += "  — " + desc
				}
				b.WriteString(line + "\n")
			}
		}
		b.WriteString("\n")
	}

	return b.String()
}

// fieldType returns a human-readable type string from a JSON Schema property.
// For enums, it returns "val1|val2|..." instead of "string".
// For arrays of objects, it returns "array<object>".
func fieldType(schema map[string]any) string {
	if vals, ok := schema["enum"].([]any); ok && len(vals) > 0 {
		var names []string
		for _, v := range vals {
			if s, ok := v.(string); ok {
				names = append(names, s)
			}
		}
		return strings.Join(names, "|")
	}
	typ, _ := schema["type"].(string)
	if typ == "" {
		return "any"
	}
	if typ == "array" {
		if items, ok := schema["items"].(map[string]any); ok {
			itemType, _ := items["type"].(string)
			if itemType != "" {
				return "array<" + itemType + ">"
			}
		}
	}
	return typ
}

// resolveServiceMethod matches ServiceName + Method to a registered tool name.
func (s *Server) resolveServiceMethod(service, method string) string {
	for _, tool := range s.tools {
		parts := strings.Split(tool.ServiceFullName, ".")
		svcName := parts[len(parts)-1]
		if svcName == service && tool.MethodName == method {
			return tool.Name
		}
	}
	return ""
}

// convertYAMLToJSON converts yaml.v3 decoded values to JSON-compatible types.
// yaml.v3 decodes map keys as string, but nested structures may need conversion.
func convertYAMLToJSON(v any) any {
	switch v := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, val := range v {
			out[k] = convertYAMLToJSON(val)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, val := range v {
			out[i] = convertYAMLToJSON(val)
		}
		return out
	case int:
		return float64(v)
	case int64:
		return float64(v)
	default:
		return v
	}
}
