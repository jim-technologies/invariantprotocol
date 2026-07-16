import { createServer, type IncomingMessage, type Server as HttpServer, type ServerResponse } from "node:http";

import { connectNodeAdapter } from "@connectrpc/connect-node";

import { asInvariantError, httpStatusFor, InvariantError, notFound, toConnectError } from "./errors.js";
import { MCP_PROTOCOL_VERSION, mcpDispatch } from "./mcp.js";
import { serverInternal, type Server, type Tool } from "./server.js";

export const PROTO_CONTENT_TYPE = "application/proto";
export const CONNECT_STREAM_JSON = "application/connect+json";
export const CONNECT_STREAM_PROTO = "application/connect+proto";

export function httpHandler(server: Server): (req: IncomingMessage, res: ServerResponse) => Promise<void> {
  server[serverInternal].freeze();
  const rpcHandler = connectNodeAdapter({
    connect: true,
    grpc: false,
    grpcWeb: false,
    readMaxBytes: Math.max(server.maxUnaryRequestBytes(), server.maxStreamRequestBytes()),
    writeMaxBytes: 0xffffffff,
    maxTimeoutMs: Number.MAX_SAFE_INTEGER,
    routes: (router) => {
      for (const tool of server.tools.values()) {
        const method = methodDesc(tool);
        if (!method) {
          continue;
        }
        if (tool.serverStreaming) {
          router.rpc(method as never, (async function* (request: any, context: any) {
            try {
              yield* server.invokeStreamTool(tool, request, server[serverInternal].mapHTTPContext(context));
            } catch (e) {
              throw toConnectError(e);
            }
          }) as never, {
            connect: true,
            grpc: false,
            grpcWeb: false,
            readMaxBytes: server.maxStreamRequestBytes(tool),
            writeMaxBytes: server.maxStreamResponseBytes(tool),
          } as never);
        } else {
          router.rpc(method as never, (async (request: any, context: any) => {
            try {
              return await server.invokeTool(tool, request, server[serverInternal].mapHTTPContext(context));
            } catch (e) {
              throw toConnectError(e);
            }
          }) as never, {
            connect: true,
            grpc: false,
            grpcWeb: false,
            readMaxBytes: server.maxUnaryRequestBytes(tool),
            writeMaxBytes: server.maxUnaryResponseBytes(tool),
          } as never);
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
        sendBytes(res, 200, PROTO_CONTENT_TYPE, server.parsed.bytes);
        return;
      }
      if (path === "/mcp") {
        if (method !== "POST") {
          res.statusCode = 405;
          res.setHeader("allow", "POST");
          res.end();
          return;
        }
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
  if (req.headers.origin !== undefined) {
    sendJson(res, 403, { error: "Origin is not accepted by the MCP endpoint." });
    return;
  }
  if (!acceptsMcpResponse(req.headers.accept)) {
    sendJson(res, 406, {
      error: "Accept must list both application/json and text/event-stream.",
    });
    return;
  }

  const body = await readBody(req, server.maxUnaryRequestBytes());
  let msg: Record<string, unknown>;
  try {
    msg = body.length > 0 ? JSON.parse(body.toString("utf8")) : {};
  } catch (e) {
    sendJson(res, 200, {
      jsonrpc: "2.0",
      id: null,
      error: { code: -32700, message: `Parse error: ${String(e)}` },
    });
    return;
  }
  if (typeof msg !== "object" || msg === null || Array.isArray(msg)) {
    sendJson(res, 200, { jsonrpc: "2.0", id: null, error: { code: -32600, message: "Invalid Request" } });
    return;
  }

  const protocolVersion = singleHeader(req.headers["mcp-protocol-version"]);
  const isInitialize = msg.method === "initialize";
  const unsupportedProtocol = protocolVersion !== undefined && protocolVersion !== MCP_PROTOCOL_VERSION;
  if ((!isInitialize && protocolVersion !== MCP_PROTOCOL_VERSION) || unsupportedProtocol) {
    sendJson(res, 400, {
      error: `MCP-Protocol-Version must be ${MCP_PROTOCOL_VERSION}.`,
    });
    return;
  }
  res.setHeader("mcp-protocol-version", MCP_PROTOCOL_VERSION);

  if (!("method" in msg) && ("result" in msg || "error" in msg)) {
    res.statusCode = 202;
    res.end();
    return;
  }

  const cancellation = new AbortController();
  const cancel = () => cancellation.abort();
  req.once("aborted", cancel);
  res.once("close", cancel);
  try {
    const response = await mcpDispatch(server, msg, {
      protocolName: "mcp",
      requestMethod: req.method ?? "POST",
      url: new URL(req.url ?? "/mcp", "http://localhost").toString(),
      timeoutMs: connectTimeoutMs(req.headers["connect-timeout-ms"]),
      requestSignal: cancellation.signal,
      requestHeader: requestHeaders(req),
      mapHTTPMetadata: true,
    });
    if (!response) {
      res.statusCode = 202;
      res.end();
      return;
    }
    sendJson(res, 200, response);
  } finally {
    req.off("aborted", cancel);
    res.off("close", cancel);
  }
}

function methodDesc(tool: Tool) {
  return tool.methodDesc;
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

function requestHeaders(req: IncomingMessage): Headers {
  const headers = new Headers();
  for (let i = 0; i < req.rawHeaders.length; i += 2) {
    const name = req.rawHeaders[i];
    const value = req.rawHeaders[i + 1];
    if (name !== undefined && value !== undefined) {
      headers.append(name, value);
    }
  }
  return headers;
}

function connectTimeoutMs(value: string | string[] | undefined): number | undefined {
  const raw = Array.isArray(value) ? value[0] : value;
  if (raw === undefined || !/^\d{1,10}$/.test(raw)) {
    return undefined;
  }
  const timeout = Number(raw);
  return Number.isSafeInteger(timeout) ? timeout : undefined;
}

function acceptsMcpResponse(value: string | undefined): boolean {
  if (value === undefined) {
    return false;
  }
  const mediaTypes = new Set<string>();
  for (const part of value.split(",")) {
    const [rawType, ...parameters] = part.split(";");
    const quality = parameters.find((parameter) => parameter.trim().toLowerCase().startsWith("q="));
    if (quality !== undefined) {
      const value = Number(quality.slice(quality.indexOf("=") + 1).trim());
      if (!Number.isFinite(value) || value <= 0) {
        continue;
      }
    }
    const mediaType = rawType?.trim().toLowerCase();
    if (mediaType) {
      mediaTypes.add(mediaType);
    }
  }
  return mediaTypes.has("application/json") && mediaTypes.has("text/event-stream");
}

function singleHeader(value: string | string[] | undefined): string | undefined {
  if (Array.isArray(value)) {
    return value.length === 1 ? value[0] : undefined;
  }
  return value;
}
