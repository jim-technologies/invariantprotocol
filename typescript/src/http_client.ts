import { type DescField, type DescMessage, fromJson, getOption, type JsonValue, toJson } from "@bufbuild/protobuf";
import { ConnectError } from "@connectrpc/connect";

import { monotonicDeadlineAfter, scheduleAbsoluteDeadline } from "./deadline.js";
import { asInvariantError, type Code, codeFromHttpStatus, InvariantError, invalidArgument } from "./errors.js";
import type { HandlerContext, Server, Tool, UnaryHandler } from "./server.js";

export type OutboundHTTPRequest = {
  methodPath: string;
  method: string;
  url: string;
  body: Uint8Array;
};

export type OutboundHTTPResponse = {
  methodPath: string;
  statusCode: number;
  headers: Record<string, string>;
  body: Uint8Array;
  durationMs: number;
  success: boolean;
  request: OutboundHTTPRequest;
};

export type HTTPHeaderProvider = (
  request: OutboundHTTPRequest,
) => Record<string, string> | undefined | Promise<Record<string, string> | undefined>;
export type HTTPQueryProvider = (
  request: OutboundHTTPRequest,
) => Record<string, string> | undefined | Promise<Record<string, string> | undefined>;
export type HTTPResponseObserver = (response: OutboundHTTPResponse) => void | Promise<void>;

export type HTTPAuth = {
  headerProvider?: HTTPHeaderProvider;
  queryProvider?: HTTPQueryProvider;
};

export type ChannelOptions = {
  maxReceiveMessageSize?: number;
  connectTimeoutMs?: number;
  readTimeoutMs?: number;
};

export type ConnectHttpOptions = {
  serviceName?: string;
  auth?: HTTPAuth | HTTPHeaderProvider;
  channelOptions?: ChannelOptions;
  observer?: HTTPResponseObserver;
};

export class HTTPConnection {
  readonly baseUrl: string;
  readonly auth: HTTPAuth;
  readonly options: Required<ChannelOptions>;
  readonly observer?: HTTPResponseObserver;
  private readonly envHeaders: Record<string, string>;

  constructor(baseUrl: string, options: ConnectHttpOptions = {}) {
    this.baseUrl = validateBaseUrl(baseUrl);
    this.auth = typeof options.auth === "function" ? { headerProvider: options.auth } : (options.auth ?? {});
    this.options = {
      maxReceiveMessageSize: options.channelOptions?.maxReceiveMessageSize ?? 16 * 1024 * 1024,
      connectTimeoutMs: options.channelOptions?.connectTimeoutMs ?? 10_000,
      readTimeoutMs: options.channelOptions?.readTimeoutMs ?? 10_000,
    };
    this.observer = options.observer;
    this.envHeaders = outboundHeadersFromEnv();
  }

  async send(
    methodPath: string,
    method: string,
    url: string,
    body: Uint8Array,
    context: HandlerContext,
  ): Promise<Uint8Array> {
    const req: OutboundHTTPRequest = { methodPath, method, url, body };
    const requestScope = outboundRequestScope(context, this.options.connectTimeoutMs + this.options.readTimeoutMs);
    let signedReq!: OutboundHTTPRequest;
    let response!: Response;
    let read!: ReadResult;
    let started = 0;
    try {
      if (requestScope.signal.aborted) {
        throw asInvariantError(requestScope.signal.reason);
      }
      const signedUrl = await this.applyQueryProvider(req);
      signedReq = { ...req, url: signedUrl };
      const headers = await this.headers(signedReq, context);
      if (requestScope.signal.aborted) {
        throw asInvariantError(requestScope.signal.reason);
      }
      started = performance.now();
      try {
        response = await fetch(signedUrl, {
          method,
          headers,
          body: method === "GET" || method === "HEAD" || body.length === 0 ? undefined : Buffer.from(body),
          signal: requestScope.signal,
        });
        read = await readResponseBody(response, this.options.maxReceiveMessageSize);
      } catch (e) {
        if (requestScope.signal.aborted) {
          throw asInvariantError(requestScope.signal.reason);
        }
        if ((e as Error).name === "AbortError" || (e as Error).name === "TimeoutError") {
          throw new InvariantError("canceled", (e as Error).message || "HTTP request canceled");
        }
        throw new InvariantError("unavailable", (e as Error).message);
      }
    } finally {
      requestScope.cleanup();
    }

    const durationMs = performance.now() - started;
    const headersRecord = Object.fromEntries(response.headers.entries());
    const responseMetadata = outboundResponseMetadata(response.headers);
    const success = response.ok && !read.exceeded;
    await this.observe({
      methodPath,
      statusCode: response.status,
      headers: headersRecord,
      body: read.body,
      durationMs,
      success,
      request: signedReq,
    });

    if (read.exceeded) {
      throw new InvariantError(
        "resource_exhausted",
        `response body exceeds ${this.options.maxReceiveMessageSize} byte limit`,
        [],
        responseMetadata,
      );
    }
    if (!response.ok) {
      throw httpError(response.status, read.body, responseMetadata);
    }
    appendHeaders(context.responseHeader, responseMetadata);
    return read.body;
  }

