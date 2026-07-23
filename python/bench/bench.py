"""Python benchmarks — same scenarios as Go's testing.B suite.

Run:
    cd python && uv run python bench/bench.py

Reports elapsed time and ns/op. Repeat runs on an otherwise idle host when
comparing revisions; the script does not enforce a release threshold.
"""

from __future__ import annotations

import asyncio
import os
import sys
import time
from contextlib import asynccontextmanager

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "src"))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "tests", "proto", "gen"))

import greet_pb2
import greet_pb2_grpc
import grpc
import httpx

from invariant import Server

DESCRIPTOR_PATH = os.path.join(os.path.dirname(__file__), "..", "tests", "proto", "descriptor.binpb")


class GreetServicer:
    async def Greet(self, request, context):
        return greet_pb2.GreetResponse(message=f"Hi {request.name}")

    async def GreetGroup(self, request, context):
        msgs = [f"Hi {p.name}" for p in request.people]
        return greet_pb2.GreetGroupResponse(messages=msgs, count=len(request.people))

    async def StreamGreet(self, request, context):
        for i in range(request.count or 1):
            yield greet_pb2.GreetResponse(message=f"Hi {request.name} #{i}")


def build_server() -> Server:
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    greet_pb2_grpc.add_GreetServiceServicer_to_server(GreetServicer(), srv)
    return srv


async def bench(label: str, fn, iters: int) -> None:
    """Run ``fn`` ``iters`` times and report ns/op."""
    # Warmup
    for _ in range(min(50, iters)):
        await fn()
    start = time.perf_counter_ns()
    for _ in range(iters):
        await fn()
    elapsed = time.perf_counter_ns() - start
    ns_per_op = elapsed / iters
    print(f"{label:<32}  {iters:>9} ops  {ns_per_op:>10,.0f} ns/op  ({elapsed / 1e6:.1f} ms total)")


async def bench_invoke_direct():
    srv = build_server()
    req = greet_pb2.GreetRequest(name="World")

    async def call():
        await srv.invoke("greet.v1.GreetService.Greet", req)

    await bench("InvokeDirect", call, 50_000)


async def bench_invoke_with_interceptor():
    srv = build_server()

    class Passthrough(grpc.aio.ServerInterceptor):
        async def intercept_service(self, continuation, handler_call_details):
            return await continuation(handler_call_details)

    srv.use(Passthrough())
    req = greet_pb2.GreetRequest(name="World")

    async def call():
        await srv.invoke("greet.v1.GreetService.Greet", req)

    await bench("InvokeWithInterceptor", call, 50_000)


@asynccontextmanager
async def http_server():
    srv = build_server()
    port = await srv._start_http(port=0)
    try:
        yield srv, port
    finally:
        await srv._stop_http()


async def bench_http_json():
    async with http_server() as (_srv, port):
        url = f"http://localhost:{port}/greet.v1.GreetService/Greet"
        async with httpx.AsyncClient() as client:

            async def call():
                resp = await client.post(url, json={"name": "World"})
                resp.read()

            await bench("HTTPJSON", call, 5_000)


async def bench_http_proto():
    async with http_server() as (_srv, port):
        url = f"http://localhost:{port}/greet.v1.GreetService/Greet"
        body = greet_pb2.GreetRequest(name="World").SerializeToString()
        headers = {"Content-Type": "application/proto", "Accept": "application/proto"}
        async with httpx.AsyncClient() as client:

            async def call():
                resp = await client.post(url, content=body, headers=headers)
                resp.read()

            await bench("HTTPProto", call, 5_000)


async def bench_grpc_unary():
    srv = build_server()
    port = await srv._start_grpc(port=0)
    try:
        async with grpc.aio.insecure_channel(f"localhost:{port}") as channel:
            from google.protobuf import descriptor_pool, message_factory

            pool = descriptor_pool.Default()
            req_class = message_factory.GetMessageClass(pool.FindMessageTypeByName("greet.v1.GreetRequest"))
            resp_class = message_factory.GetMessageClass(pool.FindMessageTypeByName("greet.v1.GreetResponse"))

            stub = channel.unary_unary(
                "/greet.v1.GreetService/Greet",
                request_serializer=lambda msg: msg.SerializeToString(),
                response_deserializer=resp_class.FromString,
            )
            req = req_class(name="World")

            async def call():
                await stub(req)

            await bench("GRPCUnary", call, 5_000)
    finally:
        await srv._stop_grpc()


async def main():
    print(f"Python {sys.version.split()[0]}, asyncio {sys.version_info}")
    print("---")
    await bench_invoke_direct()
    await bench_invoke_with_interceptor()
    await bench_http_json()
    await bench_http_proto()
    await bench_grpc_unary()


if __name__ == "__main__":
    asyncio.run(main())
