package invariant

import (
	"context"
	"errors"
	"slices"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// callbackStream is the in-memory ServerStream used by every projection.
// Each Send call hands the message off to sendFn synchronously — the
// projection decides what to do with it (write a gRPC frame, append to
// an MCP content array, emit a Connect envelope, print a CLI line).
//
// callbackStream stays single-goroutine: the handler is invoked from the
// same goroutine that owns sendFn's destination, so we don't need to
// serialize Sends ourselves.
type callbackStream struct {
	ctx    context.Context
	sendFn func(proto.Message) error
	mu     sync.Mutex
	closed bool
}

func newCallbackStream(ctx context.Context, sendFn func(proto.Message) error) *callbackStream {
	return &callbackStream{ctx: ctx, sendFn: sendFn}
}

func (s *callbackStream) Send(msg proto.Message) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return errors.New("send on closed stream")
	}
	return s.sendFn(msg)
}

func (s *callbackStream) Context() context.Context { return s.ctx }

func (s *callbackStream) close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

// invokeStream runs the streaming interceptor chain then calls the handler.
// First registered = outermost (matches Use/UseStream order).
//
// Returns the handler's error (after interceptor wrapping). Callers must
// already have produced a *callbackStream whose sendFn delivers messages
// to the underlying transport. Panics in the handler or any interceptor
// are recovered and converted to a codes.Internal status error.
func (s *Server) invokeStream(tool *Tool, req proto.Message, stream ServerStream) (err error) {
	if tool.streamHandler == nil {
		return errors.New("tool is not server-streaming")
	}

	defer func() {
		if r := recover(); r != nil {
			err = status.Errorf(codes.Internal, "panic in %s: %v", tool.callInfo.FullMethod, r)
		}
	}()

	if len(s.streamInterceptors) == 0 {
		return tool.streamHandler(req, stream)
	}

	current := tool.streamHandler
	for _, interceptor := range slices.Backward(s.streamInterceptors) {
		next := current
		current = func(req any, stream ServerStream) error {
			return interceptor(req, stream, tool.callInfo, next)
		}
	}
	return current(req, stream)
}
