"""Owned loopback tests for identity receive and physical send admission."""

from __future__ import annotations

import asyncio
import gzip
import os
from contextlib import asynccontextmanager, suppress

import grpc
import pytest
from google.api import httpbody_pb2
from google.protobuf import any_pb2, descriptor_pool, empty_pb2

from invariant import http_client
from invariant.errors import InvariantError
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
async def owned_http(provider):
    tasks = set()

    async def connection(reader, writer):
        current = asyncio.current_task()
        tasks.add(current)
        try:
            request = await asyncio.wait_for(reader.readuntil(b"\r\n\r\n"), 2)
            await provider(request, reader, writer)
        except asyncio.IncompleteReadError, ConnectionError:
            pass
        finally:
            writer.close()
            with suppress(ConnectionError):
                await writer.wait_closed()
            tasks.discard(current)

    server = await asyncio.start_server(connection, "127.0.0.1", 0)
    [socket] = server.sockets
    try:
        yield f"http://127.0.0.1:{socket.getsockname()[1]}"
    finally:
        server.close()
        await server.wait_closed()
        pending = list(tasks)
        for task in pending:
            task.cancel()
        await asyncio.gather(*pending, return_exceptions=True)


def handler(origin, options, *, retry=False, pattern="/start", auth=None):
    configuration = (
        None
        if not retry
        else {
            "method_config": [
                {
                    "name": [{}],
                    "retry_policy": {
                        "max_attempts": 2,
                        "initial_backoff": "0s",
                        "max_backoff": "0s",
                        "backoff_multiplier": 1.0,
                        "retryable_status_codes": ["UNAVAILABLE", "DEADLINE_EXCEEDED"],
                    },
                }
            ],
        }
    )
    pool = descriptor_pool.DescriptorPool()
    for descriptor in (empty_pb2.DESCRIPTOR, any_pb2.DESCRIPTOR, httpbody_pb2.DESCRIPTOR):
        pool.AddSerializedFile(descriptor.serialized_pb)
    connection = HTTPConnection(base_url=origin, options=options, service_config=configuration, auth=auth)
    tool = HTTPDynamicHandler(
        connection=connection,
        binding=HTTPClientBinding.new("GET", pattern, ""),
        output_type=httpbody_pb2.HttpBody.DESCRIPTOR.full_name,
        input_type=empty_pb2.Empty.DESCRIPTOR.full_name,
        method_path="/fixture.Raw/Get",
        pool=pool,
    )
    return connection, tool


@pytest.mark.parametrize("status", [200, 503])
@pytest.mark.parametrize("encoding", ["gzip", "deflate", "br", "zstd", "gzip, identity", "unknown"])
async def test_compression_is_refused_before_body_even_on_retryable_error(status, encoding):
    closed = asyncio.Event()
    calls = []

    async def provider(request, reader, writer):
        calls.append(request)
        writer.write(f"HTTP/1.1 {status} Fixture\r\nContent-Encoding: {encoding}\r\nContent-Length: 1\r\n\r\n".encode())
        await writer.drain()
        assert await reader.read() == b""
        closed.set()

    attempts = []

    def admit():
        attempts.append(1)
        return 1.0

    async with owned_http(provider) as origin:
        connection, tool = handler(
            origin,
            ChannelOptions(
                max_receive_message_size=32,
                identity_response_only=True,
                before_http_attempt=admit,
            ),
            retry=True,
        )
        try:
            with pytest.raises(InvariantError) as caught:
                await tool(empty_pb2.Empty(), None)
            assert caught.value.code == grpc.StatusCode.FAILED_PRECONDITION
            await asyncio.wait_for(closed.wait(), 1)
        finally:
            await connection.aclose()
    assert len(calls) == len(attempts) == 1
    assert b"accept-encoding: identity\r\n" in calls[0].lower()


@pytest.mark.parametrize("status", [200, 503])
async def test_chunked_raw_limit_precedes_httpbody_or_error_materialization(status):
    calls = []

    async def provider(request, _reader, writer):
        calls.append(request)
        writer.write(f"HTTP/1.1 {status} Fixture\r\nTransfer-Encoding: chunked\r\n\r\n".encode())
        writer.write(b"8\r\n12345678\r\n8\r\nabcdefgh\r\n1\r\nx\r\n0\r\n\r\n")
        await writer.drain()

    async with owned_http(provider) as origin:
        connection, tool = handler(
            origin,
            ChannelOptions(
                max_receive_message_size=16,
                identity_response_only=True,
                before_http_attempt=lambda: 1.0,
            ),
            retry=True,
        )
        try:
            with pytest.raises(InvariantError) as caught:
                await tool(empty_pb2.Empty(), None)
            assert caught.value.code == grpc.StatusCode.RESOURCE_EXHAUSTED
        finally:
            await connection.aclose()
    assert len(calls) == 1


