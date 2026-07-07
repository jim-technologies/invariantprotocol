import { createServer, type IncomingMessage, type Server as HttpServer, type ServerResponse } from "node:http";

import { Code as ConnectCode, ConnectError } from "@connectrpc/connect";
import { connectNodeAdapter } from "@connectrpc/connect-node";

import { asInvariantError, httpStatusFor, InvariantError, notFound, type Code } from "./errors.js";
import { mcpDispatch } from "./mcp.js";
import { type Server, type Tool } from "./server.js";

export const PROTO_CONTENT_TYPE = "application/proto";
export const CONNECT_STREAM_JSON = "application/connect+json";
export const CONNECT_STREAM_PROTO = "application/connect+proto";

export function httpHandler(server: Server): (req: IncomingMessage, res: ServerResponse) => Promise<void> {
  const rpcHandler = connectNodeAdapter({
    connect: true,
    grpc: true,
    grpcWeb: true,
    readMaxBytes: Math.max(server.maxUnaryRequestBytes(), server.maxStreamRequestBytes()),
    writeMaxBytes: 0xffffffff,
    maxTimeoutMs: Number.MAX_SAFE_INTEGER,
    routes: (router) => {
      for (const tool of server.tools.values()) {
        const method = methodDesc(server, tool);
        if (!method) {
          continue;
        }
        if (tool.serverStreaming) {
          router.rpc(method as never, (async function* (request: any, context: any) {
            try {
              yield* server.invokeStreamTool(tool, request, context);
            } catch (e) {
              throw toConnectError(e);
            }
          }) as never);
        } else {
          router.rpc(method as never, (async (request: any, context: any) => {
            try {
              return await server.invokeTool(tool, request, context);
            } catch (e) {
              throw toConnectError(e);
            }
          }) as never);
        }
      }
    },
    fallback: (_req, res) => {
      const payload = Buffer.from(JSON.stringify(notFound("Not found").toPayload()));
      res.writeHead(404, { "content-type": "application/json", "content-length": payload.length });
      res.end(payload);
    },
  });

  return async (req, res) => {
    const path = new URL(req.url ?? "/", "http://localhost").pathname;
    const method = req.method ?? "GET";

    try {
      if (method === "GET" && (path === "/" || path === "/__invariant/tools")) {
        sendJson(res, 200, { tools: server.toolCatalog() });
        return;
      }
      if (method === "GET" && (path === "/healthz" || path === "/readyz")) {
        sendJson(res, 200, { status: "ok" });
        return;
      }
      if (method === "GET" && path === "/__invariant/descriptor.binpb") {
        sendBytes(res, 200, "application/octet-stream", server.parsed.bytes);
        return;
      }
      if (method === "POST" && path === "/mcp") {
        await serveMcpHttp(server, req, res);
        return;
      }

      rpcHandler(req, res);
    } catch (e) {
      sendError(res, e);
    }
  };
}

export function serveHttp(server: Server, port: number, host = "127.0.0.1"): Promise<HttpServer> {
  const app = httpHandler(server);
  const httpServer = createServer((req, res) => {
    void app(req, res);
  });
  return new Promise((resolve, reject) => {
    httpServer.once("error", reject);
    httpServer.listen(port, host, () => {
      httpServer.off("error", reject);
      resolve(httpServer);
    });
  });
}

async function serveMcpHttp(server: Server, req: IncomingMessage, res: ServerResponse): Promise<void> {
  const body = await readBody(req, server.maxUnaryRequestBytes());
  let msg: Record<string, unknown>;
  try {
    msg = body.length > 0 ? JSON.parse(body.toString("utf8")) : {};
  } catch (e) {
    sendJson(res, 200, { jsonrpc: "2.0", id: null, error: { code: -32700, message: `Parse error: ${String(e)}` } });
    return;
  }

  const response = await mcpDispatch(server, msg);
  if (!response) {
    res.statusCode = 204;
    res.end();
    return;
  }
  sendJson(res, 200, response);
}

function methodDesc(server: Server, tool: Tool) {
  return server.parsed.services.get(tool.serviceFullName)?.desc.methods.find((method) => method.name === tool.methodName);
}

function toConnectError(err: unknown): ConnectError {
  const inv = asInvariantError(err);
  return new ConnectError(inv.message, connectCodeFor(inv.code));
}

function connectCodeFor(code: Code): ConnectCode {
  switch (code) {
    case "cancelled":
      return ConnectCode.Canceled;
    case "unknown":
      return ConnectCode.Unknown;
    case "invalid_argument":
      return ConnectCode.InvalidArgument;
    case "deadline_exceeded":
      return ConnectCode.DeadlineExceeded;
    case "not_found":
      return ConnectCode.NotFound;
    case "already_exists":
      return ConnectCode.AlreadyExists;
    case "permission_denied":
      return ConnectCode.PermissionDenied;
    case "resource_exhausted":
      return ConnectCode.ResourceExhausted;
    case "failed_precondition":
      return ConnectCode.FailedPrecondition;
    case "aborted":
      return ConnectCode.Aborted;
    case "out_of_range":
      return ConnectCode.OutOfRange;
    case "unimplemented":
      return ConnectCode.Unimplemented;
    case "internal":
      return ConnectCode.Internal;
    case "unavailable":
      return ConnectCode.Unavailable;
    case "data_loss":
      return ConnectCode.DataLoss;
    case "unauthenticated":
      return ConnectCode.Unauthenticated;
  }
}

async function readBody(req: IncomingMessage, maxBytes: number): Promise<Buffer> {
  const chunks: Buffer[] = [];
  let total = 0;
  for await (const chunk of req) {
    const buf = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk);
    total += buf.length;
    if (total > maxBytes) {
      throw new InvariantError("resource_exhausted", `request body exceeds ${maxBytes} byte limit`);
    }
    chunks.push(buf);
  }
  return Buffer.concat(chunks);
}

function sendJson(res: ServerResponse, status: number, payload: unknown): void {
  sendBytes(res, status, "application/json", Buffer.from(JSON.stringify(payload)));
}

function sendBytes(res: ServerResponse, status: number, contentType: string, payload: Uint8Array): void {
  res.statusCode = status;
  res.setHeader("content-type", contentType);
  res.setHeader("content-length", payload.length);
  res.end(payload);
}

function sendError(res: ServerResponse, e: unknown): void {
  const err = asInvariantError(e);
  sendJson(res, httpStatusFor(err.code), err.toPayload());
}