  private async headers(request: OutboundHTTPRequest, context: HandlerContext): Promise<Record<string, string>> {
    const headers: Record<string, string> = {
      ...reviewedRequestMetadata(context.requestHeader),
      ...this.envHeaders,
      "content-type": "application/json",
      accept: "application/json",
      "user-agent": "invariant-protocol/typescript",
    };
    const extra = await this.auth.headerProvider?.(request);
    if (extra) {
      Object.assign(headers, extra);
    }
    return headers;
  }

  private async applyQueryProvider(request: OutboundHTTPRequest): Promise<string> {
    const extra = await this.auth.queryProvider?.(request);
    if (!extra || Object.keys(extra).length === 0) {
      return request.url;
    }
    const url = new URL(request.url);
    for (const [key, value] of Object.entries(extra)) {
      url.searchParams.set(key, value);
    }
    return url.toString();
  }

  private async observe(response: OutboundHTTPResponse): Promise<void> {
    try {
      await this.observer?.(response);
    } catch {
      // Observers are side-effecting hooks and must not break request handling.
    }
  }
}

export class HTTPClientBinding {
  readonly method: string;
  readonly pattern: string;
  body: string;
  responseBody: string;
  private readonly template: PathTemplate;

  constructor(method: string, pattern: string, body: string, responseBody = "") {
    this.method = method.toUpperCase();
    this.pattern = pattern;
    this.body = body;
    this.responseBody = responseBody;
    this.template = PathTemplate.parse(pattern);
  }

  resolveFields(input: DescMessage, output: DescMessage): void {
    for (const segment of this.template.segments) {
      if (segment.field) {
        segment.field = jsonFieldPath(input, segment.field);
      }
    }
    if (this.body && this.body !== "*") {
      this.body = jsonFieldPath(input, this.body);
    }
    if (this.responseBody && this.responseBody !== "*") {
      this.responseBody = jsonFieldPath(output, this.responseBody);
    }
  }

  build(args: Record<string, unknown>, baseUrl: string): { body: Uint8Array; url: string } {
    const working = cloneRecord(args);
    const consumed: string[] = [];
    const path = this.template.expand(working, consumed);
    for (const field of consumed) {
      deleteNested(working, field);
    }

    const body = this.buildBody(working);
    if (body.consumed && body.consumed !== "*") {
      deleteNested(working, body.consumed);
    }

    const url = new URL(`${baseUrl.replace(/\/$/, "")}${path}`);
    if (this.body !== "*") {
      const params: Array<[string, string]> = [];
      encodeQuery("", working, params);
      for (const [key, value] of params) {
        url.searchParams.append(key, value);
      }
    }
    return { body: body.bytes, url: url.toString() };
  }

  private buildBody(args: Record<string, unknown>): { bytes: Uint8Array; consumed?: string } {
    if (!this.body) {
      return { bytes: new Uint8Array() };
    }
    if (this.body === "*") {
      return { bytes: encodeJson(args), consumed: "*" };
    }
    const value = getNested(args, this.body);
    return { bytes: value === undefined ? new Uint8Array() : encodeJson(value), consumed: this.body };
  }
}

