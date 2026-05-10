"""Test gRPC projection (grpc.aio)."""

import greet_pb2
import grpc
from google.protobuf import descriptor_pool, message_factory


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
            stub = _stub(channel, "/greet.v1.GreetService/Greet", "greet.v1.GreetResponse")
            response = await stub(greet_pb2.GreetRequest(name="World"))
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
