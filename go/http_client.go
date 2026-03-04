package invariant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"

	invpb "github.com/jim-technologies/invariantprotocol/go/gen/invariant/v1"
	annotationspb "google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

type httpDynamicHandler struct {
	client     *http.Client
	baseURL    *url.URL
	binding    *httpClientBinding
	headers    map[string]string
	reqDesc    protoreflect.MessageDescriptor
	respDesc   protoreflect.MessageDescriptor
	methodPath string
}

type httpClientBinding struct {
	method   string
	pattern  string
	body     string
	template *pathTemplate
}

func (h *httpDynamicHandler) requestDescriptor() protoreflect.MessageDescriptor {
	return h.reqDesc
}

func (h *httpDynamicHandler) callProto(ctx context.Context, req proto.Message) (proto.Message, error) {
	bodyBytes, endpointURL, err := h.binding.build(req, h.baseURL)
	if err != nil {
		return nil, err
	}

	var bodyReader io.Reader
	if len(bodyBytes) > 0 {
		bodyReader = bytes.NewReader(bodyBytes)
	}

	httpReq, err := http.NewRequestWithContext(ctx, h.binding.method, endpointURL, bodyReader)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "build HTTP request for %s: %v", h.methodPath, err)
	}
	if len(bodyBytes) > 0 {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	httpReq.Header.Set("Accept", "application/json")
	for name, value := range h.headers {
		httpReq.Header.Set(name, value)
	}

	httpResp, err := h.client.Do(httpReq) //nolint:gosec // base URL is explicit caller configuration for remote proxy mode
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "http call %s %s failed: %v", h.binding.method, endpointURL, err)
	}
	defer httpResp.Body.Close()

	rawResp, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "read HTTP response body: %v", err)
	}

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return nil, httpClientError(httpResp.StatusCode, rawResp)
	}

	resp := dynamicpb.NewMessage(h.respDesc)
	trimmed := bytes.TrimSpace(rawResp)
	if len(trimmed) == 0 {
		return resp, nil
	}

	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(trimmed, resp); err != nil {
		return nil, status.Errorf(codes.Internal, "decode HTTP response JSON: %v", err)
	}
	return resp, nil
}

func (h *httpDynamicHandler) CallJSON(ctx context.Context, argsJSON json.RawMessage) (string, error) {
	req := dynamicpb.NewMessage(h.reqDesc)
	if len(argsJSON) > 0 && string(argsJSON) != "null" {
		if err := protojson.Unmarshal(argsJSON, req); err != nil {
			return "", invalidArgumentFromJSONError(err)
		}
	}

	resp, err := h.callProto(ctx, req)
	if err != nil {
		return "", err
	}

	out, err := (protojson.MarshalOptions{UseProtoNames: true, Indent: "  "}).Marshal(resp)
	if err != nil {
		return "", status.Errorf(codes.Internal, "marshal response: %v", err)
	}
	return string(out), nil
}

