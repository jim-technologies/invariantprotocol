package invariant

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	codepb "google.golang.org/genproto/googleapis/rpc/code"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

var unknownFieldPattern = regexp.MustCompile(`unknown field "([^"]+)"`)

func statusFromError(err error) *status.Status {
	if err == nil {
		return status.New(codes.OK, "")
	}
	if st, ok := status.FromError(err); ok {
		return st
	}
	return status.New(codes.Unknown, err.Error())
}

func errorMessage(err error) string {
	return statusFromError(err).Message()
}

func errorPayload(err error) map[string]any {
	st := statusFromError(err)
	payload := map[string]any{
		"code":    grpcCodeName(st.Code()),
		"message": st.Message(),
	}

	if details := statusDetails(st); len(details) > 0 {
		payload["details"] = details
	}

	return payload
}

func grpcCodeName(code codes.Code) string {
	name := codepb.Code(int32(code)).String()
	if strings.HasPrefix(name, "Code(") {
		return "UNKNOWN"
	}
	return name
}

func statusDetails(st *status.Status) []map[string]any {
	details := st.Details()
	if len(details) == 0 {
		return nil
	}

	out := make([]map[string]any, 0, len(details))
	for _, detail := range details {
		switch detail := detail.(type) {
		case proto.Message:
			raw, err := protojson.Marshal(detail)
			if err != nil {
				out = append(out, map[string]any{
					"@type": string(detail.ProtoReflect().Descriptor().FullName()),
					"value": fmt.Sprintf("%v", detail),
				})
				continue
			}

			var decoded map[string]any
			if err := json.Unmarshal(raw, &decoded); err != nil {
				out = append(out, map[string]any{
					"@type": string(detail.ProtoReflect().Descriptor().FullName()),
					"value": string(raw),
				})
				continue
			}
			decoded["@type"] = string(detail.ProtoReflect().Descriptor().FullName())
			out = append(out, decoded)
		case error:
			out = append(out, map[string]any{"message": detail.Error()})
		default:
			out = append(out, map[string]any{"value": fmt.Sprintf("%v", detail)})
		}
	}

	return out
}

func invalidArgumentError(message string) error {
	return status.Error(codes.InvalidArgument, message)
}

func invalidArgumentFromJSONError(err error) error {
	msg := err.Error()
	st := status.New(codes.InvalidArgument, msg)

	matches := unknownFieldPattern.FindStringSubmatch(msg)
	if len(matches) == 2 {
		br := &errdetails.BadRequest{
			FieldViolations: []*errdetails.BadRequest_FieldViolation{
				{
					Field:       matches[1],
					Description: msg,
				},
			},
		}

		withDetails, detailsErr := st.WithDetails(br)
		if detailsErr == nil {
			return withDetails.Err()
		}
	}

	return st.Err()
}

func grpcCodeToHTTPStatus(code codes.Code) int {
	codeToStatus := map[codes.Code]int{
		codes.OK:                 http.StatusOK,
		codes.Canceled:           499,
		codes.Unknown:            http.StatusInternalServerError,
		codes.InvalidArgument:    http.StatusBadRequest,
		codes.DeadlineExceeded:   http.StatusGatewayTimeout,
		codes.NotFound:           http.StatusNotFound,
		codes.AlreadyExists:      http.StatusConflict,
		codes.PermissionDenied:   http.StatusForbidden,
		codes.ResourceExhausted:  http.StatusTooManyRequests,
		codes.FailedPrecondition: http.StatusBadRequest,
		codes.Aborted:            http.StatusConflict,
		codes.OutOfRange:         http.StatusBadRequest,
		codes.Unimplemented:      http.StatusNotImplemented,
		codes.Internal:           http.StatusInternalServerError,
		codes.Unavailable:        http.StatusServiceUnavailable,
		codes.DataLoss:           http.StatusInternalServerError,
		codes.Unauthenticated:    http.StatusUnauthorized,
	}
	if statusCode, ok := codeToStatus[code]; ok {
		return statusCode
	}
	return http.StatusInternalServerError
}
