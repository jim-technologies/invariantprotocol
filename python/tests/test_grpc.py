"""Test gRPC projection (grpc.aio)."""

import asyncio
import base64
from concurrent.futures import ThreadPoolExecutor

import greet_pb2
import greet_pb2_grpc
import grpc
import httpx
import pytest
from conftest import DESCRIPTOR_PATH, register_greet
from google.protobuf import any_pb2, descriptor_pool, message_factory
from google.rpc import error_details_pb2, status_pb2

from invariant import Server
from invariant.projection_context import ProjectionContext
from invariant.projections.grpc import _projected_handlers


def _stub(channel: grpc.aio.Channel, method: str, resp_type: str):
    pool = descriptor_pool.Default()
    resp_class = message_factory.GetMessageClass(pool.FindMessageTypeByName(resp_type))
    return channel.unary_unary(
        method,
        request_serializer=lambda msg: msg.SerializeToString(),
        response_deserializer=resp_class.FromString,
    )


async def test_greet_grpc(server):
    port = await server._start_grpc(port=0)
    try:
        async with grpc.aio.insecure_channel(f"localhost:{port}") as channel:
            stub = greet_pb2_grpc.GreetServiceStub(channel)
            response = await stub.Greet(greet_pb2.GreetRequest(name="World"))
        assert response.message == "Hi World"
    finally:
        await server._stop_grpc()


async def test_greet_grpc_different_name(server):
    port = await server._start_grpc(port=0)
    try:
        async with grpc.aio.insecure_channel(f"localhost:{port}") as channel:
            stub = _stub(channel, "/greet.v1.GreetService/Greet", "greet.v1.GreetResponse")
            response = await stub(greet_pb2.GreetRequest(name="Claude"))
        assert response.message == "Hi Claude"
    finally:
        await server._stop_grpc()


async def test_greet_grpc_with_enum_and_tags(server):
    port = await server._start_grpc(port=0)
    try:
        async with grpc.aio.insecure_channel(f"localhost:{port}") as channel:
            stub = _stub(channel, "/greet.v1.GreetService/Greet", "greet.v1.GreetResponse")
            response = await stub(
                greet_pb2.GreetRequest(
                    name="World",
                    mood=greet_pb2.MOOD_HAPPY,
                    tags={"lang": "en"},
                )
            )
        assert response.message == "Hi World"
        assert response.mood == greet_pb2.MOOD_HAPPY
        assert response.tags["lang"] == "en"
    finally:
        await server._stop_grpc()


async def test_greet_group_grpc(server):
    port = await server._start_grpc(port=0)
    try:
        async with grpc.aio.insecure_channel(f"localhost:{port}") as channel:
            stub = _stub(channel, "/greet.v1.GreetService/GreetGroup", "greet.v1.GreetGroupResponse")
            response = await stub(
                greet_pb2.GreetGroupRequest(
                    people=[
                        greet_pb2.Person(name="Alice", mood=greet_pb2.MOOD_HAPPY),
                        greet_pb2.Person(name="Bob", mood=greet_pb2.MOOD_SAD),
                    ]
                )
            )
        assert list(response.messages) == ["Hi Alice", "Hi Bob"]
        assert response.count == 2
    finally:
        await server._stop_grpc()


async def test_greet_group_grpc_empty(server):
    port = await server._start_grpc(port=0)
    try:
        async with grpc.aio.insecure_channel(f"localhost:{port}") as channel:
            stub = _stub(channel, "/greet.v1.GreetService/GreetGroup", "greet.v1.GreetGroupResponse")
            response = await stub(greet_pb2.GreetGroupRequest(people=[]))
        assert list(response.messages) == []
        assert response.count == 0
    finally:
        await server._stop_grpc()


async def test_grpc_reflection(server):
    """Reflection should advertise the registered services."""
    from grpc_reflection.v1alpha import reflection_pb2, reflection_pb2_grpc

    port = await server._start_grpc(port=0)
    try:
        async with grpc.aio.insecure_channel(f"localhost:{port}") as channel:
            stub = reflection_pb2_grpc.ServerReflectionStub(channel)

            async def requests():
                yield reflection_pb2.ServerReflectionRequest(list_services="")

            services: set[str] = set()
            async for resp in stub.ServerReflectionInfo(requests()):
                for svc in resp.list_services_response.service:
                    services.add(svc.name)

        assert "greet.v1.GreetService" in services
    finally:
        await server._stop_grpc()


