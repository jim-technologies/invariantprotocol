// CLI projection — call tools from command-line arguments or request files.
//
// Format: ServiceName Method [-r request]
//
// Values for -r are auto-detected:
//   - Existing file path → load by extension (.json, .binpb, .pb)
//   - Otherwise → parse as inline JSON
//
// Internally proto-first: input is deserialized directly into a proto.Message,
// passed through invoke() (proto in/out), then marshaled to JSON only at the
// terminal output boundary.
package invariant

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// cli runs one CLI invocation and returns its complete output as a string.
// Internally calls cliWrite into a buffer — convenient for tests; serveCLI uses
// cliWrite directly with os.Stdout so streaming output reaches the user in
// real time.
func (s *Server) cli(ctx context.Context, args []string) (string, error) {
	var buf bytes.Buffer
	if err := s.cliWrite(ctx, args, &buf); err != nil {
		return "", err
	}
	return strings.TrimRight(buf.String(), "\n"), nil
}

// cliWrite is the streaming-aware CLI executor. Each chunk of a server-
// streaming response is flushed to w as it arrives.
func (s *Server) cliWrite(ctx context.Context, args []string, w io.Writer) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		_, err := io.WriteString(w, s.cliHelp())
		return err
	}

	serviceName, methodName, requestValue, err := splitCLIArgs(args)
	if err != nil {
		return err
	}

	toolName := s.resolveServiceMethod(serviceName, methodName)
	if toolName == "" {
		var available []string
		for k := range s.tools {
			available = append(available, k)
		}
		return fmt.Errorf("unknown service/method: %s %s. Available: %v", serviceName, methodName, available)
	}

	tool := s.tools[toolName]

	req, err := s.newRequest(tool)
	if err != nil {
		return err
	}

	if requestValue != "" {
		if err := loadIntoProto(req, requestValue); err != nil {
			return fmt.Errorf("load request: %w", err)
		}
	}

	if tool.ServerStreaming {
		return s.cliStream(ctx, tool, req, w)
	}

	resp, err := s.invoke(ctx, tool, req)
	if err != nil {
		return err
	}

	if resp == nil {
		_, err := io.WriteString(w, "{}")
		return err
	}
	out, err := (protojson.MarshalOptions{UseProtoNames: true, Indent: "  "}).Marshal(resp)
	if err != nil {
		return fmt.Errorf("marshal response: %w", err)
	}
	_, err = w.Write(out)
	return err
}

// cliStream runs a server-streaming tool, writing each chunk as a JSON line
// to w as it arrives. The writer is flushed after every chunk so an
// unbuffered consumer (`./app StreamGreet | jq`) sees output in real time.
func (s *Server) cliStream(ctx context.Context, tool *Tool, req proto.Message, w io.Writer) error {
	flusher := newAutoFlushWriter(w)
	marshalOpts := protojson.MarshalOptions{UseProtoNames: true}
	return s.invokeStream(ctx, tool, req, func(msg proto.Message) error {
		raw, err := marshalOpts.Marshal(msg)
		if err != nil {
			return fmt.Errorf("marshal stream chunk: %w", err)
		}
		if _, err := flusher.Write(raw); err != nil {
			return err
		}
		if _, err := flusher.Write([]byte{'\n'}); err != nil {
			return err
		}
		return flusher.Flush()
	})
}

// autoFlushWriter writes through to an underlying writer, flushing to either
// http.Flusher or *bufio.Writer if the underlying writer supports it.
type autoFlushWriter struct {
	w   io.Writer
	buf *bufio.Writer
}

func newAutoFlushWriter(w io.Writer) *autoFlushWriter {
	if b, ok := w.(*bufio.Writer); ok {
		return &autoFlushWriter{w: w, buf: b}
	}
	return &autoFlushWriter{w: w}
}

func (a *autoFlushWriter) Write(p []byte) (int, error) { return a.w.Write(p) }

func (a *autoFlushWriter) Flush() error {
	if a.buf != nil {
		return a.buf.Flush()
	}
	if f, ok := a.w.(interface{ Flush() error }); ok {
		return f.Flush()
	}
	if f, ok := a.w.(interface{ Sync() error }); ok {
		return f.Sync()
	}
	return nil
}

// newRequest creates an empty proto.Message of the correct type for the tool's input.
// Reflection happens once at addTool time; this is just a cached factory call.
func (s *Server) newRequest(tool *Tool) (proto.Message, error) {
	if tool.newRequest == nil {
		return nil, fmt.Errorf("tool %q has no usable request factory (bad handler signature)", tool.Name)
	}
	return tool.newRequest(), nil
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
// Supported extensions: .json, .binpb, .pb.
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
	default:
		return invalidArgumentError(fmt.Sprintf("unsupported request file extension %q (use .json, .binpb, or .pb)", ext))
	}
}

// serveCLI reads args from os.Args and writes the result(s) to stdout.
// For streaming tools, chunks are flushed as they arrive — `./app Stream | jq`
// works as a real pipeline rather than blocking until the stream ends.
func (s *Server) serveCLI(ctx context.Context) error {
	args := os.Args[1:]
	for i, arg := range os.Args {
		if arg == "cli" {
			args = os.Args[i+1:]
			break
		}
	}

	if err := s.cliWrite(ctx, args, os.Stdout); err != nil {
		return err
	}
	// Trailing newline so the next prompt lands on a fresh line.
	_, _ = os.Stdout.WriteString("\n")
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
	b.WriteString("Usage: <binary> <ServiceName> <Method> [-r request.json|request.binpb|'{\"inline\":\"json\"}']\n\n")

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
