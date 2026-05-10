package invariant

import (
	"context"
	"fmt"

	"buf.build/go/protovalidate"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
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
func Validation() (UnaryServerInterceptor, error) {
	validator, err := protovalidate.New()
	if err != nil {
		return nil, fmt.Errorf("create protovalidate validator: %w", err)
	}

	return func(ctx context.Context, req any, info *ServerCallInfo, handler UnaryHandler) (any, error) {
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

func validationToInvariantError(err error) error {
	verr, ok := err.(*protovalidate.ValidationError)
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
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "; "
		}
		out += p
	}
	return out
}
