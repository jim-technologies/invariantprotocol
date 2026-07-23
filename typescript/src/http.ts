import { createServer, type Server as HttpServer, type IncomingMessage, type ServerResponse } from "node:http";

import type { DescMessage, MessageShape } from "@bufbuild/protobuf";
import {
  Code as ConnectCode,
  ConnectError,
  createConnectRouter,
  createContextKey,
  createContextValues,
  type HandlerContext,
} from "@connectrpc/connect";
import type { UniversalServerRequest, UniversalServerResponse } from "@connectrpc/connect/protocol";
import { universalRequestFromNodeRequest, universalResponseToNodeResponse } from "@connectrpc/connect-node";

import {
  MAX_NODE_TIMER_DELAY_MS,
  monotonicDeadlineAfter,
  remainingDeadlineMs,
  scheduleAbsoluteDeadline,
} from "./deadline.js";
import { asInvariantError, httpStatusFor, InvariantError, notFound, toConnectError } from "./errors.js";
import { invalidRequestResponse, isClientResponse, MCP_PROTOCOL_VERSION, mcpDispatchWithContext } from "./mcp.js";
import { type RegisteredTool, type Server, serverInternal } from "./server.js";

export const PROTO_CONTENT_TYPE = "application/proto";
export const CONNECT_STREAM_JSON = "application/connect+json";
export const CONNECT_STREAM_PROTO = "application/connect+proto";
const CONNECT_END_STREAM_FLAG = 0x02;
const CONNECT_CONTROL_MAX_BYTES = 1024 * 1024;
const RESOURCE_EXHAUSTED_END_STREAM = Buffer.from('{"error":{"code":"resource_exhausted"}}');
type LongConnectDeadline = {
  deadlineAt: number;
  timeoutHeader: string;
};
const longConnectDeadlineKey = createContextKey<LongConnectDeadline | undefined>(undefined, {
  description: "Invariant long Connect deadline",
});

