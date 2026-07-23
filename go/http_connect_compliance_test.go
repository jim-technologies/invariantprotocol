package invariant

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	greetpb "github.com/jim-technologies/invariantprotocol/go/tests/gen"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestHTTPProjectionUnaryCodecMatchesRequest(t *testing.T) {
	handler := newHTTPProjectionHandler(t, &httpProjectionTestServicer{}, nil)

	t.Run("JSON request ignores proto Accept", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, greetpb.GreetService_Greet_FullMethodName,
			bytes.NewReader([]byte(`{"name":"json"}`)))
		req.Header.Set("Content-Type", jsonContentType)
		req.Header.Set("Accept", protoContentType)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)

		require.Equal(t, http.StatusOK, response.Code)
		if got := response.Header().Get("Content-Type"); got != jsonContentType {
			t.Errorf("Content-Type = %q, want %q", got, jsonContentType)
		}
		var decoded greetpb.GreetResponse
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &decoded))
		assert.Equal(t, "hello json", decoded.GetMessage())
	})

	t.Run("proto request ignores JSON Accept", func(t *testing.T) {
		body, err := proto.Marshal(&greetpb.GreetRequest{Name: "proto"})
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost, greetpb.GreetService_Greet_FullMethodName, bytes.NewReader(body))
		req.Header.Set("Content-Type", protoContentType)
		req.Header.Set("Accept", jsonContentType)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)

		require.Equal(t, http.StatusOK, response.Code)
		assert.Equal(t, protoContentType, response.Header().Get("Content-Type"))
		var decoded greetpb.GreetResponse
		require.NoError(t, proto.Unmarshal(response.Body.Bytes(), &decoded))
		assert.Equal(t, "hello proto", decoded.GetMessage())
	})

	for _, contentType := range []string{"", "text/plain", connectStreamJSONType} {
		t.Run("rejects "+contentType, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, greetpb.GreetService_Greet_FullMethodName,
				bytes.NewReader([]byte(`{"name":"unsupported"}`)))
			if contentType != "" {
				req.Header.Set("Content-Type", contentType)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)

			require.Equal(t, http.StatusUnsupportedMediaType, response.Code)
			if got := response.Header().Get("Content-Type"); got != jsonContentType {
				t.Errorf("Content-Type = %q, want %q", got, jsonContentType)
			}
			var envelope map[string]any
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &envelope))
			assert.Equal(t, "invalid_argument", envelope["code"])
		})
	}
}

func TestHTTPProjectionStreamingRequestProtocolErrors(t *testing.T) {
	handler := newHTTPProjectionHandler(t, &httpProjectionTestServicer{}, nil)

	validJSON := connectRequestEnvelope(t, 0, []byte(`{"name":"valid"}`))
	twoEnvelopes := append(append([]byte(nil), validJSON...), validJSON...)
	oversizedHeader := connectRequestHeader(uint32(connectStreamMaxRequest + 1))

	tests := []struct {
		name        string
		contentType string
		body        []byte
		wantCode    string
	}{
		{name: "truncated header", contentType: connectStreamJSONType, body: []byte{0}, wantCode: "invalid_argument"},
		{name: "truncated message", contentType: connectStreamJSONType, body: append(connectRequestHeader(2), byte('{')), wantCode: "invalid_argument"},
		{name: "oversized message", contentType: connectStreamJSONType, body: oversizedHeader, wantCode: "resource_exhausted"},
		{name: "compressed flag", contentType: connectStreamJSONType, body: connectRequestEnvelope(t, 0x01, nil), wantCode: "unimplemented"},
		{name: "end-stream flag", contentType: connectStreamJSONType, body: connectRequestEnvelope(t, connectEndStreamFlag, nil), wantCode: "invalid_argument"},
		{name: "reserved flag", contentType: connectStreamJSONType, body: connectRequestEnvelope(t, 0x04, nil), wantCode: "invalid_argument"},
		{name: "second envelope", contentType: connectStreamJSONType, body: twoEnvelopes, wantCode: "invalid_argument"},
		{name: "invalid JSON", contentType: connectStreamJSONType, body: connectRequestEnvelope(t, 0, []byte{'{'}), wantCode: "invalid_argument"},
		{name: "invalid proto", contentType: connectStreamProtoType, body: connectRequestEnvelope(t, 0, []byte{0x0a, 0x02, 0x01}), wantCode: "invalid_argument"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, greetpb.GreetService_StreamGreet_FullMethodName, bytes.NewReader(tt.body))
			req.Header.Set("Content-Type", tt.contentType)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)

			require.Equal(t, http.StatusOK, response.Code)
			assert.Equal(t, tt.contentType, response.Header().Get("Content-Type"))
			frames := readAllEnvelopes(t, bytes.NewReader(response.Body.Bytes()))
			require.Len(t, frames, 1, "protocol errors must produce only an end-stream envelope")
			assert.Equal(t, connectEndStreamFlag, frames[0].flags)
			assert.Equal(t, 5+len(frames[0].payload), response.Body.Len(), "response must not contain bytes after end-stream")
			var end struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(frames[0].payload, &end))
			assert.Equal(t, tt.wantCode, end.Error.Code)
		})
	}
}

