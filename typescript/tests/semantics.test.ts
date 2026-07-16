import { createServer, request as httpRequest, type Server as HTTPServer } from "node:http";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";

import * as grpc from "@grpc/grpc-js";
import { Code, ConnectError, createHandlerContext, type HandlerContext } from "@connectrpc/connect";
import { create, fromBinary, toBinary } from "@bufbuild/protobuf";
import { afterEach, describe, expect, test } from "vitest";

import {
  Server,
  InvariantError,
  httpHandler,
  runCli,
  serveMcpStdio,
  validation,
} from "../src/index.js";
import { grpcServiceDefinitionForService } from "../src/grpc.js";
import { mcpDispatch } from "../src/mcp.js";
import { BadRequestSchema } from "../src/gen/google/rpc/error_details_pb.js";
import { StatusSchema } from "../src/gen/google/rpc/status_pb.js";
import {
  GreetResponseSchema,
  GreetService,
  type GreetRequest,
} from "./gen/greet_pb.js";

const here = dirname(fileURLToPath(import.meta.url));
const descriptorPath = resolve(here, "../../python/tests/proto/descriptor.binpb");

function initializeRequest(id: string | number, protocolVersion = "2025-11-25") {
  return {
    jsonrpc: "2.0",
    id,
    method: "initialize",
    params: {
      protocolVersion,
      capabilities: {},
      clientInfo: { name: "invariant-test", version: "1.0" },
    },
  };
}

