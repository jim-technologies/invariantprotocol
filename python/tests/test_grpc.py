"""Test gRPC projection."""

import greet_pb2
import greet_pb2_grpc
import grpc


def test_greet_grpc(server):
    port = server._start_grpc(port=0)
    try:
        channel = grpc.insecure_channel(f"localhost:{port}")
        stub = greet_pb2_grpc.GreetServiceStub(channel)

        response = stub.Greet(greet_pb2.GreetRequest(name="World"))

        assert response.message == "Hi World"
        channel.close()
    finally:
        server._stop_grpc()


def test_greet_grpc_different_name(server):
    port = server._start_grpc(port=0)
    try:
        channel = grpc.insecure_channel(f"localhost:{port}")
        stub = greet_pb2_grpc.GreetServiceStub(channel)

        response = stub.Greet(greet_pb2.GreetRequest(name="Claude"))

        assert response.message == "Hi Claude"
        channel.close()
    finally:
        server._stop_grpc()


def test_greet_grpc_with_enum_and_tags(server):
    port = server._start_grpc(port=0)
    try:
        channel = grpc.insecure_channel(f"localhost:{port}")
        stub = greet_pb2_grpc.GreetServiceStub(channel)

        response = stub.Greet(
            greet_pb2.GreetRequest(
                name="World",
                mood=greet_pb2.MOOD_HAPPY,
                tags={"lang": "en"},
            )
        )

        assert response.message == "Hi World"
        assert response.mood == greet_pb2.MOOD_HAPPY
        assert response.tags["lang"] == "en"
        channel.close()
    finally:
        server._stop_grpc()


def test_greet_group_grpc(server):
    port = server._start_grpc(port=0)
    try:
        channel = grpc.insecure_channel(f"localhost:{port}")
        stub = greet_pb2_grpc.GreetServiceStub(channel)

        response = stub.GreetGroup(
            greet_pb2.GreetGroupRequest(
                people=[
                    greet_pb2.Person(name="Alice", mood=greet_pb2.MOOD_HAPPY),
                    greet_pb2.Person(name="Bob", mood=greet_pb2.MOOD_SAD),
                ],
            )
        )

        assert list(response.messages) == ["Hi Alice", "Hi Bob"]
        assert response.count == 2
        channel.close()
    finally:
        server._stop_grpc()


def test_greet_group_grpc_empty(server):
    port = server._start_grpc(port=0)
    try:
        channel = grpc.insecure_channel(f"localhost:{port}")
        stub = greet_pb2_grpc.GreetServiceStub(channel)

        response = stub.GreetGroup(greet_pb2.GreetGroupRequest(people=[]))

        assert list(response.messages) == []
        assert response.count == 0
        channel.close()
    finally:
        server._stop_grpc()
