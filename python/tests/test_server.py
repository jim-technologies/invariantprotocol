"""Test Server registration."""

import os

import pytest

from invariant import Server

DESCRIPTOR_PATH = os.path.join(os.path.dirname(__file__), "proto", "descriptor.binpb")


class GreetServicer:
    def Greet(self, request, context):
        pass

    def GreetGroup(self, request, context):
        pass


class NoMethodServicer:
    pass


def test_register_creates_tools():
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    srv.register(GreetServicer())
    assert len(srv.tools) == 2


def test_tool_names():
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    srv.register(GreetServicer())
    assert "GreetService.Greet" in srv.tools
    assert "GreetService.GreetGroup" in srv.tools


def test_tool_description():
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    srv.register(GreetServicer())
    tool = srv.tools["GreetService.Greet"]
    assert "greet a person" in tool.description.lower()


def test_tool_input_schema():
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    srv.register(GreetServicer())
    tool = srv.tools["GreetService.Greet"]
    assert tool.input_schema["type"] == "object"
    assert "name" in tool.input_schema["properties"]


def test_register_explicit_service_name():
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    srv.register(GreetServicer(), service_name="greet.v1.GreetService")
    assert len(srv.tools) == 2


def test_register_unknown_service():
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    with pytest.raises(ValueError, match="not found"):
        srv.register(GreetServicer(), service_name="does.not.ExistService")


def test_register_no_matching_service():
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    with pytest.raises(ValueError, match="No matching service"):
        srv.register(NoMethodServicer())


def test_from_bytes():
    with open(DESCRIPTOR_PATH, "rb") as f:
        data = f.read()
    srv = Server.from_bytes(data)
    srv.register(GreetServicer())
    assert len(srv.tools) == 2