// ConnectHTTP registers tools that proxy to a remote HTTP endpoint.
// Routes are derived from google.api.http annotations when present, otherwise
// fallback to canonical RPC route: POST /{serviceFullName}/{method}.
func (s *Server) ConnectHTTP(baseURL string, serviceName ...string) error {
	if s.fds == nil {
		return errors.New("connect HTTP requires a Server created via ServerFromDescriptor or ServerFromBytes")
	}

	parsedBaseURL, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("parse base URL %q: %w", baseURL, err)
	}
	if parsedBaseURL.Scheme != "http" && parsedBaseURL.Scheme != "https" {
		return errors.New("base URL must use http:// or https://")
	}
	if parsedBaseURL.Host == "" {
		return errors.New("base URL must include host")
	}
	headers := outboundHTTPHeadersFromEnv()

	files, err := s.buildProtoFiles()
	if err != nil {
		return err
	}

	httpRules, err := s.httpRulesByMethodPath()
	if err != nil {
		return err
	}

	services := s.parsed.Services
	if len(serviceName) > 0 && serviceName[0] != "" {
		name := serviceName[0]
		svcInfo, ok := services[name]
		if !ok {
			return fmt.Errorf("service %q not found in descriptor", name)
		}
		services = map[string]*invpb.ServiceInfo{name: svcInfo}
	}

	for svcFullName, svcInfo := range services {
		for methodName, methodInfo := range svcInfo.Methods {
			if methodInfo.ClientStreaming || methodInfo.ServerStreaming {
				continue
			}

			reqDesc, err := findMessageDescriptor(files, methodInfo.InputType)
			if err != nil {
				return err
			}
			respDesc, err := findMessageDescriptor(files, methodInfo.OutputType)
			if err != nil {
				return err
			}

			methodPath := fmt.Sprintf("/%s/%s", svcFullName, methodName)
			binding, err := pickHTTPClientBinding(httpRules[methodPath], svcFullName, methodName)
			if err != nil {
				return fmt.Errorf("build HTTP binding for %s: %w", methodPath, err)
			}

			toolName := svcInfo.Name + "." + methodName
			description := methodInfo.Comment
			if description == "" {
				description = toolName
			}

			s.tools[toolName] = &Tool{
				Name:        toolName,
				Description: description,
				InputSchema: s.schemaGen.MessageToSchema(methodInfo.InputType),
				Handler: &httpDynamicHandler{
					client:     &http.Client{},
					baseURL:    parsedBaseURL,
					binding:    binding,
					headers:    headers,
					reqDesc:    reqDesc,
					respDesc:   respDesc,
					methodPath: methodPath,
				},
				InputType:       methodInfo.InputType,
				OutputType:      methodInfo.OutputType,
				ServiceFullName: svcFullName,
				MethodName:      methodName,
			}
		}
	}

	return nil
}

func (s *Server) httpRulesByMethodPath() (map[string]*annotationspb.HttpRule, error) {
	out := make(map[string]*annotationspb.HttpRule)

	for _, file := range s.fds.GetFile() {
		pkg := file.GetPackage()
		for _, svc := range file.GetService() {
			svcFullName := qualifiedName(pkg, svc.GetName())
			for _, method := range svc.GetMethod() {
				opts := method.GetOptions()
				if opts == nil || !proto.HasExtension(opts, annotationspb.E_Http) {
					continue
				}

				ext := proto.GetExtension(opts, annotationspb.E_Http)
				rule, ok := ext.(*annotationspb.HttpRule)
				if !ok || rule == nil {
					return nil, fmt.Errorf("invalid google.api.http extension for /%s/%s", svcFullName, method.GetName())
				}

				out[fmt.Sprintf("/%s/%s", svcFullName, method.GetName())] = rule
			}
		}
	}

	return out, nil
}

func pickHTTPClientBinding(rule *annotationspb.HttpRule, svcFullName, methodName string) (*httpClientBinding, error) {
	if rule == nil {
		pattern := fmt.Sprintf("/%s/%s", svcFullName, methodName)
		return newHTTPClientBinding(http.MethodPost, pattern, "*")
	}

	method, pattern, err := httpMethodAndPattern(rule)
	if err != nil {
		return nil, err
	}
	return newHTTPClientBinding(method, pattern, rule.GetBody())
}

func newHTTPClientBinding(method, pattern, body string) (*httpClientBinding, error) {
	template, err := parsePathTemplate(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid route pattern %q: %w", pattern, err)
	}
	return &httpClientBinding{
		method:   strings.ToUpper(method),
		pattern:  pattern,
		body:     body,
		template: template,
	}, nil
}

func (b *httpClientBinding) build(req proto.Message, baseURL *url.URL) ([]byte, string, error) {
	args, err := messageToMap(req)
	if err != nil {
		return nil, "", status.Errorf(codes.InvalidArgument, "marshal request JSON: %v", err)
	}

	working := cloneMap(args)
	endpointPath, consumedPath, err := b.expandPath(working)
	if err != nil {
		return nil, "", err
	}
	for _, field := range consumedPath {
		deleteNestedField(working, field)
	}

	bodyBytes, consumedBody, err := b.encodeBody(working)
	if err != nil {
		return nil, "", err
	}
	if consumedBody != "" && consumedBody != "*" {
		deleteNestedField(working, consumedBody)
	}

	query := url.Values{}
	if b.body != "*" {
		if err := encodeQueryFields("", working, query); err != nil {
			return nil, "", err
		}
	}

	u := *baseURL
	basePath := strings.TrimSuffix(u.Path, "/")
	if basePath == "" {
		u.Path = endpointPath
	} else if endpointPath == "/" {
		u.Path = basePath + "/"
	} else {
		u.Path = basePath + endpointPath
	}
	u.RawQuery = query.Encode()

	return bodyBytes, u.String(), nil
}

