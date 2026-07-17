"""Test the protovalidate interceptor."""

from pathlib import Path

import greet_pb2
import grpc
import pytest
import pytest_asyncio
from conftest import DESCRIPTOR_PATH, register_greet
from google.protobuf import descriptor_pb2, descriptor_pool
from google.rpc import error_details_pb2, status_pb2

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


def _streaming_validation_descriptor() -> bytes:
    files = descriptor_pb2.FileDescriptorSet.FromString(Path(DESCRIPTOR_PATH).read_bytes())
    file = descriptor_pb2.FileDescriptorProto(
        name="invariant/tests/streaming_validation.proto",
        package="invariant.tests.validation",
        syntax="proto3",
        dependency=["greet.proto"],
    )
    service = file.service.add(name="StreamingValidationService")
    service.method.add(
        name="ClientStream",
        input_type=".greet.v1.GreetRequest",
        output_type=".greet.v1.GreetResponse",
        client_streaming=True,
    )
    service.method.add(
        name="Bidi",
        input_type=".greet.v1.GreetRequest",
        output_type=".greet.v1.GreetResponse",
        client_streaming=True,
        server_streaming=True,
    )
    files.file.add().CopyFrom(file)

    pool = descriptor_pool.Default()
    try:
        pool.FindFileByName(file.name)
    except KeyError:
        pool.Add(file)
    return files.SerializeToString()


def _bad_request_from_rpc_error(error: grpc.aio.AioRpcError) -> error_details_pb2.BadRequest:
    trailers = dict(error.trailing_metadata())
    rich_status = status_pb2.Status.FromString(trailers["grpc-status-details-bin"])
    assert rich_status.code == 3
    detail = error_details_pb2.BadRequest()
    assert rich_status.details[0].Unpack(detail)
    return detail


async def test_validation_checks_each_native_client_streaming_and_bidi_request():
    client_stream_seen: list[str] = []
    bidi_seen: list[str] = []

    async def client_stream(request_iterator, context):
        del context
        async for request in request_iterator:
            client_stream_seen.append(request.name)
        return greet_pb2.GreetResponse(message="client complete")

    async def bidi(request_iterator, context):
        del context
        async for request in request_iterator:
            bidi_seen.append(request.name)
            yield greet_pb2.GreetResponse(message=request.name)

    handlers = {
        "ClientStream": grpc.stream_unary_rpc_method_handler(
            client_stream,
            request_deserializer=greet_pb2.GreetRequest.FromString,
            response_serializer=greet_pb2.GreetResponse.SerializeToString,
        ),
        "Bidi": grpc.stream_stream_rpc_method_handler(
            bidi,
            request_deserializer=greet_pb2.GreetRequest.FromString,
            response_serializer=greet_pb2.GreetResponse.SerializeToString,
        ),
    }
    server = Server.from_bytes(_streaming_validation_descriptor())
    server.use(validation())
    server.add_generic_rpc_handlers(
        (grpc.method_handlers_generic_handler("invariant.tests.validation.StreamingValidationService", handlers),)
    )
    server.add_registered_method_handlers("invariant.tests.validation.StreamingValidationService", handlers)
    native = server.grpc_server()
    port = native.add_insecure_port("127.0.0.1:0")
    await native.start()

    async def requests():
        yield greet_pb2.GreetRequest(name="valid")
        yield greet_pb2.GreetRequest(name="")

    try:
        async with grpc.aio.insecure_channel(f"127.0.0.1:{port}") as channel:
            client_call = channel.stream_unary(
                "/invariant.tests.validation.StreamingValidationService/ClientStream",
                request_serializer=greet_pb2.GreetRequest.SerializeToString,
                response_deserializer=greet_pb2.GreetResponse.FromString,
            )
            with pytest.raises(grpc.aio.AioRpcError) as client_error:
                await client_call(requests())
            assert client_error.value.code() == grpc.StatusCode.INVALID_ARGUMENT
            client_detail = _bad_request_from_rpc_error(client_error.value)
            assert client_detail.field_violations[0].field == "name"

            bidi_call = channel.stream_stream(
                "/invariant.tests.validation.StreamingValidationService/Bidi",
                request_serializer=greet_pb2.GreetRequest.SerializeToString,
                response_deserializer=greet_pb2.GreetResponse.FromString,
            )(requests())
            assert (await bidi_call.read()).message == "valid"
            with pytest.raises(grpc.aio.AioRpcError) as bidi_error:
                await bidi_call.read()
            assert bidi_error.value.code() == grpc.StatusCode.INVALID_ARGUMENT
            bidi_detail = _bad_request_from_rpc_error(bidi_error.value)
            assert bidi_detail.field_violations[0].field == "name"

        assert client_stream_seen == ["valid"]
        assert bidi_seen == ["valid"]
    finally:
        await server.stop(grace=0)
