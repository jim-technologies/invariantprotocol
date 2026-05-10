"""Test the protovalidate interceptor."""

import greet_pb2
import grpc
import pytest

from invariant import InvariantError, validation


async def test_validation_passes_when_constraints_satisfied(server):
    server.use(validation())
    try:
        result = await server._cli(["GreetService", "Greet", "-r", '{"name": "World"}'])
        assert result["message"] == "Hi World"
    finally:
        server._interceptors.clear()


async def test_validation_rejects_constraint_violation(server):
    """Empty name violates `string.min_len = 1` and should produce INVALID_ARGUMENT."""
    server.use(validation())
    try:
        with pytest.raises(InvariantError) as exc:
            await server._cli(["GreetService", "Greet", "-r", '{"name": ""}'])
        assert exc.value.code == grpc.StatusCode.INVALID_ARGUMENT
        payload = exc.value.to_payload()
        assert payload["code"] == "invalid_argument"
        # field-level details should mention `name`
        violations = payload["details"][0]["fieldViolations"]
        assert any(v["field"] == "name" for v in violations)
    finally:
        server._interceptors.clear()


async def test_validation_interceptor_invokes_validator(monkeypatch, server):
    """Validation interceptor calls protovalidate.Validator.validate on the request."""
    seen: list[object] = []

    import protovalidate

    real_validate = protovalidate.Validator.validate

    def spy(self, msg):
        seen.append(msg)
        return real_validate(self, msg)

    monkeypatch.setattr(protovalidate.Validator, "validate", spy)

    server.use(validation())
    try:
        await server._cli(["GreetService", "Greet", "-r", '{"name": "Spy"}'])
    finally:
        server._interceptors.clear()

    assert len(seen) == 1
    assert isinstance(seen[0], greet_pb2.GreetRequest)
    assert seen[0].name == "Spy"
