import { fromJson, type JsonValue, toJsonString } from "@bufbuild/protobuf";

import { asInvariantError } from "./errors.js";
import { serverInternal, type ProjectionContextOptions, type Server } from "./server.js";

export const MCP_PROTOCOL_VERSION = "2025-11-25";

export type JsonRpcRequest = {
  jsonrpc?: string;
  id?: string | number | null;
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
  msg: JsonRpcRequest,
  contextOptions: McpContextOptions = {},
): Promise<Record<string, unknown> | undefined> {
  server[serverInternal].freeze();
  const method = msg.method ?? "";
  const id = msg.id;
  const params = msg.params ?? {};

  if (id === undefined || id === null) {
    return undefined;
  }

  if (method === "initialize") {
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
    for await (const line of lines(input)) {
      if (signal?.aborted) {
        break;
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
      if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
        writeMcpResponse(output, err(null, -32600, "Invalid Request"));
        continue;
      }
      const msg = parsed as JsonRpcRequest;

      if (msg.id === undefined || msg.id === null) {
        if (msg.method === "notifications/cancelled") {
          const requestId = msg.params?.requestId;
          if (requestId !== undefined && requestId !== null) {
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

async function* lines(input: McpStdioInput): AsyncIterable<string> {
  let pending = "";
  for await (const chunk of input) {
    pending += typeof chunk === "string" ? chunk : Buffer.from(chunk).toString("utf8");
    for (;;) {
      const newline = pending.indexOf("\n");
      if (newline < 0) {
        break;
      }
      const line = pending.slice(0, newline).replace(/\r$/, "");
      pending = pending.slice(newline + 1);
      yield line;
    }
  }
  if (pending.length > 0) {
    yield pending.replace(/\r$/, "");
  }
}

function writeMcpResponse(output: McpStdioOutput, response: Record<string, unknown>): void {
  output.write(`${JSON.stringify(response)}\n`);
}

function idKey(id: unknown): string {
  return `${typeof id}:${String(id)}`;
}