export function httpRulesByMethodPath(server: Server): Map<string, unknown> {
  const rules = new Map<string, unknown>();
  const extension = server.parsed.registry.getExtension("google.api.http");
  if (!extension) {
    return rules;
  }

  for (const service of server.parsed.services.values()) {
    for (const method of service.desc.methods) {
      const rule = getOption(method, extension);
      if (rule && (rule as { pattern?: { case?: string } }).pattern?.case) {
        rules.set(`/${service.fullName}/${method.name}`, rule);
      }
    }
  }
  return rules;
}

export function clientBindingForMethod(rule: unknown, serviceName: string, methodName: string): HTTPClientBinding {
  if (!rule) {
    return new HTTPClientBinding("POST", `/${serviceName}/${methodName}`, "*");
  }

  const httpRule = rule as {
    pattern?: { case?: string; value?: string | { kind?: string; path?: string } };
    body?: string;
    responseBody?: string;
  };
  const patternCase = httpRule.pattern?.case;
  const patternValue = httpRule.pattern?.value;
  if (patternCase === "custom" && typeof patternValue === "object") {
    return new HTTPClientBinding(
      patternValue.kind || "POST",
      patternValue.path || `/${serviceName}/${methodName}`,
      httpRule.body || "",
      httpRule.responseBody || "",
    );
  }
  if (patternCase && typeof patternValue === "string") {
    return new HTTPClientBinding(patternCase, patternValue, httpRule.body || "", httpRule.responseBody || "");
  }
  return new HTTPClientBinding("POST", `/${serviceName}/${methodName}`, "*");
}

export function httpProxyHandler(
  server: Server,
  connection: HTTPConnection,
  binding: HTTPClientBinding,
  tool: Tool,
  methodPath: string,
): UnaryHandler {
  binding.resolveFields(tool.inputDesc, tool.outputDesc);
  return async (request, context) => {
    const args = toJson(tool.inputDesc, request, { registry: server.parsed.registry }) as Record<string, unknown>;
    const built = binding.build(args, connection.baseUrl);
    const bytes = await connection.send(methodPath, binding.method, built.url, built.body, context);
    const payload = bytes.length === 0 ? {} : JSON.parse(Buffer.from(bytes).toString("utf8"));
    return fromJson(tool.outputDesc, responseBody(payload, binding.responseBody) as JsonValue, {
      registry: server.parsed.registry,
    });
  };
}

type TemplateSegment = {
  literal: string;
  field: string;
  multi: boolean;
};

class PathTemplate {
  constructor(
    readonly segments: TemplateSegment[],
    private readonly trailingSlash: boolean,
  ) {}

  static parse(pattern: string): PathTemplate {
    const trailingSlash = pattern.length > 1 && pattern.endsWith("/");
    const parts = pattern
      .replace(/^\/+|\/+$/g, "")
      .split("/")
      .filter(Boolean);
    const segments = parts.map((part) => {
      const match = /^\{([^}=]+)(?:=([^}]+))?\}$/.exec(part);
      if (!match) {
        return { literal: part, field: "", multi: false };
      }
      const template = match[2] ?? "*";
      return { literal: "", field: match[1] ?? "", multi: template === "**" };
    });
    return new PathTemplate(segments, trailingSlash);
  }

  expand(args: Record<string, unknown>, consumed: string[]): string {
    const parts = this.segments.map((segment) => {
      if (!segment.field) {
        return segment.literal;
      }
      const value = getNested(args, segment.field);
      if (value === undefined || value === null) {
        throw invalidArgument(`missing path field "${segment.field}"`);
      }
      consumed.push(segment.field);
      return encodePathValue(value, segment.multi);
    });
    let path = `/${parts.join("/")}`;
    if (this.trailingSlash) {
      path += "/";
    }
    return path;
  }
}

function validateBaseUrl(baseUrl: string): string {
  const parsed = new URL(baseUrl);
  if (parsed.protocol !== "http:" && parsed.protocol !== "https:") {
    throw new Error("connectHttp baseUrl must use http:// or https://");
  }
  return parsed.toString().replace(/\/$/, "");
}

function outboundHeadersFromEnv(): Record<string, string> {
  const headers: Record<string, string> = {};
  for (const [key, value] of Object.entries(process.env)) {
    if (key.startsWith("INVARIANT_HTTP_HEADER_") && value !== undefined) {
      headers[key.slice("INVARIANT_HTTP_HEADER_".length).replace(/_/g, "-")] = value;
    }
  }
  return headers;
}

