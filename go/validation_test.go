package invariant

import (
	"context"
	"testing"

	greetpb "github.com/jim-technologies/invariantprotocol/go/tests/gen"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type validatingGreetServicer struct{}

func (s *validatingGreetServicer) Greet(_ context.Context, req *greetpb.GreetRequest) (*greetpb.GreetResponse, error) {
	return &greetpb.GreetResponse{Message: "Hi " + req.Name}, nil
}

func (s *validatingGreetServicer) GreetGroup(_ context.Context, _ *greetpb.GreetGroupRequest) (*greetpb.GreetGroupResponse, error) {
	return &greetpb.GreetGroupResponse{}, nil
}

func TestValidationPassesWhenConstraintsSatisfied(t *testing.T) {
	srv, err := ServerFromDescriptor(descriptorPath())
	require.NoError(t, err)
	require.NoError(t, srv.Register(&validatingGreetServicer{}))

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
	require.NoError(t, srv.Register(&validatingGreetServicer{}))

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
