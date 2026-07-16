"""CLI projection — call tools from command-line arguments or request files.

Format: ServiceName Method [-r request]

Values for -r are auto-detected:
  - Existing file path → load by extension (.json, .binpb/.pb)
  - Otherwise → parse as inline JSON

Internally proto-first: input is deserialized directly into a proto message,
passed through _invoke() (proto in/out), and returned as a proto message.
JSON conversion happens only at the terminal output boundary in server.py.
"""

from __future__ import annotations

import json
import os
from typing import TYPE_CHECKING, Any

from google.protobuf import json_format
from google.protobuf.message import DecodeError

from invariant.errors import invalid_argument, invalid_argument_from_json_error
from invariant.projection_context import ProjectionContext

if TYPE_CHECKING:
    from google.protobuf.message import Message

    from invariant.server import Server


async def run_cli(server: Server, args: list[str]) -> Message | str:
    """Execute a CLI command and return the response as a proto Message (or help string).

    Buffers stream output into a newline-delimited string. Useful for tests and
    in-process use. For real-time piped output, use ``stream_cli`` with a writer.
    """
    server._freeze()
    if not args or args[0] in ("--help", "-h"):
        return _cli_help(server)

    service_name, method_name, request_value = _split_args(args)
    tool, request = _prepare(server, service_name, method_name, request_value)
    context = ProjectionContext(peer="invariant:cli")
    try:
        if tool.server_streaming:
            lines: list[str] = []
            async for msg in server._invoke_stream(tool, request, context):
                lines.append(json_format.MessageToJson(msg, preserving_proto_field_name=True, indent=None))
            return "\n".join(lines)

        return await server._invoke(tool, request, context)
    finally:
        context.finish(cancelled=context.cancelled())


async def stream_cli(server: Server, args: list[str], write) -> None:
    """Execute a CLI command, calling ``write`` per output piece — streamed.

    Use this when output must reach the user as soon as it is produced —
    a long-running stream piped through CLI must not feel frozen. ``write``
    is invoked:

      - once for help text
      - once for a unary response (pretty-printed JSON)
      - once per chunk for server-streaming (compact JSON; terminal writers add
        the line ending)

    ``write`` should be a sync callable like ``print``; we do not await it.
    """
    server._freeze()
    if not args or args[0] in ("--help", "-h"):
        write(_cli_help(server))
        return

    service_name, method_name, request_value = _split_args(args)
    tool, request = _prepare(server, service_name, method_name, request_value)
    context = ProjectionContext(peer="invariant:cli")
    try:
        if tool.server_streaming:
            async for msg in server._invoke_stream(tool, request, context):
                write(json_format.MessageToJson(msg, preserving_proto_field_name=True, indent=None))
            return

        response = await server._invoke(tool, request, context)
        if response is None:
            write("{}")
            return
        write(json_format.MessageToJson(response, preserving_proto_field_name=True, indent=2))
    finally:
        context.finish(cancelled=context.cancelled())


def _prepare(server: Server, service_name: str, method_name: str, request_value):
    """Resolve the target tool and build its proto request."""
    tool_name = _resolve_tool(server, service_name, method_name)
    tool = server._tools.get(tool_name)
    if tool is None:
        available = list(server._tools)
        raise ValueError(f"Unknown tool '{tool_name}'. Available: {available}")

    request = tool.new_request()
    if request_value is not None:
        _load_into_proto(request, request_value)
    return tool, request


def _load_into_proto(msg: Any, value: str) -> None:
    """Populate a proto message from a file path or inline JSON string.

    File detection: if value is an existing file, load by extension.
    Inline: parse as JSON.
    """
    if os.path.isfile(value):
        _load_file_into_proto(msg, value)
        return

    try:
        d = json.loads(value)
    except (json.JSONDecodeError, ValueError) as e:
        raise invalid_argument(f"Cannot parse inline value as JSON: {e}") from None
    try:
        json_format.ParseDict(d, msg)
    except Exception as e:
        raise invalid_argument_from_json_error(e) from None


