"""CLI projection — call tools from command-line arguments or request files.

Format: ServiceName Method [-r request]

Values for -r are auto-detected:
  - Existing file path → load by extension (.yaml/.yml, .json, .binpb/.pb)
  - Otherwise → parse as inline JSON
"""

from __future__ import annotations

import json
import os
from dataclasses import dataclass
from typing import TYPE_CHECKING

import yaml
from google.protobuf import descriptor_pool, json_format, message_factory

if TYPE_CHECKING:
    from invariant.server import Server


@dataclass
class CLIResult:
    """Parsed CLI arguments."""

    tool_name: str
    response: dict


def run_cli(server: Server, args: list[str]) -> dict | str:
    """Execute a CLI command and return the response as a dict (or help string).

    Format: ServiceName Method [-r request]
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

    # Load request
    request_dict = _load_value(request_value, message_type=tool.input_type) if request_value is not None else {}

    # Invoke
    pool = descriptor_pool.Default()
    req_desc = pool.FindMessageTypeByName(tool.input_type)
    req_class = message_factory.GetMessageClass(req_desc)
    request = req_class()
    json_format.ParseDict(request_dict, request)

    response = server._invoke(tool, request, None)
    return json_format.MessageToDict(response, preserving_proto_field_name=True)


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


def _load_value(value: str, *, message_type: str | None = None) -> dict:
    """Load a value from file path or inline JSON string.

    File detection: if value is an existing file, load by extension.
    Inline: parse as JSON.
    """
    if os.path.isfile(value):
        return _load_file(value, message_type=message_type)

    try:
        return json.loads(value)
    except (json.JSONDecodeError, ValueError) as e:
        raise ValueError(f"Cannot parse inline value as JSON: {e}") from None


def _load_file(path: str, *, message_type: str | None = None) -> dict:
    """Load a file by extension: .yaml/.yml, .json, .binpb/.pb."""
    ext = os.path.splitext(path)[1].lower()

    if ext in (".binpb", ".pb"):
        if message_type is None:
            raise ValueError(
                f"Cannot load protobuf binary '{path}' without message type."
            )
        pool = descriptor_pool.Default()
        desc = pool.FindMessageTypeByName(message_type)
        cls = message_factory.GetMessageClass(desc)
        msg = cls()
        with open(path, "rb") as f:
            msg.ParseFromString(f.read())
        return json_format.MessageToDict(msg, preserving_proto_field_name=True)

    with open(path) as f:
        if ext == ".json":
            return json.load(f)
        return yaml.safe_load(f)


def _cli_help(server: Server) -> str:
    """Generate help text listing all registered tools and their fields."""
    lines = ['Usage: <binary> <ServiceName> <Method> [-r request.yaml|request.json|\'{"inline":"json"}\']', ""]

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
