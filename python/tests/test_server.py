"""Test Server registration and include/exclude filtering."""

import asyncio
import os
import subprocess
import sys
from pathlib import Path

import greet_pb2
import greet_pb2_grpc
import grpc
import pytest
from google.protobuf import descriptor_pb2, descriptor_pool, message_factory

from invariant import Server

DESCRIPTOR_PATH = os.path.join(os.path.dirname(__file__), "proto", "descriptor.binpb")


class GreetServicer:
    async def Greet(self, request, context):
        return greet_pb2.GreetResponse()

    async def GreetGroup(self, request, context):
        return greet_pb2.GreetGroupResponse()

    async def StreamGreet(self, request, context):
        if False:
            yield greet_pb2.GreetResponse()


class SyncGreetServicer:
    def Greet(self, request, context):
        pass

    def GreetGroup(self, request, context):
        pass

    def StreamGreet(self, request, context):
        pass


def add_greet(srv, servicer=None):
    greet_pb2_grpc.add_GreetServiceServicer_to_server(servicer or GreetServicer(), srv)


def test_generated_registration_captures_service_exactly_once():
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    assert not hasattr(srv, "register")
    add_greet(srv)
    assert len(srv.tools) == 3
    assert list(srv._registered_services) == ["greet.v1.GreetService"]
    assert type(srv.tools["GreetService.Greet"].new_request()) is greet_pb2.GreetRequest


def test_tools_are_read_only_snapshots_and_registration_freezes_on_projection_build():
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    add_greet(srv)

    tools = srv.tools
    with pytest.raises(TypeError):
        tools["new"] = tools["GreetService.Greet"]

    tools["GreetService.Greet"].input_schema["mutated"] = True
    assert "mutated" not in srv.tools["GreetService.Greet"].input_schema
    catalog = srv.tool_catalog()
    catalog[0]["inputSchema"]["mutated"] = True
    assert "mutated" not in srv.tool_catalog()[0]["inputSchema"]

    srv.asgi_app()
    with pytest.raises(RuntimeError, match="registration is frozen"):
        add_greet(srv)
    with pytest.raises(RuntimeError, match="cannot be changed"):
        srv.exclude("*")


def test_register_rejects_duplicate_service():
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    add_greet(srv)

    with pytest.raises(ValueError, match="already registered"):
        add_greet(srv)


def test_connect_grpc_registration_is_atomic():
    class FailingChannel:
        def __init__(self):
            self.calls = 0

        def unary_unary(self, *_args, **_kwargs):
            self.calls += 1
            if self.calls == 2:
                raise RuntimeError("cannot build second RPC")
            return object()

    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    with pytest.raises(RuntimeError, match="cannot build second RPC"):
        srv.connect_grpc(FailingChannel())  # type: ignore[arg-type]

    assert srv.tools == {}
    assert srv._registered_services == {}


def test_register_unknown_service():
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    with pytest.raises(ValueError, match="not found"):
        srv.add_registered_method_handlers("does.not.ExistService", {})


def test_register_rejects_wrong_method_set():
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    with pytest.raises(ValueError, match="missing="):
        srv.add_registered_method_handlers("greet.v1.GreetService", {})


def test_register_rejects_sync_handler():
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    with pytest.raises(TypeError, match="must be"):
        add_greet(srv, SyncGreetServicer())


def test_register_validates_cardinality_and_input_type():
    class Capture:
        def add_generic_rpc_handlers(self, handlers):
            pass

        def add_registered_method_handlers(self, service_name, handlers):
            self.service_name = service_name
            self.handlers = handlers

    captured = Capture()
    greet_pb2_grpc.add_GreetServiceServicer_to_server(GreetServicer(), captured)

    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    handlers = dict(captured.handlers)
    original = handlers["Greet"]
    handlers["Greet"] = grpc.unary_stream_rpc_method_handler(
        GreetServicer().StreamGreet,
        request_deserializer=original.request_deserializer,
        response_serializer=original.response_serializer,
    )
    with pytest.raises(ValueError, match="cardinality"):
        srv.add_registered_method_handlers(captured.service_name, handlers)

    handlers = dict(captured.handlers)
    handlers["Greet"] = grpc.unary_unary_rpc_method_handler(
        GreetServicer().Greet,
        request_deserializer=greet_pb2.GreetResponse.FromString,
        response_serializer=original.response_serializer,
    )
    with pytest.raises(ValueError, match="input type"):
        srv.add_registered_method_handlers(captured.service_name, handlers)


