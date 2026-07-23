"""Small shared type boundary for Invariant's HTTP projections and clients."""

from __future__ import annotations

from collections.abc import Callable, Mapping, Sequence
from dataclasses import dataclass

DEFAULT_HTTP_MESSAGE_BYTES = 16 * 1024 * 1024


@dataclass
class OutboundHTTPRequest:
    """Outbound HTTP request metadata for dynamic auth providers."""

    method_path: str
    method: str
    url: str
    body: bytes


HTTPHeaderProvider = Callable[[OutboundHTTPRequest], dict[str, str] | None]
HTTPQueryProvider = Callable[[OutboundHTTPRequest], dict[str, str] | None]


@dataclass
class OutboundHTTPResponse:
    """Raw outbound HTTP response metadata supplied to observers."""

    method_path: str
    status_code: int
    headers: dict[str, str]
    body: bytes
    duration_ms: float
    success: bool
    request: OutboundHTTPRequest


HTTPResponseObserver = Callable[[OutboundHTTPResponse], None]


@dataclass(slots=True)
class HTTPAuth:
    """Per-connection outbound HTTP credentials."""

    header_provider: HTTPHeaderProvider | None = None
    query_provider: HTTPQueryProvider | None = None


@dataclass(slots=True)
class ChannelOptions:
    """Transport options for ``Server.connect_http``."""

    max_receive_message_size: int = DEFAULT_HTTP_MESSAGE_BYTES
    connect_timeout: float = 10.0
    read_timeout: float = 10.0
    write_timeout: float = 10.0
    pool_timeout: float = 10.0
    max_connections: int = 100
    max_keepalive_connections: int = 20
    keepalive_expiry: float = 5.0
    proxy: str | None = None
    http2: bool = False


HTTPMetadataMapper = Callable[[Mapping[str, str]], Sequence[tuple[str, str | bytes]]]


def default_http_metadata_mapper(headers: Mapping[str, str]) -> Sequence[tuple[str, str | bytes]]:
    """Forward only common tracing and correlation headers by default."""
    mapped: list[tuple[str, str]] = []
    values_for = getattr(headers, "values_for", None)
    for key in ("traceparent", "tracestate", "baggage", "x-request-id"):
        if callable(values_for):
            mapped.extend((key, value) for value in values_for(key))
        elif key in headers:
            mapped.append((key, headers[key]))
    return tuple(mapped)
