package invariant

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// HTTPMetadataMapper selects untrusted HTTP request values that may be exposed
// to handlers as incoming gRPC metadata. Invariant applies a reserved-key
// filter after the mapper returns; identity and authorization metadata cannot
// be asserted by naming an HTTP header.
type HTTPMetadataMapper func(*http.Request) metadata.MD

// DefaultHTTPMetadataMapper forwards only common tracing/correlation headers.
// Authentication middleware should validate credentials and place trusted
// identity in context, not copy caller-provided identity headers into metadata.
func DefaultHTTPMetadataMapper(r *http.Request) metadata.MD {
	md := metadata.MD{}
	for _, key := range []string{"traceparent", "tracestate", "baggage", "x-request-id"} {
		if values := r.Header.Values(key); len(values) > 0 {
			md[key] = append([]string(nil), values...)
		}
	}
	return md
}

// UseHTTPMetadataMapper replaces the inbound HTTP metadata mapper. Reserved
// protocol, authorization, tenant, principal, role, and internal identity keys
// are removed from the mapper's result before it reaches application code.
func (s *Server) UseHTTPMetadataMapper(mapper HTTPMetadataMapper) {
	if mapper == nil {
		mapper = DefaultHTTPMetadataMapper
	}
	s.updateConfiguration("HTTP metadata mapper", func() { s.httpMetadataMapper = mapper })
}

func (s *Server) incomingHTTPMetadata(r *http.Request) metadata.MD {
	s.mu.RLock()
	mapper := s.httpMetadataMapper
	s.mu.RUnlock()
	if mapper == nil {
		mapper = DefaultHTTPMetadataMapper
	}
	mapped := mapper(r)
	out := metadata.MD{}
	for key, values := range mapped {
		key = strings.ToLower(strings.TrimSpace(key))
		if key == "" || reservedInboundMetadata(key) {
			continue
		}
		if !validMetadataKey(key) {
			continue
		}
		for _, value := range values {
			if strings.HasSuffix(key, "-bin") {
				decoded, err := base64.RawStdEncoding.DecodeString(value)
				if err != nil {
					decoded, err = base64.StdEncoding.DecodeString(value)
				}
				if err == nil {
					out[key] = append(out[key], string(decoded))
				}
				continue
			}
			if validASCIIMetadataValue(value) {
				out[key] = append(out[key], value)
			}
		}
	}
	return out
}

func reservedInboundMetadata(key string) bool {
	if strings.HasPrefix(key, "grpc-") || strings.HasPrefix(key, "connect-") ||
		strings.HasPrefix(key, "invariant-internal-") ||
		strings.HasPrefix(key, "x-invariant-internal-") ||
		strings.HasPrefix(key, "x-tenant") || strings.HasPrefix(key, "x-principal") ||
		strings.HasPrefix(key, "x-role") || strings.HasPrefix(key, "x-user") ||
		strings.HasPrefix(key, "x-auth") || strings.HasPrefix(key, "x-internal-") ||
		strings.HasPrefix(key, "internal-") || strings.HasPrefix(key, "tenant-") ||
		strings.HasPrefix(key, "principal-") || strings.HasPrefix(key, "role-") ||
		strings.HasPrefix(key, "user-") || strings.HasPrefix(key, "auth-") ||
		strings.HasPrefix(key, "subject-") || strings.HasPrefix(key, "identity-") {
		return true
	}
	// Binary gRPC metadata is only an encoding variant of the same logical
	// key. Do not let an untrusted mapper bypass an authorization or transport
	// reservation by appending "-bin".
	baseKey := strings.TrimSuffix(key, "-bin")
	switch baseKey {
	case "authorization", "proxy-authorization", "cookie", "set-cookie",
		"authentication", "api-key", "x-api-key", "tenant", "principal",
		"role", "user", "subject", "identity", "te", "host", "connection",
		"keep-alive", "proxy-connection", "transfer-encoding", "upgrade",
		"content-length", "content-type", "trailer":
		return true
	default:
		return false
	}
}