function jsonFieldPath(desc: DescMessage, fieldPath: string): string {
  const parts: string[] = [];
  let current: DescMessage | undefined = desc;
  for (const name of fieldPath.split(".")) {
    const field = current?.fields.find((candidate) => candidate.name === name);
    if (!field) {
      parts.push(name);
      current = undefined;
      continue;
    }
    parts.push(field.jsonName);
    current = messageField(field);
  }
  return parts.join(".");
}

function messageField(field: DescField): DescMessage | undefined {
  if (field.fieldKind === "message") {
    return field.message;
  }
  if (field.fieldKind === "list" && field.listKind === "message") {
    return field.message;
  }
  if (field.fieldKind === "map" && field.mapKind === "message") {
    return field.message;
  }
  return undefined;
}

function cloneRecord(value: Record<string, unknown>): Record<string, unknown> {
  return JSON.parse(JSON.stringify(value)) as Record<string, unknown>;
}

function encodeJson(value: unknown): Uint8Array {
  return Buffer.from(JSON.stringify(value));
}

function getNested(obj: Record<string, unknown>, path: string): unknown {
  let current: unknown = obj;
  for (const part of path.split(".")) {
    if (typeof current !== "object" || current === null || !(part in current)) {
      return undefined;
    }
    current = (current as Record<string, unknown>)[part];
  }
  return current;
}

function deleteNested(obj: Record<string, unknown>, path: string): void {
  const parts = path.split(".");
  let current: Record<string, unknown> = obj;
  for (const part of parts.slice(0, -1)) {
    const next = current[part];
    if (typeof next !== "object" || next === null) {
      return;
    }
    current = next as Record<string, unknown>;
  }
  delete current[parts.at(-1) ?? ""];
}

function encodePathValue(value: unknown, multi: boolean): string {
  const text = String(value);
  if (multi) {
    return text.split("/").map(encodeURIComponent).join("/");
  }
  return encodeURIComponent(text);
}

function encodeQuery(prefix: string, value: unknown, out: Array<[string, string]>): void {
  if (value === undefined || value === null) {
    return;
  }
  if (Array.isArray(value)) {
    for (const item of value) {
      encodeQuery(prefix, item, out);
    }
    return;
  }
  if (typeof value === "object") {
    for (const [key, nested] of Object.entries(value)) {
      encodeQuery(prefix ? `${prefix}.${key}` : key, nested, out);
    }
    return;
  }
  if (prefix) {
    out.push([prefix, String(value)]);
  }
}

function responseBody(payload: unknown, selector: string): unknown {
  if (!selector || selector === "*") {
    return payload;
  }
  const parts = selector.split(".").filter(Boolean);
  if (parts.length === 0) {
    return {};
  }
  let wrapped = payload;
  for (const part of parts.reverse()) {
    wrapped = { [part]: wrapped };
  }
  return wrapped;
}

function httpError(status: number, body: Uint8Array, metadata: Headers): InvariantError {
  const text = Buffer.from(body).toString("utf8");
  try {
    const payload = JSON.parse(text) as { code?: unknown; message?: unknown; details?: unknown };
    const code = connectCode(payload.code);
    if (code) {
      return new InvariantError(
        code,
        typeof payload.message === "string" ? payload.message : `HTTP ${status}`,
        Array.isArray(payload.details) ? payload.details : [],
        metadata,
      );
    }
  } catch {
    // Fall through to the generic HTTP error.
  }
  return new InvariantError(codeFromHttpStatus(status), text || `HTTP ${status}`, [], metadata);
}

type ReadResult = {
  body: Uint8Array;
  exceeded: boolean;
};

async function readResponseBody(response: Response, maxBytes: number): Promise<ReadResult> {
  if (response.body === null) {
    return { body: new Uint8Array(), exceeded: false };
  }
  const reader = response.body.getReader();
  const chunks: Uint8Array[] = [];
  let retained = 0;
  try {
    while (true) {
      const result = await reader.read();
      if (result.done) {
        return { body: Buffer.concat(chunks, retained), exceeded: false };
      }
      const chunk = result.value;
      if (retained + chunk.byteLength > maxBytes) {
        const remaining = maxBytes - retained;
        if (remaining > 0) {
          chunks.push(chunk.subarray(0, remaining));
          retained += remaining;
        }
        await reader.cancel().catch(() => undefined);
        return { body: Buffer.concat(chunks, retained), exceeded: true };
      }
      chunks.push(chunk);
      retained += chunk.byteLength;
    }
  } finally {
    reader.releaseLock();
  }
}