export function httpHandler(server: Server): (req: IncomingMessage, res: ServerResponse) => Promise<void> {
  server[serverInternal].freeze();
  const router = createConnectRouter({
    connect: true,
    grpc: false,
    grpcWeb: false,
    acceptCompression: [],
    readMaxBytes: Math.max(server.maxUnaryRequestBytes(), server.maxStreamRequestBytes()),
    writeMaxBytes: 0xffffffff,
    maxTimeoutMs: Number.MAX_SAFE_INTEGER,
  });
  const rpcTools = new Map<string, RegisteredTool>();
  for (const tool of server[serverInternal].tools()) {
    const method = methodDesc(tool);
    if (!method) {
      continue;
    }
    rpcTools.set(`/${tool.serviceFullName}/${tool.methodName}`, tool);
    if (tool.serverStreaming) {
      router.rpc(
        method as never,
        async function* (request: MessageShape<DescMessage>, context: HandlerContext) {
          try {
            restoreLongConnectDeadline(context);
            assertPositiveConnectTimeout(context.requestHeader);
            const mappedContext = server[serverInternal].mapHTTPContext(context);
            yield* invokeStreamBeforeDeadline(
              mappedContext,
              server[serverInternal].invokeStreamTool(tool, request, mappedContext),
            );
          } catch (e) {
            throw toConnectError(e);
          }
        } as never,
        {
          connect: true,
          grpc: false,
          grpcWeb: false,
          readMaxBytes: server.maxStreamRequestBytes(tool),
          writeMaxBytes: server.maxStreamResponseBytes(tool),
        } as never,
      );
    } else {
      router.rpc(
        method as never,
        (async (request: MessageShape<DescMessage>, context: HandlerContext) => {
          try {
            restoreLongConnectDeadline(context);
            assertPositiveConnectTimeout(context.requestHeader);
            const mappedContext = server[serverInternal].mapHTTPContext(context);
            return await invokeUnaryBeforeDeadline(mappedContext, () =>
              server[serverInternal].invokeTool(tool, request, mappedContext),
            );
          } catch (e) {
            throw toConnectError(e);
          }
        }) as never,
        {
          connect: true,
          grpc: false,
          grpcWeb: false,
          readMaxBytes: server.maxUnaryRequestBytes(tool),
          writeMaxBytes: server.maxUnaryResponseBytes(tool),
        } as never,
      );
    }
  }
  const rpcHandlers = new Map(router.handlers.map((handler) => [handler.requestPath, handler]));
  const rpcHandler = async (req: IncomingMessage, res: ServerResponse): Promise<void> => {
    const path = new URL(req.url ?? "/", "http://localhost").pathname;
    const handler = rpcHandlers.get(path);
    if (!handler) {
      sendError(res, notFound("Not found"), server.maxUnaryResponseBytes());
      return;
    }
    const deadlineScope = longConnectDeadlineScope(universalRequestFromNodeRequest(req, res, undefined, undefined));
    try {
      let universalResponse = await handler(deadlineScope.request);
      const tool = rpcTools.get(path);
      if (tool && !tool.serverStreaming) {
        universalResponse = await boundUnaryResponse(universalResponse, server.maxUnaryResponseBytes(tool));
      } else if (tool?.serverStreaming) {
        universalResponse = boundStreamingResponse(universalResponse, server.maxStreamResponseBytes(tool));
      }
      await universalResponseToNodeResponse(universalResponse, res);
    } catch (error) {
      if (ConnectError.from(error).code !== ConnectCode.Aborted) {
        throw error;
      }
    } finally {
      deadlineScope.cleanup();
    }
  };

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
        sendBytes(res, 200, PROTO_CONTENT_TYPE, server[serverInternal].parsed().bytes);
        return;
      }
      if (path === "/mcp") {
        if (req.headers.origin !== undefined) {
          sendJsonWithLimit(
            res,
            403,
            { error: "Origin is not accepted by the MCP endpoint." },
            server.maxUnaryResponseBytes(),
          );
          return;
        }
        if (method !== "POST") {
          res.statusCode = 405;
          res.setHeader("allow", "POST");
          res.end();
          return;
        }
        try {
          await serveMcpHttp(server, req, res);
        } catch (e) {
          sendError(res, e, server.maxUnaryResponseBytes());
        }
        return;
      }

      await rpcHandler(req, res);
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
  const maxResponseBytes = server.maxUnaryResponseBytes();
  if (!acceptsMcpResponse(req.headers.accept)) {
    sendJsonWithLimit(
      res,
      406,
      {
        error: "Accept must list both application/json and text/event-stream.",
      },
      maxResponseBytes,
    );
    return;
  }
  if (!matchesContentType(req.headers["content-type"], "application/json")) {
    sendJsonWithLimit(res, 415, { error: "Content-Type must be application/json." }, maxResponseBytes);
    return;
  }

  const timeoutMs = connectTimeoutMs(req.headers["connect-timeout-ms"]);
  const deadlineAt = timeoutMs === undefined ? undefined : monotonicDeadlineAfter(timeoutMs);
  const cancellation = new AbortController();
  let timedOut = false;
  const deadlineError = new ConnectError("deadline exceeded", ConnectCode.DeadlineExceeded);
  const expireDeadline = () => {
    timedOut = true;
    cancellation.abort(deadlineError);
  };
  const cleanupDeadline = scheduleAbsoluteDeadline(deadlineAt, expireDeadline);
  const disconnect = () => cancellation.abort(new ConnectError("client disconnected", ConnectCode.Canceled));
  req.once("aborted", disconnect);
  res.once("close", disconnect);
  try {
    const body = await withAbsoluteDeadline(
      () => readBody(req, server.maxUnaryRequestBytes()),
      cancellation.signal,
      deadlineAt,
      expireDeadline,
    );
    let msg: unknown;
    try {
      msg = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(body));
    } catch (e) {
      assertAbsoluteDeadline(deadlineAt, cancellation.signal, expireDeadline);
      sendJsonWithLimit(
        res,
        200,
        {
          jsonrpc: "2.0",
          id: null,
          error: { code: -32700, message: `Parse error: ${String(e)}` },
        },
        maxResponseBytes,
      );
      return;
    }
    const invalid = invalidRequestResponse(msg);
    if (invalid) {
      assertAbsoluteDeadline(deadlineAt, cancellation.signal, expireDeadline);
      sendJsonWithLimit(res, 200, invalid, maxResponseBytes);
      return;
    }
    const message = msg as Record<string, unknown>;

    const protocolVersion = singleHeader(req.headers["mcp-protocol-version"]);
    const isInitialize = message.method === "initialize";
    const unsupportedProtocol = protocolVersion !== undefined && protocolVersion !== MCP_PROTOCOL_VERSION;
    if ((!isInitialize && protocolVersion !== MCP_PROTOCOL_VERSION) || unsupportedProtocol) {
      assertAbsoluteDeadline(deadlineAt, cancellation.signal, expireDeadline);
      sendJsonWithLimit(
        res,
        400,
        {
          error: `MCP-Protocol-Version must be ${MCP_PROTOCOL_VERSION}.`,
        },
        maxResponseBytes,
      );
      return;
    }
    res.setHeader("mcp-protocol-version", MCP_PROTOCOL_VERSION);

    if (isClientResponse(message)) {
      assertAbsoluteDeadline(deadlineAt, cancellation.signal, expireDeadline);
      res.statusCode = 202;
      res.end();
      return;
    }

    const remainingTimeoutMs = deadlineAt === undefined ? undefined : Math.max(0, remainingDeadlineMs(deadlineAt));
    const response = await withAbsoluteDeadline(
      () =>
        mcpDispatchWithContext(server, message, {
          protocolName: "mcp",
          requestMethod: req.method ?? "POST",
          url: new URL(req.url ?? "/mcp", "http://localhost").toString(),
          timeoutMs: remainingTimeoutMs,
          requestSignal: cancellation.signal,
          requestHeader: requestHeaders(req),
          mapHTTPMetadata: true,
          maxResponseBytes,
        }),
      cancellation.signal,
      deadlineAt,
      expireDeadline,
    );
    if (!response) {
      res.statusCode = 202;
      res.end();
      return;
    }
    sendJsonWithLimit(res, 200, response, maxResponseBytes);
  } catch (error) {
    if (timedOut) {
      sendError(
        res,
        new InvariantError("deadline_exceeded", `deadline exceeded after ${timeoutMs ?? 0}ms`),
        maxResponseBytes,
      );
      return;
    }
    if (!cancellation.signal.aborted) {
      throw error;
    }
  } finally {
    cleanupDeadline();
    req.off("aborted", disconnect);
    res.off("close", disconnect);
  }
}

