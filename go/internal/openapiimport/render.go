package openapiimport

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
)

const (
	annotationsImport   = "google/api/annotations.proto"
	fieldBehaviorImport = "google/api/field_behavior.proto"
	validateImport      = "buf/validate/validate.proto"
)

func renderProto(file protoFile) ([]byte, error) {
	if err := validatePackage(file.packageName); err != nil {
		return nil, err
	}
	if err := validateGoPackage(file.goPackage); err != nil {
		return nil, err
	}
	if !protoIdentifierPattern.MatchString(file.service.name) {
		return nil, fmt.Errorf(
			"render protobuf: service name %q is not a portable identifier",
			file.service.name,
		)
	}
	if len(file.service.methods) == 0 {
		return nil, errors.New("render protobuf: service has no methods")
	}

	imports := make(map[string]struct{}, len(file.imports)+3)
	for path := range file.imports {
		imports[path] = struct{}{}
	}
	imports[annotationsImport] = struct{}{}
	for _, message := range file.messages {
		if message == nil {
			return nil, errors.New("render protobuf: message is nil")
		}
		for _, field := range message.fields {
			if field.required {
				imports[fieldBehaviorImport] = struct{}{}
			}
			if field.required && field.fieldType.hasPresence() ||
				len(field.fieldType.fieldConstraints()) > 0 {
				imports[validateImport] = struct{}{}
			}
		}
	}

	var output bytes.Buffer
	output.WriteString("// Imported from OpenAPI by invariant-openapi.\n")
	output.WriteString("// Review this one-way conversion, then maintain protobuf as the canonical contract.\n\n")
	output.WriteString("syntax = \"proto3\";\n\n")
	fmt.Fprintf(&output, "package %s;\n", file.packageName)

	importPaths := sortedKeys(imports)
	if len(importPaths) > 0 {
		output.WriteByte('\n')
		for _, path := range importPaths {
			fmt.Fprintf(&output, "import %s;\n", quoted(path))
		}
	}

	output.WriteByte('\n')
	fmt.Fprintf(&output, "option go_package = %s;\n", quoted(file.goPackage))
	output.WriteByte('\n')
	writeComment(&output, file.service.comment)
	fmt.Fprintf(&output, "service %s {\n", file.service.name)
	for index, method := range file.service.methods {
		if index > 0 {
			output.WriteByte('\n')
		}
		if err := writeMethod(&output, method); err != nil {
			return nil, err
		}
	}
	output.WriteString("}\n")

	messageNames := sortedKeys(file.messages)
	for _, name := range messageNames {
		message := file.messages[name]
		if message.name == "" {
			return nil, fmt.Errorf("render protobuf: message registered as %q has an empty name", name)
		}
		if message.name != name {
			return nil, fmt.Errorf(
				"render protobuf: message registered as %q is named %q",
				name,
				message.name,
			)
		}
		output.WriteByte('\n')
		writeComment(&output, message.comment)
		fmt.Fprintf(&output, "message %s {\n", message.name)
		for index, field := range message.fields {
			if err := writeField(&output, message.name, index, field); err != nil {
				return nil, err
			}
		}
		output.WriteString("}\n")
	}

	return output.Bytes(), nil
}