describe("cross-projection HandlerContext semantics", () => {
  const grpcServers: grpc.Server[] = [];
  const httpServers: HTTPServer[] = [];

  afterEach(async () => {
    for (const server of grpcServers.splice(0)) {
      server.forceShutdown();
    }
    await Promise.all(
      httpServers.splice(0).map(
        (server) =>
          new Promise<void>((resolveClose) => {
            server.close(() => resolveClose());
          }),
      ),
    );
  });

  test("provides the official Connect HandlerContext to invoke, CLI, and MCP", async () => {
    const protocols: string[] = [];
    const server = Server.fromDescriptor(descriptorPath);
    server.register(GreetService, {
      greet(request, context) {
        assertHandlerContext(context, "Greet");
        protocols.push(context.protocolName);
        return { message: `Hi ${request.name}` };
      },
    });

    await server.invoke("GreetService.Greet", { name: "invoke" });
    await runCli(server, ["GreetService", "Greet", "-r", '{"name":"cli"}']);
    await mcpDispatch(server, {
      jsonrpc: "2.0",
      id: 1,
      method: "tools/call",
      params: { name: "GreetService.Greet", arguments: { name: "mcp" } },
    });

    expect(protocols).toEqual(["in-process", "cli", "mcp"]);
  });

  test("runs a standard Connect interceptor exactly once with generated messages on every projection", async () => {
    const calls: Array<{ url: string; typeName: string }> = [];
    const server = registeredServer();
    server.use((next) => async (request) => {
      if (!request.stream) {
        calls.push({ url: request.url, typeName: request.message.$typeName });
      }
      return next(request);
    });

    await server.invoke("GreetService.Greet", { name: "invoke" });
    await runCli(server, ["GreetService", "Greet", "-r", '{"name":"cli"}']);
    await mcpDispatch(server, {
      jsonrpc: "2.0",
      id: 1,
      method: "tools/call",
      params: { name: "GreetService.Greet", arguments: { name: "mcp" } },
    });

    const base = await startHTTP(server, httpServers);
    await fetch(`${base}/greet.v1.GreetService/Greet`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ name: "http" }),
    });

    const started = await startGrpc(server);
    grpcServers.push(started.server);
    const client = generatedClient(started.address);
    await unaryWithMetadata(client, "greet", { name: "grpc" }, new grpc.Metadata(), {});
    client.close();

    expect(calls).toHaveLength(5);
    expect(calls.map((call) => call.typeName)).toEqual(Array(5).fill("greet.v1.GreetRequest"));
    expect(calls.filter((call) => call.url.startsWith("grpc://"))).toHaveLength(1);
    expect(calls.filter((call) => call.url.startsWith("mcp://"))).toHaveLength(1);
    expect(calls.filter((call) => call.url.startsWith("invariant-cli://"))).toHaveLength(1);
  });

  test("maps native gRPC metadata, deadline, cancellation, response headers, and trailers", async () => {
    let cancellationObserved!: () => void;
    const cancelled = new Promise<void>((resolveCancelled) => {
      cancellationObserved = resolveCancelled;
    });
    const server = Server.fromDescriptor(descriptorPath);
    server.register(GreetService, {
      async greet(request, context) {
        assertHandlerContext(context, "Greet");
        expect(context.protocolName).toBe("grpc");
        expect(context.requestHeader.get("x-request-id")).toBe("native-123");
        expect(context.requestHeader.get("x-repeat")).toBe("one, two");
        expect(context.requestHeader.get("x-repeat-bin")).toBe("AQ==, Ag==");
        expect(context.requestHeader.get("x-comma")).toBe("left,right");
        expect(context.timeoutMs()).toBeGreaterThan(0);
        if (request.name === "cancel") {
          await new Promise<never>((_resolve, reject) => {
            context.signal.addEventListener(
              "abort",
              () => {
                cancellationObserved();
                reject(new DOMException("cancelled", "AbortError"));
              },
              { once: true },
            );
          });
        }
        if (request.name === "invalid-metadata") {
          context.responseHeader.set("x-invalid-bin", "not-base64!");
          return { message: "unreachable" };
        }
        context.responseHeader.set("x-initial", "present");
        context.responseHeader.set("x-comma-header", "left,right");
        context.responseHeader.append("x-repeat-header", "first");
        context.responseHeader.append("x-repeat-header", "second");
        context.responseHeader.append("x-repeat-header-bin", Buffer.from([3]).toString("base64"));
        context.responseHeader.append("x-repeat-header-bin", Buffer.from([4]).toString("base64"));
        context.responseHeader.set("x-unpadded-bin", Buffer.from([7, 8]).toString("base64").replace(/=+$/, ""));
        context.responseTrailer.set("x-trailing", "present");
        context.responseTrailer.set("x-comma-trailer", "left,right");
        context.responseTrailer.append("x-repeat-trailer", "third");
        context.responseTrailer.append("x-repeat-trailer", "fourth");
        context.responseTrailer.append("x-repeat-trailer-bin", Buffer.from([5]).toString("base64"));
        context.responseTrailer.append("x-repeat-trailer-bin", Buffer.from([6]).toString("base64"));
        return { message: `Hi ${request.name}` };
      },
    });
    const started = await startGrpc(server);
    grpcServers.push(started.server);
    const client = generatedClient(started.address);

    const metadata = new grpc.Metadata();
    metadata.set("x-request-id", "native-123");
    metadata.add("x-repeat", "one");
    metadata.add("x-repeat", "two");
    metadata.add("x-repeat-bin", Buffer.from([1]));
    metadata.add("x-repeat-bin", Buffer.from([2]));
    metadata.add("x-comma", "left,right");
    const callResult = await unaryWithMetadata(client, "greet", { name: "Ada" }, metadata, {
      deadline: new Date(Date.now() + 10_000),
    });
    expect(callResult.response.message).toBe("Hi Ada");
    expect(callResult.header.get("x-initial")).toEqual(["present"]);
    expect(callResult.header.get("x-comma-header")).toEqual(["left,right"]);
    expect(callResult.header.get("x-repeat-header")).toEqual(["first, second"]);
    expect(callResult.header.get("x-repeat-header-bin")).toEqual([Buffer.from([3]), Buffer.from([4])]);
    expect(callResult.header.get("x-unpadded-bin")).toEqual([Buffer.from([7, 8])]);
    expect(callResult.trailer.get("x-trailing")).toEqual(["present"]);
    expect(callResult.trailer.get("x-comma-trailer")).toEqual(["left,right"]);
    expect(callResult.trailer.get("x-repeat-trailer")).toEqual(["third, fourth"]);
    expect(callResult.trailer.get("x-repeat-trailer-bin")).toEqual([Buffer.from([5]), Buffer.from([6])]);
    const invalidMetadataError = await unaryError(
      client,
      "greet",
      { name: "invalid-metadata" },
      metadata,
      { deadline: new Date(Date.now() + 10_000) },
    );
    expect(invalidMetadataError.code).toBe(grpc.status.INTERNAL);

    const canceledCall = (client as any).greet(
      { name: "cancel" },
      metadata,
      { deadline: new Date(Date.now() + 10_000) },
      () => undefined,
    ) as grpc.ClientUnaryCall;
    await new Promise<void>((resolveReady) => setImmediate(resolveReady));
    canceledCall.cancel();
    await cancelled;
    client.close();
  });

  test("owns one native grpc.Server and graceful stop drains an in-flight call", async () => {
    let release!: () => void;
    let entered!: () => void;
    const gate = new Promise<void>((resolveGate) => {
      release = resolveGate;
    });
    const startedCall = new Promise<void>((resolveEntered) => {
      entered = resolveEntered;
    });
    const server = Server.fromDescriptor(descriptorPath);
    server.register(GreetService, {
      async greet(request) {
        entered();
        await gate;
        return { message: `Hi ${request.name}` };
      },
    });
    const started = await startGrpc(server);
    grpcServers.push(started.server);
    const client = generatedClient(started.address);
    const response = new Promise<any>((resolveCall, rejectCall) => {
      (client as any).greet({ name: "Ada" }, (error: grpc.ServiceError | null, value: unknown) => {
        if (error) {
          rejectCall(error);
        } else {
          resolveCall(value);
        }
      });
    });
    await startedCall;
    let stopped = false;
    const stopping = server.stop().then(() => {
      stopped = true;
    });
    await new Promise<void>((resolveTurn) => setImmediate(resolveTurn));
    expect(stopped).toBe(false);
    release();
    expect((await response).message).toBe("Hi Ada");
    await stopping;
    expect(stopped).toBe(true);
    client.close();
  });

  test("passes native receive and send limits directly to grpc-js", async () => {
    const receiveLimited = registeredServer();
    const receiveStarted = await startGrpc(receiveLimited, {
      "grpc.max_receive_message_length": 64,
    });
    grpcServers.push(receiveStarted.server);
    const receiveClient = generatedClient(receiveStarted.address);
    const receiveError = await unaryError(receiveClient, "greet", { name: "x".repeat(256) });
    expect(receiveError.code).toBe(grpc.status.RESOURCE_EXHAUSTED);
    receiveClient.close();

    const sendLimited = Server.fromDescriptor(descriptorPath);
    sendLimited.register(GreetService, {
      greet() {
        return { message: "x".repeat(256) };
      },
    });
    const sendStarted = await startGrpc(sendLimited, {
      "grpc.max_send_message_length": 64,
    });
    grpcServers.push(sendStarted.server);
    const sendClient = generatedClient(sendStarted.address);
    const sendError = await unaryError(sendClient, "greet", { name: "Ada" });
    expect(sendError.code).toBe(grpc.status.RESOURCE_EXHAUSTED);
    sendClient.close();
  });

  test("uses a reviewed HTTP metadata mapper and cannot forward identity metadata", async () => {
    let defaultHeaders: Headers | undefined;
    const defaultServer = Server.fromDescriptor(descriptorPath);
    defaultServer.register(GreetService, {
      greet(request, context) {
        defaultHeaders = new Headers(context.requestHeader);
        return Promise.resolve({ message: `Hi ${request.name}` });
      },
    });
    const defaultBase = await startHTTP(defaultServer, httpServers);
    await fetch(`${defaultBase}/greet.v1.GreetService/Greet`, {
      method: "POST",
      headers: {
        "content-type": "application/json",
        "x-request-id": "request-123",
        "x-random": "unreviewed",
        authorization: "Bearer untrusted",
      },
      body: JSON.stringify({ name: "Ada" }),
    });
    expect(defaultHeaders?.get("x-request-id")).toBe("request-123");
    expect(defaultHeaders?.get("x-random")).toBeNull();
    expect(defaultHeaders?.get("authorization")).toBeNull();

    const seen: Headers[] = [];
    const server = Server.fromDescriptor(descriptorPath);
    server.useHttpMetadataMapper((headers) => ({
      "x-custom": headers.get("x-custom") ?? "",
      "trace-bin": "AQI",
      authorization: headers.get("authorization") ?? "",
      "authorization-bin": "AQI",
      "proxy-authorization-bin": "AQI",
      "authentication-bin": "AQI",
      "api-key-bin": "AQI",
      "x-api-key-bin": "AQI",
      "cookie-bin": "AQI",
      "set-cookie-bin": "AQI",
      tenant: "forged",
      "x-role-admin": "forged",
    }));
    server.register(GreetService, {
      greet(request, context) {
        seen.push(new Headers(context.requestHeader));
        return Promise.resolve({ message: `Hi ${request.name}` });
      },
    });
    const base = await startHTTP(server, httpServers);

    const response = await fetch(`${base}/greet.v1.GreetService/Greet`, {
      method: "POST",
      headers: {
        "content-type": "application/json",
        "x-custom": "safe",
        authorization: "Bearer untrusted",
      },
      body: JSON.stringify({ name: "Ada" }),
    });
    expect(response.status).toBe(200);
    expect(seen[0]?.get("x-custom")).toBe("safe");
    expect(seen[0]?.get("trace-bin")).toBe("AQI");
    expect(seen[0]?.get("authorization")).toBeNull();
    for (const key of [
      "authorization-bin",
      "proxy-authorization-bin",
      "authentication-bin",
      "api-key-bin",
      "x-api-key-bin",
      "cookie-bin",
      "set-cookie-bin",
    ]) {
      expect(seen[0]?.get(key)).toBeNull();
    }
    expect(seen[0]?.get("tenant")).toBeNull();
    expect(seen[0]?.get("x-role-admin")).toBeNull();
  });

  test("implements the current non-SSE MCP Streamable HTTP contract", async () => {
    let protocol = "";
    const server = Server.fromDescriptor(descriptorPath);
    server.register(GreetService, {
      greet(request, context) {
        protocol = context.protocolName;
        return { message: `Hi ${request.name}` };
      },
    });
    const base = await startHTTP(server, httpServers);
    const accept = "application/json, text/event-stream";
    const mcpHeaders = { accept, "content-type": "application/json" };

    expect((await fetch(`${base}/mcp`)).status).toBe(405);
    const missingContentType = await fetch(`${base}/mcp`, {
      method: "POST",
      headers: { accept },
      body: JSON.stringify(initializeRequest(0)),
    });
    expect(missingContentType.status).toBe(415);
    expect(
      (
        await fetch(`${base}/mcp`, {
          method: "POST",
          headers: mcpHeaders,
          body: JSON.stringify(initializeRequest(1)),
        })
      ).status,
    ).toBe(200);

    const initialized = await fetch(`${base}/mcp`, {
      method: "POST",
      headers: mcpHeaders,
      body: JSON.stringify(initializeRequest(2)),
    });
    expect(initialized.headers.get("mcp-protocol-version")).toBe("2025-11-25");
    expect(await initialized.json()).toMatchObject({
      result: { protocolVersion: "2025-11-25" },
    });
    const negotiated = await fetch(`${base}/mcp`, {
      method: "POST",
      headers: mcpHeaders,
      body: JSON.stringify(initializeRequest(22, "2099-01-01")),
    });
    expect(negotiated.status).toBe(200);
    expect(await negotiated.json()).toMatchObject({
      id: 22,
      result: { protocolVersion: "2025-11-25" },
    });

    const missingAccept = await fetch(`${base}/mcp`, {
      method: "POST",
      body: JSON.stringify(initializeRequest(3)),
    });
    expect(missingAccept.status).toBe(406);

    const rejectedStream = await fetch(`${base}/mcp`, {
      method: "POST",
      headers: {
        accept: "application/json, text/event-stream;q=0",
        "content-type": "application/json",
      },
      body: JSON.stringify(initializeRequest(3)),
    });
    expect(rejectedStream.status).toBe(406);

    const origin = await fetch(`${base}/mcp`, {
      method: "POST",
      headers: { ...mcpHeaders, origin: "https://untrusted.example" },
      body: JSON.stringify(initializeRequest(4)),
    });
    expect(origin.status).toBe(403);
    const hostileOriginGet = await fetch(`${base}/mcp`, {
      headers: { origin: "https://untrusted.example" },
    });
    expect(hostileOriginGet.status).toBe(403);

    const missingVersion = await fetch(`${base}/mcp`, {
      method: "POST",
      headers: mcpHeaders,
      body: JSON.stringify({ jsonrpc: "2.0", id: 5, method: "ping" }),
    });
    expect(missingVersion.status).toBe(400);

    const wrongVersion = await fetch(`${base}/mcp`, {
      method: "POST",
      headers: { ...mcpHeaders, "mcp-protocol-version": "2024-11-05" },
      body: JSON.stringify({ jsonrpc: "2.0", id: 6, method: "ping" }),
    });
    expect(wrongVersion.status).toBe(400);

    for (const invalidTimeout of ["0", "-1", "+1", "1.0", "abc", "12345678901"]) {
      const invalidDeadline = await fetch(`${base}/mcp`, {
        method: "POST",
        headers: { ...mcpHeaders, "connect-timeout-ms": invalidTimeout },
        body: JSON.stringify(initializeRequest(6)),
      });
      expect(invalidDeadline.status, invalidTimeout).toBe(400);
      expect(await invalidDeadline.json()).toMatchObject({ code: "invalid_argument" });
    }

    for (const payload of [
      42,
      [],
      { id: 1, method: "ping" },
      { jsonrpc: "1.0", id: 2, method: "ping" },
      { jsonrpc: "2.0", id: 3 },
      { jsonrpc: "2.0", id: 4, method: 7 },
      { jsonrpc: "2.0", id: null, method: "ping" },
      { jsonrpc: "2.0", id: false, method: "ping" },
      { jsonrpc: "2.0", id: 1.5, method: "ping" },
      { jsonrpc: "2.0", id: Number.MAX_SAFE_INTEGER + 1, method: "ping" },
      { jsonrpc: "2.0", id: 5, result: "not-an-object" },
      { jsonrpc: "2.0", error: { code: 1.5, message: "bad code" } },
    ]) {
      const invalid = await fetch(`${base}/mcp`, {
        method: "POST",
        headers: mcpHeaders,
        body: JSON.stringify(payload),
      });
      expect(invalid.status).toBe(200);
      expect(await invalid.json()).toMatchObject({ id: null, error: { code: -32600 } });
    }

    for (const body of ["", new Uint8Array([0xff])]) {
      const malformed = await fetch(`${base}/mcp`, {
        method: "POST",
        headers: mcpHeaders,
        body,
      });
      expect(malformed.status).toBe(200);
      expect(await malformed.json()).toMatchObject({ id: null, error: { code: -32700 } });
    }

    for (const payload of [
      { jsonrpc: "2.0", method: "notifications/initialized" },
      { jsonrpc: "2.0", id: 7, result: {} },
      { jsonrpc: "2.0", error: { code: -32601, message: "unknown request" } },
    ]) {
      const accepted = await fetch(`${base}/mcp`, {
        method: "POST",
        headers: { ...mcpHeaders, "mcp-protocol-version": "2025-11-25" },
        body: JSON.stringify(payload),
      });
      expect(accepted.status).toBe(202);
      expect(await accepted.text()).toBe("");
    }

    for (const payload of [
      { jsonrpc: "2.0", id: 12, method: "ping", params: [] },
      { jsonrpc: "2.0", id: 13, method: "tools/call", params: { name: [], arguments: {} } },
      {
        jsonrpc: "2.0",
        id: 14,
        method: "tools/call",
        params: { name: "GreetService.Greet", arguments: [] },
      },
    ]) {
      const invalidParams = await fetch(`${base}/mcp`, {
        method: "POST",
        headers: { ...mcpHeaders, "mcp-protocol-version": "2025-11-25" },
        body: JSON.stringify(payload),
      });
      expect(invalidParams.status).toBe(200);
      expect(await invalidParams.json()).toMatchObject({ error: { code: -32602 } });
    }

    const toolCall = await fetch(`${base}/mcp`, {
      method: "POST",
      headers: { ...mcpHeaders, "mcp-protocol-version": "2025-11-25" },
      body: JSON.stringify({
        jsonrpc: "2.0",
        id: 8,
        method: "tools/call",
        params: { name: "GreetService.Greet", arguments: { name: "Ada" } },
      }),
    });
    expect(toolCall.status).toBe(200);
    expect(protocol).toBe("mcp");

    const limited = registeredServer();
    limited.setMaxUnaryResponseBytes(160);
    const limitedBase = await startHTTP(limited, httpServers);
    const oversized = await fetch(`${limitedBase}/mcp`, {
      method: "POST",
      headers: { ...mcpHeaders, "mcp-protocol-version": "2025-11-25" },
      body: JSON.stringify({ jsonrpc: "2.0", id: 15, method: "tools/list" }),
    });
    expect(oversized.status).toBe(429);
    expect(await oversized.json()).toMatchObject({ code: "resource_exhausted" });

    const tiny = registeredServer();
    tiny.setMaxUnaryResponseBytes(1);
    const tinyBase = await startHTTP(tiny, httpServers);
    const boundedParseError = await fetch(`${tinyBase}/mcp`, {
      method: "POST",
      headers: mcpHeaders,
      body: "{bad json",
    });
    expect(boundedParseError.status).toBe(429);
    expect(await boundedParseError.text()).toBe("");
  });

  test("subtracts MCP request-body time from the handler deadline", async () => {
    let observedTimeoutMs = Number.NaN;
    const server = Server.fromDescriptor(descriptorPath);
    server.register(GreetService, {
      greet(request, context) {
        observedTimeoutMs = context.timeoutMs() ?? Number.NaN;
        return { message: `Hi ${request.name}` };
      },
    });
    const base = await startHTTP(server, httpServers);
    const payload = Buffer.from(JSON.stringify({
      jsonrpc: "2.0",
      id: 1,
      method: "tools/call",
      params: { name: "GreetService.Greet", arguments: { name: "Ada" } },
    }));
    const response = await new Promise<{ status: number; body: string }>((resolveResponse, rejectResponse) => {
      const request = httpRequest(`${base}/mcp`, {
        method: "POST",
        headers: {
          accept: "application/json, text/event-stream",
          "content-type": "application/json",
          "content-length": String(payload.length),
          "connect-timeout-ms": "1000",
          "mcp-protocol-version": "2025-11-25",
        },
      }, (incoming) => {
        const chunks: Buffer[] = [];
        incoming.on("data", (chunk) => chunks.push(Buffer.from(chunk)));
        incoming.on("end", () => {
          resolveResponse({
            status: incoming.statusCode ?? 0,
            body: Buffer.concat(chunks).toString("utf8"),
          });
        });
      });
      request.on("error", rejectResponse);
      const split = Math.floor(payload.length / 2);
      request.write(payload.subarray(0, split));
      setTimeout(() => request.end(payload.subarray(split)), 150);
    });

    expect(response.status).toBe(200);
    expect(JSON.parse(response.body)).toMatchObject({ id: 1 });
    expect(observedTimeoutMs).toBeGreaterThan(0);
    expect(observedTimeoutMs).toBeLessThan(900);

    const expired = await new Promise<{ status: number; body: string }>((resolveResponse, rejectResponse) => {
      const request = httpRequest(`${base}/mcp`, {
        method: "POST",
        headers: {
          accept: "application/json, text/event-stream",
          "content-type": "application/json",
          "content-length": String(payload.length),
          "connect-timeout-ms": "10",
          "mcp-protocol-version": "2025-11-25",
        },
      }, (incoming) => {
        const chunks: Buffer[] = [];
        incoming.on("data", (chunk) => chunks.push(Buffer.from(chunk)));
        incoming.on("end", () => {
          resolveResponse({
            status: incoming.statusCode ?? 0,
            body: Buffer.concat(chunks).toString("utf8"),
          });
        });
      });
      request.on("error", rejectResponse);
      const split = Math.floor(payload.length / 2);
      request.write(payload.subarray(0, split));
      setTimeout(() => request.end(payload.subarray(split)), 30);
    });
    expect(expired.status).toBe(504);
    expect(JSON.parse(expired.body)).toMatchObject({ code: "deadline_exceeded" });
  });

  test("forwards HandlerContext semantics through a remote gRPC projection", async () => {
    const backend = Server.fromDescriptor(descriptorPath);
    backend.register(GreetService, {
      greet(request, context) {
        expect(context.requestHeader.get("x-request-id")).toBe("proxy-123");
        expect(context.requestHeader.get("x-invalid-bin")).toBeNull();
        expect(context.requestHeader.get("x-comma")).toBe("left,right");
        if (request.name === "native") {
          expect(context.requestHeader.get("x-repeat")).toBe("one, two");
          expect(context.requestHeader.get("x-repeat-bin")).toBe("AQ==, Ag==");
        }
        expect(context.timeoutMs()).toBeGreaterThan(0);
        context.responseHeader.set("x-upstream-header", "present");
        context.responseHeader.set("x-upstream-comma", "left,right");
        context.responseHeader.append("x-upstream-repeat", "first");
        context.responseHeader.append("x-upstream-repeat", "second");
        context.responseHeader.append("x-upstream-repeat-bin", Buffer.from([3]).toString("base64"));
        context.responseHeader.append("x-upstream-repeat-bin", Buffer.from([4]).toString("base64"));
        context.responseTrailer.set("x-upstream-trailer", "present");
        context.responseTrailer.append("x-upstream-repeat-trailer", "third");
        context.responseTrailer.append("x-upstream-repeat-trailer", "fourth");
        context.responseTrailer.append("x-upstream-repeat-trailer-bin", Buffer.from([5]).toString("base64"));
        context.responseTrailer.append("x-upstream-repeat-trailer-bin", Buffer.from([6]).toString("base64"));
        if (request.name === "status") {
          throw new ConnectError(
            "upstream status",
            Code.FailedPrecondition,
            { "x-error-meta": "once" },
            [{ desc: GreetResponseSchema, value: { message: "proxy detail" } }],
          );
        }
        return { message: `Hi ${request.name}` };
      },
    });
    const started = await startGrpc(backend);
    grpcServers.push(started.server);

    const proxy = Server.fromDescriptor(descriptorPath);
    const client = generatedClient(started.address);
    proxy.connectGrpc(GreetService, client);
    const context = createHandlerContext({
      service: GreetService,
      method: GreetService.method.greet,
      protocolName: "connect",
      requestMethod: "POST",
      url: "http://proxy/greet.v1.GreetService/Greet",
      timeoutMs: 10_000,
      requestHeader: { "x-request-id": "proxy-123", "x-comma": "left,right" },
    });
    const response = await proxy.invoke("GreetService.Greet", { name: "Ada" }, context);
    expect(proxy.toJson(proxy.tools.get("GreetService.Greet")!, response)).toMatchObject({ message: "Hi Ada" });
    expect(context.responseHeader.get("x-upstream-header")).toBe("present");
    expect(context.responseTrailer.get("x-upstream-trailer")).toBe("present");

    const invalidContext = createHandlerContext({
      service: GreetService,
      method: GreetService.method.greet,
      protocolName: "connect",
      requestMethod: "POST",
      url: "http://proxy/greet.v1.GreetService/Greet",
      timeoutMs: 10_000,
      requestHeader: { "x-request-id": "proxy-123", "x-invalid-bin": "not-base64!" },
    });
    await expect(proxy.invoke("GreetService.Greet", { name: "invalid" }, invalidContext)).rejects.toMatchObject({
      code: Code.InvalidArgument,
    });
    invalidContext.abort();

    let thrown: unknown;
    try {
      await proxy.invoke("GreetService.Greet", { name: "status" }, context);
    } catch (error) {
      thrown = error;
    }
    expect(thrown).toBeInstanceOf(ConnectError);
    expect((thrown as ConnectError).code).toBe(Code.FailedPrecondition);
    expect((thrown as ConnectError).findDetails(GreetResponseSchema)[0]?.message).toBe("proxy detail");

    const proxyStarted = await startGrpc(proxy);
    grpcServers.push(proxyStarted.server);
    const proxyClient = generatedClient(proxyStarted.address);
    const metadata = new grpc.Metadata();
    metadata.set("x-request-id", "proxy-123");
    metadata.add("x-repeat", "one");
    metadata.add("x-repeat", "two");
    metadata.add("x-repeat-bin", Buffer.from([1]));
    metadata.add("x-repeat-bin", Buffer.from([2]));
    metadata.add("x-comma", "left,right");
    const nativeResponse = await unaryWithMetadata(
      proxyClient,
      "greet",
      { name: "native" },
      metadata,
      { deadline: new Date(Date.now() + 10_000) },
    );
    expect(nativeResponse.response.message).toBe("Hi native");
    expect(nativeResponse.header.get("x-upstream-header")).toEqual(["present"]);
    expect(nativeResponse.header.get("x-upstream-comma")).toEqual(["left,right"]);
    expect(nativeResponse.header.get("x-upstream-repeat")).toEqual(["first, second"]);
    expect(nativeResponse.header.get("x-upstream-repeat-bin")).toEqual([Buffer.from([3]), Buffer.from([4])]);
    expect(nativeResponse.trailer.get("x-upstream-trailer")).toEqual(["present"]);
    expect(nativeResponse.trailer.get("x-upstream-repeat-trailer")).toEqual(["third, fourth"]);
    expect(nativeResponse.trailer.get("x-upstream-repeat-trailer-bin")).toEqual([
      Buffer.from([5]),
      Buffer.from([6]),
    ]);
    const nativeError = await unaryError(
      proxyClient,
      "greet",
      { name: "status" },
      metadata,
      { deadline: new Date(Date.now() + 10_000) },
    );
    expect(nativeError.code).toBe(grpc.status.FAILED_PRECONDITION);
    expect(nativeError.metadata.get("x-error-meta")).toEqual(["once"]);
    const nativeRich = fromBinary(
      StatusSchema,
      nativeError.metadata.get("grpc-status-details-bin")[0] as Buffer,
    );
    expect(fromBinary(GreetResponseSchema, nativeRich.details[0]!.value).message).toBe("proxy detail");

    context.abort();
    proxyClient.close();
    client.close();
  });

  test("forwards safe HandlerContext semantics through a remote HTTP projection", async () => {
    const detail = Buffer.from(
      toBinary(GreetResponseSchema, create(GreetResponseSchema, { message: "remote detail" })),
    ).toString("base64");
    const backend = createServer((request, response) => {
      const path = new URL(request.url ?? "/", "http://backend").pathname;
      expect(request.headers["x-request-id"]).toBe("proxy-http-123");
      expect(request.headers.traceparent).toBe("00-0123456789abcdef0123456789abcdef-0123456789abcdef-01");
      expect(request.headers.authorization).toBe("Bearer configured");
      expect(request.headers["x-tenant-id"]).toBeUndefined();
      expect(request.headers["x-role"]).toBeUndefined();
      if (path === "/v1/greet/Ada") {
        response.setHeader("content-type", "application/json");
        response.setHeader("x-upstream-header", "present");
        response.setHeader("set-cookie", "not-for-proxy-clients=true");
        response.end(JSON.stringify({ message: "Hi Ada" }));
        return;
      }
      if (path === "/v1/greet/status") {
        response.statusCode = 400;
        response.setHeader("content-type", "application/json");
        response.setHeader("x-error-meta", "present");
        response.end(JSON.stringify({
          code: "failed_precondition",
          message: "remote status",
          details: [{ type: GreetResponseSchema.typeName, value: detail }],
        }));
        return;
      }
      response.statusCode = 404;
      response.end();
    });
    httpServers.push(backend);
    await new Promise<void>((resolveListen) => backend.listen(0, "127.0.0.1", resolveListen));
    const address = backend.address();
    if (!address || typeof address === "string") {
      throw new Error("missing backend HTTP address");
    }

    const proxy = Server.fromDescriptor(descriptorPath);
    proxy.connectHttp(`http://127.0.0.1:${address.port}`, {
      auth: () => ({ authorization: "Bearer configured" }),
    });
    const requestHeader = {
      authorization: "Bearer caller",
      traceparent: "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01",
      "x-request-id": "proxy-http-123",
      "x-role": "admin",
      "x-tenant-id": "tenant-a",
    };
    const successContext = createHandlerContext({
      service: GreetService,
      method: GreetService.method.greet,
      protocolName: "connect",
      requestMethod: "POST",
      url: "http://proxy/greet.v1.GreetService/Greet",
      timeoutMs: 10_000,
      requestHeader,
    });
    const success = await proxy.invoke("GreetService.Greet", { name: "Ada" }, successContext);
    expect(proxy.toJson(proxy.tools.get("GreetService.Greet")!, success)).toMatchObject({ message: "Hi Ada" });
    expect(successContext.responseHeader.get("x-upstream-header")).toBe("present");
    expect(successContext.responseHeader.get("set-cookie")).toBeNull();
    successContext.abort();

    const errorContext = createHandlerContext({
      service: GreetService,
      method: GreetService.method.greet,
      protocolName: "connect",
      requestMethod: "POST",
      url: "http://proxy/greet.v1.GreetService/Greet",
      timeoutMs: 10_000,
      requestHeader,
    });
    let remoteError: unknown;
    try {
      await proxy.invoke("GreetService.Greet", { name: "status" }, errorContext);
    } catch (error) {
      remoteError = error;
    }
    expect(remoteError).toBeInstanceOf(InvariantError);
    expect(remoteError).toMatchObject({ code: "failed_precondition", message: "remote status" });
    expect((remoteError as InvariantError).metadata.get("x-error-meta")).toBe("present");
    errorContext.abort();

    const nativeStarted = await startGrpc(proxy);
    grpcServers.push(nativeStarted.server);
    const nativeClient = generatedClient(nativeStarted.address);
    const nativeMetadata = new grpc.Metadata();
    nativeMetadata.set("x-request-id", "proxy-http-123");
    nativeMetadata.set("traceparent", "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01");
    nativeMetadata.set("authorization", "Bearer caller");
    nativeMetadata.set("x-tenant-id", "tenant-a");
    const nativeResponse = await unaryWithMetadata(
      nativeClient,
      "greet",
      { name: "Ada" },
      nativeMetadata,
      { deadline: new Date(Date.now() + 10_000) },
    );
    expect(nativeResponse.response.message).toBe("Hi Ada");
    expect(nativeResponse.header.get("x-upstream-header")).toEqual(["present"]);
    expect(nativeResponse.header.get("set-cookie")).toEqual([]);
    const nativeError = await unaryError(
      nativeClient,
      "greet",
      { name: "status" },
      nativeMetadata,
      { deadline: new Date(Date.now() + 10_000) },
    );
    expect(nativeError.code).toBe(grpc.status.FAILED_PRECONDITION);
    expect(nativeError.metadata.get("x-error-meta")).toEqual(["present"]);
    const nativeRich = fromBinary(
      StatusSchema,
      nativeError.metadata.get("grpc-status-details-bin")[0] as Buffer,
    );
    expect(fromBinary(GreetResponseSchema, nativeRich.details[0]!.value).message).toBe("remote detail");
    nativeClient.close();

    const base = await startHTTP(proxy, httpServers);
    const projectedSuccess = await fetch(`${base}/greet.v1.GreetService/Greet`, {
      method: "POST",
      headers: { "content-type": "application/json", ...requestHeader },
      body: JSON.stringify({ name: "Ada" }),
    });
    expect(projectedSuccess.status).toBe(200);
    expect(projectedSuccess.headers.get("x-upstream-header")).toBe("present");
    expect(projectedSuccess.headers.get("set-cookie")).toBeNull();

    const projectedError = await fetch(`${base}/greet.v1.GreetService/Greet`, {
      method: "POST",
      headers: { "content-type": "application/json", ...requestHeader },
      body: JSON.stringify({ name: "status" }),
    });
    expect(projectedError.status).toBe(400);
    expect(projectedError.headers.get("x-error-meta")).toBe("present");
    const payload = await projectedError.json() as {
      code: string;
      details: Array<{ type: string; value: string }>;
    };
    expect(payload.code).toBe("failed_precondition");
    expect(fromBinary(GreetResponseSchema, Buffer.from(payload.details[0]!.value, "base64")).message).toBe(
      "remote detail",
    );
  });

  test("bounds chunked remote HTTP responses and combines cancellation with deadlines", async () => {
    let entered!: () => void;
    let closed!: () => void;
    let requestEntered = Promise.resolve();
    let responseClosed = Promise.resolve();
    const preparePendingRequest = () => {
      requestEntered = new Promise<void>((resolveEntered) => {
        entered = resolveEntered;
      });
      responseClosed = new Promise<void>((resolveClosed) => {
        closed = resolveClosed;
      });
    };
    const backend = createServer((request, response) => {
      const path = new URL(request.url ?? "/", "http://backend").pathname;
      if (path === "/v1/greet/Oversized") {
        response.setHeader("content-type", "application/json");
        response.write('{"message":"');
        response.write("x".repeat(40));
        setImmediate(() => {
          response.write("x".repeat(40));
          response.end('"}');
        });
        return;
      }
      entered();
      response.on("close", closed);
    });
    httpServers.push(backend);
    await new Promise<void>((resolveListen) => backend.listen(0, "127.0.0.1", resolveListen));
    const address = backend.address();
    if (!address || typeof address === "string") {
      throw new Error("missing backend HTTP address");
    }

    const observedBodies: Uint8Array[] = [];
    const proxy = Server.fromDescriptor(descriptorPath);
    proxy.connectHttp(`http://127.0.0.1:${address.port}`, {
      channelOptions: {
        connectTimeoutMs: 250,
        readTimeoutMs: 250,
        maxReceiveMessageSize: 64,
      },
      observer: (response) => {
        observedBodies.push(response.body);
      },
    });

    const cancelController = new AbortController();
    const cancelContext = createHandlerContext({
      service: GreetService,
      method: GreetService.method.greet,
      protocolName: "connect",
      requestMethod: "POST",
      url: "http://proxy/greet.v1.GreetService/Greet",
      timeoutMs: 10_000,
      requestSignal: cancelController.signal,
    });
    preparePendingRequest();
    const canceledCall = proxy.invoke("GreetService.Greet", { name: "Cancel" }, cancelContext);
    await requestEntered;
    cancelController.abort();
    await expect(canceledCall).rejects.toMatchObject({ code: "canceled" });
    await responseClosed;
    cancelContext.abort();

    const deadlineContext = createHandlerContext({
      service: GreetService,
      method: GreetService.method.greet,
      protocolName: "connect",
      requestMethod: "POST",
      url: "http://proxy/greet.v1.GreetService/Greet",
      timeoutMs: 250,
    });
    preparePendingRequest();
    const deadlineCall = proxy.invoke("GreetService.Greet", { name: "Deadline" }, deadlineContext);
    await requestEntered;
    await expect(deadlineCall).rejects.toMatchObject({ code: "deadline_exceeded" });
    await responseClosed;
    deadlineContext.abort();

    const configuredContext = createHandlerContext({
      service: GreetService,
      method: GreetService.method.greet,
      protocolName: "connect",
      requestMethod: "POST",
      url: "http://proxy/greet.v1.GreetService/Greet",
      timeoutMs: 10_000,
    });
    preparePendingRequest();
    const configuredCall = proxy.invoke("GreetService.Greet", { name: "Configured" }, configuredContext);
    await requestEntered;
    await expect(configuredCall).rejects.toMatchObject({ code: "deadline_exceeded" });
    await responseClosed;
    configuredContext.abort();

    await expect(proxy.invoke("GreetService.Greet", { name: "Oversized" })).rejects.toMatchObject({
      code: "resource_exhausted",
    });
    expect(observedBodies.at(-1)?.byteLength).toBe(64);
  });

  test("preserves long deadlines in the outbound HTTP client", async () => {
    const backend = createServer((_request, response) => {
      setTimeout(() => {
        response.setHeader("content-type", "application/json");
        response.end(JSON.stringify({ message: "Hi Long" }));
      }, 20);
    });
    httpServers.push(backend);
    await new Promise<void>((resolveListen) => backend.listen(0, "127.0.0.1", resolveListen));
    const address = backend.address();
    if (!address || typeof address === "string") {
      throw new Error("missing backend HTTP address");
    }

    const proxy = Server.fromDescriptor(descriptorPath);
    proxy.connectHttp(`http://127.0.0.1:${address.port}`, {
      channelOptions: {
        connectTimeoutMs: 1_500_000_000,
        readTimeoutMs: 1_500_000_000,
      },
    });
    const response = await proxy.invoke("GreetService.Greet", { name: "Long" });
    expect(proxy.toJson(proxy.tools.get("GreetService.Greet")!, response)).toMatchObject({
      message: "Hi Long",
    });
  });

  test("preserves ConnectError code, details, metadata and classifies unexpected failures", async () => {
    const server = Server.fromDescriptor(descriptorPath);
    server.register(GreetService, {
      greet(request, context) {
        if (request.name === "panic") {
          throw new Error("broken");
        }
        context.responseHeader.set("x-before-error", "present");
        throw new ConnectError(
          "not ready",
          Code.FailedPrecondition,
          { "x-error-meta": "present" },
          [{ desc: GreetResponseSchema, value: { message: "detail" } }],
        );
      },
    });
    const mcpError = await mcpDispatch(server, {
      jsonrpc: "2.0",
      id: 1,
      method: "tools/call",
      params: { name: "GreetService.Greet", arguments: { name: "status" } },
    }) as {
      result: {
        error: {
          details: Array<{ type: string; value: string }>;
        };
      };
    };
    expect(mcpError.result.error.details[0]?.type).toBe(GreetResponseSchema.typeName);
    expect(
      fromBinary(
        GreetResponseSchema,
        Buffer.from(mcpError.result.error.details[0]!.value, "base64"),
      ).message,
    ).toBe("detail");

    const base = await startHTTP(server, httpServers);
    const httpError = await fetch(`${base}/greet.v1.GreetService/Greet`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ name: "status" }),
    });
    expect(httpError.status).toBe(400);
    const httpPayload = await httpError.json() as {
      code: string;
      message: string;
      details: Array<{ type: string; value: string }>;
    };
    expect(httpPayload).toMatchObject({
      code: "failed_precondition",
      message: "not ready",
      details: [{ type: "greet.v1.GreetResponse" }],
    });
    expect(fromBinary(GreetResponseSchema, Buffer.from(httpPayload.details[0]!.value, "base64")).message).toBe(
      "detail",
    );

    const unexpected = await fetch(`${base}/greet.v1.GreetService/Greet`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ name: "panic" }),
    });
    expect(await unexpected.json()).toMatchObject({
      code: "internal",
      message: expect.stringContaining("/greet.v1.GreetService/Greet"),
    });

    const native = Server.fromDescriptor(descriptorPath);
    native.register(GreetService, {
      greet() {
        throw new ConnectError(
          "not ready",
          Code.FailedPrecondition,
          { "x-error-meta": "present" },
          [{ desc: GreetResponseSchema, value: { message: "detail" } }],
        );
      },
    });
    const started = await startGrpc(native);
    grpcServers.push(started.server);
    const client = generatedClient(started.address);
    const error = await unaryError(client, "greet", { name: "status" });
    expect(error.code).toBe(grpc.status.FAILED_PRECONDITION);
    expect(error.metadata.get("x-error-meta")).toEqual(["present"]);
    const encodedStatus = error.metadata.get("grpc-status-details-bin")[0];
    expect(Buffer.isBuffer(encodedStatus)).toBe(true);
    const rich = fromBinary(StatusSchema, encodedStatus as Buffer);
    expect(rich.code).toBe(Code.FailedPrecondition);
    expect(fromBinary(GreetResponseSchema, rich.details[0]!.value).message).toBe("detail");
    client.close();
  });
});

