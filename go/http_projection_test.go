package invariant

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	greetpb "github.com/jim-technologies/invariantprotocol/go/tests/gen"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type httpProjectionTestServicer struct {
	greetpb.UnimplementedGreetServiceServer
	greet  func(context.Context, *greetpb.GreetRequest) (*greetpb.GreetResponse, error)
	stream func(*greetpb.StreamGreetRequest, grpc.ServerStreamingServer[greetpb.GreetResponse]) error
}

func (s *httpProjectionTestServicer) Greet(ctx context.Context, req *greetpb.GreetRequest) (*greetpb.GreetResponse, error) {
	if s.greet != nil {
		return s.greet(ctx, req)
	}
	return &greetpb.GreetResponse{Message: "hello " + req.GetName()}, nil
}

func (s *httpProjectionTestServicer) StreamGreet(req *greetpb.StreamGreetRequest, stream grpc.ServerStreamingServer[greetpb.GreetResponse]) error {
	if s.stream != nil {
		return s.stream(req, stream)
	}
	return nil
}

func newHTTPProjectionHandler(
	t *testing.T,
	servicer greetpb.GreetServiceServer,
	configure func(*Server),
) http.Handler {
	t.Helper()
	srv, err := ServerFromDescriptor(descriptorPath())
	require.NoError(t, err)
	if configure != nil {
		configure(srv)
	}
	greetpb.RegisterGreetServiceServer(srv, servicer)
	handler := srv.HTTPHandler()
	return handler
}

func TestHTTPProjectionUnaryResponseLimitJSONAndProto(t *testing.T) {
	largeResponse := strings.Repeat("x", 512)

	protoRequest, err := proto.Marshal(&greetpb.GreetRequest{Name: "proto"})
	require.NoError(t, err)

	tests := []struct {
		name        string
		contentType string
		accept      string
		body        []byte
	}{
		{
			name:        "json",
			contentType: "application/json",
			body:        []byte(`{"name":"json"}`),
		},
		{
			name:        "proto",
			contentType: protoContentType,
			accept:      protoContentType,
			body:        protoRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := newHTTPProjectionHandler(t, &httpProjectionTestServicer{
				greet: func(context.Context, *greetpb.GreetRequest) (*greetpb.GreetResponse, error) {
					return &greetpb.GreetResponse{Message: largeResponse}, nil
				},
			}, func(srv *Server) {
				srv.SetMaxUnaryResponseBytes(128)
			})

			req := httptest.NewRequest(http.MethodPost, "/greet.v1.GreetService/Greet", bytes.NewReader(tt.body))
			req.Header.Set("Content-Type", tt.contentType)
			if tt.accept != "" {
				req.Header.Set("Accept", tt.accept)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)

			assert.Equal(t, http.StatusTooManyRequests, response.Code)
			assert.Equal(t, "application/json", response.Header().Get("Content-Type"))
			var payload map[string]any
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &payload))
			assert.Equal(t, "resource_exhausted", payload["code"])
			assert.Contains(t, payload["message"], "128")
		})
	}
}

func TestHTTPProjectionPerMethodUnaryResponseOverride(t *testing.T) {
	largeResponse := strings.Repeat("x", 512)
	handler := newHTTPProjectionHandler(t, &httpProjectionTestServicer{
		greet: func(context.Context, *greetpb.GreetRequest) (*greetpb.GreetResponse, error) {
			return &greetpb.GreetResponse{Message: largeResponse}, nil
		},
	}, func(srv *Server) {
		srv.SetMaxUnaryResponseBytes(64)
		srv.ConfigureMethod(greetpb.GreetService_Greet_FullMethodName, MethodConfig{
			MaxUnaryResponseBytes: 1024,
		})
	})

	req := httptest.NewRequest(http.MethodPost, greetpb.GreetService_Greet_FullMethodName, strings.NewReader(`{"name":"override"}`))
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)

	require.Equal(t, http.StatusOK, response.Code)
	require.Greater(t, response.Body.Len(), 64, "response must exceed the server-wide cap for the override to matter")
	var decoded greetpb.GreetResponse
	require.NoError(t, protojson.Unmarshal(response.Body.Bytes(), &decoded))
	assert.Equal(t, largeResponse, decoded.GetMessage())
}