@pytest.mark.parametrize("budget", [3, 4])
async def test_every_redirect_and_retry_requires_its_own_physical_admission(budget):
    calls, admissions = [], []

    def admit():
        if len(admissions) == budget:
            raise RuntimeError("synthetic actual-attempt allowance exhausted")
        admissions.append(1)
        return 1.0

    async def provider(request, _reader, writer):
        target = request.split(b" ", 2)[1]
        calls.append(target)
        if target == b"/start":
            writer.write(b"HTTP/1.1 307 Fixture\r\nLocation: /target\r\nContent-Length: 1\r\n\r\nr")
        elif calls.count(b"/target") == 1:
            writer.write(b"HTTP/1.1 503 Fixture\r\nContent-Length: 1\r\n\r\ne")
        else:
            writer.write(b"HTTP/1.1 200 Fixture\r\nContent-Length: 2\r\n\r\nok")
        await writer.drain()

    async with owned_http(provider) as origin:
        connection, tool = handler(
            origin,
            ChannelOptions(
                max_receive_message_size=16,
                identity_response_only=True,
                before_http_attempt=admit,
            ),
            retry=True,
        )
        try:
            if budget == 3:
                with pytest.raises(RuntimeError, match="allowance exhausted"):
                    await tool(empty_pb2.Empty(), None)
            else:
                assert (await tool(empty_pb2.Empty(), None)).data == b"ok"
        finally:
            await connection.aclose()
    assert calls == [b"/start", b"/target", b"/start", b"/target"][:budget]
    assert len(admissions) == len(calls) == budget


@pytest.mark.parametrize("body_started", [False, True])
async def test_admitted_deadline_covers_stalled_headers_and_body(body_started):
    calls, admissions = [], []

    async def provider(request, reader, writer):
        calls.append(request)
        if body_started:
            writer.write(b"HTTP/1.1 200 Fixture\r\nContent-Length: 10\r\n\r\n1")
            await writer.drain()
        await reader.read()

    def admit():
        admissions.append(1)
        return 0.05

    async with owned_http(provider) as origin:
        connection, tool = handler(
            origin,
            ChannelOptions(
                max_receive_message_size=16,
                identity_response_only=True,
                before_http_attempt=admit,
            ),
            retry=True,
        )
        try:
            with pytest.raises(InvariantError) as caught:
                await tool(empty_pb2.Empty(), None)
            assert caught.value.code == grpc.StatusCode.DEADLINE_EXCEEDED
        finally:
            await connection.aclose()
    assert len(admissions) == len(calls) == 2


async def test_cancellation_closes_owned_response_without_a_retry():
    entered, closed = asyncio.Event(), asyncio.Event()
    admissions = []

    async def provider(_request, reader, writer):
        writer.write(b"HTTP/1.1 200 Fixture\r\nContent-Length: 10\r\n\r\n1")
        await writer.drain()
        entered.set()
        assert await reader.read() == b""
        closed.set()

    def admit():
        admissions.append(1)
        return 1.0

    async with owned_http(provider) as origin:
        connection, tool = handler(
            origin,
            ChannelOptions(
                max_receive_message_size=16,
                identity_response_only=True,
                before_http_attempt=admit,
            ),
            retry=True,
        )
        try:
            task = asyncio.create_task(tool(empty_pb2.Empty(), None))
            await asyncio.wait_for(entered.wait(), 1)
            task.cancel()
            with pytest.raises(asyncio.CancelledError):
                await task
            await asyncio.wait_for(closed.wait(), 1)
            assert task.done()
            assert len(admissions) == 1
        finally:
            await connection.aclose()


@pytest.mark.parametrize("mode", ["ordinary", "isolated", "callback"])
async def test_ordinary_gzip_and_explicit_environment_selection_are_preserved(monkeypatch, mode):
    key = http_client._OUTBOUND_HTTP_HEADER_ENV_PREFIX + "X_FIXTURE"
    monkeypatch.setenv(key, "synthetic-fixture-header")
    compressed = gzip.compress(b"ordinary")
    admissions = []

    def admit():
        admissions.append(1)
        return 1.0

    async def provider(request, _reader, writer):
        present = b"x-fixture: synthetic-fixture-header\r\n" in request.lower()
        assert present is (mode != "isolated")
        writer.write(
            f"HTTP/1.1 200 OK\r\nContent-Encoding: gzip\r\nContent-Length: {len(compressed)}\r\n\r\n".encode()
            + compressed
        )
        await writer.drain()

    async with owned_http(provider) as origin:
        options = ChannelOptions(
            trust_env=mode != "isolated",
            before_http_attempt=admit if mode == "callback" else None,
        )
        connection, tool = handler(origin, options)
        try:
            assert (await tool(empty_pb2.Empty(), None)).data == b"ordinary"
        finally:
            await connection.aclose()
        strict = HTTPConnection(base_url=origin, options=ChannelOptions(identity_response_only=True))
        try:
            assert strict.headers == {"User-Agent": http_client._DEFAULT_USER_AGENT}
        finally:
            await strict.aclose()
    assert len(admissions) == int(mode == "callback")


