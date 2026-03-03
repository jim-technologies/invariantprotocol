"""Standalone gRPC server for GreetService — used for testing remote MCP mode.

Usage:
    uv run --project ../.. python grpc_server.py [port]

Default port is 50051.
"""

import os
import signal
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "..", "src"))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "proto", "gen"))

import greet_pb2

from invariant import Server

DESCRIPTOR_PATH = os.path.join(os.path.dirname(__file__), "..", "proto", "descriptor.binpb")


class GreetServicer:
    def Greet(self, request, context):
        return greet_pb2.GreetResponse(
            message="Hi " + request.name + "!",
            mood=request.mood,
            tags=dict(request.tags),
        )

    def GreetGroup(self, request, context):
        messages = ["Hi " + p.name for p in request.people]
        return greet_pb2.GreetGroupResponse(
            messages=messages,
            count=len(request.people),
        )


def main():
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 50051

    server = Server.from_descriptor(DESCRIPTOR_PATH)
    server.register(GreetServicer())
    actual_port = server._start_grpc(port=port)
    print(f"gRPC server listening on port {actual_port}")

    # Block until Ctrl-C
    signal.signal(signal.SIGINT, lambda *_: None)
    signal.signal(signal.SIGTERM, lambda *_: None)
    signal.pause()
    server.stop()


if __name__ == "__main__":
    main()