func TestHTTPProjectionStreamResponseLimitIsPerMessage(t *testing.T) {
	first := &greetpb.GreetResponse{Message: strings.Repeat("a", 128)}
	second := &greetpb.GreetResponse{Message: strings.Repeat("b", 128)}
	marshal := protojson.MarshalOptions{UseProtoNames: true}
	firstBytes, err := marshal.Marshal(first)
	require.NoError(t, err)
	secondBytes, err := marshal.Marshal(second)
	require.NoError(t, err)
	capBytes := max(int64(len(firstBytes)), int64(len(secondBytes)))
	require.Greater(t, int64(len(firstBytes)+len(secondBytes)), capBytes)

	handler := newHTTPProjectionHandler(t, &httpProjectionTestServicer{
		stream: func(_ *greetpb.StreamGreetRequest, stream grpc.ServerStreamingServer[greetpb.GreetResponse]) error {
			if err := stream.Send(first); err != nil {
				return err
			}
			return stream.Send(second)
		},
	}, func(srv *Server) {
		srv.SetMaxStreamResponseBytes(capBytes)
	})

	response := serveHTTPProjectionStream(t, handler)
	require.Equal(t, http.StatusOK, response.Code)
	frames := readAllEnvelopes(t, bytes.NewReader(response.Body.Bytes()))
	require.Len(t, frames, 3, "two messages plus one end-stream envelope")
	assert.Equal(t, byte(0), frames[0].flags)
	assert.Equal(t, byte(0), frames[1].flags)
	assert.Equal(t, connectEndStreamFlag, frames[2].flags)

	var gotFirst, gotSecond greetpb.GreetResponse
	require.NoError(t, protojson.Unmarshal(frames[0].payload, &gotFirst))
	require.NoError(t, protojson.Unmarshal(frames[1].payload, &gotSecond))
	assert.Equal(t, first.GetMessage(), gotFirst.GetMessage())
	assert.Equal(t, second.GetMessage(), gotSecond.GetMessage())
	var end map[string]any
	require.NoError(t, json.Unmarshal(frames[2].payload, &end))
	assert.NotContains(t, end, "error")
}

func TestHTTPProjectionStreamRejectsOversizedIndividualMessage(t *testing.T) {
	small := &greetpb.GreetResponse{Message: "small"}
	large := &greetpb.GreetResponse{Message: strings.Repeat("z", 1024)}
	var sendCode codes.Code
	thirdMessageReached := false

	handler := newHTTPProjectionHandler(t, &httpProjectionTestServicer{
		stream: func(_ *greetpb.StreamGreetRequest, stream grpc.ServerStreamingServer[greetpb.GreetResponse]) error {
			if err := stream.Send(small); err != nil {
				return err
			}
			if err := stream.Send(large); err != nil {
				sendCode = status.Code(err)
				return err
			}
			thirdMessageReached = true
			return stream.Send(&greetpb.GreetResponse{Message: "unexpected"})
		},
	}, func(srv *Server) {
		srv.SetMaxStreamResponseBytes(256)
	})

	response := serveHTTPProjectionStream(t, handler)
	require.Equal(t, http.StatusOK, response.Code)
	assert.Equal(t, codes.ResourceExhausted, sendCode)
	assert.False(t, thirdMessageReached)

	frames := readAllEnvelopes(t, bytes.NewReader(response.Body.Bytes()))
	require.Len(t, frames, 2, "the oversized message must be omitted and followed by end-stream")
	var gotSmall greetpb.GreetResponse
	require.NoError(t, protojson.Unmarshal(frames[0].payload, &gotSmall))
	assert.Equal(t, small.GetMessage(), gotSmall.GetMessage())
	assert.Equal(t, connectEndStreamFlag, frames[1].flags)
	var end struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(frames[1].payload, &end))
	assert.Equal(t, "resource_exhausted", end.Error.Code)
}

