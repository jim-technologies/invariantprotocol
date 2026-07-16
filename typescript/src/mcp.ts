import { fromJson, type JsonValue, toJsonString } from "@bufbuild/protobuf";

import { asInvariantError } from "./errors.js";
import { serverInternal, type ProjectionContextOptions, type Server } from "./server.js";

export const MCP_PROTOCOL_VERSION = "2025-11-25";

export type JsonRpcRequest = {
  jsonrpc?: string;
  id?: string | number;
  method?: string;
  params?: Record<string, unknown>;
};

export type McpContextOptions = Partial<ProjectionContextOptions> & {
  mapHTTPMetadata?: boolean;
};

export type McpStdioInput = AsyncIterable<string | Uint8Array>;
export type McpStdioOutput = {
  write(chunk: string): unknown;
};

export async function mcpDispatch(
  server: Server,
  msg: unknown,
  contextOptions: McpContextOptions = {},
): Promise<Record<string, unknown> | undefined> {
  server[serverInternal].freeze();
  const invalid = invalidRequestResponse(msg);
  if (invalid) {
    return invalid;
  }
  if (isClientResponse(msg)) {
    return undefined;
  }
  const request = msg as JsonRpcRequest;
  const method = request.method ?? "";
  const id = canonicalJsonRpcId(request.id);

  if (id === undefined) {
    return undefined;
  }
  const params = request.params ?? {};
  if (!isJsonRpcObject(params)) {
    return err(id, -32602, "Invalid params");
  }

  if (method === "initialize") {
    if (
      typeof params.protocolVersion !== "string" ||
      !isJsonRpcObject(params.capabilities) ||
      !isJsonRpcObject(params.clientInfo) ||
      typeof params.clientInfo.name !== "string" ||
      typeof params.clientInfo.version !== "string"
    ) {
      return err(id, -32602, "Invalid params");
    }
    return ok(id, {
      protocolVersion: MCP_PROTOCOL_VERSION,
      capabilities: { tools: {} },
      serverInfo: { name: server.name, version: server.version },
    });
  }

  if (method === "tools/list") {
    return ok(id, { tools: server.toolCatalog() });
  }

  if (method === "tools/call") {
    if (
      typeof params.name !== "string" ||
      ("arguments" in params && !isJsonRpcObject(params.arguments))
    ) {
      return err(id, -32602, "Invalid params");
    }
    return mcpCallTool(server, id, params, contextOptions);
  }

  if (method === "ping") {
    return ok(id, {});
  }

  return err(id, -32601, `Method not found: ${method}`);
}

export async function mcpCallTool(
  server: Server,
  id: string | number,
  params: Record<string, unknown>,
  contextOptions: McpContextOptions = {},
): Promise<Record<string, unknown>> {
  const toolName = String(params.name ?? "");
  const args = (params.arguments ?? {}) as Record<string, unknown>;
  const tool = server.tools.get(toolName);
  if (!tool) {
    return err(id, -32602, `Unknown tool: ${toolName}`);
  }

  let context = server[serverInternal].createContext(tool.methodDesc, {
    protocolName: contextOptions.protocolName ?? "mcp",
    requestMethod: contextOptions.requestMethod,
    url: contextOptions.url ?? `mcp:///${tool.serviceFullName}/${tool.methodName}`,
    timeoutMs: contextOptions.timeoutMs,
    requestSignal: contextOptions.requestSignal,
    requestHeader: contextOptions.requestHeader,
  });
  if (contextOptions.mapHTTPMetadata) {
    context = server[serverInternal].mapHTTPContext(context) as typeof context;
  }

  try {
    const request = fromJson(tool.inputDesc, args as JsonValue, { registry: server.parsed.registry });
    if (tool.serverStreaming) {
      const content: Record<string, string>[] = [];
      for await (const chunk of server.invokeStreamTool(tool, request, context)) {
        content.push({
          type: "text",
          text: toJsonString(tool.outputDesc, chunk, {
            prettySpaces: 2,
            useProtoFieldName: true,
            registry: server.parsed.registry,
          }),
        });
      }
      return ok(id, { content });
    }

    const response = await server.invokeTool(tool, request, context);
    return ok(id, {
      content: [
        {
          type: "text",
          text: toJsonString(tool.outputDesc, response, {
            prettySpaces: 2,
            useProtoFieldName: true,
            registry: server.parsed.registry,
          }),
        },
      ],
    });
  } catch (e) {
    const inv = asInvariantError(e);
    return ok(id, {
      content: [{ type: "text", text: inv.message }],
      isError: true,
      error: inv.toPayload(),
    });
  } finally {
    context.abort();
  }
}

/**
 * Serve newline-delimited MCP JSON-RPC over stdio-compatible streams.
 * Long-running tools/call requests execute concurrently so the standard
 * notifications/cancelled notification can abort their HandlerContext signal.
 */
