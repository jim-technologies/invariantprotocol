import { readFileSync } from "node:fs";
import {
  createServer,
  type IncomingHttpHeaders,
  type IncomingMessage,
  type OutgoingHttpHeaders,
  type ServerResponse,
} from "node:http";
import {
  type ClientHttp2Session,
  connect as connectHttp2,
  createServer as createHttp2Server,
  type Http2Session,
} from "node:http2";
import { type AddressInfo, connect } from "node:net";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import {
  Code,
  ConnectError,
  type ContextValues,
  createClient,
  createContextKey,
  createContextValues,
  type ServiceImpl,
} from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-node";
import { afterEach, describe, expect, expectTypeOf, test, vi } from "vitest";

import { InvariantError, Server } from "../src/index.js";
import { GreetService } from "./gen/greet_pb.js";
import { forceGc, type Host, ResponseAborted, startFetchHost, startNodeHost } from "./http_hosts.js";

const here = dirname(fileURLToPath(import.meta.url));
const descriptorPath = resolve(here, "../../python/tests/proto/descriptor.binpb");
const GREET = "/greet.v1.GreetService/Greet";
const STREAM_GREET = "/greet.v1.GreetService/StreamGreet";
const MCP_UNVERSIONED = { accept: "application/json, text/event-stream", "content-type": "application/json" };
const MCP_HEADERS: Record<string, string> = { ...MCP_UNVERSIONED, "mcp-protocol-version": "2025-11-25" };
const INITIALIZE = {
  jsonrpc: "2.0",
  id: 1,
  method: "initialize",
  params: { protocolVersion: "2025-11-25", capabilities: {}, clientInfo: { name: "hosts-test", version: "1.0.0" } },
};
const CHALLENGE = 'Bearer resource_metadata="https://example.com/.well-known/oauth-protected-resource/mcp"';
const REUSED = "contextValues must return a new ContextValues for every request";

type Principal = { subject: string; scopes: string[] };
const principalKey = createContextKey<Principal | undefined>(undefined, { description: "verified caller" });

type HostKind = "node" | "fetch";
type HookRequest = IncomingMessage | Request;
type Hook = (request: HookRequest) => ContextValues | Promise<ContextValues>;
type Observed = {
  invocations: number;
  finals: number;
  reasons: unknown[];
  principals: (string | undefined)[];
  urls: string[];
  headers: Headers[];
};

const hosts: Host[] = [];

afterEach(async () => {
  await Promise.all(hosts.splice(0).map((host) => host.close()));
});

function observe(): Observed {
  return { invocations: 0, finals: 0, reasons: [], principals: [], urls: [], headers: [] };
}

function greeter(observed: Observed): ServiceImpl<typeof GreetService> {
  const record = (context: { values: ContextValues; url: string; requestHeader: Headers }) => {
    observed.invocations++;
    observed.principals.push(context.values.get(principalKey)?.subject);
    observed.urls.push(context.url);
    observed.headers.push(new Headers(context.requestHeader));
    return context.values.get(principalKey)?.subject ?? "";
  };
  return {
    async greet(request, context) {
      const subject = record(context);
      switch (request.name) {
        case "fail":
          throw new ConnectError("greeting refused", Code.PermissionDenied, { "x-reason": "policy" });
        case "slow":
          await settle(context.signal, 2_000);
          return { message: "late" };
        case "wait":
          await settle(context.signal);
          observed.reasons.push(context.signal.reason);
          throw context.signal.reason;
        case "big":
          return { message: "x".repeat(2048) };
        case "remember":
          context.values.set(principalKey, { subject: "remembered", scopes: [] });
          return { message: "Hi remember", responseLabel: subject };
        case "control":
          context.responseHeader.set("x-label", "v\x01");
          return { message: "Hi control" };
        case "upgrade":
          context.responseHeader.set("upgrade", "h2c");
          return { message: "Hi upgrade" };
      }
      return { message: `Hi ${request.name}`, responseLabel: subject };
    },
    async greetGroup(request) {
      return { messages: request.people.map((person) => person.name), count: request.people.length };
    },
    async *streamGreet(request, context) {
      const subject = record(context);
      try {
        if (request.name === "big") {
          yield { message: "x".repeat(2048) };
          return;
        }
        for (let i = 1; i <= Math.max(request.count, 1); i++) {
          yield { message: `Hi ${request.name} #${i}`, responseLabel: subject };
        }
        if (request.name === "wait") {
          await settle(context.signal);
        } else if (request.name === "slow") {
          await settle(context.signal, 2_000);
          yield { message: "late" };
        }
      } finally {
        observed.finals++;
        if (context.signal.aborted) {
          observed.reasons.push(context.signal.reason);
        }
      }
    },
  };
}

function greetServer(observed = observe(), configure?: (server: Server) => void): Server {
  const server = Server.fromDescriptor(descriptorPath);
  configure?.(server);
  server.register(GreetService, greeter(observed));
  return server;
}

async function start(kind: HostKind, server: Server, contextValues?: Hook): Promise<Host> {
  const options = contextValues ? { contextValues } : undefined;
  const host = kind === "node" ? await startNodeHost(server, options) : await startFetchHost(server, options);
  hosts.push(host);
  return host;
}

function client(host: Host, useBinaryFormat = false) {
  return createClient(
    GreetService,
    createConnectTransport({ baseUrl: host.baseUrl, httpVersion: "1.1", useBinaryFormat }),
  );
}

function post(
  host: Host,
  path: string,
  body: unknown,
  headers: Record<string, string> = {},
  signal?: AbortSignal,
): Promise<Response> {
  return fetch(`${host.baseUrl}${path}`, {
    method: "POST",
    headers: { "content-type": "application/json", ...headers },
    body: typeof body === "string" ? body : JSON.stringify(body),
    signal,
  });
}

function greet(host: Host, body: unknown, headers?: Record<string, string>, signal?: AbortSignal): Promise<Response> {
  return post(host, GREET, body, headers, signal);
}

function streamGreet(host: Host, body: unknown, headers: Record<string, string> = {}): Promise<Response> {
  return fetch(`${host.baseUrl}${STREAM_GREET}`, {
    method: "POST",
    headers: { "content-type": "application/connect+json", ...headers },
    body: envelope(body),
  });
}

function mcp(host: Host, message: unknown, headers: Record<string, string> = MCP_HEADERS): Promise<Response> {
  return post(host, "/mcp", message, headers);
}

