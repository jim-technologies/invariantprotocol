"""Serve the generated Greet service for the Connect-ES interoperability test."""

from __future__ import annotations

import asyncio
import socket
import sys
from pathlib import Path

import grpc
import uvicorn

sys.path.insert(0, str(Path(__file__).parent / "proto" / "gen"))

import greet_pb2
import greet_pb2_grpc

from invariant import Server


class GreetServicer:
    async def Greet(self, request, context):
        if request.name == "error":
            await context.abort(grpc.StatusCode.FAILED_PRECONDITION, "interop status")
        return greet_pb2.GreetResponse(message=f"Hi {request.name}")

    async def GreetGroup(self, request, context):
        messages = [f"Hi {person.name}" for person in request.people]
        return greet_pb2.GreetGroupResponse(messages=messages, count=len(messages))

    async def StreamGreet(self, request, context):
        for index in range(request.count or 1):
            yield greet_pb2.GreetResponse(message=f"Hi {request.name} #{index}")


async def main() -> None:
    server = Server.from_descriptor(str(Path(__file__).parent / "proto" / "descriptor.binpb"))
    greet_pb2_grpc.add_GreetServiceServicer_to_server(GreetServicer(), server)

    listener = socket.socket()
    listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    listener.bind(("127.0.0.1", 0))
    listener.listen()
    port = listener.getsockname()[1]

    uvicorn_server = uvicorn.Server(uvicorn.Config(server.asgi_app(), log_level="warning"))
    print(f"http://127.0.0.1:{port}", flush=True)
    try:
        await uvicorn_server.serve(sockets=[listener])
    finally:
        await server.stop()


if __name__ == "__main__":
    asyncio.run(main())