function methodDesc(tool: RegisteredTool) {
  return tool.methodDesc;
}

function longConnectDeadlineScope(request: UniversalServerRequest): {
  request: UniversalServerRequest;
  cleanup: () => void;
} {
  const timeoutHeader = request.header.get("connect-timeout-ms");
  if (timeoutHeader === null || !/^\d{1,10}$/.test(timeoutHeader) || timeoutHeader === "0") {
    return { request, cleanup: () => undefined };
  }
  const timeoutMs = Number(timeoutHeader);
  if (timeoutMs <= MAX_NODE_TIMER_DELAY_MS) {
    return { request, cleanup: () => undefined };
  }

  const deadlineAt = monotonicDeadlineAfter(timeoutMs);
  const deadline = new AbortController();
  const cleanup = scheduleAbsoluteDeadline(deadlineAt, () => {
    deadline.abort(new ConnectError("deadline exceeded", ConnectCode.DeadlineExceeded));
  });
  const header = new Headers(request.header);
  header.delete("connect-timeout-ms");
  const contextValues = request.contextValues ?? createContextValues();
  contextValues.set(longConnectDeadlineKey, { deadlineAt, timeoutHeader });
  return {
    request: {
      ...request,
      header,
      signal: AbortSignal.any([request.signal, deadline.signal]),
      contextValues,
    },
    cleanup,
  };
}

function restoreLongConnectDeadline(context: HandlerContext): void {
  const deadline = context.values.get(longConnectDeadlineKey);
  if (deadline === undefined) {
    return;
  }
  context.requestHeader.set("connect-timeout-ms", deadline.timeoutHeader);
  Object.assign(context, {
    timeoutMs: () => remainingDeadlineMs(deadline.deadlineAt),
  });
}

