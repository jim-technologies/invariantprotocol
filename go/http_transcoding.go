package invariant

// Path-template parsing for outbound HTTP client (`google.api.http` proxying).
// Server-side does NOT use this — the HTTP server only exposes the canonical
// Connect route POST /{package.Service}/{Method}.

import (
	"errors"
	"fmt"
	"strings"
)

type pathTemplate struct {
	segments []pathSegment
}

type pathSegment struct {
	literal string
	field   string
	multi   bool
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