func TestHTTPProjectionPerMethodStreamLimitOverrides(t *testing.T) {
	name := strings.Repeat("request", 32)
	message := strings.Repeat("response", 32)
	handler := newHTTPProjectionHandler(t, &httpProjectionTestServicer{
		stream: func(req *greetpb.StreamGreetRequest, stream grpc.ServerStreamingServer[greetpb.GreetResponse]) error {
			assert.Equal(t, name, req.GetName())
			return stream.Send(&greetpb.GreetResponse{Message: message})
		},
	}, func(srv *Server) {
		srv.SetMaxStreamRequestBytes(16)
		srv.SetMaxStreamResponseBytes(16)
		srv.ConfigureMethod(greetpb.GreetService_StreamGreet_FullMethodName, MethodConfig{
			MaxStreamRequestBytes:  1024,
			MaxStreamResponseBytes: 1024,
		})
	})

	payload, err := protojson.Marshal(&greetpb.StreamGreetRequest{Name: name, Count: 1})
	require.NoError(t, err)
	require.Greater(t, len(payload), 16)
	request := httptest.NewRequest(http.MethodPost, greetpb.GreetService_StreamGreet_FullMethodName,
		bytes.NewReader(connectRequestEnvelope(t, 0, payload)))
	request.Header.Set("Content-Type", connectStreamJSONType)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code)
	frames := readAllEnvelopes(t, bytes.NewReader(response.Body.Bytes()))
	require.Len(t, frames, 2)
	assert.Equal(t, byte(0), frames[0].flags)
	assert.Greater(t, len(frames[0].payload), 16)
	var got greetpb.GreetResponse
	require.NoError(t, protojson.Unmarshal(frames[0].payload, &got))
	assert.Equal(t, message, got.GetMessage())
	assert.Equal(t, connectEndStreamFlag, frames[1].flags)
}

func TestHTTPProjectionStreamingSendHeaderFlushesImmediately(t *testing.T) {
	headerSent := make(chan struct{})
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	handler := newHTTPProjectionHandler(t, &httpProjectionTestServicer{
		stream: func(_ *greetpb.StreamGreetRequest, stream grpc.ServerStreamingServer[greetpb.GreetResponse]) error {
			if err := stream.SendHeader(metadata.Pairs("x-early-header", "ready")); err != nil {
				return err
			}
			close(headerSent)
			<-release
			return nil
		},
	}, nil)
	server := httptest.NewServer(handler)
	defer server.Close()

	body := connectRequestEnvelope(t, 0, []byte(`{"name":"headers"}`))
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		server.URL+greetpb.GreetService_StreamGreet_FullMethodName, bytes.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Content-Type", connectStreamJSONType)
	responseCh := make(chan *http.Response, 1)
	errCh := make(chan error, 1)
	go func() {
		response, err := http.DefaultClient.Do(request) //nolint:bodyclose // the receiver consumes and closes the streaming body
		if err != nil {
			errCh <- err
			return
		}
		responseCh <- response
	}()

	select {
	case <-headerSent:
	case <-time.After(2 * time.Second):
		t.Fatal("stream handler did not call SendHeader")
	}
	var response *http.Response
	select {
	case response = <-responseCh:
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP headers were not flushed by SendHeader")
	}
	require.Equal(t, http.StatusOK, response.StatusCode)
	assert.Equal(t, "ready", response.Header.Get("X-Early-Header"))

	close(release)
	released = true
	_, err = io.Copy(io.Discard, response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
}

func TestHTTPProjectionBoundsErrorControlPayloads(t *testing.T) {
	t.Run("unary", func(t *testing.T) {
		handler := newHTTPProjectionHandler(t, &httpProjectionTestServicer{
			greet: func(context.Context, *greetpb.GreetRequest) (*greetpb.GreetResponse, error) {
				return nil, status.Error(codes.Internal, strings.Repeat("x", 4096))
			},
		}, func(server *Server) { server.SetMaxUnaryResponseBytes(128) })
		request := httptest.NewRequest(http.MethodPost, greetpb.GreetService_Greet_FullMethodName,
			strings.NewReader(`{"name":"error"}`))
		request.Header.Set("Content-Type", jsonContentType)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)

		assert.LessOrEqual(t, response.Body.Len(), 128)
		assert.Equal(t, http.StatusTooManyRequests, response.Code)
		var envelope map[string]any
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &envelope))
		assert.Equal(t, "resource_exhausted", envelope["code"])
	})

	t.Run("stream control envelope is independent and bounded", func(t *testing.T) {
		handler := newHTTPProjectionHandler(t, &httpProjectionTestServicer{
			stream: func(*greetpb.StreamGreetRequest, grpc.ServerStreamingServer[greetpb.GreetResponse]) error {
				return status.Error(codes.Internal, strings.Repeat("x", maxConnectControlEnvelope+1024))
			},
		}, func(server *Server) { server.SetMaxStreamResponseBytes(1) })
		response := serveHTTPProjectionStream(t, handler)
		frames := readAllEnvelopes(t, bytes.NewReader(response.Body.Bytes()))
		require.Len(t, frames, 1)
		assert.Equal(t, connectEndStreamFlag, frames[0].flags)
		assert.LessOrEqual(t, len(frames[0].payload), maxConnectControlEnvelope)
		var end struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(frames[0].payload, &end))
		assert.Equal(t, "resource_exhausted", end.Error.Code)
	})
}