async function boundUnaryResponse(
  response: UniversalServerResponse,
  maxResponseBytes: number,
): Promise<UniversalServerResponse> {
  const chunks: Uint8Array[] = [];
  let size = 0;
  if (response.body) {
    for await (const chunk of response.body) {
      chunks.push(chunk);
      size += chunk.byteLength;
    }
  }
  if (maxResponseBytes > 0 && size > maxResponseBytes) {
    let body = Buffer.from(
      JSON.stringify({
        code: "resource_exhausted",
      }),
    );
    if (body.length > maxResponseBytes) {
      body = Buffer.alloc(0);
    }
    const header = new Headers(response.header);
    header.set("content-type", "application/json");
    header.set("content-length", String(body.length));
    header.delete("content-encoding");
    return {
      ...response,
      status: 429,
      header,
      body: oneChunk(body),
    };
  }
  return {
    ...response,
    body: (async function* () {
      yield* chunks;
    })(),
  };
}

function boundStreamingResponse(response: UniversalServerResponse, maxResponseBytes: number): UniversalServerResponse {
  if (!response.body) {
    return response;
  }
  return {
    ...response,
    body: boundConnectStreamBody(response.body, maxResponseBytes),
  };
}

async function* boundConnectStreamBody(
  body: AsyncIterable<Uint8Array>,
  maxResponseBytes: number,
): AsyncIterable<Uint8Array> {
  let pending = Buffer.alloc(0);
  for await (const chunk of body) {
    pending = Buffer.concat([pending, Buffer.from(chunk)]);
    while (pending.length >= 5) {
      const payloadLength = pending.readUInt32BE(1);
      const frameLength = 5 + payloadLength;
      if (pending.length < frameLength) {
        break;
      }
      const frame = pending.subarray(0, frameLength);
      pending = pending.subarray(frameLength);
      if (((frame[0] ?? 0) & CONNECT_END_STREAM_FLAG) !== 0) {
        if (payloadLength > CONNECT_CONTROL_MAX_BYTES) {
          yield resourceExhaustedEndStreamFrame();
        } else {
          yield frame;
        }
        return;
      }
      if (payloadLength > maxResponseBytes) {
        yield resourceExhaustedEndStreamFrame();
        return;
      }
      yield frame;
    }
  }
  if (pending.length > 0) {
    yield resourceExhaustedEndStreamFrame();
  }
}

function resourceExhaustedEndStreamFrame(): Uint8Array {
  const payload = RESOURCE_EXHAUSTED_END_STREAM;
  const frame = Buffer.allocUnsafe(5 + payload.length);
  frame[0] = CONNECT_END_STREAM_FLAG;
  frame.writeUInt32BE(payload.length, 1);
  payload.copy(frame, 5);
  return frame;
}

async function* oneChunk(bytes: Uint8Array): AsyncIterable<Uint8Array> {
  if (bytes.byteLength > 0) {
    yield bytes;
  }
}

function assertPositiveConnectTimeout(headers: Headers): void {
  const raw = headers.get("connect-timeout-ms");
  if (raw === null) {
    return;
  }
  if (!/^\d{1,10}$/.test(raw) || Number(raw) === 0) {
    throw new ConnectError(
      "Connect-Timeout-Ms must be a positive ASCII integer with at most 10 digits",
      ConnectCode.InvalidArgument,
    );
  }
}

function assertHandlerDeadline(context: HandlerContext): void {
  const remaining = context.timeoutMs();
  if (remaining !== undefined && remaining <= 0) {
    throw new ConnectError("deadline exceeded", ConnectCode.DeadlineExceeded);
  }
}

async function invokeUnaryBeforeDeadline<T>(context: HandlerContext, operation: () => Promise<T>): Promise<T> {
  assertHandlerDeadline(context);
  const result = await withAbort(operation, context.signal);
  assertHandlerDeadline(context);
  return result;
}

