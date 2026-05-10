"""Test CLI projection."""

import json
import os
import tempfile

import grpc
import pytest

from invariant import InvariantError


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


async def test_greet_cli_unknown_field_rejected(server):
    with pytest.raises(InvariantError, match='field named "extra"') as exc:
        await server._cli(["GreetService", "Greet", "-r", '{"name": "World", "extra": "x"}'])
    assert exc.value.code == grpc.StatusCode.INVALID_ARGUMENT
    payload = exc.value.to_payload()
    assert payload["code"] == "invalid_argument"