def test_from_bytes():
    with open(DESCRIPTOR_PATH, "rb") as f:
        data = f.read()
    srv = Server.from_bytes(data)
    add_greet(srv)
    assert len(srv.tools) == 3


def test_include_filter():
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    srv.include("greet.v1.GreetService.Greet")
    add_greet(srv)
    assert len(srv.tools) == 1
    assert "GreetService.Greet" in srv.tools


def test_exclude_filter():
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    srv.exclude("*GreetGroup")
    add_greet(srv)
    assert len(srv.tools) == 2
    assert "GreetService.Greet" in srv.tools


def test_include_exclude_combined():
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    srv.include("greet.v1.GreetService.*")
    srv.exclude("*GreetGroup")
    add_greet(srv)
    assert len(srv.tools) == 2
    assert "GreetService.Greet" in srv.tools


def test_projection_filters_freeze_at_first_registration():
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    add_greet(srv)

    with pytest.raises(RuntimeError, match="include filters must be configured before service registration"):
        srv.include("*.Greet")
    with pytest.raises(RuntimeError, match="exclude filters must be configured before service registration"):
        srv.exclude("*.GreetGroup")


def test_include_env_var(monkeypatch):
    monkeypatch.setenv("INVARIANT_INCLUDE", "greet.v1.GreetService.Greet")
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    add_greet(srv)
    assert len(srv.tools) == 1
    assert "GreetService.Greet" in srv.tools


def test_exclude_env_var(monkeypatch):
    monkeypatch.setenv("INVARIANT_EXCLUDE", "*GreetGroup")
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    add_greet(srv)
    assert len(srv.tools) == 2
    assert "GreetService.Greet" in srv.tools


async def test_stop_idempotent():
    """Calling stop() twice (or before serve) is safe."""
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    add_greet(srv)
    await srv.stop()
    await srv.stop()  # second call is a no-op


async def test_serve_projections_cancels_peers_after_first_completion(monkeypatch):
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    add_greet(srv)
    slow_started = asyncio.Event()
    slow_cancelled = asyncio.Event()

    async def quick_projection():
        return

    async def slow_projection(_port):
        slow_started.set()
        try:
            await asyncio.Event().wait()
        finally:
            slow_cancelled.set()

    monkeypatch.setattr(srv, "_serve_mcp", quick_projection)
    monkeypatch.setattr(srv, "_serve_http", slow_projection)

    await srv.serve_projections(mcp=True, http=8080)
    assert slow_started.is_set()
    assert slow_cancelled.is_set()
    assert not hasattr(srv, "serve")


async def test_serve_projections_requires_an_optional_projection():
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    with pytest.raises(ValueError, match="No projections specified"):
        await srv.serve_projections()

    # A rejected no-op serve does not freeze configuration.
    srv.exclude("*")


async def test_invoke_unknown_tool_raises_not_found():
    """invoke() of an unregistered name returns InvariantError(NOT_FOUND)."""
    import grpc as _grpc

    from invariant import InvariantError

    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    add_greet(srv)
    with pytest.raises(InvariantError) as exc:
        await srv.invoke("Nope.DoesNotExist", None)
    assert exc.value.code == _grpc.StatusCode.NOT_FOUND
    await srv.stop()


def test_generated_descriptor_must_match_runtime_image():
    source = descriptor_pb2.FileDescriptorSet.FromString(Path(DESCRIPTOR_PATH).read_bytes())

    wrong_output = descriptor_pb2.FileDescriptorSet()
    wrong_output.CopyFrom(source)
    greet_file = next(file for file in wrong_output.file if file.name == "greet.proto")
    greet_method = next(method for method in greet_file.service[0].method if method.name == "Greet")
    greet_method.output_type = ".greet.v1.GreetGroupResponse"
    srv = Server.from_bytes(wrong_output.SerializeToString())
    with pytest.raises(ValueError, match="generated output type"):
        add_greet(srv)

    stale_message = descriptor_pb2.FileDescriptorSet()
    stale_message.CopyFrom(source)
    greet_file = next(file for file in stale_message.file if file.name == "greet.proto")
    greet_request = next(message for message in greet_file.message_type if message.name == "GreetRequest")
    greet_request.field[0].type = descriptor_pb2.FieldDescriptorProto.TYPE_BYTES
    srv = Server.from_bytes(stale_message.SerializeToString())
    with pytest.raises(ValueError, match=r"does not match descriptor\.binpb"):
        add_greet(srv)