export async function serveMcpStdio(
  server: Server,
  input: McpStdioInput = process.stdin,
  output: McpStdioOutput = process.stdout,
  signal?: AbortSignal,
): Promise<void> {
  server[serverInternal].freeze();
  const inflight = new Map<string, AbortController>();
  const background = new Set<Promise<void>>();
  const cancelAll = () => {
    for (const controller of inflight.values()) {
      controller.abort();
    }
  };
  signal?.addEventListener("abort", cancelAll, { once: true });

  try {
    const decoder = new TextDecoder("utf-8", { fatal: true });
    for await (const bytes of lines(input)) {
      if (signal?.aborted) {
        break;
      }
      let line: string;
      try {
        line = decoder.decode(bytes);
      } catch (error) {
        writeMcpResponse(output, err(null, -32700, `Parse error: ${error instanceof Error ? error.message : String(error)}`));
        continue;
      }
      if (line.trim().length === 0) {
        continue;
      }

      let parsed: unknown;
      try {
        parsed = JSON.parse(line);
      } catch (error) {
        writeMcpResponse(output, err(null, -32700, `Parse error: ${error instanceof Error ? error.message : String(error)}`));
        continue;
      }
      const invalid = invalidRequestResponse(parsed);
      if (invalid) {
        writeMcpResponse(output, invalid);
        continue;
      }
      if (isClientResponse(parsed)) {
        continue;
      }
      const msg = parsed as JsonRpcRequest;

      if (msg.id === undefined) {
        if (msg.method === "notifications/cancelled") {
          const requestId = msg.params?.requestId;
          if (validJsonRpcId(requestId)) {
            inflight.get(idKey(requestId))?.abort();
          }
        }
        continue;
      }

      if (msg.method !== "tools/call") {
        const response = await mcpDispatch(server, msg, {
          protocolName: "mcp",
          requestSignal: signal,
        });
        if (response) {
          writeMcpResponse(output, response);
        }
        continue;
      }

      const controller = new AbortController();
      const abortFromParent = () => controller.abort(signal?.reason);
      if (signal?.aborted) {
        controller.abort(signal.reason);
      } else {
        signal?.addEventListener("abort", abortFromParent, { once: true });
      }
      const key = idKey(msg.id);
      inflight.set(key, controller);
      const task = (async () => {
        try {
          const response = await mcpDispatch(server, msg, {
            protocolName: "mcp",
            requestSignal: controller.signal,
          });
          if (response && !controller.signal.aborted) {
            writeMcpResponse(output, response);
          }
        } finally {
          signal?.removeEventListener("abort", abortFromParent);
          inflight.delete(key);
        }
      })();
      background.add(task);
      void task.then(
        () => background.delete(task),
        () => background.delete(task),
      );
    }
  } finally {
    signal?.removeEventListener("abort", cancelAll);
    if (signal?.aborted) {
      cancelAll();
    }
    await Promise.allSettled(background);
  }
}

function ok(id: string | number, result: Record<string, unknown>): Record<string, unknown> {
  return { jsonrpc: "2.0", id, result };
}

function err(id: string | number | null, code: number, message: string): Record<string, unknown> {
  return { jsonrpc: "2.0", id, error: { code, message } };
}

export function invalidRequestResponse(msg: unknown): Record<string, unknown> | undefined {
  if (!isJsonRpcObject(msg)) {
    return err(null, -32600, "Invalid Request");
  }
  if (isClientResponse(msg)) {
    return undefined;
  }
  if (
    msg.jsonrpc !== "2.0" ||
    typeof msg.method !== "string" ||
    ("id" in msg && !validJsonRpcId(msg.id))
  ) {
    return err(null, -32600, "Invalid Request");
  }
  return undefined;
}

export function isClientResponse(msg: unknown): boolean {
  if (!isJsonRpcObject(msg) || msg.jsonrpc !== "2.0" || "method" in msg) {
    return false;
  }
  if ("result" in msg && !("error" in msg)) {
    return "id" in msg && validJsonRpcId(msg.id) && isJsonRpcObject(msg.result);
  }
  if ("error" in msg && !("result" in msg)) {
    return (
      (!("id" in msg) || validJsonRpcId(msg.id)) &&
      isJsonRpcObject(msg.error) &&
      typeof msg.error.code === "number" &&
      Number.isInteger(msg.error.code) &&
      typeof msg.error.message === "string"
    );
  }
  return false;
}

function isJsonRpcObject(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function validJsonRpcId(value: unknown): value is string | number {
  return typeof value === "string" || (typeof value === "number" && Number.isSafeInteger(value));
}

function canonicalJsonRpcId(value: string | number | undefined): string | number | undefined {
  return typeof value === "number" && value === 0 ? 0 : value;
}

async function* lines(input: McpStdioInput): AsyncIterable<Uint8Array> {
  let pending = Buffer.alloc(0);
  for await (const chunk of input) {
    pending = Buffer.concat([
      pending,
      typeof chunk === "string" ? Buffer.from(chunk, "utf8") : Buffer.from(chunk),
    ]);
    for (;;) {
      const newline = pending.indexOf(0x0a);
      if (newline < 0) {
        break;
      }
      let line = pending.subarray(0, newline);
      pending = pending.slice(newline + 1);
      if (line.at(-1) === 0x0d) {
        line = line.subarray(0, line.length - 1);
      }
      yield line;
    }
  }
  if (pending.length > 0) {
    if (pending.at(-1) === 0x0d) {
      pending = pending.subarray(0, pending.length - 1);
    }
    yield pending;
  }
}

function writeMcpResponse(output: McpStdioOutput, response: Record<string, unknown>): void {
  output.write(`${JSON.stringify(response)}\n`);
}

function idKey(id: unknown): string {
  return `${typeof id}:${String(id)}`;
}