function callTool(method: "Greet" | "StreamGreet", args: Record<string, unknown>) {
  return {
    jsonrpc: "2.0",
    id: 7,
    method: "tools/call",
    params: { name: `greet.v1.GreetService.${method}`, arguments: args },
  };
}

async function toolReplies(response: Response): Promise<Record<string, unknown>[]> {
  expect(response.status).toBe(200);
  const { result } = (await response.json()) as { result: { content: { text: string }[] } };
  return result.content.map((block) => JSON.parse(block.text) as Record<string, unknown>);
}

function envelope(payload: unknown): Buffer<ArrayBuffer> {
  const json = Buffer.from(JSON.stringify(payload));
  const frame = Buffer.alloc(5 + json.length);
  frame.writeUInt32BE(json.length, 1);
  json.copy(frame, 5);
  return frame;
}

async function frames(response: Response): Promise<{ end: boolean; payload: any }[]> {
  expect(response.status).toBe(200);
  const bytes = Buffer.from(await response.arrayBuffer());
  const out: { end: boolean; payload: any }[] = [];
  for (let offset = 0; offset + 5 <= bytes.length; ) {
    const length = bytes.readUInt32BE(offset + 1);
    out.push({
      end: (bytes[offset]! & 0x02) !== 0,
      payload: JSON.parse(bytes.subarray(offset + 5, offset + 5 + length).toString()),
    });
    offset += 5 + length;
  }
  return out;
}

async function drain<T>(stream: AsyncIterable<T>): Promise<T[]> {
  const items: T[] = [];
  for await (const item of stream) {
    items.push(item);
  }
  return items;
}

function settle(signal: AbortSignal, ms?: number): Promise<void> {
  return new Promise((resolve) => {
    const timer = ms === undefined ? undefined : setTimeout(resolve, ms);
    const done = () => {
      clearTimeout(timer);
      resolve();
    };
    if (signal.aborted) {
      done();
    } else {
      signal.addEventListener("abort", done, { once: true });
    }
  });
}

