import os
import sys

import pytest_asyncio

# Add framework and generated stubs to path
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "src"))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "proto", "gen"))

import greet_pb2
import greet_pb2_grpc

from invariant import Server

DESCRIPTOR_PATH = os.path.join(os.path.dirname(__file__), "proto", "descriptor.binpb")


class GreetServicer:
    async def Greet(self, request, context):
        return greet_pb2.GreetResponse(
            message=f"Hi {request.name}",
            mood=request.mood,
            tags=dict(request.tags),
        )

    async def GreetGroup(self, request, context):
        messages = [f"Hi {p.name}" for p in request.people]
        return greet_pb2.GreetGroupResponse(
            messages=messages,
            count=len(request.people),
        )

    async def StreamGreet(self, request, context):
        for i in range(request.count or 1):
            yield greet_pb2.GreetResponse(message=f"Hi {request.name} #{i}")


def register_greet(server: Server, servicer) -> None:
    """Register partial test doubles through grpcio's generated helper.

    Production implementations implement the complete generated interface.
    Tests focused on one method use this adapter to keep their setup compact.
    Registration itself still goes exclusively through generated code.
    """

    class CompleteTestServicer:
        async def Greet(self, request, context):
            handler = getattr(servicer, "Greet", None)
            if handler is None:
                return greet_pb2.GreetResponse()
            return await handler(request, context)

        async def GreetGroup(self, request, context):
            handler = getattr(servicer, "GreetGroup", None)
            if handler is None:
                return greet_pb2.GreetGroupResponse()
            return await handler(request, context)

        async def StreamGreet(self, request, context):
            handler = getattr(servicer, "StreamGreet", None)
            if handler is None:
                return
            async for response in handler(request, context):
                yield response

    greet_pb2_grpc.add_GreetServiceServicer_to_server(CompleteTestServicer(), server)


@pytest_asyncio.fixture
async def server():
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    srv.exclude("*StreamGreet")
    register_greet(srv, GreetServicer())
    yield srv
    await srv.stop()
