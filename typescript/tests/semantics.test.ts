import { createServer, type Server as HTTPServer } from "node:http";
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
        context.responseHeader.set("x-initial", "present");
        context.responseTrailer.set("x-trailing", "present");
        return { message: `Hi ${request.name}` };
      },
    });
    const started = await startGrpc(server);
    grpcServers.push(started.server);
    const client = generatedClient(started.address);

    const metadata = new grpc.Metadata();
    metadata.set("x-request-id", "native-123");
    const callResult = await unaryWithMetadata(client, "greet", { name: "Ada" }, metadata, {
      deadline: new Date(Date.now() + 10_000),
    });
    expect(callResult.response.message).toBe("Hi Ada");
    expect(callResult.header.get("x-initial")).toEqual(["present"]);
    expect(callResult.trailer.get("x-trailing")).toEqual(["present"]);

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
      authorization: headers.get("authorization") ?? "",
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
    expect(seen[0]?.get("authorization")).toBeNull();
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

    expect((await fetch(`${base}/mcp`)).status).toBe(405);
    expect(
      (
        await fetch(`${base}/mcp`, {
          method: "POST",
          headers: { accept },
          body: JSON.stringify({ jsonrpc: "2.0", id: 1, method: "initialize" }),
        })
      ).status,
    ).toBe(200);

    const initialized = await fetch(`${base}/mcp`, {
      method: "POST",
      headers: { accept },
      body: JSON.stringify({ jsonrpc: "2.0", id: 2, method: "initialize" }),
    });
    expect(initialized.headers.get("mcp-protocol-version")).toBe("2025-11-25");
    expect(await initialized.json()).toMatchObject({
      result: { protocolVersion: "2025-11-25" },
    });

    const missingAccept = await fetch(`${base}/mcp`, {
      method: "POST",
      body: JSON.stringify({ jsonrpc: "2.0", id: 3, method: "initialize" }),
    });
    expect(missingAccept.status).toBe(406);

    const rejectedStream = await fetch(`${base}/mcp`, {
      method: "POST",
      headers: { accept: "application/json, text/event-stream;q=0" },
      body: JSON.stringify({ jsonrpc: "2.0", id: 3, method: "initialize" }),
    });
    expect(rejectedStream.status).toBe(406);

    const origin = await fetch(`${base}/mcp`, {
      method: "POST",
      headers: { accept, origin: "https://untrusted.example" },
      body: JSON.stringify({ jsonrpc: "2.0", id: 4, method: "initialize" }),
    });
    expect(origin.status).toBe(403);

    const missingVersion = await fetch(`${base}/mcp`, {
      method: "POST",
      headers: { accept },
      body: JSON.stringify({ jsonrpc: "2.0", id: 5, method: "ping" }),
    });
    expect(missingVersion.status).toBe(400);

    const wrongVersion = await fetch(`${base}/mcp`, {
      method: "POST",
      headers: { accept, "mcp-protocol-version": "2024-11-05" },
      body: JSON.stringify({ jsonrpc: "2.0", id: 6, method: "ping" }),
    });
    expect(wrongVersion.status).toBe(400);

    for (const payload of [
      { jsonrpc: "2.0", method: "notifications/initialized" },
      { jsonrpc: "2.0", id: 7, result: {} },
    ]) {
      const accepted = await fetch(`${base}/mcp`, {
        method: "POST",
        headers: { accept, "mcp-protocol-version": "2025-11-25" },
        body: JSON.stringify(payload),
      });
      expect(accepted.status).toBe(202);
      expect(await accepted.text()).toBe("");
    }

    const toolCall = await fetch(`${base}/mcp`, {
      method: "POST",
      headers: { accept, "mcp-protocol-version": "2025-11-25" },
      body: JSON.stringify({
        jsonrpc: "2.0",
        id: 8,
        method: "tools/call",
        params: { name: "GreetService.Greet", arguments: { name: "Ada" } },
      }),
    });
    expect(toolCall.status).toBe(200);
    expect(protocol).toBe("mcp");
  });

  test("forwards HandlerContext semantics through a remote gRPC projection", async () => {
    const backend = Server.fromDescriptor(descriptorPath);
    backend.register(GreetService, {
      greet(request, context) {
        expect(context.requestHeader.get("x-request-id")).toBe("proxy-123");
        expect(context.timeoutMs()).toBeGreaterThan(0);
        context.responseHeader.set("x-upstream-header", "present");
        context.responseTrailer.set("x-upstream-trailer", "present");
        if (request.name === "status") {
          throw new ConnectError(
            "upstream status",
            Code.FailedPrecondition,
            undefined,
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
      requestHeader: { "x-request-id": "proxy-123" },
    });
    const response = await proxy.invoke("GreetService.Greet", { name: "Ada" }, context);
    expect(proxy.toJson(proxy.tools.get("GreetService.Greet")!, response)).toMatchObject({ message: "Hi Ada" });
    expect(context.responseHeader.get("x-upstream-header")).toBe("present");
    expect(context.responseTrailer.get("x-upstream-trailer")).toBe("present");

    let thrown: unknown;
    try {
      await proxy.invoke("GreetService.Greet", { name: "status" }, context);
    } catch (error) {
      thrown = error;
    }
    expect(thrown).toBeInstanceOf(ConnectError);
    expect((thrown as ConnectError).code).toBe(Code.FailedPrecondition);
    expect((thrown as ConnectError).findDetails(GreetResponseSchema)[0]?.message).toBe("proxy detail");
    context.abort();
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
  test("implements parse errors, method-not-found, and notification silence", async () => {
    const output = new StringOutput();
    await serveMcpStdio(
      registeredServer(),
      chunks([
        "{bad json}\n",
        JSON.stringify({ jsonrpc: "2.0", id: 1, method: "missing" }) + "\n",
        JSON.stringify({ jsonrpc: "2.0", id: 2, method: "initialize" }) + "\n",
        JSON.stringify({ jsonrpc: "2.0", method: "ping" }) + "\n",
      ]),
      output,
    );
    const responses = output.responses();
    expect(responses).toHaveLength(3);
    expect(responses[0]).toMatchObject({ id: null, error: { code: -32700 } });
    expect(responses[1]).toMatchObject({ id: 1, error: { code: -32601 } });
    expect(responses[2]).toMatchObject({ id: 2, result: { protocolVersion: "2025-11-25" } });
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

function unaryError(client: grpc.Client, method: string, request: unknown): Promise<grpc.ServiceError> {
  return new Promise((resolveError, rejectError) => {
    (client as any)[method](request, (error: grpc.ServiceError | null) => {
      if (error) {
        resolveError(error);
      } else {
        rejectError(new Error("RPC unexpectedly succeeded"));
      }
    });
  });
}

async function* chunks(values: string[]): AsyncIterable<string> {
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