describe("MCP stdio", () => {
  test("validates direct JSON-RPC messages and parameters", async () => {
    const server = registeredServer();
    for (const message of [
      42,
      [],
      { id: 1, method: "ping" },
      { jsonrpc: "1.0", id: 2, method: "ping" },
      { jsonrpc: "2.0", id: 3 },
      { jsonrpc: "2.0", id: 4, method: 7 },
      { jsonrpc: "2.0", id: null, method: "ping" },
      { jsonrpc: "2.0", id: false, method: "ping" },
      { jsonrpc: "2.0", id: 1.5, method: "ping" },
      { jsonrpc: "2.0", id: Number.MAX_SAFE_INTEGER + 1, method: "ping" },
      { jsonrpc: "2.0", id: 5, result: "not-an-object" },
      { jsonrpc: "2.0", id: 6, result: {}, error: {} },
      { jsonrpc: "2.0", id: null, error: { code: -32601, message: "missing" } },
      { jsonrpc: "2.0", error: { code: 1.5, message: "bad code" } },
    ]) {
      await expect(mcpDispatch(server, message)).resolves.toMatchObject({
        id: null,
        error: { code: -32600 },
      });
    }
    const negativeZero = await mcpDispatch(server, {
      jsonrpc: "2.0",
      id: -0,
      method: "ping",
    });
    expect(negativeZero?.id).toBe(0);
    expect(Object.is(negativeZero?.id, -0)).toBe(false);
    for (const id of [Number.MAX_SAFE_INTEGER, Number.MIN_SAFE_INTEGER]) {
      await expect(mcpDispatch(server, {
        jsonrpc: "2.0",
        id,
        method: "ping",
      })).resolves.toMatchObject({ id, result: {} });
    }
    for (const message of [
      { jsonrpc: "2.0", id: 7, result: {} },
      { jsonrpc: "2.0", id: "response-8", error: { code: -32601, message: "missing" } },
      { jsonrpc: "2.0", error: { code: -32601, message: "unknown request" } },
    ]) {
      await expect(mcpDispatch(server, message)).resolves.toBeUndefined();
    }
    for (const message of [
      { jsonrpc: "2.0", id: 9, method: "ping", params: [] },
      { jsonrpc: "2.0", id: 10, method: "tools/call", params: { name: [], arguments: {} } },
      {
        jsonrpc: "2.0",
        id: 11,
        method: "tools/call",
        params: { name: "GreetService.Greet", arguments: [] },
      },
    ]) {
      await expect(mcpDispatch(server, message)).resolves.toMatchObject({
        error: { code: -32602 },
      });
    }
    for (const [index, params] of [
      undefined,
      {},
      {
        protocolVersion: 1,
        capabilities: {},
        clientInfo: { name: "test", version: "1" },
      },
      {
        protocolVersion: "2025-11-25",
        capabilities: [],
        clientInfo: { name: "test", version: "1" },
      },
      { protocolVersion: "2025-11-25", capabilities: {}, clientInfo: [] },
      {
        protocolVersion: "2025-11-25",
        capabilities: {},
        clientInfo: { name: 1, version: "1" },
      },
      {
        protocolVersion: "2025-11-25",
        capabilities: {},
        clientInfo: { name: "test", version: 1 },
      },
    ].entries()) {
      await expect(mcpDispatch(server, {
        jsonrpc: "2.0",
        id: index + 20,
        method: "initialize",
        ...(params === undefined ? {} : { params }),
      })).resolves.toMatchObject({
        id: index + 20,
        error: { code: -32602 },
      });
    }
    await expect(mcpDispatch(server, initializeRequest(30, "2099-01-01"))).resolves.toMatchObject({
      id: 30,
      result: { protocolVersion: "2025-11-25" },
    });
  });

  test("implements parse errors, validation, client-response acceptance, and notification silence", async () => {
    const output = new StringOutput();
    await serveMcpStdio(
      registeredServer(),
      chunks([
        "{bad json}\n",
        new Uint8Array([0xff, 0x0a]),
        "42\n",
        JSON.stringify({ jsonrpc: "1.0", id: 9, method: "ping" }) + "\n",
        JSON.stringify({ jsonrpc: "2.0", id: null, method: "ping" }) + "\n",
        JSON.stringify({ jsonrpc: "2.0", id: 10, result: {} }) + "\n",
        JSON.stringify({ jsonrpc: "2.0", error: { code: -32601, message: "unknown" } }) + "\n",
        JSON.stringify({ jsonrpc: "2.0", id: 11, method: "ping", params: [] }) + "\n",
        JSON.stringify({ jsonrpc: "2.0", id: 1, method: "missing" }) + "\n",
        JSON.stringify(initializeRequest(2)) + "\n",
        JSON.stringify({ jsonrpc: "2.0", method: "ping" }) + "\n",
      ]),
      output,
    );
    const responses = output.responses();
    expect(responses).toHaveLength(8);
    expect(responses.slice(0, 2)).toEqual([
      expect.objectContaining({ id: null, error: expect.objectContaining({ code: -32700 }) }),
      expect.objectContaining({ id: null, error: expect.objectContaining({ code: -32700 }) }),
    ]);
    expect(responses.slice(2, 5)).toEqual(
      expect.arrayContaining([
        expect.objectContaining({ id: null, error: { code: -32600, message: "Invalid Request" } }),
      ]),
    );
    expect(responses[5]).toMatchObject({ id: 11, error: { code: -32602 } });
    expect(responses[6]).toMatchObject({ id: 1, error: { code: -32601 } });
    expect(responses[7]).toMatchObject({ id: 2, result: { protocolVersion: "2025-11-25" } });
  });

  test("cancels an in-flight tool through its HandlerContext AbortSignal without a response", async () => {
    let protocol = "";
    let cancellationObserved!: () => void;
    const cancelled = new Promise<void>((resolveCancelled) => {
      cancellationObserved = resolveCancelled;
    });
    const server = Server.fromDescriptor(descriptorPath);
    server.register(GreetService, {
      greet(_request, context) {
        protocol = context.protocolName;
        return new Promise((_resolve, reject) => {
          context.signal.addEventListener(
            "abort",
            () => {
              cancellationObserved();
              reject(new DOMException("cancelled", "AbortError"));
            },
            { once: true },
          );
        });
      },
    });
    const output = new StringOutput();
    await serveMcpStdio(
      server,
      (async function* () {
        yield `${JSON.stringify({
          jsonrpc: "2.0",
          id: 7,
          method: "tools/call",
          params: { name: "GreetService.Greet", arguments: { name: "wait" } },
        })}\n`;
        await new Promise<void>((resolveTurn) => setImmediate(resolveTurn));
        yield `${JSON.stringify({
          jsonrpc: "2.0",
          method: "notifications/cancelled",
          params: { requestId: 7 },
        })}\n`;
      })(),
      output,
    );
    await cancelled;
    expect(protocol).toBe("mcp");
    expect(output.responses()).toEqual([]);
  });
});