async def test_public_grpc_server_accepts_native_controls_and_builds_once():
    native_methods: list[str] = []
    shared_calls: list[tuple[type, str]] = []
    shared_stream_calls: list[tuple[type, str]] = []

    class NativeInterceptor(grpc.aio.ServerInterceptor):
        async def intercept_service(self, continuation, handler_call_details):
            native_methods.append(handler_call_details.method)
            return await continuation(handler_call_details)

    class SharedInterceptor(grpc.aio.ServerInterceptor):
        async def intercept_service(self, continuation, handler_call_details):
            handler = await continuation(handler_call_details)
            assert handler is not None
            if handler.response_streaming:
                terminal = handler.unary_stream
                assert terminal is not None

                async def stream(request, context):
                    shared_stream_calls.append((type(request), handler_call_details.method))
                    async for response in terminal(request, context):
                        yield response

                return grpc.unary_stream_rpc_method_handler(
                    stream,
                    request_deserializer=handler.request_deserializer,
                    response_serializer=handler.response_serializer,
                )

            terminal = handler.unary_unary
            assert terminal is not None

            async def unary(request, context):
                shared_calls.append((type(request), handler_call_details.method))
                return await terminal(request, context)

            return grpc.unary_unary_rpc_method_handler(
                unary,
                request_deserializer=handler.request_deserializer,
                response_serializer=handler.response_serializer,
            )

    class Servicer:
        async def Greet(self, request, context):
            return greet_pb2.GreetResponse(message=f"Hi {request.name}")

        async def StreamGreet(self, request, context):
            for index in range(request.count):
                yield greet_pb2.GreetResponse(message=f"Hi {request.name} #{index}")

    server = Server.from_descriptor(DESCRIPTOR_PATH)
    server.use(SharedInterceptor())
    register_greet(server, Servicer())
    migration_pool = ThreadPoolExecutor(max_workers=1)
    native = server.grpc_server(
        migration_pool,
        interceptors=(NativeInterceptor(),),
        options=(("grpc.max_receive_message_length", 1024),),
        maximum_concurrent_rpcs=4,
        compression=grpc.Compression.Gzip,
    )
    with pytest.raises(RuntimeError, match="already been built"):
        server.grpc_server()

    port = native.add_insecure_port("127.0.0.1:0")
    await native.start()
    try:
        async with grpc.aio.insecure_channel(f"127.0.0.1:{port}") as channel:
            stub = greet_pb2_grpc.GreetServiceStub(channel)
            response = await stub.Greet(greet_pb2.GreetRequest(name="native"))
            streamed = [
                item.message async for item in stub.StreamGreet(greet_pb2.StreamGreetRequest(name="native", count=2))
            ]
        assert response.message == "Hi native"
        assert streamed == ["Hi native #0", "Hi native #1"]
        assert native_methods == [
            "/greet.v1.GreetService/Greet",
            "/greet.v1.GreetService/StreamGreet",
        ]
        assert shared_calls == [(greet_pb2.GreetRequest, "/greet.v1.GreetService/Greet")]
        assert shared_stream_calls == [(greet_pb2.StreamGreetRequest, "/greet.v1.GreetService/StreamGreet")]
    finally:
        await server.stop(grace=0)
        migration_pool.shutdown(wait=True)