func (b *httpClientBinding) expandPath(args map[string]any) (string, []string, error) {
	if b.template == nil || len(b.template.segments) == 0 {
		return "/", nil, nil
	}

	parts := make([]string, 0, len(b.template.segments))
	consumed := make([]string, 0, len(b.template.segments))

	for _, seg := range b.template.segments {
		if seg.field == "" {
			parts = append(parts, seg.literal)
			continue
		}

		raw, ok := getNestedField(args, seg.field)
		if !ok {
			return "", nil, status.Errorf(codes.InvalidArgument, "missing path field %q", seg.field)
		}
		encoded, err := encodePathValue(raw, seg.multi)
		if err != nil {
			return "", nil, status.Errorf(codes.InvalidArgument, "invalid path field %q: %v", seg.field, err)
		}

		parts = append(parts, encoded)
		consumed = append(consumed, seg.field)
	}

	return "/" + strings.Join(parts, "/"), consumed, nil
}

func (b *httpClientBinding) encodeBody(args map[string]any) ([]byte, string, error) {
	switch b.body {
	case "":
		return nil, "", nil
	case "*":
		if len(args) == 0 {
			return nil, "*", nil
		}
		body, err := json.Marshal(args)
		if err != nil {
			return nil, "", status.Errorf(codes.InvalidArgument, "marshal body: %v", err)
		}
		return body, "*", nil
	default:
		val, ok := getNestedField(args, b.body)
		if !ok {
			return nil, b.body, nil
		}
		body, err := json.Marshal(val)
		if err != nil {
			return nil, "", status.Errorf(codes.InvalidArgument, "marshal body field %q: %v", b.body, err)
		}
		return body, b.body, nil
	}
}

func messageToMap(msg proto.Message) (map[string]any, error) {
	raw, err := (protojson.MarshalOptions{UseProtoNames: true}).Marshal(msg)
	if err != nil {
		return nil, err
	}

	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return map[string]any{}, nil
	}

	var out map[string]any
	if err := json.Unmarshal(trimmed, &out); err != nil {
		return nil, err
	}
	if out == nil {
		return map[string]any{}, nil
	}
	return out, nil
}

func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	maps.Copy(out, in)
	for key, val := range out {
		switch v := val.(type) {
		case map[string]any:
			out[key] = cloneMap(v)
		case []any:
			out[key] = cloneSlice(v)
		}
	}
	return out
}

func cloneSlice(in []any) []any {
	out := make([]any, len(in))
	copy(out, in)
	for i, val := range out {
		switch v := val.(type) {
		case map[string]any:
			out[i] = cloneMap(v)
		case []any:
			out[i] = cloneSlice(v)
		}
	}
	return out
}

func getNestedField(root map[string]any, fieldPath string) (any, bool) {
	current := any(root)
	for part := range strings.SplitSeq(fieldPath, ".") {
		obj, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		next, ok := obj[part]
		if !ok {
			return nil, false
		}
		current = next
	}
	return current, true
}

func deleteNestedField(root map[string]any, fieldPath string) {
	parts := strings.Split(fieldPath, ".")
	if len(parts) == 0 {
		return
	}

	current := root
	for i := range len(parts) - 1 {
		next, ok := current[parts[i]].(map[string]any)
		if !ok {
			return
		}
		current = next
	}
	delete(current, parts[len(parts)-1])
}

func encodePathValue(val any, multi bool) (string, error) {
	raw, err := scalarToString(val)
	if err != nil {
		return "", err
	}

	if !multi {
		return url.PathEscape(raw), nil
	}

	chunks := strings.Split(raw, "/")
	for i, chunk := range chunks {
		chunks[i] = url.PathEscape(chunk)
	}
	return strings.Join(chunks, "/"), nil
}

