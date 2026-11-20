import type { DescMessage, MessageShape } from "@bufbuild/protobuf";
import {
  Code as ConnectCode,
  ConnectError,
  type ContextValues,
  createConnectRouter,
  createContextKey,
  createContextValues,
  type HandlerContext,
} from "@connectrpc/connect";
import type { UniversalHandlerFn, UniversalServerRequest, UniversalServerResponse } from "@connectrpc/connect/protocol";

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
// Connect's protocol headers, which it never relays from error metadata, and
// the hop-by-hop headers that belong to the host.
const UNRELAYED_HEADERS = new Set([
  "content-type",
  "content-length",
  "content-encoding",
  "accept-encoding",
  "host",
  "user-agent",
  "date",
  "trailer",
  "connection",
  "keep-alive",
  "proxy-connection",
  "te",
  "transfer-encoding",
  "upgrade",
  "http2-settings",
]);
type LongConnectDeadline = {
  deadlineAt: number;
  timeoutHeader: string;
};
const longConnectDeadlineKey = createContextKey<LongConnectDeadline | undefined>(undefined, {
  description: "Invariant long Connect deadline",
});
// Each request's values carry its principal and Invariant's private deadline
// key, so one instance must never serve two requests.
const issuedContextValues = new WeakSet<object>();

/** A host request with a byte-stream body. Hosts normalize every disconnect to {@link clientDisconnected}. */
export type HttpCoreRequest = UniversalServerRequest & { body: AsyncIterable<Uint8Array> };

/** Buffered responses carry bytes; only Connect server streams are lazy. */
export type HttpCoreResponse = {
  status: number;
  header: Headers;
  body: Uint8Array<ArrayBuffer> | AsyncIterable<Uint8Array>;
};

/** Never rejects. A streamed body releases its exchange exactly once. */
export type HttpCore = (
  request: HttpCoreRequest,
  contextValues: () => ContextValues | Promise<ContextValues>,
) => Promise<HttpCoreResponse>;

export function clientDisconnected(): ConnectError {
  return new ConnectError("client disconnected", ConnectCode.Canceled);
}