describe("Protovalidate", () => {
  test("uses one Connect interceptor for unary and streaming requests with BadRequest details", async () => {
    const server = registeredServer();
    server.use(validation());

    let unaryError: unknown;
    try {
      await server.invoke("GreetService.Greet", { name: "" });
    } catch (error) {
      unaryError = error;
    }
    expect(unaryError).toBeInstanceOf(ConnectError);
    expect((unaryError as ConnectError).code).toBe(Code.InvalidArgument);
    expect((unaryError as ConnectError).findDetails(BadRequestSchema)[0]?.fieldViolations[0]).toMatchObject({
      field: "name",
    });

    let streamError: unknown;
    try {
      for await (const _message of server.invokeStream("GreetService.StreamGreet", { name: "", count: 1 })) {
        // Validation must fail before the generated handler emits a message.
      }
    } catch (error) {
      streamError = error;
    }
    expect(streamError).toBeInstanceOf(ConnectError);
    expect((streamError as ConnectError).code).toBe(Code.InvalidArgument);
    expect((streamError as ConnectError).findDetails(BadRequestSchema)[0]?.fieldViolations[0]).toMatchObject({
      field: "name",
    });
  });
});

function assertHandlerContext(context: HandlerContext, method: string): void {
  expect(context.method.name).toBe(method);
  expect(context.service.typeName).toBe("greet.v1.GreetService");
  expect(context.signal).toBeInstanceOf(AbortSignal);
  expect(context.requestHeader).toBeInstanceOf(Headers);
  expect(context.responseHeader).toBeInstanceOf(Headers);
  expect(context.responseTrailer).toBeInstanceOf(Headers);
}