function outboundRequestScope(
  context: HandlerContext,
  configuredTimeoutMs: number,
): { signal: AbortSignal; cleanup: () => void } {
  const controller = new AbortController();
  const callerTimeoutMs = context.timeoutMs();
  const transportTimeoutMs =
    callerTimeoutMs === undefined || configuredTimeoutMs < callerTimeoutMs
      ? Math.max(0, configuredTimeoutMs)
      : undefined;
  const cleanupDeadline = scheduleAbsoluteDeadline(
    transportTimeoutMs === undefined ? undefined : monotonicDeadlineAfter(transportTimeoutMs),
    () => {
      controller.abort(new InvariantError("deadline_exceeded", `HTTP request exceeded ${transportTimeoutMs}ms`));
    },
  );
  const cancel = () => controller.abort(contextAbortError(context));
  if (context.signal.aborted) {
    cancel();
  } else {
    context.signal.addEventListener("abort", cancel, { once: true });
  }
  return {
    signal: controller.signal,
    cleanup: () => {
      cleanupDeadline();
      context.signal.removeEventListener("abort", cancel);
    },
  };
}

function contextAbortError(context: HandlerContext): InvariantError {
  const reason = context.signal.reason;
  if (reason instanceof InvariantError) {
    return reason;
  }
  if (reason instanceof ConnectError) {
    return asInvariantError(reason);
  }
  const remaining = context.timeoutMs();
  if (remaining !== undefined && remaining <= 0) {
    return new InvariantError("deadline_exceeded", "HTTP request deadline exceeded");
  }
  return new InvariantError(
    "canceled",
    reason instanceof Error && reason.message ? reason.message : "HTTP request canceled",
  );
}

const REVIEWED_REQUEST_METADATA = ["traceparent", "tracestate", "baggage", "x-request-id"] as const;

function reviewedRequestMetadata(headers: Headers): Record<string, string> {
  const reviewed: Record<string, string> = {};
  for (const name of REVIEWED_REQUEST_METADATA) {
    const value = headers.get(name);
    if (value !== null && validMetadataValue(value)) {
      reviewed[name] = value;
    }
  }
  return reviewed;
}

function outboundResponseMetadata(headers: Headers): Headers {
  const metadata = new Headers();
  headers.forEach((value, rawName) => {
    const name = rawName.trim().toLowerCase();
    if (validMetadataName(name) && validMetadataValue(value) && !reservedResponseHeader(name)) {
      metadata.append(name, value);
    }
  });
  return metadata;
}

function appendHeaders(target: Headers, source: Headers): void {
  source.forEach((value, name) => {
    target.append(name, value);
  });
}

function validMetadataName(name: string): boolean {
  return name.length > 0 && /^[a-z0-9._-]+$/.test(name);
}

function validMetadataValue(value: string): boolean {
  return value.length > 0 && /^[\x20-\x7e]+$/.test(value);
}

function reservedResponseHeader(name: string): boolean {
  if (name.startsWith("grpc-") || name.startsWith("connect-")) {
    return true;
  }
  return new Set([
    "connection",
    "content-encoding",
    "content-length",
    "content-type",
    "date",
    "keep-alive",
    "proxy-authenticate",
    "proxy-authorization",
    "server",
    "set-cookie",
    "te",
    "trailer",
    "transfer-encoding",
    "upgrade",
  ]).has(name);
}

const CONNECT_CODES = new Set<Code>([
  "canceled",
  "unknown",
  "invalid_argument",
  "deadline_exceeded",
  "not_found",
  "already_exists",
  "permission_denied",
  "resource_exhausted",
  "failed_precondition",
  "aborted",
  "out_of_range",
  "unimplemented",
  "internal",
  "unavailable",
  "data_loss",
  "unauthenticated",
]);

function connectCode(value: unknown): Code | undefined {
  return typeof value === "string" && CONNECT_CODES.has(value as Code) ? (value as Code) : undefined;
}
