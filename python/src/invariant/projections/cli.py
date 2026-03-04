"""CLI projection — call tools from command-line arguments or request files.

Format: ServiceName Method [-r request]

Values for -r are auto-detected:
  - Existing file path → load by extension (.yaml/.yml, .json, .binpb/.pb)
  - Otherwise → parse as inline JSON

Internally proto-first: input is deserialized directly into a proto message,
passed through _invoke() (proto in/out), and returned as a proto message.
JSON conversion happens only at the terminal output boundary in server.py.
"""

from __future__ import annotations

import json
import os
from typing import TYPE_CHECKING, Any

import yaml
from google.protobuf import descriptor_pool, json_format, message_factory

from invariant.errors import invalid_argument, invalid_argument_from_json_error

if TYPE_CHECKING:
    from google.protobuf.message import Message

    from invariant.server import Server


def run_cli(server: Server, args: list[str]) -> Message | str:
    """Execute a CLI command and return the response as a proto Message (or help string).

    Format: ServiceName Method [-r request]

    Equivalent to Go's Server.cli() — proto-first, JSON only at output boundary.
    """
    if not args or args[0] in ("--help", "-h"):
        return _cli_help(server)

    service_name, method_name, request_value = _split_args(args)

    # Resolve tool
    tool_name = _resolve_tool(server, service_name, method_name)
    tool = server.tools.get(tool_name)
    if tool is None:
        available = list(server.tools.keys())
        raise ValueError(f"Unknown tool '{tool_name}'. Available: {available}")

    # Build proto request directly from input.
    request = _new_request(tool.input_type)
    if request_value is not None:
        _load_into_proto(request, request_value)

    # Core dispatch (proto in / proto out).
    return server._invoke(tool, request, None)


def _new_request(input_type: str) -> Message:
    """Create an empty proto message for the given type name."""
    pool = descriptor_pool.Default()
    req_desc = pool.FindMessageTypeByName(input_type)
    req_class = message_factory.GetMessageClass(req_desc)
    return req_class()


def _load_into_proto(msg: Any, value: str) -> None:
    """Populate a proto message from a file path or inline JSON string.

    File detection: if value is an existing file, load by extension.
    Inline: parse as JSON.

    Equivalent to Go's loadIntoProto().
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

    Equivalent to Go's loadFileIntoProto().
    """
    ext = os.path.splitext(path)[1].lower()

    if ext in (".binpb", ".pb"):
        with open(path, "rb") as f:
            msg.ParseFromString(f.read())
        return

    if ext == ".json":
        with open(path) as f:
            d = json.load(f)
    else:  # .yaml, .yml
        with open(path) as f:
            d = yaml.safe_load(f)

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

    return service_name, method_name, request_value


def _resolve_tool(server: Server, service_name: str, method_name: str) -> str:
    """Resolve ServiceName + Method to a tool name."""
    for tool in server.tools.values():
        svc_name = tool.service_full_name.rsplit(".", 1)[-1]
        if svc_name == service_name and tool.method_name == method_name:
            return tool.name
    available = list(server.tools.keys())
    raise ValueError(
        f"Unknown service/method: {service_name} {method_name}. Available: {available}"
    )


def _cli_help(server: Server) -> str:
    """Generate help text listing all registered tools and their fields."""
    lines = ['Usage: <binary> <ServiceName> <Method> [-r request.yaml|request.json|request.binpb|\'{"inline":"json"}\']', ""]

    if not server.tools:
        lines.append("No tools registered.")
        return "\n".join(lines)

    # Sort tools by service name then method name.
    entries = []
    for tool in server.tools.values():
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
