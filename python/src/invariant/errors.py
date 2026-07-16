"""Shared error helpers for projecting gRPC-style errors to all protocols."""

from __future__ import annotations

import base64
import contextlib
import re
from collections.abc import Iterable, Sequence
from typing import Any

import grpc
from google.protobuf import any_pb2, json_format, message
from google.rpc import error_details_pb2 as _error_details_pb2  # noqa: F401  (registers detail descriptors)
from google.rpc import status_pb2

_NO_FIELD_PATTERNS = [
    re.compile(r'no field named "([^"]+)"', re.IGNORECASE),
    re.compile(r"unknown field[: ]+\"?([^\" ]+)\"?", re.IGNORECASE),
]

_HTTP_STATUS_BY_CODE = {
    grpc.StatusCode.OK: 200,
    grpc.StatusCode.CANCELLED: 499,
    grpc.StatusCode.UNKNOWN: 500,
    grpc.StatusCode.INVALID_ARGUMENT: 400,
    grpc.StatusCode.DEADLINE_EXCEEDED: 504,
    grpc.StatusCode.NOT_FOUND: 404,
    grpc.StatusCode.ALREADY_EXISTS: 409,
    grpc.StatusCode.PERMISSION_DENIED: 403,
    grpc.StatusCode.RESOURCE_EXHAUSTED: 429,
    grpc.StatusCode.FAILED_PRECONDITION: 400,
    grpc.StatusCode.ABORTED: 409,
    grpc.StatusCode.OUT_OF_RANGE: 400,
    grpc.StatusCode.UNIMPLEMENTED: 501,
    grpc.StatusCode.INTERNAL: 500,
    grpc.StatusCode.UNAVAILABLE: 503,
    grpc.StatusCode.DATA_LOSS: 500,
    grpc.StatusCode.UNAUTHENTICATED: 401,
}


class InvariantError(Exception):
    """gRPC-aligned runtime error with optional structured details.

    Connect-style payload: lowercase code, optional details, no wrapper.
    """

    __slots__ = ("_detail_anys", "_trailing_metadata", "code", "details", "message")

    def __init__(
        self,
        code: grpc.StatusCode,
        message: str,
        details: Iterable[dict[str, Any] | message.Message] | None = None,
        *,
        trailing_metadata: Iterable[tuple[str, str | bytes]] = (),
    ):
        super().__init__(message)
        self.code = code
        self.message = message
        details = list(details) if details is not None else None
        self.details = _details_to_payload(details)
        self._detail_anys = _details_to_anys(details)
        self._trailing_metadata = tuple(trailing_metadata)

    def __str__(self) -> str:
        return self.message

    def to_payload(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "code": "canceled" if self.code == grpc.StatusCode.CANCELLED else self.code.name.lower(),
            "message": self.message,
        }
        if self.details:
            payload["details"] = self.details
        return payload

    def to_connect_payload(self) -> dict[str, Any]:
        """Encode this error using Connect's canonical rich-detail wire shape."""
        payload: dict[str, Any] = {
            "code": "canceled" if self.code == grpc.StatusCode.CANCELLED else self.code.name.lower(),
            "message": self.message,
        }
        if self._detail_anys:
            payload["details"] = [
                {
                    "type": detail.type_url.rsplit("/", 1)[-1],
                    "value": base64.b64encode(detail.value).decode().rstrip("="),
                }
                for detail in self._detail_anys
            ]
        return payload

    def to_status_proto(self) -> status_pb2.Status:
        status = status_pb2.Status(
            code=_grpc_code_number(self.code),
            message=self.message,
        )
        status.details.extend(self._detail_anys)
        return status

    def grpc_trailing_metadata(self) -> tuple[tuple[str, str | bytes], ...]:
        metadata: list[tuple[str, str | bytes]] = [
            (key, value) for key, value in self._trailing_metadata if key != "grpc-status-details-bin"
        ]
        if self._detail_anys:
            metadata.append(("grpc-status-details-bin", self.to_status_proto().SerializeToString()))
        return tuple(metadata)