def test_generated_descriptor_rejects_reachable_import_drift():
    package = "invariant.test.imported_drift"
    types_path = "invariant/test/imported_drift_types.proto"
    service_path = "invariant/test/imported_drift_service.proto"

    types_file = descriptor_pb2.FileDescriptorProto(name=types_path, package=package, syntax="proto3")
    imported = types_file.message_type.add(name="ImportedMessage")
    imported.field.add(
        name="value",
        number=1,
        label=descriptor_pb2.FieldDescriptorProto.LABEL_OPTIONAL,
        type=descriptor_pb2.FieldDescriptorProto.TYPE_STRING,
    )

    service_file = descriptor_pb2.FileDescriptorProto(name=service_path, package=package, syntax="proto3")
    service_file.dependency.append(types_path)
    service = service_file.service.add(name="DriftService")
    method = service.method.add(name="Call")
    method.input_type = f".{package}.ImportedMessage"
    method.output_type = f".{package}.ImportedMessage"

    generated_pool = descriptor_pool.Default()
    try:
        generated_pool.FindFileByName(service_path)
    except KeyError:
        generated_pool.Add(types_file)
        generated_pool.Add(service_file)
    generated_message = message_factory.GetMessageClass(
        generated_pool.FindMessageTypeByName(f"{package}.ImportedMessage")
    )

    runtime_types = descriptor_pb2.FileDescriptorProto()
    runtime_types.CopyFrom(types_file)
    runtime_types.message_type[0].field[0].type = descriptor_pb2.FieldDescriptorProto.TYPE_BYTES
    runtime = descriptor_pb2.FileDescriptorSet(file=[service_file, runtime_types])
    srv = Server.from_bytes(runtime.SerializeToString())

    async def call(request, context):
        return request

    handlers = {
        "Call": grpc.unary_unary_rpc_method_handler(
            call,
            request_deserializer=generated_message.FromString,
            response_serializer=generated_message.SerializeToString,
        )
    }
    with pytest.raises(ValueError, match=types_path):
        srv.add_registered_method_handlers(f"{package}.DriftService", handlers)


def test_remote_registration_uses_descriptor_without_generated_imports():
    src = Path(__file__).resolve().parents[1] / "src"
    script = f"""
import asyncio
import sys
import grpc
from invariant import Server

assert "greet_pb2" not in sys.modules

async def main():
    server = Server.from_descriptor({DESCRIPTOR_PATH!r})
    channel = grpc.aio.insecure_channel("localhost:1")
    server.connect_grpc(channel)
    request = server.tools["GreetService.Greet"].new_request()
    assert request.DESCRIPTOR.full_name == "greet.v1.GreetRequest"
    await server.stop()
    await channel.close()

    server = Server.from_descriptor({DESCRIPTOR_PATH!r})
    server.connect_http("http://localhost:1")
    request = server.tools["GreetService.Greet"].new_request()
    assert request.DESCRIPTOR.full_name == "greet.v1.GreetRequest"
    await server.stop()

asyncio.run(main())
"""
    env = os.environ.copy()
    env["PYTHONPATH"] = str(src)
    result = subprocess.run(
        [sys.executable, "-c", script],
        check=False,
        capture_output=True,
        text=True,
        env=env,
        cwd=src.parent,
    )
    assert result.returncode == 0, result.stderr


async def test_cli_and_mcp_supply_standard_projection_contexts():
    peers: list[str] = []

    class ContextServicer(GreetServicer):
        async def Greet(self, request, context):
            peers.append(context.peer())
            assert tuple(context.invocation_metadata()) == ()
            return greet_pb2.GreetResponse(message=request.name)

    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    add_greet(srv, ContextServicer())
    await srv._cli(["GreetService", "Greet", "-r", '{"name":"cli"}'])

    from invariant.projections.mcp import mcp_dispatch

    response = await mcp_dispatch(
        srv,
        {
            "jsonrpc": "2.0",
            "id": 1,
            "method": "tools/call",
            "params": {"name": "GreetService.Greet", "arguments": {"name": "mcp"}},
        },
    )
    assert response is not None
    assert peers == ["invariant:cli", "invariant:mcp"]
    await srv.stop()
