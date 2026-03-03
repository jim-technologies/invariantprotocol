"""MCP server entry point for the Invariant Protocol test service."""

import json
import os
import sys

import fire

# Add framework and test proto stubs to path
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "..", "src"))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "proto", "gen"))

import greet_pb2

from invariant import Server

DESCRIPTOR_PATH = os.path.join(os.path.dirname(__file__), "..", "proto", "descriptor.binpb")


class GreetServicer:
    """Test servicer that implements GreetService RPCs."""

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


class MCPServerCLI:
    """Invariant Protocol test server — serve via MCP (stdio) or run CLI commands."""

    def __init__(self, server: Server):
        self._server = server

    def mcp(self, remote: str | None = None):
        """Serve MCP over stdio. Use --remote to proxy to a gRPC server."""
        if remote:
            self._server.connect(remote)
        else:
            self._server.register(GreetServicer())
        self._server.serve(mcp=True)

    def cli(self, tool_name: str, remote: str | None = None, r: str | None = None, **kwargs):
        """Run a CLI command. Usage: cli GreetService.Greet --name Alice"""
        if remote:
            self._server.connect(remote)
        else:
            self._server.register(GreetServicer())

        args = [tool_name]
        if r:
            args.extend(["-r", r])
        for k, v in kwargs.items():
            args.extend([f"--{k}", json.dumps(v) if isinstance(v, dict | list) else str(v)])
        result = self._server._cli(args)
        print(json.dumps(result, indent=2))


if __name__ == "__main__":
    server = Server.from_descriptor(DESCRIPTOR_PATH)
    fire.Fire(MCPServerCLI(server))
