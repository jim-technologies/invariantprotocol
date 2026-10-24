"""Owned HTTP peers for explicit environment isolation and ordinary defaults."""

from __future__ import annotations

import asyncio
import os
from contextlib import asynccontextmanager, suppress

import pytest
from google.protobuf import descriptor_pool, empty_pb2

from invariant.http_client import HTTPClientBinding, HTTPConnection, HTTPDynamicHandler
from invariant.http_types import ChannelOptions, HTTPAuth


@pytest.fixture(autouse=True)
def synthetic_environment_only(monkeypatch):
    for name in tuple(os.environ):
        if name.startswith("INVARIANT_HTTP_HEADER_") or name.lower() in {
            "http_proxy",
            "https_proxy",
            "all_proxy",
            "no_proxy",
            "ssl_cert_file",
            "ssl_cert_dir",
        }:
            monkeypatch.delenv(name, raising=False)


@asynccontextmanager
async def peer():
    requests = []
    tasks = set()

    async def serve(reader, writer):
        current = asyncio.current_task()
        tasks.add(current)
        try:
            raw = await asyncio.wait_for(reader.readuntil(b"\r\n\r\n"), 2)
            first, *lines = raw.decode("ascii").split("\r\n")
            headers = {name.lower(): value for name, value in (line.split(": ", 1) for line in lines if line)}
            length = int(headers.get("content-length", "0"))
            assert 0 <= length <= 1024
            await asyncio.wait_for(reader.readexactly(length), 2)
            requests.append((first, headers))
            writer.write(
                b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{}"
            )
            await writer.drain()
        finally:
            writer.close()
            with suppress(ConnectionError):
                await writer.wait_closed()
            tasks.discard(current)

    server = await asyncio.start_server(serve, "127.0.0.1", 0, limit=8192)
    [socket] = server.sockets
    try:
        yield f"http://127.0.0.1:{socket.getsockname()[1]}", requests
    finally:
        server.close()
        await server.wait_closed()
        pending = list(tasks)
        for task in pending:
            task.cancel()
        await asyncio.gather(*pending, return_exceptions=True)


def client(origin, options=None):
    pool = descriptor_pool.DescriptorPool()
    pool.AddSerializedFile(empty_pb2.DESCRIPTOR.serialized_pb)
    connection = HTTPConnection(
        base_url=origin,
        options=options,
        auth=HTTPAuth(header_provider=lambda _: {"Authorization": "Bearer selected-fixture"}),
    )
    call = HTTPDynamicHandler(
        connection=connection,
        binding=HTTPClientBinding.new("POST", "/fixture", "*"),
        output_type=empty_pb2.Empty.DESCRIPTOR.full_name,
        input_type=empty_pb2.Empty.DESCRIPTOR.full_name,
        method_path="/fixture.Service/Inspect",
        pool=pool,
    )
    return connection, call


def proxy_environment(monkeypatch, origin):
    for name in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"):
        monkeypatch.setenv(name, origin)
    for name in ("NO_PROXY", "no_proxy"):
        monkeypatch.setenv(name, "")
    for name in ("SSL_CERT_FILE", "SSL_CERT_DIR"):
        monkeypatch.delenv(name, raising=False)


async def test_strict_channels_ignore_headers_and_proxy_while_ordinary_retains_both(monkeypatch):
    monkeypatch.setenv("INVARIANT_HTTP_HEADER_X_UNRELATED", "synthetic-unrelated")
    monkeypatch.setenv("INVARIANT_HTTP_HEADER_AUTHORIZATION", "Bearer ambient-must-not-win")
    async with peer() as (target, direct), peer() as (proxy, proxied):
        proxy_environment(monkeypatch, proxy)
        first, first_call = client(target, ChannelOptions(trust_env=False))
        ordinary, ordinary_call = client(target)
        second, second_call = client(target, ChannelOptions(trust_env=False))
        try:
            for call in (first_call, ordinary_call, second_call):
                response = await call(empty_pb2.Empty(), None)
                assert response.DESCRIPTOR.full_name == empty_pb2.Empty.DESCRIPTOR.full_name
                assert response.SerializeToString() == b""
        finally:
            await asyncio.gather(first.aclose(), ordinary.aclose(), second.aclose())
    assert len(direct) == 2
    assert len(proxied) == 1
    for first_line, headers in direct:
        assert first_line == "POST /fixture HTTP/1.1"
        assert "x-unrelated" not in headers
        assert headers["authorization"] == "Bearer selected-fixture"
    assert proxied[0][0] == f"POST {target}/fixture HTTP/1.1"
    assert proxied[0][1]["x-unrelated"] == "synthetic-unrelated"
    assert proxied[0][1]["authorization"] == "Bearer selected-fixture"
    assert ChannelOptions().trust_env is True


async def test_explicit_proxy_still_works_when_ambient_discovery_is_disabled(monkeypatch):
    async with peer() as (target, direct), peer() as (proxy, proxied):
        proxy_environment(monkeypatch, "http://127.0.0.1:1")
        connection, call = client(target, ChannelOptions(trust_env=False, proxy=proxy))
        try:
            await call(empty_pb2.Empty(), None)
        finally:
            await connection.aclose()
    assert not direct
    assert len(proxied) == 1
    assert proxied[0][0] == f"POST {target}/fixture HTTP/1.1"


async def test_tls_environment_is_ignored_only_by_the_selected_channel(tmp_path, monkeypatch):
    # This intentionally absent synthetic path is never a real certificate.
    monkeypatch.setenv("SSL_CERT_FILE", str(tmp_path / "synthetic-absent-ca.pem"))
    for name in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"):
        monkeypatch.delenv(name, raising=False)
    async with peer() as (target, direct):
        strict, call = client(target, ChannelOptions(trust_env=False))
        ordinary, _ = client(target)
        try:
            await call(empty_pb2.Empty(), None)
            with pytest.raises(FileNotFoundError):
                _ = ordinary.client
        finally:
            await strict.aclose()
            await ordinary.aclose()
    assert len(direct) == 1


async def test_strict_construction_never_calls_ambient_header_discovery(monkeypatch):
    from invariant import http_client

    def forbidden():
        raise AssertionError("strict channel discovered ambient headers")

    monkeypatch.setattr(http_client, "_outbound_http_headers_from_env", forbidden)
    async with peer() as (target, requests):
        connection, call = client(target, ChannelOptions(trust_env=False))
        try:
            await call(empty_pb2.Empty(), None)
        finally:
            await connection.aclose()
    assert len(requests) == 1