func TestHTTPProjectionContextErrorOverridesNominalSuccess(t *testing.T) {
	t.Run("unary cancellation", func(t *testing.T) {
		started := make(chan struct{})
		handler := newHTTPProjectionHandler(t, &httpProjectionTestServicer{
			greet: func(ctx context.Context, _ *greetpb.GreetRequest) (*greetpb.GreetResponse, error) {
				close(started)
				<-ctx.Done()
				return &greetpb.GreetResponse{Message: "late success"}, nil
			},
		}, nil)

		ctx, cancel := context.WithCancel(t.Context())
		req := httptest.NewRequest(http.MethodPost, greetpb.GreetService_Greet_FullMethodName,
			bytes.NewReader([]byte(`{"name":"cancel"}`))).WithContext(ctx)
		req.Header.Set("Content-Type", jsonContentType)
		response := httptest.NewRecorder()
		done := make(chan struct{})
		go func() {
			handler.ServeHTTP(response, req)
			close(done)
		}()
		<-started
		cancel()
		<-done

		require.Equal(t, 499, response.Code)
		var envelope map[string]any
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &envelope))
		assert.Equal(t, grpcCodeName(codes.Canceled), envelope["code"])
	})

	t.Run("stream deadline", func(t *testing.T) {
		handler := newHTTPProjectionHandler(t, &httpProjectionTestServicer{
			stream: func(_ *greetpb.StreamGreetRequest, stream grpc.ServerStreamingServer[greetpb.GreetResponse]) error {
				<-stream.Context().Done()
				return nil
			},
		}, nil)
		body := connectRequestEnvelope(t, 0, []byte(`{"name":"deadline"}`))
		req := httptest.NewRequest(http.MethodPost, greetpb.GreetService_StreamGreet_FullMethodName, bytes.NewReader(body))
		req.Header.Set("Content-Type", connectStreamJSONType)
		req.Header.Set("Connect-Timeout-Ms", "5")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)

		require.Equal(t, http.StatusOK, response.Code)
		frames := readAllEnvelopes(t, bytes.NewReader(response.Body.Bytes()))
		require.Len(t, frames, 1)
		var end struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(frames[0].payload, &end))
		assert.Equal(t, "deadline_exceeded", end.Error.Code)
	})
}

func TestConnectEnvelopePayloadLengthGuard(t *testing.T) {
	size, err := connectEnvelopeSize(1<<32 - 1)
	require.NoError(t, err)
	assert.Equal(t, ^uint32(0), size)

	_, err = connectEnvelopeSize(1 << 32)
	require.Error(t, err)
	assert.Equal(t, codes.ResourceExhausted, status.Code(err))
}

