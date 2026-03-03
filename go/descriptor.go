// Package invariant parses FileDescriptorSets into typed descriptor info and
// projects gRPC services into MCP/CLI/HTTP tools.
package invariant

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	invpb "github.com/jim-technologies/invariantprotocol/go/gen/invariant/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// parseDescriptorFile reads a FileDescriptorSet binary file and returns a ParsedDescriptor.
func parseDescriptorFile(path string) (*invpb.ParsedDescriptor, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read descriptor file: %w", err)
	}
	return ParseDescriptorBytes(data)
}

// ParseDescriptorBytes parses a serialized FileDescriptorSet and returns a ParsedDescriptor.
// Use this with go:embed to avoid runtime file dependencies.
func ParseDescriptorBytes(data []byte) (*invpb.ParsedDescriptor, error) {
	var fds descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(data, &fds); err != nil {
		return nil, fmt.Errorf("unmarshal FileDescriptorSet: %w", err)
	}
	return parseFileDescriptorSet(&fds), nil
}

func parseFileDescriptorSet(fds *descriptorpb.FileDescriptorSet) *invpb.ParsedDescriptor {
	pd := &invpb.ParsedDescriptor{
		Services: make(map[string]*invpb.ServiceInfo),
		Messages: make(map[string]*invpb.MessageInfo),
		Enums:    make(map[string]*invpb.EnumInfo),
	}

	for _, file := range fds.GetFile() {
		comments := extractComments(file)
		pkg := file.GetPackage()

		for i, enumProto := range file.GetEnumType() {
			fullName := qualifiedName(pkg, enumProto.GetName())
			pd.Enums[fullName] = parseEnum(enumProto, fullName, comments, []int32{5, int32(i)})
		}

		for i, msgProto := range file.GetMessageType() {
			fullName := qualifiedName(pkg, msgProto.GetName())
			parseMessage(pd, msgProto, fullName, comments, []int32{4, int32(i)})
		}

		for i, svcProto := range file.GetService() {
			fullName := qualifiedName(pkg, svcProto.GetName())
			svc := &invpb.ServiceInfo{
				Name:     svcProto.GetName(),
				FullName: fullName,
				Methods:  make(map[string]*invpb.MethodInfo),
				Comment:  comments[pathKey([]int32{6, int32(i)})],
			}
			for j, methodProto := range svcProto.GetMethod() {
				method := &invpb.MethodInfo{
					Name:            methodProto.GetName(),
					InputType:       strings.TrimPrefix(methodProto.GetInputType(), "."),
					OutputType:      strings.TrimPrefix(methodProto.GetOutputType(), "."),
					Comment:         comments[pathKey([]int32{6, int32(i), 2, int32(j)})],
					ClientStreaming: methodProto.GetClientStreaming(),
					ServerStreaming: methodProto.GetServerStreaming(),
				}
				svc.Methods[methodProto.GetName()] = method
			}
			pd.Services[fullName] = svc
		}
	}

	return pd
}

func parseMessage(
	pd *invpb.ParsedDescriptor,
	msgProto *descriptorpb.DescriptorProto,
	fullName string,
	comments map[string]string,
	pathPrefix []int32,
) {
	// Nested enums
	for i, enumProto := range msgProto.GetEnumType() {
		enumFullName := fullName + "." + enumProto.GetName()
		pd.Enums[enumFullName] = parseEnum(enumProto, enumFullName, comments, append(sliceClone(pathPrefix), 4, int32(i)))
	}

	// Nested messages
	for i, nestedProto := range msgProto.GetNestedType() {
		nestedFullName := fullName + "." + nestedProto.GetName()
		parseMessage(pd, nestedProto, nestedFullName, comments, append(sliceClone(pathPrefix), 3, int32(i)))
	}

	// Oneofs
	var oneofs []*invpb.OneofInfo
	for i, oneofProto := range msgProto.GetOneofDecl() {
		oneofs = append(oneofs, &invpb.OneofInfo{
			Name:    oneofProto.GetName(),
			Comment: comments[pathKey(append(sliceClone(pathPrefix), 8, int32(i)))],
		})
	}

	// Fields
	var fields []*invpb.FieldInfo
	for i, fieldProto := range msgProto.GetField() {
		var oneofIdx *int32
		if fieldProto.OneofIndex != nil {
			idx := fieldProto.GetOneofIndex()
			// proto3 optional uses a synthetic oneof — treat as regular optional
			if fieldProto.GetProto3Optional() {
				oneofIdx = nil
			} else {
				oneofIdx = &idx
			}
		}

		typeName := strings.TrimPrefix(fieldProto.GetTypeName(), ".")

		field := &invpb.FieldInfo{
			Name:       fieldProto.GetName(),
			Number:     fieldProto.GetNumber(),
			Type:       int32(fieldProto.GetType()),
			TypeName:   typeName,
			Label:      int32(fieldProto.GetLabel()),
			Comment:    comments[pathKey(append(sliceClone(pathPrefix), 2, int32(i)))],
			OneofIndex: oneofIdx,
			Optional:   fieldProto.GetProto3Optional(),
		}
		fields = append(fields, field)

		if oneofIdx != nil && int(*oneofIdx) < len(oneofs) {
			oneofs[*oneofIdx].FieldNames = append(oneofs[*oneofIdx].FieldNames, field.Name)
		}
	}

	isMapEntry := false
	if msgProto.GetOptions() != nil {
		isMapEntry = msgProto.GetOptions().GetMapEntry()
	}

	pd.Messages[fullName] = &invpb.MessageInfo{
		Name:       msgProto.GetName(),
		FullName:   fullName,
		Fields:     fields,
		Oneofs:     oneofs,
		Comment:    comments[pathKey(pathPrefix)],
		IsMapEntry: isMapEntry,
	}
}

func parseEnum(
	enumProto *descriptorpb.EnumDescriptorProto,
	fullName string,
	comments map[string]string,
	pathPrefix []int32,
) *invpb.EnumInfo {
	var values []*invpb.EnumValueInfo
	for i, val := range enumProto.GetValue() {
		values = append(values, &invpb.EnumValueInfo{
			Name:    val.GetName(),
			Number:  val.GetNumber(),
			Comment: comments[pathKey(append(sliceClone(pathPrefix), 2, int32(i)))],
		})
	}
	return &invpb.EnumInfo{
		Name:     enumProto.GetName(),
		FullName: fullName,
		Values:   values,
		Comment:  comments[pathKey(pathPrefix)],
	}
}

func extractComments(file *descriptorpb.FileDescriptorProto) map[string]string {
	comments := make(map[string]string)
	sci := file.GetSourceCodeInfo()
	if sci == nil {
		return comments
	}
	for _, loc := range sci.GetLocation() {
		comment := strings.TrimSpace(loc.GetLeadingComments())
		if comment == "" {
			comment = strings.TrimSpace(loc.GetTrailingComments())
		}
		if comment != "" {
			comments[pathKey(loc.GetPath())] = comment
		}
	}
	return comments
}

func qualifiedName(pkg, name string) string {
	if pkg == "" {
		return name
	}
	return pkg + "." + name
}

func pathKey(path []int32) string {
	parts := make([]string, len(path))
	for i, p := range path {
		parts[i] = strconv.Itoa(int(p))
	}
	return strings.Join(parts, ",")
}

func sliceClone(s []int32) []int32 {
	c := make([]int32, len(s))
	copy(c, s)
	return c
}