function deferred(): { promise: Promise<void>; resolve: () => void } {
  let resolve = () => {};
  const promise = new Promise<void>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

function headerOf(request: HookRequest, name: string): string | undefined {
  if (request instanceof Request) {
    return request.headers.get(name) ?? undefined;
  }
  const value = request.headers[name];
  return Array.isArray(value) ? value.join(", ") : value;
}

/** Resolves once the host has seen the client go away. */
function clientGone(request: HookRequest): Promise<void> {
  return new Promise((resolve) => {
    if (request instanceof Request) {
      request.signal.addEventListener("abort", () => resolve(), { once: true });
    } else {
      request.socket.once("close", () => resolve());
    }
  });
}

function headerNames(header: Headers): string[] {
  const names: string[] = [];
  header.forEach((_value, name) => {
    names.push(name);
  });
  return names.sort();
}

function chunkedBody(size: number): ReadableStream<Uint8Array> {
  let sent = 0;
  return new ReadableStream({
    pull(controller) {
      if (sent >= size) {
        controller.close();
        return;
      }
      controller.enqueue(new Uint8Array(64 * 1024).fill(0x20));
      sent += 64 * 1024;
    },
  });
}

/**
 * Uploads a complete over-limit POST /mcp body on one raw connection, then
 * sends GET /healthz on the same connection. With "deadline", the client sends
 * 100 bytes and the rest only after its 200 ms deadline's response. Resolves
 * both status lines.
 */
function overLimitThenHealth(host: Host, size: number, framing: "length" | "chunked" | "deadline"): Promise<string[]> {
  return new Promise((resolveStatuses) => {
    const statuses: string[] = [];
    let received = Buffer.alloc(0);
    const send = (data: Buffer) =>
      socket.write(
        framing === "length"
          ? data
          : Buffer.concat([Buffer.from(`${data.length.toString(16)}\r\n`), data, Buffer.from("\r\n")]),
      );
    const upload = () => {
      for (let sent = 0; sent < size; sent += 64 * 1024) {
        send(Buffer.alloc(64 * 1024, 0x20));
      }
      socket.write(framing === "length" ? "" : "0\r\n\r\n");
    };
    const socket = connect(Number(new URL(host.baseUrl).port), "127.0.0.1", () => {
      const head =
        framing === "length"
          ? `content-length: ${size}`
          : `transfer-encoding: chunked${framing === "deadline" ? "\r\nconnect-timeout-ms: 200" : ""}`;
      socket.write(
        `POST /mcp HTTP/1.1\r\nhost: example.com\r\naccept: ${MCP_HEADERS.accept}\r\n` +
          `content-type: application/json\r\nmcp-protocol-version: 2025-11-25\r\n${head}\r\n\r\n`,
      );
      if (framing === "deadline") {
        send(Buffer.alloc(100, 0x20));
      } else {
        upload();
      }
    });
    socket.setTimeout(10_000, () => socket.destroy());
    socket.on("error", () => undefined);
    socket.on("close", () => resolveStatuses(statuses));
    socket.on("data", (data: Buffer) => {
      received = Buffer.concat([received, data]);
      for (let end = received.indexOf("\r\n\r\n"); end >= 0; end = received.indexOf("\r\n\r\n")) {
        const head = received.subarray(0, end).toString("latin1");
        const length = Number(/content-length: (\d+)/i.exec(head)?.[1] ?? 0);
        if (received.length < end + 4 + length) {
          return;
        }
        statuses.push(head.slice(0, head.indexOf("\r\n")));
        received = received.subarray(end + 4 + length);
        if (statuses.length === 1) {
          if (framing === "deadline") {
            upload();
          }
          socket.write("GET /healthz HTTP/1.1\r\nhost: example.com\r\n\r\n");
        } else {
          socket.destroy();
        }
      }
    });
  });
}

/** An upstream Connect server that rejects every call with headers of its own. */
async function startUpstream(): Promise<Host> {
  const http = createServer((_req, res) => {
    res.writeHead(401, {
      "content-type": "application/json",
      "set-cookie": "session=upstream",
      "x-upstream": "internal",
    });
    res.end(JSON.stringify({ code: "unauthenticated", message: "upstream rejected the token" }));
  });
  await new Promise<void>((resolveListen) => http.listen(0, "127.0.0.1", resolveListen));
  const host = {
    baseUrl: `http://127.0.0.1:${(http.address() as AddressInfo).port}`,
    handled: [],
    close: () =>
      new Promise<void>((resolveClose) => {
        http.closeAllConnections();
        http.close(() => resolveClose());
      }),
  };
  hosts.push(host);
  return host;
}

function h2Exchange(
  session: ClientHttp2Session,
  headers: OutgoingHttpHeaders,
  body?: string,
): Promise<{ status: number; headers: IncomingHttpHeaders; body: string }> {
  return new Promise((resolveExchange, rejectExchange) => {
    const stream = session.request(headers);
    let received: IncomingHttpHeaders = {};
    let text = "";
    stream.setEncoding("utf8");
    stream.on("response", (responseHeaders) => {
      received = responseHeaders;
    });
    stream.on("data", (chunk: string) => {
      text += chunk;
    });
    stream.on("end", () => resolveExchange({ status: Number(received[":status"]), headers: received, body: text }));
    stream.on("error", rejectExchange);
    stream.end(body);
  });
}

/** Writes one raw request and collects everything until the server closes the connection. */
function rawExchange(host: Host, request: string): Promise<string> {
  return new Promise((resolveExchange) => {
    const socket = connect(Number(new URL(host.baseUrl).port), "127.0.0.1", () => socket.write(request));
    let response = "";
    socket.setEncoding("latin1");
    socket.on("data", (data: string) => {
      response += data;
    });
    socket.on("error", () => undefined);
    socket.on("close", () => resolveExchange(response));
  });
}

describe.each(["node", "fetch"] as const)("%s host", (kind) => {
  test("serves Connect, MCP, discovery, health, and not-found routes", async () => {
    const host = await start(kind, greetServer());
    for (const useBinaryFormat of [false, true]) {
      const greeter = client(host, useBinaryFormat);
      expect((await greeter.greet({ name: "Ada" })).message).toBe("Hi Ada");
      expect((await drain(greeter.streamGreet({ name: "Ada", count: 3 }))).map((reply) => reply.message)).toEqual([
        "Hi Ada #1",
        "Hi Ada #2",
        "Hi Ada #3",
      ]);
      const refused = ConnectError.from(await greeter.greet({ name: "fail" }).catch((e: unknown) => e));
      expect([refused.code, refused.rawMessage, refused.metadata.get("x-reason")]).toEqual([
        Code.PermissionDenied,
        "greeting refused",
        "policy",
      ]);
    }
    const refused = await greet(host, { name: "fail" });
    expect([refused.status, refused.headers.get("x-reason")]).toEqual([403, "policy"]);
    expect(await refused.json()).toMatchObject({ code: "permission_denied", message: "greeting refused" });

    for (const path of ["/", "/__invariant/tools"]) {
      const catalog = await fetch(`${host.baseUrl}${path}`);
      const { tools } = (await catalog.json()) as { tools: { name: string }[] };
      expect([catalog.status, tools.map((tool) => tool.name)]).toEqual([
        200,
        ["greet.v1.GreetService.Greet", "greet.v1.GreetService.GreetGroup", "greet.v1.GreetService.StreamGreet"],
      ]);
    }
    const descriptor = await fetch(`${host.baseUrl}/__invariant/descriptor.binpb`);
    expect(descriptor.headers.get("content-type")).toBe("application/proto");
    expect(Buffer.from(await descriptor.arrayBuffer()).equals(readFileSync(descriptorPath))).toBe(true);
    for (const path of ["/healthz", "/readyz"]) {
      const health = await fetch(`${host.baseUrl}${path}`);
      expect([health.status, await health.json()]).toEqual([200, { status: "ok" }]);
    }
    const missing = await post(host, "/greet.v1.GreetService/Missing", {});
    expect([missing.status, await missing.json()]).toEqual([
      404,
      { code: "not_found", message: "Not found", details: [] },
    ]);

    const initialized = await mcp(host, INITIALIZE, MCP_UNVERSIONED);
    expect([initialized.status, initialized.headers.get("mcp-protocol-version")]).toEqual([200, "2025-11-25"]);
    expect(await initialized.json()).toMatchObject({ id: 1, result: { protocolVersion: "2025-11-25" } });
    const pong = await mcp(host, { jsonrpc: "2.0", id: 2, method: "ping" });
    expect(await pong.json()).toEqual({ jsonrpc: "2.0", id: 2, result: {} });
    const listed = (await (await mcp(host, { jsonrpc: "2.0", id: 3, method: "tools/list" })).json()) as {
      result: { tools: { name: string }[] };
    };
    expect(listed.result.tools.map((tool) => tool.name)).toContain("greet.v1.GreetService.StreamGreet");
    expect(await toolReplies(await mcp(host, callTool("Greet", { name: "Ada" })))).toMatchObject([
      { message: "Hi Ada" },
    ]);
    expect(await toolReplies(await mcp(host, callTool("StreamGreet", { name: "Ada", count: 2 })))).toMatchObject([
      { message: "Hi Ada #1" },
      { message: "Hi Ada #2" },
    ]);
    const notified = await mcp(host, { jsonrpc: "2.0", method: "notifications/initialized" });
    expect([notified.status, await notified.text()]).toEqual([202, ""]);
    expect((await mcp(host, INITIALIZE, { ...MCP_HEADERS, origin: "https://example.com" })).status).toBe(403);
    const get = await fetch(`${host.baseUrl}/mcp`);
    expect([get.status, get.headers.get("allow")]).toEqual([405, "POST"]);
    expect((await mcp(host, INITIALIZE, { ...MCP_HEADERS, accept: "application/json" })).status).toBe(406);
    expect((await mcp(host, INITIALIZE, { ...MCP_HEADERS, "content-type": "text/plain" })).status).toBe(415);
    const unsupported = { ...MCP_HEADERS, "mcp-protocol-version": "2024-11-05" };
    expect((await mcp(host, { jsonrpc: "2.0", id: 4, method: "ping" }, unsupported)).status).toBe(400);
    expect(await (await mcp(host, "{")).json()).toMatchObject({ id: null, error: { code: -32700 } });
    expect(await (await mcp(host, [])).json()).toMatchObject({ id: null, error: { code: -32600 } });
  });

  test("enforces deadlines and releases long-deadline timers", async () => {
    const host = await start(kind, greetServer());
    const malformed = await greet(host, { name: "Ada" }, { "connect-timeout-ms": "soon" });
    expect([malformed.status, (await malformed.json()).code]).toEqual([400, "invalid_argument"]);
    const malformedMcp = await mcp(host, callTool("Greet", { name: "Ada" }), {
      ...MCP_HEADERS,
      "connect-timeout-ms": "0",
    });
    expect([malformedMcp.status, (await malformedMcp.json()).code]).toEqual([400, "invalid_argument"]);

    const expired = await greet(host, { name: "slow" }, { "connect-timeout-ms": "50" });
    expect([expired.status, (await expired.json()).code]).toEqual([504, "deadline_exceeded"]);
    const expiredMcp = await mcp(host, callTool("Greet", { name: "slow" }), {
      ...MCP_HEADERS,
      "connect-timeout-ms": "50",
    });
    expect([expiredMcp.status, (await expiredMcp.json()).code]).toEqual([504, "deadline_exceeded"]);
    const expiredStream = await frames(await streamGreet(host, { name: "slow" }, { "connect-timeout-ms": "50" }));
    expect(expiredStream.at(-1)).toMatchObject({ end: true, payload: { error: { code: "deadline_exceeded" } } });

    // Deadlines beyond Node's timer range re-arm a timer that each exchange must clear.
    const activeTimers = () => process.getActiveResourcesInfo().filter((resource) => resource === "Timeout").length;
    const before = activeTimers();
    const long = { "connect-timeout-ms": "9999999999", connection: "close" };
    const unary = await greet(host, { name: "Ada" }, long);
    expect([unary.status, (await unary.json()).message]).toEqual([200, "Hi Ada"]);
    expect((await frames(await streamGreet(host, { name: "Ada", count: 2 }, long))).at(-1)).toEqual({
      end: true,
      payload: {},
    });
    for (let attempt = 0; attempt < 100 && activeTimers() > before; attempt++) {
      await new Promise((resolveDelay) => setTimeout(resolveDelay, 20));
    }
    expect(activeTimers()).toBeLessThanOrEqual(before);
  });

  test("bounds message sizes and answers an over-limit MCP body on a usable connection", async () => {
    const host = await start(
      kind,
      greetServer(observe(), (server) => {
        server.setMaxUnaryRequestBytes(1024);
        server.setMaxUnaryResponseBytes(1024);
        server.setMaxStreamResponseBytes(1024);
      }),
    );
    const largeRequest = await greet(host, { name: "x".repeat(2048) });
    expect([largeRequest.status, (await largeRequest.json()).code]).toEqual([429, "resource_exhausted"]);
    const largeResponse = await greet(host, { name: "big" });
    expect([largeResponse.status, (await largeResponse.json()).code]).toEqual([429, "resource_exhausted"]);
    expect((await frames(await streamGreet(host, { name: "big" }))).at(-1)).toMatchObject({
      end: true,
      payload: { error: { code: "resource_exhausted" } },
    });

    const declared = "x".repeat(8 * 1024 * 1024);
    for (const body of [declared, chunkedBody(8 * 1024 * 1024)]) {
      const response = await fetch(`${host.baseUrl}/mcp`, {
        method: "POST",
        headers: MCP_HEADERS,
        body,
        duplex: "half",
        signal: AbortSignal.timeout(10_000),
      } as RequestInit);
      expect([response.status, await response.json()]).toEqual([
        429,
        { code: "resource_exhausted", message: "request body exceeds 1024 byte limit", details: [] },
      ]);
      const pong = await mcp(host, { jsonrpc: "2.0", id: 1, method: "ping" });
      expect([pong.status, (await pong.json()).result]).toEqual([200, {}]);
    }

    // A client that finishes its upload keeps its connection for the next
    // request: a declared length is rejected unread and left to the host, and
    // the rest of a partly read chunked body is discarded, also when the
    // deadline answered while the body was still being read.
    for (const framing of ["length", "chunked"] as const) {
      expect(await overLimitThenHealth(host, 192 * 1024, framing)).toEqual([
        "HTTP/1.1 429 Too Many Requests",
        "HTTP/1.1 200 OK",
      ]);
    }
    expect(await overLimitThenHealth(host, 192 * 1024, "deadline")).toEqual([
      "HTTP/1.1 504 Gateway Timeout",
      "HTTP/1.1 200 OK",
    ]);
  });

  test("cancels handlers with Canceled when the client disconnects and settles every exchange", async () => {
    const observed = observe();
    const host = await start(kind, greetServer(observed));
    const greeter = client(host);
    expect((await greeter.greet({ name: "Ada" })).message).toBe("Hi Ada");
    expect(await drain(greeter.streamGreet({ name: "Ada", count: 2 }))).toHaveLength(2);
    expect(ConnectError.from(await greeter.greet({ name: "fail" }).catch((e: unknown) => e)).code).toBe(
      Code.PermissionDenied,
    );

    const unary = new AbortController();
    const waiting = greeter.greet({ name: "wait" }, { signal: unary.signal }).catch((e: unknown) => e);
    await vi.waitFor(() => expect(observed.invocations).toBe(4));
    unary.abort();
    expect(ConnectError.from(await waiting).code).toBe(Code.Canceled);
    await vi.waitFor(() => expect(observed.reasons).toHaveLength(1));

    const streaming = new AbortController();
    const stream = greeter.streamGreet({ name: "wait" }, { signal: streaming.signal })[Symbol.asyncIterator]();
    expect((await stream.next()).value?.message).toBe("Hi wait #1");
    streaming.abort();
    await expect(stream.next()).rejects.toMatchObject({ code: Code.Canceled });
    await vi.waitFor(() => expect(observed.reasons).toHaveLength(2));
    await vi.waitFor(() => expect(observed.finals).toBe(2));

    const call = new AbortController();
    const calling = post(host, "/mcp", callTool("Greet", { name: "wait" }), MCP_HEADERS, call.signal).catch(() => null);
    await vi.waitFor(() => expect(observed.invocations).toBe(6));
    call.abort();
    expect(await calling).toBeNull();
    await vi.waitFor(() => expect(observed.reasons).toHaveLength(3));
    for (const reason of observed.reasons) {
      expect(reason).toBeInstanceOf(ConnectError);
      expect([(reason as ConnectError).code, (reason as ConnectError).rawMessage]).toEqual([
        Code.Canceled,
        "client disconnected",
      ]);
    }

    expect(host.handled).toHaveLength(6);
    await Promise.all(host.handled);
    expect(observed.finals).toBe(2);
  });

  test("calls contextValues once per in-mount request, before the body and the deadline", async () => {
    let calls = 0;
    const untouched: boolean[] = [];
    const host = await start(kind, greetServer(), async (request) => {
      calls++;
      untouched.push(request instanceof Request ? !request.bodyUsed : !request.readableDidRead);
      if (headerOf(request, "x-slow-hook")) {
        await new Promise((resolveDelay) => setTimeout(resolveDelay, 300));
      }
      return createContextValues();
    });
    const expectCalls = async (send: () => Promise<Response>, status: number, expected: number) => {
      const before = calls;
      const response = await send();
      await response.arrayBuffer();
      expect([response.status, calls - before]).toEqual([status, expected]);
    };
    await expectCalls(() => fetch(`${host.baseUrl}/healthz`), 200, 0);
    await expectCalls(() => fetch(`${host.baseUrl}/readyz`), 200, 0);
    await expectCalls(() => post(host, "/healthz", {}), 404, 1);
    await expectCalls(() => fetch(`${host.baseUrl}/`), 200, 1);
    await expectCalls(() => fetch(`${host.baseUrl}/__invariant/tools`), 200, 1);
    await expectCalls(() => fetch(`${host.baseUrl}/__invariant/descriptor.binpb`), 200, 1);
    await expectCalls(() => fetch(`${host.baseUrl}/unknown`), 404, 1);
    await expectCalls(() => mcp(host, INITIALIZE, { ...MCP_HEADERS, origin: "https://example.com" }), 403, 1);
    await expectCalls(() => fetch(`${host.baseUrl}/mcp`), 405, 1);
    await expectCalls(() => mcp(host, INITIALIZE, { ...MCP_HEADERS, accept: "application/json" }), 406, 1);
    await expectCalls(() => mcp(host, INITIALIZE, { ...MCP_HEADERS, "content-type": "text/plain" }), 415, 1);
    await expectCalls(() => mcp(host, INITIALIZE), 200, 1);
    await expectCalls(() => mcp(host, { jsonrpc: "2.0", method: "notifications/initialized" }), 202, 1);
    await expectCalls(() => greet(host, { name: "Ada" }), 200, 1);
    await expectCalls(() => streamGreet(host, { name: "Ada" }), 200, 1);
    // A 300 ms hook does not spend a 150 ms deadline: the deadline starts after it.
    const slow = { "connect-timeout-ms": "150", "x-slow-hook": "1" };
    await expectCalls(() => greet(host, { name: "Ada" }, slow), 200, 1);
    await expectCalls(() => mcp(host, callTool("Greet", { name: "Ada" }), { ...MCP_HEADERS, ...slow }), 200, 1);
    expect(untouched).toHaveLength(15);
    expect(untouched.every(Boolean)).toBe(true);
  });

  test("rejects at the HTTP level when contextValues fails, before any dispatch", async () => {
    const observed = observe();
    const shared = createContextValues();
    const hookEntered = deferred();
    const hookReturned = deferred();
    const upstream = await startUpstream();
    const host = await start(kind, greetServer(observed), async (request) => {
      switch (headerOf(request, "x-hook")) {
        case "unauthenticated":
          throw new ConnectError("a valid bearer token is required", Code.Unauthenticated, {
            "www-authenticate": CHALLENGE,
            "content-type": "text/plain",
            "grpc-status": "16",
            trailer: "x-late",
          });
        case "details":
          throw new InvariantError(
            "unauthenticated",
            "token expired",
            [{ exp: 1n }, { type: "example.v1.Expiry", value: "AQI=" }],
            { "www-authenticate": CHALLENGE },
          );
        case "uncoded":
          throw new InvariantError("unauthorized" as never, "token missing", [], { "www-authenticate": CHALLENGE });
        case "opaque":
          throw Object.create(null);
        case "revoked": {
          const { proxy, revoke } = Proxy.revocable(new Error("revoked"), {});
          revoke();
          throw proxy;
        }
        case "getter":
          throw Object.defineProperty(new Error("denied"), "name", {
            get() {
              throw new Error("name getter failed");
            },
          });
        case "upstream":
          await createClient(
            GreetService,
            createConnectTransport({ baseUrl: upstream.baseUrl, httpVersion: "1.1" }),
          ).greet({ name: "Ada" });
          return createContextValues();
        case "forbidden":
          throw new InvariantError("permission_denied", "the greet scope is required");
        case "broken":
          throw new TypeError("verifier unavailable");
        case "shared":
          return shared;
        case "bogus":
          return {} as ContextValues;
        case "gated": {
          const gone = clientGone(request);
          hookEntered.resolve();
          await gone;
          hookReturned.resolve();
          return createContextValues();
        }
      }
      return createContextValues();
    });

    const unauthenticated = { "x-hook": "unauthenticated" };
    for (const response of [
      await mcp(host, INITIALIZE, { ...MCP_HEADERS, ...unauthenticated }),
      await fetch(`${host.baseUrl}/mcp`, { headers: unauthenticated }),
      await fetch(`${host.baseUrl}/`, { headers: unauthenticated }),
      await greet(host, { name: "Ada" }, unauthenticated),
      await streamGreet(host, { name: "Ada" }, unauthenticated),
    ]) {
      expect([response.status, response.headers.get("www-authenticate")]).toEqual([401, CHALLENGE]);
      expect(response.headers.get("content-type")).toBe("application/json");
      expect([response.headers.get("grpc-status"), response.headers.get("trailer")]).toEqual([null, null]);
      expect(await response.json()).toEqual({
        code: "unauthenticated",
        message: "a valid bearer token is required",
        details: [],
      });
    }
    const unary = ConnectError.from(
      await client(host)
        .greet({ name: "Ada" }, { headers: unauthenticated })
        .catch((e: unknown) => e),
    );
    expect([unary.code, unary.metadata.get("www-authenticate")]).toEqual([Code.Unauthenticated, CHALLENGE]);
    const stream = ConnectError.from(
      await drain(client(host, true).streamGreet({ name: "Ada" }, { headers: unauthenticated })).catch(
        (e: unknown) => e,
      ),
    );
    expect(stream.code).toBe(Code.Unauthenticated);

    const forbidden = await greet(host, { name: "Ada" }, { "x-hook": "forbidden" });
    expect([forbidden.status, await forbidden.json()]).toEqual([
      403,
      { code: "permission_denied", message: "the greet scope is required", details: [] },
    ]);
    const broken = await mcp(host, INITIALIZE, { ...MCP_HEADERS, "x-hook": "broken" });
    expect([broken.status, await broken.json()]).toEqual([
      500,
      { code: "internal", message: "verifier unavailable", details: [] },
    ]);
    // As for a handler's error, only details in Connect's wire shape remain and
    // a code outside Code becomes unknown; the challenge stays.
    const detailed = await mcp(host, INITIALIZE, { ...MCP_HEADERS, "x-hook": "details" });
    expect([detailed.status, detailed.headers.get("www-authenticate")]).toEqual([401, CHALLENGE]);
    expect(await detailed.json()).toEqual({
      code: "unauthenticated",
      message: "token expired",
      details: [{ type: "example.v1.Expiry", value: "AQI=" }],
    });
    const expired = ConnectError.from(
      await client(host)
        .greet({ name: "Ada" }, { headers: { "x-hook": "details" } })
        .catch((e: unknown) => e),
    );
    expect([expired.code, expired.rawMessage, expired.details.length]).toEqual([
      Code.Unauthenticated,
      "token expired",
      1,
    ]);
    const uncoded = await greet(host, { name: "Ada" }, { "x-hook": "uncoded" });
    expect([uncoded.status, uncoded.headers.get("www-authenticate"), await uncoded.json()]).toEqual([
      500,
      CHALLENGE,
      { code: "unknown", message: "token missing", details: [] },
    ]);
    for (const hook of ["opaque", "revoked", "getter"]) {
      const hostile = await greet(host, { name: "Ada" }, { "x-hook": hook });
      expect([hostile.status, await hostile.json()]).toEqual([
        500,
        { code: "internal", message: "internal error", details: [] },
      ]);
    }
    // An error parsed from an upstream response relays none of its headers.
    const relayed = await greet(host, { name: "Ada" }, { "x-hook": "upstream" });
    expect([relayed.status, relayed.headers.get("set-cookie"), relayed.headers.get("x-upstream")]).toEqual([
      401,
      null,
      null,
    ]);
    expect(await relayed.json()).toEqual({
      code: "unauthenticated",
      message: "upstream rejected the token",
      details: [],
    });
    expect((await greet(host, { name: "Ada" }, { "x-hook": "shared" })).status).toBe(200);
    const reused = await greet(host, { name: "Ada" }, { "x-hook": "shared" });
    expect([reused.status, (await reused.json()).message]).toEqual([500, REUSED]);
    const bogus = await fetch(`${host.baseUrl}/`, { headers: { "x-hook": "bogus" } });
    expect([bogus.status, (await bogus.json()).message]).toEqual([500, REUSED]);
    expect(observed.invocations).toBe(1);

    const abandoned = new AbortController();
    const pending = greet(host, { name: "Ada" }, { "x-hook": "gated" }, abandoned.signal).catch(() => undefined);
    await hookEntered.promise;
    abandoned.abort();
    await pending;
    await hookReturned.promise;
    await new Promise((resolveDelay) => setTimeout(resolveDelay, 100));
    expect(observed.invocations).toBe(1);
  });

  test("hands the hook's values to handlers and interceptors while headers stay untrusted", async () => {
    const observed = observe();
    const intercepted: { principal?: string; header: Headers }[] = [];
    const tokens: (string | undefined)[] = [];
    const server = greetServer(observed, (configured) => {
      configured.use((next) => async (req) => {
        intercepted.push({ principal: req.contextValues.get(principalKey)?.subject, header: new Headers(req.header) });
        return next(req);
      });
    });
    const host = await start(kind, server, (request) => {
      const token = headerOf(request, "authorization");
      tokens.push(token);
      const values = createContextValues();
      return token === "Bearer alice-token"
        ? values.set(principalKey, { subject: "alice", scopes: ["greet"] })
        : values;
    });
    const headers = {
      authorization: "Bearer alice-token",
      "x-principal": "root",
      "invariant-internal-principal": "root",
      "x-user": "mallory",
      traceparent: "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
      "x-request-id": "req-1",
    };
    expect((await (await greet(host, { name: "Ada" }, headers)).json()).wireDisplayLabel).toBe("alice");
    const streamed = await frames(await streamGreet(host, { name: "Ada", count: 2 }, headers));
    expect(streamed.map((frame) => frame.payload.wireDisplayLabel)).toEqual(["alice", "alice", undefined]);
    const mcpHeaders = { ...MCP_HEADERS, ...headers };
    expect(await toolReplies(await mcp(host, callTool("Greet", { name: "Ada" }), mcpHeaders))).toMatchObject([
      { wireDisplayLabel: "alice" },
    ]);
    expect(await toolReplies(await mcp(host, callTool("StreamGreet", { name: "Ada" }), mcpHeaders))).toMatchObject([
      { wireDisplayLabel: "alice" },
    ]);

    expect(tokens).toEqual(Array(4).fill("Bearer alice-token"));
    expect(observed.principals).toEqual(Array(4).fill("alice"));
    expect(intercepted.map((call) => call.principal)).toEqual(Array(4).fill("alice"));
    for (const header of [...observed.headers, ...intercepted.map((call) => call.header)]) {
      expect(headerNames(header)).toEqual(["traceparent", "x-request-id"]);
    }

    // Without a hook every request still gets fresh values.
    const plain = await start(kind, greetServer());
    for (let i = 0; i < 2; i++) {
      expect((await (await greet(plain, { name: "remember" })).json()).wireDisplayLabel).toBeUndefined();
    }
  });
});

describe("fetch host", () => {
  test("validates the mount prefix and routes only inside it", async () => {
    const observed = observe();
    const server = greetServer(observed);
    for (const prefix of ["/", "api", "/api/", "/a//b", "/api?query", "/api#fragment", "/api/../rpc"]) {
      expect(() => server.fetchHandler({ requestPathPrefix: prefix })).toThrow(TypeError);
    }
    let calls = 0;
    const host = await startFetchHost(server, {
      requestPathPrefix: "/api/rpc",
      contextValues: () => {
        calls++;
        return createContextValues();
      },
    });
    hosts.push(host);
    const catalog = await fetch(`${host.baseUrl}/api/rpc`);
    expect([catalog.status, calls]).toEqual([200, 1]);
    expect((await (await post(host, `/api/rpc${GREET}`, { name: "Ada" })).json()).message).toBe("Hi Ada");
    const mcpResponse = await post(host, "/api/rpc/mcp", callTool("Greet", { name: "Ada" }), MCP_HEADERS);
    expect(await toolReplies(mcpResponse)).toMatchObject([{ message: "Hi Ada" }]);
    expect(observed.urls).toEqual([`${host.baseUrl}/api/rpc${GREET}`, `${host.baseUrl}/api/rpc/mcp`]);
    expect((await fetch(`${host.baseUrl}/api/rpc/healthz`)).status).toBe(200);
    for (const path of [`/api/rpcx${GREET}`, GREET, "/healthz", "/api"]) {
      const outside = await post(host, path, { name: "Ada" });
      expect([outside.status, (await outside.json()).code]).toEqual([404, "not_found"]);
    }
    expect(calls).toBe(3);
  });

  test("declares one Request parameter and never rejects", async () => {
    const handler = greetServer().fetchHandler({
      contextValues: () => {
        throw new Error("hook failed synchronously");
      },
    });
    expect(handler.length).toBe(1);
    expectTypeOf(handler).parameters.toEqualTypeOf<[Request]>();
    expectTypeOf(handler).returns.toEqualTypeOf<Promise<Response>>();
    const response = await handler(new Request("https://example.com/__invariant/tools"));
    expect([response.status, await response.json()]).toEqual([
      500,
      { code: "internal", message: "hook failed synchronously", details: [] },
    ]);

    // The body is read after the hook, so the hook may read a clone.
    const cloning = greetServer().fetchHandler({
      contextValues: async (request) => {
        await request.clone().arrayBuffer();
        return createContextValues();
      },
    });
    const cloned = await cloning(
      new Request(`https://example.com${GREET}`, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ name: "Ada" }),
      }),
    );
    expect([cloned.status, (await cloned.json()).message]).toEqual([200, "Hi Ada"]);
    const clonedMcp = await cloning(
      new Request("https://example.com/mcp", {
        method: "POST",
        headers: MCP_HEADERS,
        body: JSON.stringify(callTool("Greet", { name: "Ada" })),
      }),
    );
    expect(await toolReplies(clonedMcp)).toMatchObject([{ message: "Hi Ada" }]);

    // A Request built without a body is an empty MCP message.
    const empty = await greetServer().fetchHandler()(
      new Request("https://example.com/mcp", { method: "POST", headers: MCP_HEADERS }),
    );
    expect([empty.status, (await empty.json()).error.code]).toEqual([200, -32700]);
  });

  test("normalizes a body cancel and a ResponseAborted source to Canceled", async () => {
    const observed = observe();
    const handler = greetServer(observed).fetchHandler();
    const abandoned = await handler(
      new Request(`https://example.com${GREET}`, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: "{}",
        signal: AbortSignal.abort(new ResponseAborted()),
      }),
    );
    expect([abandoned.status, await abandoned.json()]).toEqual([
      499,
      { code: "canceled", message: "client disconnected", details: [] },
    ]);
    expect(observed.invocations).toBe(0);
    const open = async (read: boolean, signal?: AbortSignal) => {
      const response = await handler(
        new Request(`https://example.com${STREAM_GREET}`, {
          method: "POST",
          headers: { "content-type": "application/connect+json", "connect-timeout-ms": "600000" },
          body: envelope({ name: "wait" }),
          signal,
        }),
      );
      const reader = response.body!.getReader();
      if (read) {
        expect((await reader.read()).done).toBe(false);
      }
      return reader;
    };
    // Each stream arms Connect's deadline timer, and closing it clears the
    // timer, even when the host stops before its first read.
    const activeTimers = () => process.getActiveResourcesInfo().filter((resource) => resource === "Timeout").length;
    const before = activeTimers();
    for (const read of [true, false]) {
      await (await open(read)).cancel();
      const source = new AbortController();
      await open(read, source.signal);
      source.abort(new ResponseAborted());
    }
    await vi.waitFor(() => expect(observed.finals).toBe(4));
    expect(observed.reasons).toHaveLength(4);
    for (const reason of observed.reasons) {
      expect(reason).toBeInstanceOf(ConnectError);
      expect((reason as ConnectError).code).toBe(Code.Canceled);
    }
    await vi.waitFor(() => expect(activeTimers()).toBeLessThanOrEqual(before), { timeout: 2_000 });
  });

  test("bounds draining an over-limit body, even into a clone the hook left unread", async () => {
    const clones: Request[] = [];
    const handler = greetServer(observe(), (server) => server.setMaxUnaryRequestBytes(1024)).fetchHandler({
      contextValues: (request) => {
        clones.push(request.clone());
        return createContextValues();
      },
    });
    for (const [path, headers] of [
      ["/mcp", MCP_HEADERS],
      [GREET, { "content-type": "application/json" }],
    ] as const) {
      let pulled = 0;
      const upload = new ReadableStream<Uint8Array>(
        {
          pull(controller) {
            if (pulled >= 32 * 1024 * 1024) {
              controller.close();
              return;
            }
            pulled += 64 * 1024;
            controller.enqueue(new Uint8Array(64 * 1024).fill(0x20));
          },
        },
        { highWaterMark: 0 },
      );
      const response = await handler(
        new Request(`https://example.com${path}`, {
          method: "POST",
          headers,
          body: upload,
          duplex: "half",
        } as RequestInit),
      );
      expect(response.status).toBe(429);
      await new Promise((resolveDelay) => setTimeout(resolveDelay, 200));
      expect(pulled).toBeLessThanOrEqual(1024 * 1024);
    }
    expect(clones).toHaveLength(2);
  });

  test("delivers a disconnect after the host drops its Request and garbage collection runs", async () => {
    const observed = observe();
    const handler = greetServer(observed).fetchHandler();
    const source = new AbortController();
    await (async () => {
      const response = await handler(
        new Request(`https://example.com${STREAM_GREET}`, {
          method: "POST",
          headers: { "content-type": "application/connect+json" },
          body: envelope({ name: "wait" }),
          signal: source.signal,
        }),
      );
      const reader = response.body!.getReader();
      expect((await reader.read()).done).toBe(false);
      reader.releaseLock();
    })();
    forceGc();
    await new Promise((resolveDelay) => setTimeout(resolveDelay, 0));
    forceGc();
    source.abort(new ResponseAborted());
    await vi.waitFor(() => expect(observed.finals).toBe(1));
    expect(ConnectError.from(observed.reasons[0]).code).toBe(Code.Canceled);
  });
});

