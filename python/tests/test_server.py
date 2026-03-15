"""Test Server registration and include/exclude filtering."""

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


def test_include_filter():
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    srv.include("greet.v1.GreetService.Greet")
    srv.register(GreetServicer())
    assert len(srv.tools) == 1
    assert "GreetService.Greet" in srv.tools


def test_exclude_filter():
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    srv.exclude("*GreetGroup")
    srv.register(GreetServicer())
    assert len(srv.tools) == 1
    assert "GreetService.Greet" in srv.tools


def test_include_exclude_combined():
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    srv.include("greet.v1.GreetService.*")
    srv.exclude("*GreetGroup")
    srv.register(GreetServicer())
    assert len(srv.tools) == 1
    assert "GreetService.Greet" in srv.tools


def test_include_env_var(monkeypatch):
    monkeypatch.setenv("INVARIANT_INCLUDE", "greet.v1.GreetService.Greet")
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    srv.register(GreetServicer())
    assert len(srv.tools) == 1
    assert "GreetService.Greet" in srv.tools


def test_exclude_env_var(monkeypatch):
    monkeypatch.setenv("INVARIANT_EXCLUDE", "*GreetGroup")
    srv = Server.from_descriptor(DESCRIPTOR_PATH)
    srv.register(GreetServicer())
    assert len(srv.tools) == 1
    assert "GreetService.Greet" in srv.tools