func TestHTTPClientErrorRejectsOKAndPreservesEmptyDetails(t *testing.T) {
	t.Run("malformed body uses Connect HTTP mapping", func(t *testing.T) {
		tests := map[int]codes.Code{
			http.StatusBadRequest:          codes.Internal,
			http.StatusUnauthorized:        codes.Unauthenticated,
			http.StatusForbidden:           codes.PermissionDenied,
			http.StatusNotFound:            codes.Unimplemented,
			http.StatusTooManyRequests:     codes.Unavailable,
			http.StatusBadGateway:          codes.Unavailable,
			http.StatusServiceUnavailable:  codes.Unavailable,
			http.StatusGatewayTimeout:      codes.Unavailable,
			http.StatusInternalServerError: codes.Unknown,
		}
		for httpStatus, want := range tests {
			err := httpClientError(httpStatus, []byte("not JSON"))
			assert.Equal(t, want, status.Code(err), "HTTP %d", httpStatus)
		}
	})

	t.Run("OK code on HTTP error", func(t *testing.T) {
		err := httpClientError(http.StatusInternalServerError, []byte(`{"code":"ok","message":"not actually OK"}`))
		require.Error(t, err)
		assert.Equal(t, codes.Unknown, status.Code(err))
		assert.Equal(t, "HTTP 500", status.Convert(err).Message())
	})

	t.Run("noncanonical envelope uses HTTP fallback", func(t *testing.T) {
		err := httpClientError(http.StatusBadRequest, []byte(`{"error":{"code":"INVALID_ARGUMENT","message":"old shape"}}`))
		require.Error(t, err)
		assert.Equal(t, codes.Internal, status.Code(err))
		assert.Equal(t, "HTTP 400", status.Convert(err).Message())
	})

	t.Run("zero-byte rich detail", func(t *testing.T) {
		err := httpClientError(http.StatusBadRequest, []byte(
			`{"code":"failed_precondition","message":"empty detail","details":[{"type":"google.protobuf.Empty","value":""}]}`,
		))
		require.Error(t, err)
		st, ok := status.FromError(err)
		require.True(t, ok)
		assert.Equal(t, codes.FailedPrecondition, st.Code())
		require.Len(t, st.Proto().GetDetails(), 1)
		assert.Equal(t, "type.googleapis.com/google.protobuf.Empty", st.Proto().GetDetails()[0].GetTypeUrl())
		assert.Empty(t, st.Proto().GetDetails()[0].GetValue())
	})
}

func TestConnectHTTPContextErrorsAreNotRetried(t *testing.T) {
	for _, tt := range []struct {
		name     string
		newCtx   func() (context.Context, context.CancelFunc)
		wantCode codes.Code
	}{
		{
			name: "canceled",
			newCtx: func() (context.Context, context.CancelFunc) {
				return context.WithCancel(t.Context())
			},
			wantCode: codes.Canceled,
		},
		{
			name: "deadline exceeded",
			newCtx: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(t.Context(), 20*time.Millisecond)
			},
			wantCode: codes.DeadlineExceeded,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var attempts atomic.Int32
			started := make(chan struct{})
			backend := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
				if attempts.Add(1) == 1 {
					close(started)
				}
				<-req.Context().Done()
			}))
			defer backend.Close()
			srv, err := ServerFromDescriptor(descriptorPath())
			require.NoError(t, err)
			require.NoError(t, srv.ConnectHTTP(backend.URL))

			ctx, cancel := tt.newCtx()
			defer cancel()
			errCh := make(chan error, 1)
			go func() {
				_, callErr := srv.Invoke(ctx, "greet.v1.GreetService.Greet", &greetpb.GreetRequest{Name: "context"})
				errCh <- callErr
			}()
			<-started
			if tt.wantCode == codes.Canceled {
				cancel()
			}
			callErr := <-errCh
			require.Error(t, callErr)
			assert.Equal(t, tt.wantCode, status.Code(callErr))
			assert.Equal(t, int32(1), attempts.Load())
		})
	}
}

func connectRequestEnvelope(t *testing.T, flags byte, payload []byte) []byte {
	t.Helper()
	var body bytes.Buffer
	require.NoError(t, writeConnectEnvelope(&body, flags, payload))
	return body.Bytes()
}

func connectRequestHeader(size uint32) []byte {
	return []byte{0, byte(size >> 24), byte(size >> 16), byte(size >> 8), byte(size)}
}