func encodeQueryFields(prefix string, val any, query url.Values) error {
	switch v := val.(type) {
	case nil:
		return nil
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			childKey := key
			if prefix != "" {
				childKey = prefix + "." + key
			}
			if err := encodeQueryFields(childKey, v[key], query); err != nil {
				return err
			}
		}
		return nil
	case []any:
		for _, item := range v {
			raw, err := scalarToString(item)
			if err != nil {
				return status.Errorf(codes.InvalidArgument, "invalid query value for %q: %v", prefix, err)
			}
			query.Add(prefix, raw)
		}
		return nil
	default:
		raw, err := scalarToString(v)
		if err != nil {
			return status.Errorf(codes.InvalidArgument, "invalid query value for %q: %v", prefix, err)
		}
		query.Add(prefix, raw)
		return nil
	}
}

func scalarToString(val any) (string, error) {
	switch v := val.(type) {
	case string:
		return v, nil
	case bool:
		return strconv.FormatBool(v), nil
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), nil
	case json.Number:
		return v.String(), nil
	case int:
		return strconv.Itoa(v), nil
	case int32:
		return strconv.FormatInt(int64(v), 10), nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	case uint32:
		return strconv.FormatUint(uint64(v), 10), nil
	case uint64:
		return strconv.FormatUint(v, 10), nil
	default:
		return "", fmt.Errorf("expected scalar value, got %T", val)
	}
}

func httpClientError(statusCode int, body []byte) error {
	code := grpcCodeFromHTTPStatus(statusCode)
	msg := fmt.Sprintf("HTTP %d", statusCode)

	var payload struct {
		Error struct {
			Code    string           `json:"code"`
			Message string           `json:"message"`
			Details []map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err == nil {
		if payload.Error.Message != "" {
			msg = payload.Error.Message
		}
		if payload.Error.Code != "" {
			code = grpcCodeFromName(payload.Error.Code)
		}
	}

	return status.Error(code, msg)
}

func grpcCodeFromHTTPStatus(statusCode int) codes.Code {
	switch statusCode {
	case http.StatusOK:
		return codes.OK
	case 499:
		return codes.Canceled
	case http.StatusBadRequest:
		return codes.InvalidArgument
	case http.StatusGatewayTimeout:
		return codes.DeadlineExceeded
	case http.StatusNotFound:
		return codes.NotFound
	case http.StatusConflict:
		return codes.AlreadyExists
	case http.StatusForbidden:
		return codes.PermissionDenied
	case http.StatusTooManyRequests:
		return codes.ResourceExhausted
	case http.StatusNotImplemented:
		return codes.Unimplemented
	case http.StatusInternalServerError:
		return codes.Internal
	case http.StatusServiceUnavailable:
		return codes.Unavailable
	case http.StatusUnauthorized:
		return codes.Unauthenticated
	default:
		return codes.Unknown
	}
}

func grpcCodeFromName(name string) codes.Code {
	for i := codes.OK; i <= codes.Unauthenticated; i++ {
		if grpcCodeName(i) == name {
			return i
		}
	}
	return codes.Unknown
}

const outboundHTTPHeaderEnvPrefix = "INVARIANT_HTTP_HEADER_"

func outboundHTTPHeadersFromEnv() map[string]string {
	out := make(map[string]string)
	for _, pair := range os.Environ() {
		key, value, ok := strings.Cut(pair, "=")
		if !ok || value == "" || !strings.HasPrefix(key, outboundHTTPHeaderEnvPrefix) {
			continue
		}

		suffix := strings.TrimPrefix(key, outboundHTTPHeaderEnvPrefix)
		if suffix == "" {
			continue
		}

		name := envHeaderSuffixToHTTPHeader(suffix)
		if name == "Accept" || name == "Content-Type" {
			continue
		}
		out[name] = value
	}
	return out
}

func envHeaderSuffixToHTTPHeader(suffix string) string {
	parts := strings.Split(strings.ToLower(suffix), "_")
	for i, part := range parts {
		if part == "" {
			continue
		}
		parts[i] = strings.ToUpper(part[:1]) + part[1:]
	}
	return strings.Join(parts, "-")
}
