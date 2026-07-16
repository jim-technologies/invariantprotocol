package invariant

import (
	"context"
	"testing"

	greetpb "github.com/jim-technologies/invariantprotocol/go/tests/gen"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type validatingGreetServicer struct {
	greetpb.UnimplementedGreetServiceServer
}

func (s *validatingGreetServicer) Greet(_ context.Context, req *greetpb.GreetRequest) (*greetpb.GreetResponse, error) {
	return &greetpb.GreetResponse{Message: "Hi " + req.Name}, nil
}

func (s *validatingGreetServicer) GreetGroup(_ context.Context, _ *greetpb.GreetGroupRequest) (*greetpb.GreetGroupResponse, error) {
	return &greetpb.GreetGroupResponse{}, nil
}

func TestValidationPassesWhenConstraintsSatisfied(t *testing.T) {
	srv, err := ServerFromDescriptor(descriptorPath())
	require.NoError(t, err)
	greetpb.RegisterGreetServiceServer(srv, &validatingGreetServicer{})

	v, err := Validation()
	require.NoError(t, err)
	srv.Use(v)

	resp, err := srv.Invoke(t.Context(), "GreetService.Greet", &greetpb.GreetRequest{Name: "World"})
	require.NoError(t, err)
	assert.Contains(t, resp.(*greetpb.GreetResponse).Message, "Hi World")
}

func TestValidationRejectsConstraintViolation(t *testing.T) {
	srv, err := ServerFromDescriptor(descriptorPath())
	require.NoError(t, err)
	greetpb.RegisterGreetServiceServer(srv, &validatingGreetServicer{})

	v, err := Validation()
	require.NoError(t, err)
	srv.Use(v)

	_, err = srv.Invoke(t.Context(), "GreetService.Greet", &greetpb.GreetRequest{Name: ""})
	require.Error(t, err)

	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.InvalidArgument, st.Code())
	assert.Contains(t, st.Message(), "name")
}

// -- ValidationStream covers server-streaming RPCs. --

func TestValidationStreamRejectsConstraintViolation(t *testing.T) {
	srv := streamServer(t, &streamServicer{})

	vs, err := ValidationStream()
	require.NoError(t, err)
	srv.UseStream(vs)

	// Empty name violates string.min_len = 1 on StreamGreetRequest.name.
	var emitted int
	err = srv.InvokeStream(t.Context(), "GreetService.StreamGreet",
		&greetpb.StreamGreetRequest{Name: "", Count: 3},
		func(proto.Message) error {
			emitted++
			return nil
		},
	)
	require.Error(t, err)

	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.InvalidArgument, st.Code())
	assert.Contains(t, st.Message(), "name")
	assert.Zero(t, emitted, "no messages should be emitted on validation failure")
}

func TestValidationStreamPassesWhenSatisfied(t *testing.T) {
	srv := streamServer(t, &streamServicer{})

	vs, err := ValidationStream()
	require.NoError(t, err)
	srv.UseStream(vs)

	var emitted int
	err = srv.InvokeStream(t.Context(), "GreetService.StreamGreet",
		&greetpb.StreamGreetRequest{Name: "ok", Count: 2},
		func(proto.Message) error {
			emitted++
			return nil
		},
	)
	require.NoError(t, err)
	assert.Equal(t, 2, emitted)
}