def _load_file_into_proto(msg: Any, path: str) -> None:
    """Read a file and deserialize it into a proto message.

    Supported extensions: .json, .binpb, .pb.
    """
    ext = os.path.splitext(path)[1].lower()

    if ext in (".binpb", ".pb"):
        try:
            with open(path, "rb") as f:
                encoded = f.read()
        except OSError as error:
            raise invalid_argument(f"read request file: {error}") from None
        try:
            msg.ParseFromString(encoded)
        except DecodeError as error:
            raise invalid_argument(f"decode binary proto: {error}") from None
        return

    if ext != ".json":
        raise invalid_argument(f"unsupported request file extension: {ext} (use .json, .binpb, or .pb)")

    try:
        with open(path, encoding="utf-8") as f:
            d = json.load(f)
    except OSError as error:
        raise invalid_argument(f"read request file: {error}") from None
    except (json.JSONDecodeError, UnicodeDecodeError) as error:
        raise invalid_argument(f"Cannot parse request file as JSON: {error}") from None

    try:
        json_format.ParseDict(d, msg)
    except Exception as e:
        raise invalid_argument_from_json_error(e) from None


def _split_args(
    args: list[str],
) -> tuple[str, str, str | None]:
    """Split args into (service_name, method_name, request_value).

    Format: ServiceName Method [-r request]
    """
    i = 0

    # ServiceName
    if i >= len(args) or args[i].startswith("-"):
        raise ValueError("Expected ServiceName as first argument.")
    service_name = args[i]
    i += 1

    # Method
    if i >= len(args) or args[i].startswith("-"):
        raise ValueError("Expected Method name after ServiceName.")
    method_name = args[i]
    i += 1

    # Optional -r request
    request_value = None
    if i < len(args) and args[i] == "-r":
        i += 1
        if i >= len(args):
            raise ValueError("Missing value after -r.")
        request_value = args[i]
        i += 1
    if i < len(args):
        raise ValueError(f"Unexpected argument: {args[i]}")

    return service_name, method_name, request_value


def _resolve_tool(server: Server, service_name: str, method_name: str) -> str:
    """Resolve ServiceName + Method to a tool name."""
    for tool in server._tools.values():
        svc_name = tool.service_full_name.rsplit(".", 1)[-1]
        if svc_name == service_name and tool.method_name == method_name:
            return tool.name
    available = list(server._tools)
    raise ValueError(f"Unknown service/method: {service_name} {method_name}. Available: {available}")


def _cli_help(server: Server) -> str:
    """Generate help text listing all registered tools and their fields."""
    lines = [
        'Usage: <binary> <ServiceName> <Method> [-r request.json|request.binpb|\'{"inline":"json"}\']',
        "",
    ]

    if not server._tools:
        lines.append("No tools registered.")
        return "\n".join(lines)

    # Sort tools by service name then method name.
    entries = []
    for tool in server._tools.values():
        svc_name = tool.service_full_name.rsplit(".", 1)[-1]
        entries.append((svc_name, tool))
    entries.sort(key=lambda e: (e[0], e[1].method_name))

    lines.append("Available methods:")
    lines.append("")
    for svc_name, tool in entries:
        lines.append(f"  {svc_name} {tool.method_name}")
        if tool.description and tool.description != tool.name:
            lines.append(f"    {tool.description}")

        props = tool.input_schema.get("properties", {})
        required = set(tool.input_schema.get("required", []))

        if props:
            fields = sorted(props.keys())
            lines.append("    Fields:")
            for name in fields:
                field_schema = props[name]
                typ = _field_type(field_schema)
                tag = " (required)" if name in required else ""
                desc = field_schema.get("description", "")
                line = f"      {name:<20s} {typ:<10s}{tag}"
                if desc:
                    line += f"  \u2014 {desc}"
                lines.append(line)
        lines.append("")

    return "\n".join(lines)


def _field_type(schema: dict) -> str:
    """Return a human-readable type from a JSON Schema property.

    For enums, returns "VAL1|VAL2|..." instead of "string".
    For arrays of objects, returns "array<object>".
    """
    if "enum" in schema:
        return "|".join(str(v) for v in schema["enum"])
    typ = schema.get("type", "any")
    if typ == "array":
        item_type = schema.get("items", {}).get("type", "")
        if item_type:
            return f"array<{item_type}>"
    return typ
