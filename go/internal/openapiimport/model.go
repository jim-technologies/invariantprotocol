package openapiimport

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

const MaxDocumentBytes = 16 << 20

// Options controls the small amount of information OpenAPI cannot provide for
// a protobuf contract. Package and GoPackage are required. Service defaults
// from info.title.
type Options struct {
	Package string
	// GoPackage is the generated Go import path, without a semicolon package
	// alias. An OpenAPI document cannot supply this repository identity.
	GoPackage string
	Service   string
}

// Result is a reviewable protobuf source file plus non-fatal fidelity notes.
type Result struct {
	Source   []byte
	Warnings []string
}

type protoFile struct {
	packageName string
	goPackage   string
	service     protoService
	messages    map[string]*protoMessage
	imports     map[string]struct{}
}

type protoService struct {
	name    string
	comment string
	methods []protoMethod
}

type protoMethod struct {
	name         string
	comment      string
	input        string
	output       string
	httpMethod   string
	httpPath     string
	body         string
	responseBody string
	deprecated   bool
}

type protoMessage struct {
	name    string
	comment string
	fields  []protoField
}

type protoField struct {
	name       string
	jsonName   string
	comment    string
	fieldType  protoType
	number     int
	required   bool
	deprecated bool
}

type protoType struct {
	name        string
	kind        typeKind
	constraints []constraint
	element     *protoType
}

type typeKind uint8

const (
	typeScalar typeKind = iota
	typeMessage
	typeList
	typeMap
)

type constraint struct {
	path  string
	value string
}

func (t protoType) declaration() string {
	switch t.kind {
	case typeList:
		return "repeated " + t.element.name
	case typeMap:
		return "map<string, " + t.element.name + ">"
	default:
		return t.name
	}
}

func (t protoType) hasPresence() bool {
	return t.kind == typeScalar || t.kind == typeMessage
}

func (t protoType) needsOptionalKeyword() bool {
	return t.kind == typeScalar && !strings.Contains(t.name, ".")
}

func (t protoType) fieldConstraints() []constraint {
	switch t.kind {
	case typeList:
		out := make([]constraint, 0, len(t.constraints)+len(t.element.constraints))
		out = append(out, t.constraints...)
		for _, rule := range t.element.constraints {
			out = append(out, constraint{path: "repeated.items." + rule.path, value: rule.value})
		}
		return out
	case typeMap:
		out := make([]constraint, 0, len(t.constraints)+len(t.element.constraints))
		out = append(out, t.constraints...)
		for _, rule := range t.element.constraints {
			out = append(out, constraint{path: "map.values." + rule.path, value: rule.value})
		}
		return out
	default:
		return append([]constraint(nil), t.constraints...)
	}
}

var (
	packagePattern   = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$`)
	goPackagePattern = regexp.MustCompile(
		`^[A-Za-z0-9][A-Za-z0-9._~-]*(/[A-Za-z0-9][A-Za-z0-9._~-]*)+$`,
	)
)

func validatePackage(name string) error {
	if !packagePattern.MatchString(name) {
		return fmt.Errorf(
			"package %q must be a lowercase protobuf package such as example.catalog.v1",
			name,
		)
	}
	for segment := range strings.SplitSeq(name, ".") {
		if _, reserved := protoKeywords[segment]; reserved {
			return fmt.Errorf(
				"package %q uses reserved protobuf keyword %q",
				name,
				segment,
			)
		}
	}
	return nil
}

func validateGoPackage(name string) error {
	if !goPackagePattern.MatchString(name) {
		return fmt.Errorf(
			"go package %q must be a portable Go import path such as github.com/acme/project/gen/example/v1",
			name,
		)
	}
	if strings.Contains(name, ";") {
		return fmt.Errorf("go package %q must not contain a semicolon package alias", name)
	}
	return nil
}

func protoTypeName(value string) string {
	words := splitWords(value)
	var out strings.Builder
	for _, word := range words {
		if word == "" {
			continue
		}
		lower := strings.ToLower(word)
		out.WriteString(strings.ToUpper(lower[:1]))
		out.WriteString(lower[1:])
	}
	name := out.String()
	if name == "" {
		name = "Api"
	}
	if name[0] >= '0' && name[0] <= '9' {
		name = "Api" + name
	}
	return name
}

func protoFieldName(value string) string {
	words := splitWords(value)
	for i := range words {
		words[i] = strings.ToLower(words[i])
	}
	name := strings.Join(words, "_")
	if name == "" {
		name = "field"
	}
	if name[0] >= '0' && name[0] <= '9' {
		name = "field_" + name
	}
	if _, reserved := protoKeywords[name]; reserved {
		name += "_"
	}
	return name
}

func splitWords(value string) []string {
	runes := []rune(value)
	var words []string
	start := -1
	flush := func(end int) {
		if start >= 0 && end > start {
			words = append(words, string(runes[start:end]))
		}
		start = -1
	}
	for i, current := range runes {
		if !unicode.IsLetter(current) && !unicode.IsDigit(current) {
			flush(i)
			continue
		}
		if start < 0 {
			start = i
			continue
		}
		previous := runes[i-1]
		nextLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
		if unicode.IsUpper(current) &&
			(unicode.IsLower(previous) || unicode.IsDigit(previous) ||
				(unicode.IsUpper(previous) && nextLower)) {
			flush(i)
			start = i
		}
	}
	flush(len(runes))
	return words
}

func defaultJSONName(protoName string) string {
	var out strings.Builder
	upperNext := false
	for _, current := range protoName {
		if current == '_' {
			upperNext = true
			continue
		}
		if upperNext {
			out.WriteRune(unicode.ToUpper(current))
			upperNext = false
			continue
		}
		out.WriteRune(current)
	}
	return out.String()
}

func comment(summary, description string) string {
	summary = strings.TrimSpace(summary)
	description = strings.TrimSpace(description)
	switch {
	case summary == "":
		return description
	case description == "":
		return summary
	case strings.EqualFold(summary, description):
		return description
	default:
		return summary + "\n\n" + description
	}
}

func quoted(value string) string {
	return strconv.Quote(value)
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

var protoKeywords = map[string]struct{}{
	"bool": {}, "bytes": {}, "double": {}, "enum": {}, "extend": {}, "extensions": {},
	"false": {}, "fixed32": {}, "fixed64": {}, "float": {}, "group": {}, "import": {},
	"int32": {}, "int64": {}, "map": {}, "max": {}, "message": {}, "oneof": {},
	"option": {}, "optional": {}, "package": {}, "public": {}, "repeated": {},
	"required": {}, "reserved": {}, "returns": {}, "rpc": {}, "service": {},
	"sfixed32": {}, "sfixed64": {}, "sint32": {}, "sint64": {}, "stream": {},
	"string": {}, "syntax": {}, "to": {}, "true": {}, "uint32": {}, "uint64": {},
	"weak": {},
}
