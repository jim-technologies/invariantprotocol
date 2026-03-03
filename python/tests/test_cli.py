"""Test CLI projection."""

import json
import os
import tempfile

import pytest


def test_greet_cli(server):
    result = server._cli(["GreetService", "Greet", "-r", '{"name": "World"}'])
    assert result["message"] == "Hi World"


def test_greet_cli_inline_invalid_json(server):
    with pytest.raises(ValueError, match="Cannot parse inline value as JSON"):
        server._cli(["GreetService", "Greet", "-r", "not json"])


def test_greet_cli_request_yaml_file(server):
    with tempfile.NamedTemporaryFile(mode="w", suffix=".yaml", delete=False) as f:
        f.write("name: World\n")
        f.flush()
        try:
            result = server._cli(["GreetService", "Greet", "-r", f.name])
            assert result["message"] == "Hi World"
        finally:
            os.unlink(f.name)


def test_greet_cli_request_json_file(server):
    with tempfile.NamedTemporaryFile(mode="w", suffix=".json", delete=False) as f:
        json.dump({"name": "Claude"}, f)
        f.flush()
        try:
            result = server._cli(["GreetService", "Greet", "-r", f.name])
            assert result["message"] == "Hi Claude"
        finally:
            os.unlink(f.name)


def test_greet_cli_no_request(server):
    result = server._cli(["GreetService", "Greet"])
    assert "message" in result


def test_greet_cli_unknown_tool(server):
    with pytest.raises(ValueError, match="Unknown service/method"):
        server._cli(["NoSuchService", "Greet"])


def test_greet_cli_no_arguments_shows_help(server):
    result = server._cli([])
    assert "Usage:" in result
    assert "GreetService" in result
    assert "Greet" in result


def test_greet_cli_help_flag(server):
    result = server._cli(["--help"])
    assert "Usage:" in result
    assert "Available methods:" in result


def test_greet_cli_missing_method(server):
    with pytest.raises(ValueError, match="Expected Method"):
        server._cli(["GreetService"])


def test_greet_cli_with_enum_and_tags(server):
    with tempfile.NamedTemporaryFile(mode="w", suffix=".yaml", delete=False) as f:
        f.write("name: World\nmood: MOOD_HAPPY\ntags:\n  lang: en\n")
        f.flush()
        try:
            result = server._cli(["GreetService", "Greet", "-r", f.name])
            assert result["message"] == "Hi World"
            assert result["mood"] == "MOOD_HAPPY"
            assert result["tags"]["lang"] == "en"
        finally:
            os.unlink(f.name)


def test_greet_group_cli(server):
    with tempfile.NamedTemporaryFile(mode="w", suffix=".yaml", delete=False) as f:
        f.write("people:\n  - name: Alice\n    mood: MOOD_HAPPY\n  - name: Bob\n")
        f.flush()
        try:
            result = server._cli(["GreetService", "GreetGroup", "-r", f.name])
            assert result["messages"] == ["Hi Alice", "Hi Bob"]
            assert result["count"] == 2
        finally:
            os.unlink(f.name)


def test_greet_cli_missing_r_value(server):
    with pytest.raises(ValueError, match="Missing value after -r"):
        server._cli(["GreetService", "Greet", "-r"])