describe("node host", () => {
  test("serves requests without Host, fixes the context origin, and closes malformed targets", async () => {
    const observed = observe();
    const host = await start("node", greetServer(observed));
    const body = JSON.stringify({ name: "Ada" });
    // Neither a missing Host nor an absolute-form or "//" target chooses context.url's origin.
    for (const target of [GREET, `http://attacker.example${GREET}`, `//attacker.example${GREET}`]) {
      const response = await rawExchange(
        host,
        `POST ${target} HTTP/1.0\r\ncontent-type: application/json\r\ncontent-length: ${body.length}\r\n\r\n${body}`,
      );
      expect(response).toMatch(/^HTTP\/1\.1 200 /);
    }
    expect(observed.urls).toEqual(Array(3).fill(`http://localhost${GREET}`));
    expect(await rawExchange(host, "GET http://[/ HTTP/1.1\r\nhost: example.com\r\n\r\n")).toBe("");
    expect((await fetch(`${host.baseUrl}/healthz`)).status).toBe(200);
  });

  test("answers 500 when Node rejects a response header that Fetch Headers accept", async () => {
    const host = await start("node", greetServer(), (request) => {
      if (headerOf(request, "x-hook") === "control") {
        throw new ConnectError("a valid bearer token is required", Code.Unauthenticated, {
          "www-authenticate": "Bearer\x01",
        });
      }
      return createContextValues();
    });
    for (const [response, name] of [
      [await greet(host, { name: "control" }), "x-label"],
      [await greet(host, { name: "Ada" }, { "x-hook": "control" }), "www-authenticate"],
    ] as const) {
      expect([response.status, response.headers.get(name)]).toEqual([500, null]);
      expect(await response.json()).toEqual({
        code: "internal",
        message: expect.stringContaining(`"${name}"`),
        details: [],
      });
    }
    expect((await fetch(`${host.baseUrl}/healthz`)).status).toBe(200);
  });

  test("serves node:http2 compatibility requests and reports a stream reset as Canceled", async () => {
    const observed = observe();
    const handler = greetServer(observed, (server) => server.setMaxUnaryRequestBytes(1024)).httpHandler({
      contextValues: (request) => {
        if (headerOf(request, "x-hook") === "connection-specific") {
          throw new ConnectError("a valid bearer token is required", Code.Unauthenticated, {
            "www-authenticate": CHALLENGE,
            "http2-settings": "AAMAAABk",
            upgrade: "h2c",
          });
        }
        return createContextValues();
      },
    });
    const handled: Promise<void>[] = [];
    const sessions = new Set<Http2Session>();
    const h2 = createHttp2Server((req, res) => {
      handled.push(handler(req as unknown as IncomingMessage, res as unknown as ServerResponse));
    });
    h2.on("session", (session) => sessions.add(session));
    await new Promise<void>((resolveListen) => h2.listen(0, "127.0.0.1", resolveListen));
    const baseUrl = `http://127.0.0.1:${(h2.address() as AddressInfo).port}`;
    hosts.push({
      baseUrl,
      handled: [],
      close: () =>
        new Promise<void>((resolveClose) => {
          for (const session of sessions) {
            session.destroy();
          }
          h2.close(() => resolveClose());
        }),
    });

    const greeter = createClient(GreetService, createConnectTransport({ baseUrl, httpVersion: "2" }));
    expect((await greeter.greet({ name: "Ada" })).message).toBe("Hi Ada");
    // node:http2 refuses a connection-specific response header after accepting
    // the ones before it; the response is a clean 500 all the same.
    const refused = ConnectError.from(await greeter.greet({ name: "upgrade" }).catch((e: unknown) => e));
    expect([refused.code, refused.rawMessage]).toEqual([Code.Internal, expect.stringContaining("upgrade")]);
    expect((await drain(greeter.streamGreet({ name: "Ada", count: 2 }))).map((reply) => reply.message)).toEqual([
      "Hi Ada #1",
      "Hi Ada #2",
    ]);
    const session = connectHttp2(baseUrl);
    const health = await h2Exchange(session, { ":method": "GET", ":path": "/healthz" });
    expect([health.status, JSON.parse(health.body)]).toEqual([200, { status: "ok" }]);
    const initialized = await h2Exchange(
      session,
      { ":method": "POST", ":path": "/mcp", ...MCP_UNVERSIONED },
      JSON.stringify(INITIALIZE),
    );
    expect([initialized.status, initialized.headers["mcp-protocol-version"]]).toEqual([200, "2025-11-25"]);
    // HEAD responses keep their length but carry no body.
    const head = await h2Exchange(session, { ":method": "HEAD", ":path": "/healthz" });
    expect([head.status, head.headers["content-length"], head.body]).toEqual([404, "55", ""]);
    // Connection-specific metadata, which HTTP/2 forbids, is not relayed.
    const rejected = await h2Exchange(session, {
      ":method": "GET",
      ":path": "/__invariant/tools",
      "x-hook": "connection-specific",
    });
    expect([rejected.status, rejected.headers["www-authenticate"]]).toEqual([401, CHALLENGE]);
    // An opaque :path has no absolute path, so it cannot choose context.url's
    // origin: the stream is reset before the hook or any handler runs. (Node's
    // HTTP/2 client keeps a reset stream open while its body is still queued,
    // so these run before the large upload below.)
    const invocations = observed.invocations;
    for (const path of [`foo:@attacker.example${GREET}`, "foo:@attacker.example/mcp"]) {
      const opaque = await h2Exchange(
        session,
        { ":method": "POST", ":scheme": "foo", ":path": path, ...MCP_HEADERS },
        JSON.stringify(path.endsWith("/mcp") ? callTool("Greet", { name: "Ada" }) : { name: "Ada" }),
      );
      expect(opaque.status).toBeNaN();
    }
    expect(observed.invocations).toBe(invocations);
    // The core reads part of an over-limit body; the rest is discarded so the
    // client can finish sending and the stream closes.
    const overLimit = await h2Exchange(
      session,
      { ":method": "POST", ":path": "/mcp", ...MCP_HEADERS },
      " ".repeat(256 * 1024),
    );
    expect(overLimit.status).toBe(429);
    session.close();

    const reset = new AbortController();
    const stream = greeter.streamGreet({ name: "wait" }, { signal: reset.signal })[Symbol.asyncIterator]();
    expect((await stream.next()).value?.message).toBe("Hi wait #1");
    reset.abort();
    await expect(stream.next()).rejects.toMatchObject({ code: Code.Canceled });
    await vi.waitFor(() => expect(observed.reasons).toHaveLength(1));
    const reason = ConnectError.from(observed.reasons[0]);
    expect([reason.code, reason.rawMessage]).toEqual([Code.Canceled, "client disconnected"]);
    // Every response closed, so every handler promise settled.
    await Promise.all(handled);
  });
});