async def test_shared_interceptor_wraps_native_client_streaming_and_bidi_once():
    calls: list[tuple[str, tuple[tuple[str, str], ...]]] = []
    terminal_requests: list[type] = []

    class SharedInterceptor(grpc.aio.ServerInterceptor):
        async def intercept_service(self, continuation, handler_call_details):
            calls.append((handler_call_details.method, tuple(handler_call_details.invocation_metadata)))
            return await continuation(handler_call_details)

    async def upload(request_iterator, context):
        del context
        names = []
        async for request in request_iterator:
            terminal_requests.append(type(request))
            names.append(request.name)
        return greet_pb2.GreetResponse(message=",".join(names))

    async def chat(request_iterator, context):
        del context
        async for request in request_iterator:
            terminal_requests.append(type(request))
            yield greet_pb2.GreetResponse(message=request.name)

    server = Server.from_descriptor(DESCRIPTOR_PATH)
    register_greet(server, object())
    server.use(SharedInterceptor())
    registered = server._registered_services["greet.v1.GreetService"]
    registered["Upload"] = grpc.stream_unary_rpc_method_handler(
        upload,
        request_deserializer=greet_pb2.GreetRequest.FromString,
        response_serializer=greet_pb2.GreetResponse.SerializeToString,
    )
    registered["Chat"] = grpc.stream_stream_rpc_method_handler(
        chat,
        request_deserializer=greet_pb2.GreetRequest.FromString,
        response_serializer=greet_pb2.GreetResponse.SerializeToString,
    )

    handlers = _projected_handlers(server)["greet.v1.GreetService"]
    context = ProjectionContext(
        peer="test:client-streaming",
        invocation_metadata=(("x-request-id", "stream-123"),),
    )

    async def upload_requests():
        yield greet_pb2.GreetRequest(name="one")
        yield greet_pb2.GreetRequest(name="two")

    upload_terminal = handlers["Upload"].stream_unary
    assert upload_terminal is not None
    response = await upload_terminal(upload_requests(), context)
    assert response.message == "one,two"

    async def chat_requests():
        yield greet_pb2.GreetRequest(name="three")
        yield greet_pb2.GreetRequest(name="four")

    chat_terminal = handlers["Chat"].stream_stream
    assert chat_terminal is not None
    streamed = [response.message async for response in chat_terminal(chat_requests(), context)]
    assert streamed == ["three", "four"]
    assert calls == [
        ("/greet.v1.GreetService/Upload", (("x-request-id", "stream-123"),)),
        ("/greet.v1.GreetService/Chat", (("x-request-id", "stream-123"),)),
    ]
    assert terminal_requests == [greet_pb2.GreetRequest] * 4


async def test_stop_grace_drains_inflight_native_rpc():
    started = asyncio.Event()
    release = asyncio.Event()
    finished = asyncio.Event()

    class Servicer:
        async def Greet(self, request, context):
            started.set()
            await release.wait()
            finished.set()
            return greet_pb2.GreetResponse(message=f"Hi {request.name}")

    server = Server.from_descriptor(DESCRIPTOR_PATH)
    register_greet(server, Servicer())
    native = server.grpc_server()
    port = native.add_insecure_port("127.0.0.1:0")
    await native.start()
    channel = grpc.aio.insecure_channel(f"127.0.0.1:{port}")
    call = greet_pb2_grpc.GreetServiceStub(channel).Greet(greet_pb2.GreetRequest(name="drain"))
    stop_task: asyncio.Task | None = None
    try:
        await asyncio.wait_for(started.wait(), timeout=2)
        stop_task = asyncio.create_task(server.stop(grace=2))
        await asyncio.sleep(0.05)
        assert not stop_task.done()

        release.set()
        response = await call
        await asyncio.wait_for(stop_task, timeout=2)
        assert response.message == "Hi drain"
        assert finished.is_set()
    finally:
        release.set()
        if stop_task is not None and not stop_task.done():
            await server.stop(grace=0)
            await asyncio.gather(stop_task, return_exceptions=True)
        else:
            await server.stop(grace=0)
        await channel.close()


