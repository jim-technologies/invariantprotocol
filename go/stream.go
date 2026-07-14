package invariant

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
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

// projectedServerStream is the direct (no transport hop) grpc.ServerStream
// presented to captured generated stream handlers and standard stream
// interceptors.
type projectedServerStream struct {
	ctx          context.Context
	method       string
	request      proto.Message
	send         func(proto.Message) error
	mu           sync.Mutex
	received     bool
	header       metadata.MD
	trailer      metadata.MD
	headerSent   bool
	onSendHeader func() error
	sendErr      error
}

func newProjectedServerStream(ctx context.Context, method string, request proto.Message, send func(proto.Message) error) *projectedServerStream {
	stream := &projectedServerStream{
		method:  method,
		request: request,
		send:    send,
		header:  metadata.MD{},
		trailer: metadata.MD{},
	}
	stream.ctx = grpc.NewContextWithServerTransportStream(ctx, projectedTransportStream{stream: stream})
	return stream
}

type projectedTransportStream struct {
	stream *projectedServerStream
}

func (s projectedTransportStream) Method() string { return s.stream.Method() }
func (s projectedTransportStream) SetHeader(md metadata.MD) error {
	return s.stream.SetHeader(md)
}

func (s projectedTransportStream) SendHeader(md metadata.MD) error {
	return s.stream.SendHeader(md)
}

func (s projectedTransportStream) SetTrailer(md metadata.MD) error {
	s.stream.SetTrailer(md)
	return nil
}

func (s *projectedServerStream) Context() context.Context { return s.ctx }

func (s *projectedServerStream) Method() string { return s.method }

func (s *projectedServerStream) SetHeader(md metadata.MD) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.headerSent {
		return errors.New("grpc: cannot set header after SendHeader")
	}
	s.header = metadata.Join(s.header, md)
	return nil
}

func (s *projectedServerStream) SendHeader(md metadata.MD) error {
	s.mu.Lock()
	if s.headerSent {
		s.mu.Unlock()
		return errors.New("grpc: SendHeader called multiple times")
	}
	s.header = metadata.Join(s.header, md)
	s.headerSent = true
	onSendHeader := s.onSendHeader
	s.mu.Unlock()
	if onSendHeader != nil {
		return onSendHeader()
	}
	return nil
}

func (s *projectedServerStream) setHeaderSender(send func() error) {
	s.mu.Lock()
	s.onSendHeader = send
	s.mu.Unlock()
}

func (s *projectedServerStream) SetTrailer(md metadata.MD) {
	s.mu.Lock()
	s.trailer = metadata.Join(s.trailer, md)
	s.mu.Unlock()
}

func (s *projectedServerStream) RecvMsg(dst any) error {
	s.mu.Lock()
	if s.received {
		s.mu.Unlock()
		return io.EOF
	}
	s.received = true
	s.mu.Unlock()
	return copyProtoMessage(dst, s.request)
}

func (s *projectedServerStream) SendMsg(src any) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	msg, ok := src.(proto.Message)
	if !ok {
		return fmt.Errorf("stream response does not implement proto.Message: %T", src)
	}
	s.mu.Lock()
	if s.sendErr != nil {
		err := s.sendErr
		s.mu.Unlock()
		return err
	}
	s.headerSent = true
	s.mu.Unlock()
	err := s.send(msg)
	if err != nil {
		s.mu.Lock()
		if s.sendErr == nil {
			s.sendErr = err
		}
		s.mu.Unlock()
	}
	return err
}

func (s *projectedServerStream) metadata() (metadata.MD, metadata.MD) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.header.Copy(), s.trailer.Copy()
}

func (s *projectedServerStream) sendError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sendErr
}

// invokeStream runs the streaming interceptor chain then calls the handler.
// First registered = outermost (matches Use/UseStream order).
//
// Returns the handler's error (after interceptor wrapping). Callers must
// already have produced a *callbackStream whose sendFn delivers messages
// to the underlying transport. Panics in the handler or any interceptor
// are recovered and converted to a codes.Internal status error.
func (s *Server) invokeStream(tool *Tool, req proto.Message, stream ServerStream) (err error) {
	s.freeze()
	if tool.streamDesc == nil && tool.streamHandler == nil {
		return errors.New("tool is not server-streaming")
	}
	projected := newProjectedServerStream(stream.Context(), tool.streamInfo.FullMethod, req, stream.Send)
	return s.invokeGRPCStream(tool, projected)
}

func (s *Server) invokeGRPCStream(tool *Tool, stream grpc.ServerStream) (err error) {
	if tool.streamDesc == nil {
		return status.Error(codes.Internal, "streaming tool has no grpc.StreamDesc")
	}
	terminal := func(_ any, stream grpc.ServerStream) error {
		return tool.streamDesc.Handler(tool.serviceImpl, stream)
	}
	err = s.sharedStreamInterceptor(tool.serviceImpl, stream, tool.streamInfo, terminal)
	if err == nil {
		if projected, ok := stream.(*projectedServerStream); ok {
			err = projected.sendError()
		}
	}
	if err == nil {
		if contextErr := stream.Context().Err(); contextErr != nil {
			err = status.FromContextError(contextErr).Err()
		}
	}
	return err
}
