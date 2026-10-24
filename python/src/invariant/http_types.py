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
    """Raw response metadata; ``url`` is the final response URL.

    ``request.url`` retains the original outbound request URL. An empty
    ``url`` means a caller constructed metadata without a response URL.
    """

    method_path: str
    status_code: int
    headers: dict[str, str]
    body: bytes
    duration_ms: float
    success: bool
    request: OutboundHTTPRequest
    url: str = ""


HTTPResponseObserver = Callable[[OutboundHTTPResponse], None]


@dataclass(slots=True)
class HTTPAuth:
    """Per-connection outbound HTTP credentials.

    A header provider may add any header except ``Accept`` and
    ``Content-Type``, which belong to the JSON codec and are ignored.
    """

    header_provider: HTTPHeaderProvider | None = None
    query_provider: HTTPQueryProvider | None = None


@dataclass(slots=True)
class ChannelOptions:
    """Transport options for ``Server.connect_http``.

    ``trust_env`` controls outbound ``INVARIANT_HTTP_HEADER_*`` headers and
    HTTPX environment discovery, including proxy and TLS certificate settings.
    Explicit auth providers and explicit ``proxy`` remain authoritative options.

    ``follow_redirects=None`` follows redirects for ``google.api.HttpBody``
    responses and refuses them for other response types. An explicit boolean
    overrides that policy for every method on the connection.
    """

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
    trust_env: bool = True
    identity_response_only: bool = False
    """Request identity encoding and refuse compressed bodies before reading.

    Raw identity bytes are capped before retention, including errors and
    redirects. This profile disables ambient header and HTTPX environment
    discovery, regardless of trust_env; explicit auth/proxy remain available.
    False preserves ordinary response decoding and configuration.
    """
    before_http_attempt: Callable[[], float | None] | None = None
    """Admit each physical send, including retries and redirect hops.

    A callback may refuse before network IO. A positive return bounds that
    send's elapsed seconds; None retains the ordinary phase timeouts.
    Admission is not refunded on failure or cancellation. The caller owns
    shared counters/deadlines; the callback runs synchronously before each
    send without an intervening await.
    """
    follow_redirects: bool | None = None


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
