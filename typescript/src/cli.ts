import { existsSync, readFileSync } from "node:fs";
import { extname } from "node:path";

import { fromBinary, fromJsonString, toJsonString } from "@bufbuild/protobuf";

import { invalidArgument } from "./errors.js";
import { type Server, serverInternal, type Tool } from "./server.js";

export async function runCli(server: Server, args: string[]): Promise<string> {
  server[serverInternal].freeze();
  if (args.length === 0 || args[0] === "--help" || args[0] === "-h") {
    return cliHelp(server);
  }

  const [serviceName, methodName, requestValue] = splitArgs(args);
  const tool = resolveTool(server, serviceName, methodName);
  const request = loadRequest(tool, requestValue, server);
  const context = server[serverInternal].createContext(tool.methodDesc, {
    protocolName: "cli",
    url: `invariant-cli:///${tool.serviceFullName}/${tool.methodName}`,
  });

  try {
    if (tool.serverStreaming) {
      const lines: string[] = [];
      for await (const chunk of server.invokeStreamTool(tool, request, context)) {
        lines.push(toJsonString(tool.outputDesc, chunk, { useProtoFieldName: true, registry: server.parsed.registry }));
      }
      return lines.join("\n");
    }

    const response = await server.invokeTool(tool, request, context);
    return toJsonString(tool.outputDesc, response, {
      prettySpaces: 2,
      useProtoFieldName: true,
      registry: server.parsed.registry,
    });
  } finally {
    context.abort();
  }
}

export function cliHelp(server: Server): string {
  const lines = ['Usage: <binary> <ServiceName> <Method> [-r request.json|request.binpb|\'{"inline":"json"}\']', ""];
  if (server.tools.size === 0) {
    lines.push("No tools registered.");
    return lines.join("\n");
  }

  lines.push("Available methods:", "");
  const entries = [...server.tools.values()].sort((a, b) => {
    const sa = a.serviceFullName.split(".").at(-1) ?? a.serviceFullName;
    const sb = b.serviceFullName.split(".").at(-1) ?? b.serviceFullName;
    return sa.localeCompare(sb) || a.methodName.localeCompare(b.methodName);
  });
  for (const tool of entries) {
    const service = tool.serviceFullName.split(".").at(-1) ?? tool.serviceFullName;
    lines.push(`  ${service} ${tool.methodName}`);
    if (tool.description && tool.description !== tool.name) {
      lines.push(`    ${tool.description}`);
    }

    const props = (tool.inputSchema.properties ?? {}) as Record<string, Record<string, unknown>>;
    const required = new Set((tool.inputSchema.required ?? []) as string[]);
    const names = Object.keys(props);
    if (names.length > 0) {
      lines.push("    Fields:");
      for (const name of names) {
        const prop = props[name];
        const marker = required.has(name) ? "(required)" : "";
        const desc = prop.description ? ` - ${prop.description}` : "";
        lines.push(`      ${name.padEnd(20)} ${typeLabel(prop).padEnd(18)} ${marker}${desc}`);
      }
    }
  }
  return lines.join("\n");
}

function splitArgs(args: string[]): [string, string, string | undefined] {
  const service = args[0];
  const method = args[1];
  if (!service || service.startsWith("-")) {
    throw new Error("Expected ServiceName as first argument.");
  }
  if (!method || method.startsWith("-")) {
    throw new Error("Expected Method name after ServiceName.");
  }

  let request: string | undefined;
  if (args.length > 2) {
    if (args[2] !== "-r") {
      throw new Error(`Unknown argument: ${args[2]}`);
    }
    if (!args[3]) {
      throw new Error("Missing value after -r.");
    }
    request = args[3];
    if (args.length > 4) {
      throw new Error(`Unexpected argument: ${args[4]}`);
    }
  }
  return [service, method, request];
}

function resolveTool(server: Server, serviceName: string, methodName: string): Tool {
  for (const tool of server.tools.values()) {
    const service = tool.serviceFullName.split(".").at(-1);
    if (service === serviceName && tool.methodName === methodName) {
      return tool;
    }
  }
  throw new Error(
    `Unknown service/method: ${serviceName} ${methodName}. Available: ${JSON.stringify([...server.tools.keys()])}`,
  );
}

function loadRequest(tool: Tool, value: string | undefined, server: Server) {
  if (!value) {
    return server.coerceMessage(tool.inputDesc, {});
  }

  if (existsSync(value)) {
    const ext = extname(value).toLowerCase();
    if (ext !== ".json" && ext !== ".binpb" && ext !== ".pb") {
      throw invalidArgument(`unsupported request file extension: ${ext} (use .json, .binpb, or .pb)`);
    }
    let data: Buffer;
    try {
      data = readFileSync(value);
    } catch (error) {
      throw invalidArgument(`read request file: ${errorMessage(error)}`);
    }
    if (ext === ".binpb" || ext === ".pb") {
      try {
        return fromBinary(tool.inputDesc, data);
      } catch (error) {
        throw invalidArgument(`decode binary proto: ${errorMessage(error)}`);
      }
    }
    return parseJsonRequest(tool, data.toString("utf8"), server);
  }

  return parseJsonRequest(tool, value, server);
}

function parseJsonRequest(tool: Tool, value: string, server: Server) {
  try {
    return fromJsonString(tool.inputDesc, value, { registry: server.parsed.registry });
  } catch (error) {
    throw invalidArgument(`decode protobuf JSON: ${errorMessage(error)}`);
  }
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

function typeLabel(schema: Record<string, unknown>): string {
  if (Array.isArray(schema.enum)) {
    return schema.enum.join("|");
  }
  if (schema.type === "array") {
    const items = schema.items as Record<string, unknown> | undefined;
    return `array<${items ? typeLabel(items) : "any"}>`;
  }
  if (schema.type === "object" && schema.additionalProperties && !schema.properties) {
    return "map";
  }
  return String(schema.type ?? "any");
}