async def test_channel_limits_and_callbacks_are_isolated_from_other_and_ordinary_clients():
    calls, first_admissions, second_admissions = [], [], []

    def admit(values, limit):
        if len(values) == limit:
            raise RuntimeError("synthetic budget exhausted")
        values.append(1)
        return 1.0

    async def provider(request, _reader, writer):
        strict = b"accept-encoding: identity\r\n" in request.lower()
        calls.append(strict)
        body = b"ok" if strict else gzip.compress(b"ok")
        encoding = b"" if strict else b"Content-Encoding: gzip\r\n"
        writer.write(f"HTTP/1.1 200 OK\r\nContent-Length: {len(body)}\r\n".encode() + encoding + b"\r\n" + body)
        await writer.drain()

    async with owned_http(provider) as origin:
        first, first_tool = handler(
            origin,
            ChannelOptions(
                identity_response_only=True,
                max_receive_message_size=2,
                before_http_attempt=lambda: admit(first_admissions, 1),
            ),
        )
        second, second_tool = handler(
            origin,
            ChannelOptions(
                identity_response_only=True,
                max_receive_message_size=8,
                before_http_attempt=lambda: admit(second_admissions, 2),
            ),
        )
        ordinary, ordinary_tool = handler(origin, ChannelOptions())
        try:
            results = await asyncio.gather(
                *(tool(empty_pb2.Empty(), None) for tool in (first_tool, second_tool, ordinary_tool))
            )
            assert [result.data for result in results] == [b"ok", b"ok", b"ok"]
            assert (await second_tool(empty_pb2.Empty(), None)).data == b"ok"
            with pytest.raises(RuntimeError, match="budget exhausted"):
                await first_tool(empty_pb2.Empty(), None)
            assert (await ordinary_tool(empty_pb2.Empty(), None)).data == b"ok"
            assert len(first_admissions) == 1
            assert len(second_admissions) == 2
        finally:
            await asyncio.gather(*(connection.aclose() for connection in (first, second, ordinary)))
    assert calls.count(True) == 3
    assert calls.count(False) == 2


async def test_missing_path_selector_refuses_before_admission_or_network():
    calls, admissions = [], []

    async def provider(request, _reader, _writer):
        calls.append(request)

    def admit():
        admissions.append(1)
        return 1.0

    async with owned_http(provider) as origin:
        connection, tool = handler(
            origin,
            ChannelOptions(
                identity_response_only=True,
                before_http_attempt=admit,
            ),
            pattern="/{station}",
        )
        try:
            with pytest.raises(InvariantError) as caught:
                await tool(empty_pb2.Empty(), None)
            assert caught.value.code == grpc.StatusCode.INVALID_ARGUMENT
        finally:
            await connection.aclose()
    assert calls == admissions == []


@pytest.mark.parametrize("status", [200, 503])
async def test_declared_oversize_is_refused_before_waiting_for_body(status):
    calls = []
    closed = asyncio.Event()

    async def provider(request, reader, writer):
        calls.append(request)
        writer.write(f"HTTP/1.1 {status} Fixture\r\nContent-Length: 17\r\n\r\n".encode())
        await writer.drain()
        assert await reader.read() == b""
        closed.set()

    async with owned_http(provider) as origin:
        connection, tool = handler(
            origin,
            ChannelOptions(
                identity_response_only=True,
                max_receive_message_size=16,
                before_http_attempt=lambda: 1.0,
            ),
            retry=True,
        )
        try:
            with pytest.raises(InvariantError) as caught:
                await tool(empty_pb2.Empty(), None)
            assert caught.value.code == grpc.StatusCode.RESOURCE_EXHAUSTED
            await asyncio.wait_for(closed.wait(), 1)
        finally:
            await connection.aclose()
    assert len(calls) == 1


async def test_identity_profile_ignores_ambient_discovery_but_retains_explicit_auth(
    tmp_path,
    monkeypatch,
):
    for name in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"):
        monkeypatch.setenv(name, "http://127.0.0.1:1")
    monkeypatch.setenv("NO_PROXY", "")
    monkeypatch.setenv("SSL_CERT_FILE", str(tmp_path / "synthetic-absent-ca.pem"))
    monkeypatch.setenv("INVARIANT_HTTP_HEADER_AUTHORIZATION", "Bearer ambient-must-not-win")
    calls = []

    def forbidden():
        raise AssertionError("identity profile discovered ambient headers")

    monkeypatch.setattr(http_client, "_outbound_http_headers_from_env", forbidden)

    async def provider(request, _reader, writer):
        calls.append(request)
        headers = request.lower()
        assert b"authorization: bearer selected-fixture\r\n" in headers
        assert b"ambient-must-not-win" not in headers
        assert b"accept-encoding: identity\r\n" in headers
        assert b"user-agent: " in headers
        writer.write(b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
        await writer.drain()

    async with owned_http(provider) as origin:
        connection, tool = handler(
            origin,
            ChannelOptions(identity_response_only=True, trust_env=True),
            auth=HTTPAuth(header_provider=lambda _: {"Authorization": "Bearer selected-fixture"}),
        )
        try:
            assert (await tool(empty_pb2.Empty(), None)).data == b"ok"
        finally:
            await connection.aclose()
    assert len(calls) == 1
