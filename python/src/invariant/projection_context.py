"""gRPC ServicerContext semantics for non-gRPC projections."""

from __future__ import annotations

import asyncio
import contextlib
import time
from collections.abc import Awaitable, Callable, Iterable, Mapping, Sequence
from typing import Any, NoReturn

import grpc
from google.rpc import status_pb2

from invariant.errors import InvariantError

MetadataValue = str | bytes
Metadata = tuple[tuple[str, MetadataValue], ...]
MetadataInput = grpc.aio.Metadata | Sequence[tuple[str, MetadataValue]]


class ProjectionContext(grpc.aio.ServicerContext[Any, Any]):
    """The standard async gRPC context shape backed by projection state."""

    __slots__ = (
        "_callbacks",
        "_cancelled",
        "_code",
        "_compression",
        "_deadline",
        "_details",
        "_disable_next_compression",
        "_done",
        "_initial_metadata",
        "_initial_sender",
        "_initial_sent",
        "_invocation_metadata",
        "_owner",
        "_peer",
        "_trailing_metadata",
    )

    def __init__(
        self,
        *,
        peer: str,
        invocation_metadata: Sequence[tuple[str, MetadataValue]] = (),
        deadline: float | None = None,
    ) -> None:
        self._peer = peer
        self._invocation_metadata = _normalize_metadata(invocation_metadata)
        self._deadline = deadline
        self._initial_metadata: Metadata = ()
        self._trailing_metadata: Metadata = ()
        self._initial_sent = False
        self._initial_sender: Callable[[], Awaitable[None]] | None = None
        self._code: grpc.StatusCode | None = None
        self._details = ""
        self._compression: grpc.Compression | None = None
        self._disable_next_compression = False
        self._cancelled = False
        self._done = False
        self._callbacks: list[Callable[[ProjectionContext], None]] = []
        self._owner = asyncio.current_task()

    async def read(self) -> Any:
        raise InvariantError(grpc.StatusCode.UNIMPLEMENTED, "client-streaming projections are not supported")

    async def write(self, message: Any) -> None:
        del message
        raise InvariantError(grpc.StatusCode.UNIMPLEMENTED, "client-streaming projections are not supported")

    async def send_initial_metadata(
        self,
        initial_metadata: MetadataInput,
    ) -> None:
        if self._initial_sent:
            raise RuntimeError("grpc: initial metadata has already been sent")
        self._initial_metadata = _normalize_metadata(initial_metadata)
        self._initial_sent = True
        if self._initial_sender is not None:
            await self._initial_sender()

    async def abort(
        self,
        code: grpc.StatusCode,
        details: str = "",
        trailing_metadata: MetadataInput = (),
    ) -> NoReturn:
        if code == grpc.StatusCode.OK:
            raise ValueError("grpc: abort requires a non-OK status code")
        self._code = code
        self._details = details
        self._trailing_metadata = _normalize_metadata(trailing_metadata)
        raise self.status_error()

    def set_trailing_metadata(self, trailing_metadata: MetadataInput) -> None:
        self._trailing_metadata = _normalize_metadata(trailing_metadata)

    def invocation_metadata(self) -> Metadata:
        return self._invocation_metadata

    def set_code(self, code: grpc.StatusCode) -> None:
        self._code = code

    def set_details(self, details: str) -> None:
        self._details = details

    def set_compression(self, compression: grpc.Compression) -> None:
        self._compression = compression

    def disable_next_message_compression(self) -> None:
        self._disable_next_compression = True

    def peer(self) -> str:
        return self._peer

    def peer_identities(self) -> None:
        return None

    def peer_identity_key(self) -> None:
        return None

    def auth_context(self) -> Mapping[str, Iterable[bytes]]:
        return {}

    def time_remaining(self) -> float | None:  # ty: ignore[invalid-method-override] — grpc docs permit None
        if self._deadline is None:
            return None
        return max(0.0, self._deadline - time.monotonic())

    def trailing_metadata(self) -> Metadata:
        return self._trailing_metadata

    def initial_metadata(self) -> Metadata:
        return self._initial_metadata

    def code(self) -> grpc.StatusCode | None:
        return self._code

    def details(self) -> str:
        return self._details

    def add_done_callback(self, callback: Callable[[Any], None]) -> None:
        if self.done():
            callback(self)
            return
        self._callbacks.append(callback)

    def cancelled(self) -> bool:
        return self._cancelled or bool(self._owner is not None and self._owner.cancelling())

    def done(self) -> bool:
        return self._done or bool(self._owner is not None and self._owner.done())

    def set_initial_sender(self, sender: Callable[[], Awaitable[None]]) -> None:
        self._initial_sender = sender

    def mark_initial_sent(self) -> None:
        self._initial_sent = True

    def mark_cancelled(self) -> None:
        self._cancelled = True

    def raise_for_status(self) -> None:
        if self._code not in (None, grpc.StatusCode.OK):
            raise self.status_error()

    def status_error(self) -> InvariantError:
        code = self._code or grpc.StatusCode.UNKNOWN
        message = self._details
        rich_details: list[Any] = []
        for key, value in self._trailing_metadata:
            if key != "grpc-status-details-bin" or not isinstance(value, bytes):
                continue
            status = status_pb2.Status()
            with contextlib.suppress(Exception):
                status.ParseFromString(value)
                if status.message:
                    message = status.message
                rich_details.extend(status.details)
        return InvariantError(code, message, rich_details)

    def finish(self, *, cancelled: bool = False) -> None:
        if self._done:
            return
        self._cancelled = self._cancelled or cancelled
        self._done = True
        callbacks, self._callbacks = self._callbacks, []
        for callback in callbacks:
            with contextlib.suppress(Exception):
                callback(self)


def _normalize_metadata(metadata: MetadataInput) -> Metadata:
    if not metadata:
        return ()
    return tuple((str(key).lower(), value) for key, value in metadata)


__all__ = ["Metadata", "MetadataValue", "ProjectionContext"]