func TestHTTPProjectionInboundMetadataIsFiltered(t *testing.T) {
	captured := make(chan metadata.MD, 1)
	handler := newHTTPProjectionHandler(t, &httpProjectionTestServicer{
		greet: func(ctx context.Context, _ *greetpb.GreetRequest) (*greetpb.GreetResponse, error) {
			md, _ := metadata.FromIncomingContext(ctx)
			captured <- md.Copy()
			return &greetpb.GreetResponse{Message: "ok"}, nil
		},
	}, func(srv *Server) {
		// Deliberately attempt to map both the allowed correlation header and
		// caller-controlled identity headers. Invariant must filter the latter.
		srv.UseHTTPMetadataMapper(func(r *http.Request) metadata.MD {
			return metadata.MD{
				"x-request-id":                 r.Header.Values("X-Request-Id"),
				"x-empty":                      {""},
				"trace-bin":                    {"AP8", "YWJjZA=="},
				"authorization":                r.Header.Values("Authorization"),
				"authorization-bin":            {"Y2FsbGVyLWNvbnRyb2xsZWQ"},
				"proxy-authorization-bin":      {"Y2FsbGVyLWNvbnRyb2xsZWQ"},
				"authentication-bin":           {"Y2FsbGVyLWNvbnRyb2xsZWQ"},
				"api-key-bin":                  {"Y2FsbGVyLWNvbnRyb2xsZWQ"},
				"x-api-key-bin":                {"Y2FsbGVyLWNvbnRyb2xsZWQ"},
				"x-tenant":                     r.Header.Values("X-Tenant"),
				"x-principal":                  r.Header.Values("X-Principal"),
				"x-role":                       r.Header.Values("X-Role"),
				"invariant-internal-principal": {"caller-controlled"},
			}
		})
	})

	req := httptest.NewRequest(http.MethodPost, greetpb.GreetService_Greet_FullMethodName, strings.NewReader(`{"name":"metadata"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "request-123")
	req.Header.Set("Authorization", "Bearer caller-controlled")
	req.Header.Set("X-Tenant", "tenant-caller-controlled")
	req.Header.Set("X-Principal", "principal-caller-controlled")
	req.Header.Set("X-Role", "admin")
	// Authentication middleware may place validated identity in the request
	// context. That trusted metadata is preserved independently of the
	// caller-controlled header mapper.
	req = req.WithContext(metadata.NewIncomingContext(req.Context(), metadata.Pairs(
		"x-tenant", "trusted-tenant",
		"invariant-internal-principal", "trusted-principal",
	)))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	require.Equal(t, http.StatusOK, response.Code)

	md := <-captured
	assert.Equal(t, []string{"request-123"}, md.Get("x-request-id"))
	assert.Equal(t, []string{""}, md.Get("x-empty"))
	assert.Equal(t, []string{string([]byte{0, 0xff}), "abcd"}, md.Get("trace-bin"))
	assert.Equal(t, []string{"trusted-tenant"}, md.Get("x-tenant"))
	assert.Equal(t, []string{"trusted-principal"}, md.Get("invariant-internal-principal"))
	for _, key := range []string{
		"authorization",
		"authorization-bin",
		"proxy-authorization-bin",
		"authentication-bin",
		"api-key-bin",
		"x-api-key-bin",
		"x-principal",
		"x-role",
	} {
		assert.Empty(t, md.Get(key), "%s must not be caller-assertable", key)
	}
}

func TestHTTPProjectionMapsUnaryGRPCHeadersAndTrailers(t *testing.T) {
	handler := newHTTPProjectionHandler(t, &httpProjectionTestServicer{
		greet: func(ctx context.Context, _ *greetpb.GreetRequest) (*greetpb.GreetResponse, error) {
			if err := grpc.SetHeader(ctx, metadata.Pairs(
				"x-projection-header", "leading",
				"x-tenant", "trusted-response-tenant",
				"payload-bin", string([]byte{0, 0xff}),
				"content-type", "text/plain",
				"connect-corrupt", "blocked",
				"invariant-internal-secret", "blocked",
			)); err != nil {
				return nil, err
			}
			if err := grpc.SetTrailer(ctx, metadata.Pairs("x-projection-trailer", "trailing")); err != nil {
				return nil, err
			}
			return &greetpb.GreetResponse{Message: "metadata"}, nil
		},
	}, nil)

	req := httptest.NewRequest(http.MethodPost, greetpb.GreetService_Greet_FullMethodName, strings.NewReader(`{"name":"metadata"}`))
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)

	require.Equal(t, http.StatusOK, response.Code)
	assert.Equal(t, "leading", response.Header().Get("X-Projection-Header"))
	assert.Equal(t, "trusted-response-tenant", response.Header().Get("X-Tenant"))
	assert.Equal(t, "AP8", response.Header().Get("Payload-Bin"))
	assert.Equal(t, "application/json", response.Header().Get("Content-Type"))
	assert.Empty(t, response.Header().Get("Connect-Corrupt"))
	assert.Empty(t, response.Header().Get("Invariant-Internal-Secret"))
	assert.Equal(t, "trailing", response.Header().Get("Trailer-X-Projection-Trailer"))
}

func TestHTTPProjectionRejectsLateMetadataMapperChange(t *testing.T) {
	server := nativeTestServer(t)
	greetpb.RegisterGreetServiceServer(server, &httpProjectionTestServicer{})
	server.HTTPHandler()

	assert.PanicsWithValue(t,
		"invariant: HTTP metadata mapper cannot be changed after serving begins",
		func() { server.UseHTTPMetadataMapper(DefaultHTTPMetadataMapper) },
	)
}

func TestHTTPProjectionRichStatusDetailsRoundTrip(t *testing.T) {
	detail := &errdetails.BadRequest{FieldViolations: []*errdetails.BadRequest_FieldViolation{{
		Field: "name", Description: "reserved",
	}}}
	richStatus, err := status.New(codes.FailedPrecondition, "cannot greet").WithDetails(detail)
	require.NoError(t, err)

	handler := newHTTPProjectionHandler(t, &httpProjectionTestServicer{
		greet: func(context.Context, *greetpb.GreetRequest) (*greetpb.GreetResponse, error) {
			return nil, richStatus.Err()
		},
	}, nil)

	req := httptest.NewRequest(http.MethodPost, greetpb.GreetService_Greet_FullMethodName, strings.NewReader(`{"name":"status"}`))
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)

	require.Equal(t, http.StatusBadRequest, response.Code)
	var envelope struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Details []struct {
			Type  string `json:"type"`
			Value string `json:"value"`
		} `json:"details"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &envelope))
	assert.Equal(t, "failed_precondition", envelope.Code)
	assert.Equal(t, "cannot greet", envelope.Message)
	require.Len(t, envelope.Details, 1)
	assert.Equal(t, "google.rpc.BadRequest", envelope.Details[0].Type)
	assert.NotEmpty(t, envelope.Details[0].Value)

	roundTrip := httpClientError(response.Code, response.Body.Bytes())
	gotStatus, ok := status.FromError(roundTrip)
	require.True(t, ok)
	assert.Equal(t, codes.FailedPrecondition, gotStatus.Code())
	assert.Equal(t, "cannot greet", gotStatus.Message())
	require.Len(t, gotStatus.Details(), 1)
	gotDetail, ok := gotStatus.Details()[0].(*errdetails.BadRequest)
	require.True(t, ok)
	require.Len(t, gotDetail.GetFieldViolations(), 1)
	assert.Equal(t, "name", gotDetail.GetFieldViolations()[0].GetField())
	assert.Equal(t, "reserved", gotDetail.GetFieldViolations()[0].GetDescription())
}

func serveHTTPProjectionStream(t *testing.T, handler http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	require.NoError(t, writeConnectEnvelope(&body, 0, []byte(`{"name":"stream","count":2}`)))
	req := httptest.NewRequest(http.MethodPost, greetpb.GreetService_StreamGreet_FullMethodName, &body)
	req.Header.Set("Content-Type", connectStreamJSONType)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	return response
}