export function createHttpCore(server: Server, requestPathPrefix = ""): HttpCore {
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
    rpcTools.set(`/${tool.serviceFullName}/${tool.methodName}`, tool);
    if (tool.serverStreaming) {
      router.rpc(
        tool.methodDesc as never,
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
        tool.methodDesc as never,
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

  const route = async (
    request: HttpCoreRequest,
    resolveContextValues: () => ContextValues | Promise<ContextValues>,
  ): Promise<HttpCoreResponse> => {
    const pathname = new URL(request.url).pathname;
    const path =
      pathname === requestPathPrefix
        ? "/"
        : pathname.startsWith(`${requestPathPrefix}/`)
          ? pathname.slice(requestPathPrefix.length)
          : undefined;
    if (path === undefined) {
      return errorResponse(notFound("Not found"), server.maxUnaryResponseBytes());
    }
    if (request.method === "GET" && (path === "/healthz" || path === "/readyz")) {
      return jsonResponse(200, { status: "ok" });
    }
    const contextValues = await issueContextValues(resolveContextValues);
    if (request.signal.aborted) {
      return errorResponse(request.signal.reason, server.maxUnaryResponseBytes());
    }
    if (request.method === "GET" && (path === "/" || path === "/__invariant/tools")) {
      return jsonResponse(200, { tools: server.toolCatalog() });
    }
    if (request.method === "GET" && path === "/__invariant/descriptor.binpb") {
      return bufferedResponse(
        200,
        new Headers({ "content-type": PROTO_CONTENT_TYPE }),
        new Uint8Array(server[serverInternal].parsed().bytes),
      );
    }
    if (path === "/mcp") {
      return serveMcp(server, request, contextValues);
    }
    const handler = rpcHandlers.get(path);
    const tool = rpcTools.get(path);
    if (!handler || !tool) {
      return errorResponse(notFound("Not found"), server.maxUnaryResponseBytes());
    }
    return serveRpc(server, handler, tool, request, contextValues);
  };

  return async (request, resolveContextValues) => {
    try {
      return await route(request, resolveContextValues);
    } catch (e) {
      return errorResponse(e, server.maxUnaryResponseBytes());
    }
  };
}

async function serveRpc(
  server: Server,
  handler: UniversalHandlerFn,
  tool: RegisteredTool,
  request: HttpCoreRequest,
  contextValues: ContextValues,
): Promise<HttpCoreResponse> {
  const stopped = new AbortController();
  const scope = longConnectDeadlineScope({
    ...request,
    signal: AbortSignal.any([request.signal, stopped.signal]),
    contextValues,
  });
  let response: UniversalServerResponse;
  try {
    response = await handler(scope.request);
  } catch (e) {
    scope.cleanup();
    throw e;
  }
  if (tool.serverStreaming && response.body) {
    return {
      status: response.status,
      header: new Headers(response.header),
      body: lifecycle(
        response.body[Symbol.asyncIterator](),
        server.maxStreamResponseBytes(tool),
        request.signal,
        stopped,
        scope.cleanup,
      ),
    };
  }
  try {
    return await boundUnaryResponse(response, server.maxUnaryResponseBytes(tool));
  } finally {
    scope.cleanup();
  }
}

async function serveMcp(
  server: Server,
  request: HttpCoreRequest,
  contextValues: ContextValues,
): Promise<HttpCoreResponse> {
  const maxResponseBytes = server.maxUnaryResponseBytes();
  if (request.header.has("origin")) {
    return jsonResponse(403, { error: "Origin is not accepted by the MCP endpoint." }, maxResponseBytes);
  }
  if (request.method !== "POST") {
    return bufferedResponse(405, new Headers({ allow: "POST" }), Buffer.alloc(0));
  }
  if (!acceptsMcpResponse(request.header.get("accept"))) {
    return jsonResponse(
      406,
      {
        error: "Accept must list both application/json and text/event-stream.",
      },
      maxResponseBytes,
    );
  }
  if (!matchesContentType(request.header.get("content-type"), "application/json")) {
    return jsonResponse(415, { error: "Content-Type must be application/json." }, maxResponseBytes);
  }

  const timeoutMs = connectTimeoutMs(request.header.get("connect-timeout-ms"));
  const deadlineAt = timeoutMs === undefined ? undefined : monotonicDeadlineAfter(timeoutMs);
  const cancellation = new AbortController();
  let timedOut = false;
  const deadlineError = new ConnectError("deadline exceeded", ConnectCode.DeadlineExceeded);
  const expireDeadline = () => {
    timedOut = true;
    cancellation.abort(deadlineError);
  };
  const cleanupDeadline = scheduleAbsoluteDeadline(deadlineAt, expireDeadline);
  const disconnect = () => cancellation.abort(request.signal.reason);
  request.signal.addEventListener("abort", disconnect);
  const header = new Headers();
  try {
    const body = await withAbsoluteDeadline(
      () => readBody(request, server.maxUnaryRequestBytes()),
      cancellation.signal,
      deadlineAt,
      expireDeadline,
    );
    let msg: unknown;
    try {
      msg = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(body));
    } catch (e) {
      assertAbsoluteDeadline(deadlineAt, cancellation.signal, expireDeadline);
      return jsonResponse(
        200,
        {
          jsonrpc: "2.0",
          id: null,
          error: { code: -32700, message: `Parse error: ${String(e)}` },
        },
        maxResponseBytes,
      );
    }
    const invalid = invalidRequestResponse(msg);
    if (invalid) {
      assertAbsoluteDeadline(deadlineAt, cancellation.signal, expireDeadline);
      return jsonResponse(200, invalid, maxResponseBytes);
    }
    const message = msg as Record<string, unknown>;

    const protocolVersion = request.header.get("mcp-protocol-version");
    const isInitialize = message.method === "initialize";
    const unsupportedProtocol = protocolVersion !== null && protocolVersion !== MCP_PROTOCOL_VERSION;
    if ((!isInitialize && protocolVersion !== MCP_PROTOCOL_VERSION) || unsupportedProtocol) {
      assertAbsoluteDeadline(deadlineAt, cancellation.signal, expireDeadline);
      return jsonResponse(
        400,
        {
          error: `MCP-Protocol-Version must be ${MCP_PROTOCOL_VERSION}.`,
        },
        maxResponseBytes,
      );
    }
    header.set("mcp-protocol-version", MCP_PROTOCOL_VERSION);

    if (isClientResponse(message)) {
      assertAbsoluteDeadline(deadlineAt, cancellation.signal, expireDeadline);
      return bufferedResponse(202, header, Buffer.alloc(0));
    }

    const response = await withAbsoluteDeadline(
      () =>
        mcpDispatchWithContext(server, message, {
          protocolName: "mcp",
          requestMethod: request.method,
          url: request.url,
          deadlineAt,
          requestSignal: cancellation.signal,
          requestHeader: request.header,
          contextValues,
          mapHTTPMetadata: true,
          maxResponseBytes,
        }),
      cancellation.signal,
      deadlineAt,
      expireDeadline,
    );
    if (!response) {
      return bufferedResponse(202, header, Buffer.alloc(0));
    }
    return jsonResponse(200, response, maxResponseBytes, header);
  } catch (error) {
    if (timedOut) {
      return errorResponse(
        new InvariantError("deadline_exceeded", `deadline exceeded after ${timeoutMs ?? 0}ms`),
        maxResponseBytes,
        header,
      );
    }
    return errorResponse(error, maxResponseBytes, header);
  } finally {
    cleanupDeadline();
    request.signal.removeEventListener("abort", disconnect);
  }
}

async function issueContextValues(
  resolveContextValues: () => ContextValues | Promise<ContextValues>,
): Promise<ContextValues> {
  const values = (await resolveContextValues()) as Partial<ContextValues> | null | undefined;
  if (
    typeof values?.get !== "function" ||
    typeof values.set !== "function" ||
    typeof values.delete !== "function" ||
    issuedContextValues.has(values)
  ) {
    throw new InvariantError("internal", "contextValues must return a new ContextValues for every request");
  }
  issuedContextValues.add(values);
  return values as ContextValues;
}

/**
 * Releases a streamed exchange exactly once: when the stream ends or fails,
 * when the host stops reading, or when the client disconnects.
 */
function lifecycle(
  source: AsyncIterator<Uint8Array>,
  maxResponseBytes: number,
  hostSignal: AbortSignal,
  stopped: AbortController,
  cleanup: () => void,
): AsyncIterableIterator<Uint8Array> {
  const inner = boundConnectStreamBody({ [Symbol.asyncIterator]: () => source }, maxResponseBytes)[
    Symbol.asyncIterator
  ]();
  let released = false;
  const release = () => {
    if (!released) {
      released = true;
      hostSignal.removeEventListener("abort", stop);
      cleanup();
    }
  };
  const stop = () => {
    if (released) {
      return;
    }
    // Abort before return(): a handler suspended in an await only observes its
    // signal, and an async generator queues return() behind a pending next().
    stopped.abort(clientDisconnected());
    release();
    void inner.return?.().catch(() => undefined);
    // A generator that never started ignores return(), so close Connect's
    // stream as well: that runs the handler's finally and clears Connect's
    // deadline timer even when the host stops before its first read.
    void source.return?.().catch(() => undefined);
  };
  if (hostSignal.aborted) {
    stop();
  } else {
    hostSignal.addEventListener("abort", stop, { once: true });
  }
  return {
    async next() {
      try {
        const result = await inner.next();
        if (result.done) {
          release();
        }
        return result;
      } catch (e) {
        release();
        throw e;
      }
    },
    async return() {
      stop();
      return { done: true, value: undefined };
    },
    async throw() {
      stop();
      return { done: true, value: undefined };
    },
    [Symbol.asyncIterator]() {
      return this;
    },
  };
}

function bufferedResponse(status: number, header: Headers, body: Uint8Array<ArrayBuffer>): HttpCoreResponse {
  header.set("content-length", String(body.byteLength));
  return { status, header, body };
}

function jsonResponse(
  status: number,
  payload: unknown,
  maxResponseBytes = 0,
  header = new Headers(),
): HttpCoreResponse {
  const body = Buffer.from(JSON.stringify(payload));
  if (maxResponseBytes > 0 && body.length > maxResponseBytes) {
    return errorResponse(
      new InvariantError("resource_exhausted", "encoded MCP response exceeds configured byte limit"),
      maxResponseBytes,
      header,
    );
  }
  header.set("content-type", "application/json");
  return bufferedResponse(status, header, body);
}

/** Never throws: it renders hook failures, the core's last resort. */
function errorResponse(e: unknown, maxResponseBytes: number, header = new Headers()): HttpCoreResponse {
  let err: InvariantError;
  let relayMetadata: boolean;
  try {
    // Like Connect's handlers, relay the metadata of application errors only: a
    // ConnectError parsed from an upstream response carries that response's
    // headers.
    relayMetadata = !(e instanceof ConnectError && e.isWireError);
    // Like a handler's, a hook's InvariantError goes through ConnectError, so a
    // code outside Code becomes unknown and only Connect-shaped details remain.
    err = asInvariantError(e instanceof InvariantError ? toConnectError(e) : e);
  } catch {
    // A value that defeats inspection, such as a revoked Proxy or a throwing getter.
    err = new InvariantError("internal", "internal error");
    relayMetadata = false;
  }
  let body = Buffer.from(JSON.stringify(err.toPayload()));
  if (maxResponseBytes > 0 && body.length > maxResponseBytes) {
    err = new InvariantError("resource_exhausted", "encoded error response exceeds configured byte limit");
    body = Buffer.from(JSON.stringify(err.toPayload()));
    if (body.length > maxResponseBytes) {
      body = Buffer.alloc(0);
    }
  }
  if (relayMetadata) {
    err.metadata.forEach((value, key) => {
      if (!UNRELAYED_HEADERS.has(key) && !key.startsWith("connect-") && !key.startsWith("grpc-")) {
        header.append(key, value);
      }
    });
  }
  header.set("content-type", "application/json");
  return bufferedResponse(httpStatusFor(err.code), header, body);
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
): Promise<HttpCoreResponse> {
  const chunks: Uint8Array[] = [];
  if (response.body) {
    for await (const chunk of response.body) {
      chunks.push(chunk);
    }
  }
  const body = Buffer.concat(chunks);
  const header = new Headers(response.header);
  if (maxResponseBytes > 0 && body.length > maxResponseBytes) {
    let exhausted = Buffer.from(
      JSON.stringify({
        code: "resource_exhausted",
      }),
    );
    if (exhausted.length > maxResponseBytes) {
      exhausted = Buffer.alloc(0);
    }
    header.set("content-type", "application/json");
    header.delete("content-encoding");
    return bufferedResponse(429, header, exhausted);
  }
  return bufferedResponse(response.status, header, body);
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

async function readBody(request: HttpCoreRequest, maxBytes: number): Promise<Buffer> {
  const declared = request.header.get("content-length");
  // A declared length is exact. Rejecting it unread leaves the body for the
  // host to discard, so the connection stays usable.
  if (declared !== null && Number(declared) > maxBytes) {
    throw new InvariantError("resource_exhausted", `request body exceeds ${maxBytes} byte limit`);
  }
  const chunks: Uint8Array[] = [];
  let total = 0;
  for await (const chunk of request.body) {
    total += chunk.byteLength;
    if (total > maxBytes) {
      throw new InvariantError("resource_exhausted", `request body exceeds ${maxBytes} byte limit`);
    }
    chunks.push(chunk);
  }
  return Buffer.concat(chunks);
}

function connectTimeoutMs(raw: string | null): number | undefined {
  if (raw === null) {
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

function acceptsMcpResponse(value: string | null): boolean {
  if (value === null) {
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

function matchesContentType(value: string | null, expected: string): boolean {
  return value?.split(";", 1)[0]?.trim().toLowerCase() === expected;
}
