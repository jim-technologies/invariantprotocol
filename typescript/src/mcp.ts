import { fromJson, type JsonValue, toJsonString } from "@bufbuild/protobuf";

import { asInvariantError } from "./errors.js";
import { type Server } from "./server.js";

const PROTOCOL_VERSION = "2024-11-05";

export type JsonRpcRequest = {
  jsonrpc?: string;
  id?: string | number | null;
  method?: string;
  params?: Record<string, unknown>;
};

export async function mcpDispatch(server: Server, msg: JsonRpcRequest): Promise<Record<string, unknown> | undefined> {
  const method = msg.method ?? "";
  const id = msg.id;
  const params = msg.params ?? {};

  if (id === undefined || id === null) {
    return undefined;
  }

  if (method === "initialize") {
    return ok(id, {
      protocolVersion: PROTOCOL_VERSION,
      capabilities: { tools: {} },
      serverInfo: { name: server.name, version: server.version },
    });
  }

  if (method === "tools/list") {
    return ok(id, { tools: server.toolCatalog() });
  }

  if (method === "tools/call") {
    return mcpCallTool(server, id, params);
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
): Promise<Record<string, unknown>> {
  const toolName = String(params.name ?? "");
  const args = (params.arguments ?? {}) as Record<string, unknown>;
  const tool = server.tools.get(toolName);
  if (!tool) {
    return err(id, -32602, `Unknown tool: ${toolName}`);
  }

  try {
    const request = fromJson(tool.inputDesc, args as JsonValue, { registry: server.parsed.registry });
    if (tool.serverStreaming) {
      const content: Record<string, string>[] = [];
      for await (const chunk of server.invokeStreamTool(tool, request, undefined)) {
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

    const response = await server.invokeTool(tool, request, undefined);
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
  }
}

function ok(id: string | number, result: Record<string, unknown>): Record<string, unknown> {
  return { jsonrpc: "2.0", id, result };
}

function err(id: string | number, code: number, message: string): Record<string, unknown> {
  return { jsonrpc: "2.0", id, error: { code, message } };
}