func writeMethod(output *bytes.Buffer, method protoMethod) error {
	if method.name == "" || method.input == "" || method.output == "" {
		return errors.New("render protobuf: RPC name, input, and output must not be empty")
	}
	if !protoIdentifierPattern.MatchString(method.name) {
		return fmt.Errorf(
			"render protobuf: RPC name %q is not a portable identifier",
			method.name,
		)
	}
	switch method.httpMethod {
	case "get", "post", "put", "patch", "delete":
	default:
		return fmt.Errorf(
			"render protobuf: RPC %s has unsupported HTTP method %q",
			method.name,
			method.httpMethod,
		)
	}
	if method.httpPath == "" {
		return fmt.Errorf("render protobuf: RPC %s has an empty HTTP path", method.name)
	}

	writeIndentedComment(output, method.comment, "  ")
	fmt.Fprintf(
		output,
		"  rpc %s(%s) returns (%s) {\n",
		method.name,
		method.input,
		method.output,
	)
	if method.body == "" && method.responseBody == "" {
		fmt.Fprintf(
			output,
			"    option (google.api.http) = {%s: %s};\n",
			method.httpMethod,
			quoted(method.httpPath),
		)
	} else {
		output.WriteString("    option (google.api.http) = {\n")
		fmt.Fprintf(output, "      %s: %s\n", method.httpMethod, quoted(method.httpPath))
		if method.body != "" {
			fmt.Fprintf(output, "      body: %s\n", quoted(method.body))
		}
		if method.responseBody != "" {
			fmt.Fprintf(output, "      response_body: %s\n", quoted(method.responseBody))
		}
		output.WriteString("    };\n")
	}
	if method.deprecated {
		output.WriteString("    option deprecated = true;\n")
	}
	output.WriteString("  }\n")
	return nil
}

func writeField(
	output *bytes.Buffer,
	messageName string,
	index int,
	field protoField,
) error {
	if field.name == "" || field.fieldType.declaration() == "" {
		return fmt.Errorf(
			"render protobuf: message %s field %d has an empty name or type",
			messageName,
			index+1,
		)
	}
	if !protoIdentifierPattern.MatchString(field.name) {
		return fmt.Errorf(
			"render protobuf: message %s field name %q is not a portable identifier",
			messageName,
			field.name,
		)
	}
	if field.number <= 0 {
		return fmt.Errorf(
			"render protobuf: message %s field %s has invalid number %d",
			messageName,
			field.name,
			field.number,
		)
	}

	writeIndentedComment(output, field.comment, "  ")
	output.WriteString("  ")
	if field.fieldType.needsOptionalKeyword() {
		output.WriteString("optional ")
	}
	fmt.Fprintf(
		output,
		"%s %s = %d",
		field.fieldType.declaration(),
		field.name,
		field.number,
	)

	options := fieldOptions(field)
	switch len(options) {
	case 0:
	case 1:
		output.WriteString(" [")
		output.WriteString(options[0])
		output.WriteByte(']')
	default:
		output.WriteString(" [\n")
		for optionIndex, option := range options {
			output.WriteString("    ")
			output.WriteString(option)
			if optionIndex+1 < len(options) {
				output.WriteByte(',')
			}
			output.WriteByte('\n')
		}
		output.WriteString("  ]")
	}
	output.WriteString(";\n")
	return nil
}

func fieldOptions(field protoField) []string {
	var options []string
	if field.jsonName != "" && field.jsonName != defaultJSONName(field.name) {
		options = append(options, "json_name = "+quoted(field.jsonName))
	}
	if field.deprecated {
		options = append(options, "deprecated = true")
	}
	if field.required {
		options = append(options, "(google.api.field_behavior) = REQUIRED")
		if field.fieldType.hasPresence() {
			options = append(options, "(buf.validate.field).required = true")
		}
	}
	rules := field.fieldType.fieldConstraints()
	slices.SortFunc(rules, func(left, right constraint) int {
		if left.path != right.path {
			return strings.Compare(left.path, right.path)
		}
		return strings.Compare(left.value, right.value)
	})
	for _, rule := range rules {
		options = append(
			options,
			fmt.Sprintf("(buf.validate.field).%s = %s", rule.path, rule.value),
		)
	}
	return options
}

func writeComment(output *bytes.Buffer, value string) {
	writeIndentedComment(output, value, "")
}

func writeIndentedComment(output *bytes.Buffer, value, indent string) {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	for line := range strings.SplitSeq(value, "\n") {
		output.WriteString(indent)
		output.WriteString("//")
		if strings.TrimSpace(line) != "" {
			output.WriteByte(' ')
			output.WriteString(strings.TrimRight(line, " \t"))
		}
		output.WriteByte('\n')
	}
}
