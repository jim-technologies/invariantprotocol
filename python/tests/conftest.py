import os
import sys

import pytest

# Add framework and generated stubs to path
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "src"))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "proto", "gen"))

import greet_pb2

from invariant import Server

DESCRIPTOR_PATH = os.path.join(os.path.dirname(__file__), "proto", "descriptor.binpb")


class GreetServicer:
    def Greet(self, request, context):
        return greet_pb2.GreetResponse(
            message=f"Hi {request.name}",
            mood=request.mood,
            tags=dict(request.tags),
        )

    def GreetGroup(self, request, context):
        messages = [f"Hi {p.name}" for p in request.people]
        return greet_pb2.GreetGroupResponse(
            messages=messages,
            count=len(request.people),
        )


@pytest.fixture(scope="session")
def server():
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    srv.register(GreetServicer())
    yield srv
    srv.stop()
