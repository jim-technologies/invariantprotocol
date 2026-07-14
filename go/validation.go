package invariant

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"buf.build/go/protovalidate"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Validation returns an interceptor that runs protovalidate on each request.
//
// Validation failures are returned as INVALID_ARGUMENT with field-level
// BadRequest details. Requests of types without protovalidate constraints
// pass through unchanged.
//
//	v, err := invariant.Validation()
//	if err != nil {
//	    return err
//	}
//	server.Use(v)
//
// Streaming RPCs are not covered by the unary interceptor — pair it with
// ValidationStream and `server.UseStream(vs)` when you have streaming
// methods with protovalidate constraints.
func Validation() (grpc.UnaryServerInterceptor, error) {
	validator, err := protovalidate.New()
	if err != nil {
		return nil, fmt.Errorf("create protovalidate validator: %w", err)
	}

	return func(ctx context.Context, req any, _ *ServerCallInfo, handler UnaryHandler) (any, error) {
		msg, ok := req.(proto.Message)
		if !ok {
			return handler(ctx, req)
		}
		if err := validator.Validate(msg); err != nil {
			return nil, validationToInvariantError(err)
		}
		return handler(ctx, req)
	}, nil
}

// ValidationStream returns a stream interceptor that runs protovalidate on
// the request before opening the stream. Failures short-circuit with
// INVALID_ARGUMENT and never produce any response messages — the same
// guarantee callers expect from the unary variant.
func ValidationStream() (grpc.StreamServerInterceptor, error) {
	validator, err := protovalidate.New()
	if err != nil {
		return nil, fmt.Errorf("create protovalidate validator: %w", err)
	}

	return func(srv any, stream grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		wrapped := &validationServerStream{
			ServerStream: stream,
			validate: func(msg proto.Message) error {
				if err := validator.Validate(msg); err != nil {
					return validationToInvariantError(err)
				}
				return nil
			},
		}
		return handler(srv, wrapped)
	}, nil
}

type validationServerStream struct {
	grpc.ServerStream
	validate func(proto.Message) error
}

func (s *validationServerStream) RecvMsg(msg any) error {
	if err := s.ServerStream.RecvMsg(msg); err != nil {
		return err
	}
	if protoMessage, ok := msg.(proto.Message); ok {
		return s.validate(protoMessage)
	}
	return nil
}

func validationToInvariantError(err error) error {
	verr := &protovalidate.ValidationError{}
	ok := errors.As(err, &verr)
	if !ok {
		return status.Error(codes.InvalidArgument, err.Error())
	}

	violations := verr.Violations
	if len(violations) == 0 {
		return status.Error(codes.InvalidArgument, err.Error())
	}

	br := &errdetails.BadRequest{}
	parts := make([]string, 0, len(violations))
	for _, v := range violations {
		field := ""
		if fp := v.Proto.GetField(); fp != nil {
			for _, el := range fp.GetElements() {
				if name := el.GetFieldName(); name != "" {
					if field != "" {
						field += "."
					}
					field += name
				}
			}
		}
		msg := v.Proto.GetMessage()
		br.FieldViolations = append(br.FieldViolations, &errdetails.BadRequest_FieldViolation{
			Field:       field,
			Description: msg,
		})
		parts = append(parts, field+": "+msg)
	}

	st := status.New(codes.InvalidArgument, joinSemi(parts))
	withDetails, derr := st.WithDetails(br)
	if derr != nil {
		return st.Err()
	}
	return withDetails.Err()
}

func joinSemi(parts []string) string {
	return strings.Join(parts, "; ")
}