async def test_generated_client_proxy_preserves_grpc_semantics_and_channel_ownership():
    observed: asyncio.Queue[tuple[dict[str, str], float | None]] = asyncio.Queue()
    cancel_started = asyncio.Event()
    cancel_seen = asyncio.Event()

    class Backend:
        async def Greet(self, request, context):
            await observed.put((dict(context.invocation_metadata()), context.time_remaining()))
            if request.name == "cancel":
                cancel_started.set()
                try:
                    await asyncio.sleep(60)
                finally:
                    cancel_seen.set()
            if request.name == "error":
                detail = error_details_pb2.BadRequest(
                    field_violations=[error_details_pb2.BadRequest.FieldViolation(field="name", description="reserved")]
                )
                packed = any_pb2.Any()
                packed.Pack(detail)
                rich = status_pb2.Status(code=9, message="cannot greet", details=[packed])
                await context.abort(
                    grpc.StatusCode.FAILED_PRECONDITION,
                    "cannot greet",
                    (
                        ("x-remote-trailer", "error-trailer"),
                        ("grpc-status-details-bin", rich.SerializeToString()),
                    ),
                )
            await context.send_initial_metadata((("x-remote-header", "leading"),))
            context.set_trailing_metadata((("x-remote-trailer", "trailing"),))
            return greet_pb2.GreetResponse(message=f"Hi {request.name}")

    backend = Server.from_descriptor(DESCRIPTOR_PATH)
    register_greet(backend, Backend())
    backend_port = await backend._start_grpc(0)
    backend_channel = grpc.aio.insecure_channel(f"127.0.0.1:{backend_port}")

    proxy = Server.from_descriptor(DESCRIPTOR_PATH)
    proxy.connect_grpc(backend_channel)
    proxy_port = await proxy._start_grpc(0)
    proxy_http_port = await proxy._start_http(0)
    client_channel = grpc.aio.insecure_channel(f"127.0.0.1:{proxy_port}")
    stub = greet_pb2_grpc.GreetServiceStub(client_channel)
    try:
        call = stub.Greet(
            greet_pb2.GreetRequest(name="proxy"),
            metadata=(("x-client-metadata", "forwarded"),),
            timeout=2,
        )
        response = await call
        assert response.message == "Hi proxy"
        metadata, remaining = await observed.get()
        assert metadata["x-client-metadata"] == "forwarded"
        assert remaining is not None
        assert remaining > 0
        assert dict(await call.initial_metadata())["x-remote-header"] == "leading"
        assert dict(await call.trailing_metadata())["x-remote-trailer"] == "trailing"

        with pytest.raises(grpc.aio.AioRpcError) as caught:
            await stub.Greet(greet_pb2.GreetRequest(name="error"), timeout=2)
        assert caught.value.code() == grpc.StatusCode.FAILED_PRECONDITION
        assert caught.value.details() == "cannot greet"
        trailers = dict(caught.value.trailing_metadata())
        assert trailers["x-remote-trailer"] == "error-trailer"
        rich = status_pb2.Status.FromString(trailers["grpc-status-details-bin"])
        unpacked = error_details_pb2.BadRequest()
        assert rich.details[0].Unpack(unpacked)
        assert unpacked.field_violations[0].field == "name"

        async with httpx.AsyncClient(base_url=f"http://127.0.0.1:{proxy_http_port}") as client:
            projected_error = await client.post(
                "/greet.v1.GreetService/Greet",
                json={"name": "error"},
            )
        assert projected_error.status_code == 400
        assert projected_error.json()["code"] == "failed_precondition"
        detail = projected_error.json()["details"][0]
        assert detail["type"] == "google.rpc.BadRequest"
        value = base64.b64decode(detail["value"] + "=" * (-len(detail["value"]) % 4))
        projected_bad_request = error_details_pb2.BadRequest.FromString(value)
        assert projected_bad_request.field_violations[0].field == "name"
        assert projected_error.headers["trailer-x-remote-trailer"] == "error-trailer"

        cancel_call = stub.Greet(greet_pb2.GreetRequest(name="cancel"))
        await asyncio.wait_for(cancel_started.wait(), timeout=2)
        cancel_call.cancel()
        with pytest.raises(asyncio.CancelledError):
            await cancel_call
        await asyncio.wait_for(cancel_seen.wait(), timeout=2)

        # Stopping Invariant does not close the caller-owned backend channel.
        await proxy.stop()
        direct = await greet_pb2_grpc.GreetServiceStub(backend_channel).Greet(greet_pb2.GreetRequest(name="owned"))
        assert direct.message == "Hi owned"
    finally:
        await client_channel.close()
        await proxy.stop()
        await backend_channel.close()
        await backend.stop()