function registeredServer(): Server {
  const server = Server.fromDescriptor(descriptorPath);
  server.register(GreetService, {
    greet(request) {
      return Promise.resolve({ message: `Hi ${request.name}` });
    },
  });
  return server;
}

async function startGrpc(
  server: Server,
  options?: grpc.ServerOptions,
): Promise<{ server: grpc.Server; address: string }> {
  const grpcServer = server.grpcServer(options);
  const port = await new Promise<number>((resolvePort, rejectPort) => {
    grpcServer.bindAsync("127.0.0.1:0", grpc.ServerCredentials.createInsecure(), (error, boundPort) => {
      if (error) {
        rejectPort(error);
      } else {
        resolvePort(boundPort);
      }
    });
  });
  return { server: grpcServer, address: `127.0.0.1:${port}` };
}

async function startHTTP(server: Server, servers: HTTPServer[]): Promise<string> {
  const nodeServer = createServer((req, res) => void httpHandler(server)(req, res));
  servers.push(nodeServer);
  await new Promise<void>((resolveListen) => nodeServer.listen(0, "127.0.0.1", resolveListen));
  const address = nodeServer.address();
  if (!address || typeof address === "string") {
    throw new Error("missing HTTP address");
  }
  return `http://127.0.0.1:${address.port}`;
}