type projectionUnaryTransport struct {
	method     string
	mu         sync.Mutex
	header     metadata.MD
	trailer    metadata.MD
	headerSent bool
}

func newProjectionUnaryTransport(method string) *projectionUnaryTransport {
	return &projectionUnaryTransport{method: method, header: metadata.MD{}, trailer: metadata.MD{}}
}

func (s *projectionUnaryTransport) Method() string { return s.method }

func (s *projectionUnaryTransport) SetHeader(md metadata.MD) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.headerSent {
		return errors.New("grpc: cannot set header after SendHeader")
	}
	s.header = metadata.Join(s.header, md)
	return nil
}

func (s *projectionUnaryTransport) SendHeader(md metadata.MD) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.headerSent {
		return errors.New("grpc: SendHeader called multiple times")
	}
	s.header = metadata.Join(s.header, md)
	s.headerSent = true
	return nil
}

func (s *projectionUnaryTransport) SetTrailer(md metadata.MD) error {
	s.mu.Lock()
	s.trailer = metadata.Join(s.trailer, md)
	s.mu.Unlock()
	return nil
}

func (s *projectionUnaryTransport) metadata() (metadata.MD, metadata.MD) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.header.Copy(), s.trailer.Copy()
}

func withProjectionUnaryTransport(ctx context.Context, method string) (context.Context, *projectionUnaryTransport) {
	if grpc.ServerTransportStreamFromContext(ctx) != nil {
		return ctx, nil
	}
	transport := newProjectionUnaryTransport(method)
	return grpc.NewContextWithServerTransportStream(ctx, transport), transport
}

func writeConnectMetadataHeaders(header http.Header, md metadata.MD, trailers bool) {
	for key, values := range md {
		key = strings.ToLower(key)
		if reservedResponseMetadata(key) || !validMetadataKey(key) {
			continue
		}
		name := http.CanonicalHeaderKey(key)
		if trailers {
			name = "Trailer-" + name
		}
		for _, value := range values {
			if strings.HasSuffix(key, "-bin") {
				value = base64.RawStdEncoding.EncodeToString([]byte(value))
			} else if !validASCIIMetadataValue(value) {
				continue
			}
			header.Add(name, value)
		}
	}
}

func writeUnaryProjectionMetadata(header http.Header, transport *projectionUnaryTransport) {
	if transport == nil {
		return
	}
	leading, trailing := transport.metadata()
	writeConnectMetadataHeaders(header, leading, false)
	writeConnectMetadataHeaders(header, trailing, true)
}

func connectEndStreamMetadata(md metadata.MD) map[string][]string {
	out := map[string][]string{}
	for key, values := range md {
		key = strings.ToLower(key)
		if reservedResponseMetadata(key) || !validMetadataKey(key) {
			continue
		}
		for _, value := range values {
			if strings.HasSuffix(key, "-bin") {
				value = base64.RawStdEncoding.EncodeToString([]byte(value))
			} else if !validASCIIMetadataValue(value) {
				continue
			}
			out[key] = append(out[key], value)
		}
	}
	return out
}

func reservedResponseMetadata(key string) bool {
	if strings.HasPrefix(key, "grpc-") || strings.HasPrefix(key, "connect-") ||
		strings.HasPrefix(key, "trailer-") || strings.HasPrefix(key, "invariant-internal-") ||
		strings.HasPrefix(key, "x-invariant-internal-") {
		return true
	}
	switch key {
	case "content-length", "content-type", "content-encoding", "transfer-encoding",
		"accept-encoding", "connection", "keep-alive", "proxy-connection", "te",
		"trailer", "upgrade", "host":
		return true
	default:
		return false
	}
}

func validMetadataKey(key string) bool {
	if key == "" {
		return false
	}
	for _, char := range key {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') ||
			char == '-' || char == '_' || char == '.' {
			continue
		}
		return false
	}
	return true
}

func validASCIIMetadataValue(value string) bool {
	for _, char := range []byte(value) {
		if char < 0x20 || char > 0x7e {
			return false
		}
	}
	return true
}
