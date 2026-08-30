"""Protovalidate interceptor — validates requests against `buf.validate.field` constraints.

Usage::

    server.use(invariant.validation())
"""

from __future__ import annotations

import grpc
import protovalidate

from invariant.errors import InvariantError, invalid_argument


def validation() -> grpc.aio.ServerInterceptor:
    """Return one standard aio interceptor covering every gRPC cardinality."""
    return _ValidationInterceptor()


class _ValidationInterceptor(grpc.aio.ServerInterceptor):
    def __init__(self) -> None:
        self._validator = protovalidate.Validator()

    def _validate(self, request) -> None:
        try:
            self._validator.validate(request)
        except protovalidate.ValidationError as error:
            raise _to_invariant_error(error) from None

    async def intercept_service(self, continuation, handler_call_details):
        handler = await continuation(handler_call_details)
        if handler is None:
            return handler

        if handler.request_streaming:
            if handler.response_streaming:
                terminal = handler.stream_stream
                if terminal is None:
                    return handler

                async def validated_bidi(request_iterator, context):
                    async def validated_requests():
                        async for request in request_iterator:
                            self._validate(request)
                            yield request

                    async for response in terminal(validated_requests(), context):
                        yield response

                return grpc.stream_stream_rpc_method_handler(
                    validated_bidi,
                    request_deserializer=handler.request_deserializer,
                    response_serializer=handler.response_serializer,
                )

            terminal = handler.stream_unary
            if terminal is None:
                return handler

            async def validated_client_stream(request_iterator, context):
                async def validated_requests():
                    async for request in request_iterator:
                        self._validate(request)
                        yield request

                return await terminal(validated_requests(), context)

            return grpc.stream_unary_rpc_method_handler(
                validated_client_stream,
                request_deserializer=handler.request_deserializer,
                response_serializer=handler.response_serializer,
            )

        if handler.response_streaming:
            terminal = handler.unary_stream
            if terminal is None:
                return handler

            async def validated_stream(request, context):
                self._validate(request)
                async for response in terminal(request, context):
                    yield response

            return grpc.unary_stream_rpc_method_handler(
                validated_stream,
                request_deserializer=handler.request_deserializer,
                response_serializer=handler.response_serializer,
            )

        terminal = handler.unary_unary
        if terminal is None:
            return handler

        async def validated_unary(request, context):
            self._validate(request)
            return await terminal(request, context)

        return grpc.unary_unary_rpc_method_handler(
            validated_unary,
            request_deserializer=handler.request_deserializer,
            response_serializer=handler.response_serializer,
        )


def _to_invariant_error(err: protovalidate.ValidationError) -> InvariantError:
    """Convert a protovalidate ValidationError into an InvariantError with field-level details."""
    field_violations = []
    for v in err.violations:
        proto_v = v.proto
        # `field` is an optional path: message-level rules violate no single
        # field and leave it unset, which reads back as None.
        path = proto_v.field
        elements = path.elements if path is not None else ()
        field = ".".join(el.field_name for el in elements if el.field_name)
        field_violations.append({"field": field, "description": proto_v.message})

    if not field_violations:
        return invalid_argument(str(err))

    message = "; ".join(f"{fv['field']}: {fv['description']}" for fv in field_violations)
    details = [
        {
            "@type": "type.googleapis.com/google.rpc.BadRequest",
            "fieldViolations": field_violations,
        }
    ]
    return InvariantError(invalid_argument("").code, message, details)