async function* invokeStreamBeforeDeadline<T>(context: HandlerContext, stream: AsyncIterable<T>): AsyncIterable<T> {
  const iterator = stream[Symbol.asyncIterator]();
  try {
    for (;;) {
      assertHandlerDeadline(context);
      const item = await withAbort(() => iterator.next(), context.signal);
      assertHandlerDeadline(context);
      if (item.done) {
        return;
      }
      yield item.value;
    }
  } finally {
    const closing = iterator.return?.();
    if (closing !== undefined) {
      const remaining = context.timeoutMs();
      if (context.signal.aborted || (remaining !== undefined && remaining <= 0)) {
        // The handler receives this signal and should stop cooperatively. A
        // handler that ignores it must not hold the transport open forever.
        void closing.catch(() => undefined);
      } else {
        await closing;
      }
    }
  }
}

function withAbort<T>(operation: () => Promise<T>, signal: AbortSignal): Promise<T> {
  if (signal.aborted) {
    return Promise.reject(signal.reason);
  }
  return new Promise<T>((resolve, reject) => {
    const abort = () => {
      signal.removeEventListener("abort", abort);
      reject(signal.reason);
    };
    signal.addEventListener("abort", abort, { once: true });
    void Promise.resolve()
      .then(() => {
        if (signal.aborted) {
          throw signal.reason;
        }
        return operation();
      })
      .then(
        (value) => {
          signal.removeEventListener("abort", abort);
          resolve(value);
        },
        (error) => {
          signal.removeEventListener("abort", abort);
          reject(error);
        },
      );
  });
}

async function withAbsoluteDeadline<T>(
  operation: () => Promise<T>,
  signal: AbortSignal,
  deadlineAt: number | undefined,
  expireDeadline: () => void,
): Promise<T> {
  assertAbsoluteDeadline(deadlineAt, signal, expireDeadline);
  const result = await withAbort(operation, signal);
  assertAbsoluteDeadline(deadlineAt, signal, expireDeadline);
  return result;
}

function assertAbsoluteDeadline(deadlineAt: number | undefined, signal: AbortSignal, expireDeadline: () => void): void {
  if (deadlineAt !== undefined && remainingDeadlineMs(deadlineAt) <= 0) {
    expireDeadline();
    throw signal.reason;
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

function sendJsonWithLimit(res: ServerResponse, status: number, payload: unknown, maxResponseBytes: number): void {
  const body = Buffer.from(JSON.stringify(payload));
  if (maxResponseBytes > 0 && body.length > maxResponseBytes) {
    sendError(
      res,
      new InvariantError("resource_exhausted", "encoded MCP response exceeds configured byte limit"),
      maxResponseBytes,
    );
    return;
  }
  sendBytes(res, status, "application/json", body);
}

function sendBytes(res: ServerResponse, status: number, contentType: string, payload: Uint8Array): void {
  res.statusCode = status;
  res.setHeader("content-type", contentType);
  res.setHeader("content-length", payload.length);
  res.end(payload);
}

function sendError(res: ServerResponse, e: unknown, maxResponseBytes = 0): void {
  let err = asInvariantError(e);
  let body = Buffer.from(JSON.stringify(err.toPayload()));
  if (maxResponseBytes > 0 && body.length > maxResponseBytes) {
    err = new InvariantError("resource_exhausted", "encoded error response exceeds configured byte limit");
    body = Buffer.from(JSON.stringify(err.toPayload()));
    if (body.length > maxResponseBytes) {
      body = Buffer.alloc(0);
    }
  }
  sendBytes(res, httpStatusFor(err.code), "application/json", body);
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
  const raw = singleHeader(value);
  if (raw === undefined) {
    return undefined;
  }
  if (!/^\d{1,10}$/.test(raw) || raw === "0") {
    throw new InvariantError(
      "invalid_argument",
      "Connect-Timeout-Ms must be a positive ASCII integer with at most 10 digits",
    );
  }
  const timeout = Number(raw);
  if (!Number.isSafeInteger(timeout)) {
    throw new InvariantError("invalid_argument", "Connect-Timeout-Ms is out of range");
  }
  return timeout;
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

function matchesContentType(value: string | string[] | undefined, expected: string): boolean {
  const raw = singleHeader(value);
  return raw?.split(";", 1)[0]?.trim().toLowerCase() === expected;
}