function generatedClient(address: string): grpc.Client {
  const Client = grpc.makeGenericClientConstructor(
    grpcServiceDefinitionForService(GreetService),
    GreetService.typeName,
  );
  return new Client(address, grpc.credentials.createInsecure());
}

function unaryWithMetadata(
  client: grpc.Client,
  method: string,
  request: GreetRequest | { name: string },
  metadata: grpc.Metadata,
  options: grpc.CallOptions,
): Promise<{ response: any; header: grpc.Metadata; trailer: grpc.Metadata }> {
  return new Promise((resolveCall, rejectCall) => {
    let header = new grpc.Metadata();
    let response: any;
    let callbackDone = false;
    let trailer: grpc.Metadata | undefined;
    const settle = () => {
      if (callbackDone && trailer) {
        resolveCall({ response, header, trailer });
      }
    };
    const call = (client as any)[method](request, metadata, options, (error: grpc.ServiceError | null, value: any) => {
      if (error) {
        rejectCall(error);
        return;
      }
      response = value;
      callbackDone = true;
      settle();
    }) as grpc.ClientUnaryCall;
    call.on("metadata", (incoming) => {
      header = incoming;
    });
    call.on("status", (status) => {
      trailer = status.metadata;
      settle();
    });
  });
}

function unaryError(
  client: grpc.Client,
  method: string,
  request: unknown,
  metadata = new grpc.Metadata(),
  options: grpc.CallOptions = {},
): Promise<grpc.ServiceError> {
  return new Promise((resolveError, rejectError) => {
    (client as any)[method](request, metadata, options, (error: grpc.ServiceError | null) => {
      if (error) {
        resolveError(error);
      } else {
        rejectError(new Error("RPC unexpectedly succeeded"));
      }
    });
  });
}

async function* chunks(values: (string | Uint8Array)[]): AsyncIterable<string | Uint8Array> {
  for (const value of values) {
    yield value;
  }
}

class StringOutput {
  readonly chunks: string[] = [];

  write(chunk: string): void {
    this.chunks.push(chunk);
  }

  responses(): any[] {
    return this.chunks
      .join("")
      .split("\n")
      .filter(Boolean)
      .map((line) => JSON.parse(line));
  }
}
