"""Test the protovalidate interceptor."""

import greet_pb2
import grpc
import pytest
import pytest_asyncio
from conftest import DESCRIPTOR_PATH, register_greet

from invariant import InvariantError, Server, validation


async def test_validation_passes_when_constraints_satisfied(server):
    server.use(validation())
    result = await server._cli(["GreetService", "Greet", "-r", '{"name": "World"}'])
    assert result["message"] == "Hi World"


async def test_validation_rejects_constraint_violation(server):
    """Empty name violates `string.min_len = 1` and should produce INVALID_ARGUMENT."""
    server.use(validation())
    with pytest.raises(InvariantError) as exc:
        await server._cli(["GreetService", "Greet", "-r", '{"name": ""}'])
    assert exc.value.code == grpc.StatusCode.INVALID_ARGUMENT
    payload = exc.value.to_payload()
    assert payload["code"] == "invalid_argument"
    # field-level details should mention `name`
    violations = payload["details"][0]["fieldViolations"]
    assert any(v["field"] == "name" for v in violations)


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
    await server._cli(["GreetService", "Greet", "-r", '{"name": "Spy"}'])

    assert len(seen) == 1
    assert isinstance(seen[0], greet_pb2.GreetRequest)
    assert seen[0].name == "Spy"


# -- The same standard validation interceptor covers server streaming. --


class _StreamSvc:
    async def StreamGreet(self, request, context):
        n = request.count or 1
        for i in range(n):
            yield greet_pb2.GreetResponse(message=f"Hi {request.name} #{i}")


@pytest_asyncio.fixture
async def stream_validation_server():
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    register_greet(srv, _StreamSvc())
    srv.use(validation())
    yield srv
    await srv.stop()


async def test_validation_stream_rejects_constraint_violation(stream_validation_server):
    """Empty name violates StreamGreetRequest.name.min_len = 1."""

    async def drain():
        async for _ in stream_validation_server.invoke_stream(
            "GreetService.StreamGreet", greet_pb2.StreamGreetRequest(name="", count=3)
        ):
            pass

    with pytest.raises(InvariantError) as exc:
        await drain()
    assert exc.value.code == grpc.StatusCode.INVALID_ARGUMENT
    payload = exc.value.to_payload()
    violations = payload["details"][0]["fieldViolations"]
    assert any(v["field"] == "name" for v in violations)


async def test_validation_stream_passes_when_satisfied(stream_validation_server):
    msgs = [
        m.message
        async for m in stream_validation_server.invoke_stream(
            "GreetService.StreamGreet", greet_pb2.StreamGreetRequest(name="ok", count=2)
        )
    ]
    assert msgs == ["Hi ok #0", "Hi ok #1"]
