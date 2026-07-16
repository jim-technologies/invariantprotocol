"""Test CLI projection."""

import asyncio
import json
import os
import tempfile

import greet_pb2
import grpc
import pytest
from conftest import DESCRIPTOR_PATH, register_greet

from invariant import InvariantError, Server


async def test_greet_cli(server):
    result = await server._cli(["GreetService", "Greet", "-r", '{"name": "World"}'])
    assert result["message"] == "Hi World"


async def test_greet_cli_inline_invalid_json(server):
    with pytest.raises(InvariantError, match="Cannot parse inline value as JSON") as exc:
        await server._cli(["GreetService", "Greet", "-r", "not json"])
    assert exc.value.code == grpc.StatusCode.INVALID_ARGUMENT


async def test_greet_cli_unsupported_extension(server):
    with tempfile.NamedTemporaryFile(mode="w", suffix=".yaml", delete=False) as f:
        f.write("name: World\n")
        f.flush()
        try:
            with pytest.raises(InvariantError, match="unsupported request file extension"):
                await server._cli(["GreetService", "Greet", "-r", f.name])
        finally:
            os.unlink(f.name)


async def test_greet_cli_request_json_file(server):
    with tempfile.NamedTemporaryFile(mode="w", suffix=".json", delete=False) as f:
        json.dump({"name": "Claude"}, f)
        f.flush()
        try:
            result = await server._cli(["GreetService", "Greet", "-r", f.name])
            assert result["message"] == "Hi Claude"
        finally:
            os.unlink(f.name)


@pytest.mark.parametrize("extension", [".binpb", ".pb"])
async def test_greet_cli_request_binary_file_preserves_unknown_fields(server, tmp_path, extension):
    request = greet_pb2.GreetRequest(name="BinaryFile").SerializeToString()
    request += b"\x9a\x06\x03new"
    path = tmp_path / f"request{extension}"
    path.write_bytes(request)

    result = await server._cli(["GreetService", "Greet", "-r", str(path)])
    assert result["message"] == "Hi BinaryFile"


async def test_greet_cli_malformed_request_files_are_invalid_argument(server, tmp_path):
    malformed_json = tmp_path / "request.json"
    malformed_json.write_text("{", encoding="utf-8")
    malformed_binary = tmp_path / "request.binpb"
    malformed_binary.write_bytes(b"\xff")

    for path in (malformed_json, malformed_binary):
        with pytest.raises(InvariantError) as exc:
            await server._cli(["GreetService", "Greet", "-r", str(path)])
        assert exc.value.code == grpc.StatusCode.INVALID_ARGUMENT


async def test_greet_cli_no_request(server):
    result = await server._cli(["GreetService", "Greet"])
    assert "message" in result


async def test_greet_cli_unknown_tool(server):
    with pytest.raises(ValueError, match="Unknown service/method"):
        await server._cli(["NoSuchService", "Greet"])


async def test_greet_cli_no_arguments_shows_help(server):
    result = await server._cli([])
    assert "Usage:" in result
    assert "GreetService" in result
    assert "Greet" in result


async def test_greet_cli_help_flag(server):
    result = await server._cli(["--help"])
    assert "Usage:" in result
    assert "Available methods:" in result


async def test_greet_cli_missing_method(server):
    with pytest.raises(ValueError, match="Expected Method"):
        await server._cli(["GreetService"])


async def test_greet_cli_with_enum_and_tags(server):
    with tempfile.NamedTemporaryFile(mode="w", suffix=".json", delete=False) as f:
        json.dump({"name": "World", "mood": "MOOD_HAPPY", "tags": {"lang": "en"}}, f)
        f.flush()
        try:
            result = await server._cli(["GreetService", "Greet", "-r", f.name])
            assert result["message"] == "Hi World"
            assert result["mood"] == "MOOD_HAPPY"
            assert result["tags"]["lang"] == "en"
        finally:
            os.unlink(f.name)


async def test_greet_group_cli(server):
    with tempfile.NamedTemporaryFile(mode="w", suffix=".json", delete=False) as f:
        json.dump(
            {"people": [{"name": "Alice", "mood": "MOOD_HAPPY"}, {"name": "Bob"}]},
            f,
        )
        f.flush()
        try:
            result = await server._cli(["GreetService", "GreetGroup", "-r", f.name])
            assert result["messages"] == ["Hi Alice", "Hi Bob"]
            assert result["count"] == 2
        finally:
            os.unlink(f.name)


async def test_greet_cli_missing_r_value(server):
    with pytest.raises(ValueError, match="Missing value after -r"):
        await server._cli(["GreetService", "Greet", "-r"])


@pytest.mark.parametrize(
    "args",
    [
        ["GreetService", "Greet", "extra"],
        ["GreetService", "Greet", "-r", "{}", "extra"],
    ],
)
async def test_greet_cli_rejects_unexpected_arguments(server, args):
    with pytest.raises(ValueError, match="Unexpected argument"):
        await server._cli(args)


async def test_greet_cli_unknown_field_rejected(server):
    with pytest.raises(InvariantError, match='field named "extra"') as exc:
        await server._cli(["GreetService", "Greet", "-r", '{"name": "World", "extra": "x"}'])
    assert exc.value.code == grpc.StatusCode.INVALID_ARGUMENT
    payload = exc.value.to_payload()
    assert payload["code"] == "invalid_argument"


async def test_greet_cli_preserves_status():
    class StatusServicer:
        async def Greet(self, request, context):
            del request
            await context.abort(grpc.StatusCode.FAILED_PRECONDITION, "cli status")

    server = Server.from_descriptor(DESCRIPTOR_PATH)
    register_greet(server, StatusServicer())
    try:
        with pytest.raises(InvariantError) as exc:
            await server._cli(["GreetService", "Greet", "-r", '{"name":"status"}'])
        assert exc.value.code == grpc.StatusCode.FAILED_PRECONDITION
        assert exc.value.message == "cli status"
    finally:
        await server.stop()


async def test_greet_cli_task_cancellation_reaches_handler():
    started = asyncio.Event()
    cancelled = asyncio.Event()

    class BlockingServicer:
        async def Greet(self, request, context):
            del request
            started.set()
            try:
                await asyncio.Future()
            finally:
                assert context.cancelled()
                cancelled.set()

    server = Server.from_descriptor(DESCRIPTOR_PATH)
    register_greet(server, BlockingServicer())
    task = asyncio.create_task(server._cli(["GreetService", "Greet", "-r", '{"name":"cancel"}']))
    try:
        await asyncio.wait_for(started.wait(), timeout=1)
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task
        await asyncio.wait_for(cancelled.wait(), timeout=1)
    finally:
        if not task.done():
            task.cancel()
        await server.stop()