def as_invariant_error(err: Exception) -> InvariantError:
    if isinstance(err, InvariantError):
        return err
    if isinstance(err, grpc.RpcError):
        code_fn = getattr(err, "code", None)
        details_fn = getattr(err, "details", None)
        code = code_fn() if callable(code_fn) else grpc.StatusCode.UNKNOWN
        message_text = details_fn() if callable(details_fn) else str(err)
        rich_details: list[message.Message] = []
        trailing_fn = getattr(err, "trailing_metadata", None)
        trailing = trailing_fn() if callable(trailing_fn) else ()
        for key, value in trailing or ():
            if key != "grpc-status-details-bin" or not isinstance(value, bytes):
                continue
            status = status_pb2.Status()
            with contextlib.suppress(Exception):
                status.ParseFromString(value)
                if status.message:
                    message_text = status.message
                rich_details.extend(status.details)
        return InvariantError(code, message_text, rich_details, trailing_metadata=trailing or ())
    return InvariantError(grpc.StatusCode.UNKNOWN, str(err))


def invalid_argument(message: str, *, field: str | None = None) -> InvariantError:
    details = None
    if field:
        details = [
            {
                "@type": "type.googleapis.com/google.rpc.BadRequest",
                "fieldViolations": [
                    {
                        "field": field,
                        "description": message,
                    }
                ],
            }
        ]
    return InvariantError(grpc.StatusCode.INVALID_ARGUMENT, message, details)


def invalid_argument_from_json_error(err: Exception) -> InvariantError:
    message = str(err)
    field = _extract_unknown_field(message)
    return invalid_argument(message, field=field)


def not_found(message: str) -> InvariantError:
    return InvariantError(grpc.StatusCode.NOT_FOUND, message)


def http_status_for(code: grpc.StatusCode) -> int:
    return _HTTP_STATUS_BY_CODE.get(code, 500)


_GRPC_CODE_NUMBERS = {
    grpc.StatusCode.OK: 0,
    grpc.StatusCode.CANCELLED: 1,
    grpc.StatusCode.UNKNOWN: 2,
    grpc.StatusCode.INVALID_ARGUMENT: 3,
    grpc.StatusCode.DEADLINE_EXCEEDED: 4,
    grpc.StatusCode.NOT_FOUND: 5,
    grpc.StatusCode.ALREADY_EXISTS: 6,
    grpc.StatusCode.PERMISSION_DENIED: 7,
    grpc.StatusCode.RESOURCE_EXHAUSTED: 8,
    grpc.StatusCode.FAILED_PRECONDITION: 9,
    grpc.StatusCode.ABORTED: 10,
    grpc.StatusCode.OUT_OF_RANGE: 11,
    grpc.StatusCode.UNIMPLEMENTED: 12,
    grpc.StatusCode.INTERNAL: 13,
    grpc.StatusCode.UNAVAILABLE: 14,
    grpc.StatusCode.DATA_LOSS: 15,
    grpc.StatusCode.UNAUTHENTICATED: 16,
}


def _grpc_code_number(code: grpc.StatusCode) -> int:
    return _GRPC_CODE_NUMBERS.get(code, 2)


def _details_to_anys(
    details: Sequence[dict[str, Any] | message.Message] | None,
) -> list[any_pb2.Any]:
    if not details:
        return []

    out: list[any_pb2.Any] = []
    for detail in details:
        if isinstance(detail, message.Message):
            any_msg = any_pb2.Any()
            if detail.DESCRIPTOR.full_name == "google.protobuf.Any":
                any_msg.CopyFrom(detail)
            else:
                any_msg.Pack(detail)
            out.append(any_msg)
            continue

        if not isinstance(detail, dict):
            continue
        any_msg = any_pb2.Any()
        with contextlib.suppress(Exception):
            json_format.ParseDict(detail, any_msg)
            out.append(any_msg)
    return out


def _any_to_payload(detail: any_pb2.Any) -> dict[str, Any]:
    try:
        return dict(json_format.MessageToDict(detail))
    except TypeError, ValueError:
        return {
            "@type": detail.type_url,
            "value": base64.b64encode(detail.value).decode(),
        }


def _details_to_payload(
    details: Sequence[dict[str, Any] | message.Message] | None,
) -> list[dict[str, Any]] | None:
    if not details:
        return None

    out: list[dict[str, Any]] = []
    for detail in details:
        if isinstance(detail, message.Message):
            any_msg = any_pb2.Any()
            if detail.DESCRIPTOR.full_name == "google.protobuf.Any":
                any_msg.CopyFrom(detail)
            else:
                any_msg.Pack(detail)
            out.append(_any_to_payload(any_msg))
            continue
        if isinstance(detail, dict):
            out.append(dict(detail))
    return out or None


def _extract_unknown_field(message: str) -> str | None:
    for pattern in _NO_FIELD_PATTERNS:
        match = pattern.search(message)
        if match:
            return match.group(1)
    return None
