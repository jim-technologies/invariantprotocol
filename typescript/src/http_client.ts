import { fromJson, getOption, toJson, type DescField, type DescMessage, type JsonValue } from "@bufbuild/protobuf";

import { codeFromHttpStatus, InvariantError, invalidArgument } from "./errors.js";
import { type Server, type Tool, type UnaryHandler } from "./server.js";

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

export type HTTPHeaderProvider = (request: OutboundHTTPRequest) => Record<string, string> | undefined | Promise<Record<string, string> | undefined>;
export type HTTPQueryProvider = (request: OutboundHTTPRequest) => Record<string, string> | undefined | Promise<Record<string, string> | undefined>;
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
    this.auth =
      typeof options.auth === "function"
        ? { headerProvider: options.auth }
        : (options.auth ?? {});
    this.options = {
      maxReceiveMessageSize: options.channelOptions?.maxReceiveMessageSize ?? 16 * 1024 * 1024,
      connectTimeoutMs: options.channelOptions?.connectTimeoutMs ?? 10_000,
      readTimeoutMs: options.channelOptions?.readTimeoutMs ?? 10_000,
    };
    this.observer = options.observer;
    this.envHeaders = outboundHeadersFromEnv();
  }

  async send(methodPath: string, method: string, url: string, body: Uint8Array): Promise<Uint8Array> {
    const req: OutboundHTTPRequest = { methodPath, method, url, body };
    const signedUrl = await this.applyQueryProvider(req);
    const signedReq: OutboundHTTPRequest = { ...req, url: signedUrl };
    const headers = await this.headers(signedReq);
    const started = performance.now();
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), this.options.connectTimeoutMs + this.options.readTimeoutMs);
    let response: Response;
    let bytes: Uint8Array;
    try {
      response = await fetch(signedUrl, {
        method,
        headers,
        body: method === "GET" || method === "HEAD" || body.length === 0 ? undefined : Buffer.from(body),
        signal: controller.signal,
      });
      bytes = new Uint8Array(await response.arrayBuffer());
    } catch (e) {
      if ((e as Error).name === "AbortError") {
        throw new InvariantError("deadline_exceeded", `HTTP request exceeded ${this.options.connectTimeoutMs + this.options.readTimeoutMs}ms`);
      }
      throw new InvariantError("unavailable", (e as Error).message);
    } finally {
      clearTimeout(timeout);
    }

    const durationMs = performance.now() - started;
    const headersRecord = Object.fromEntries(response.headers.entries());
    const success = response.ok && bytes.length <= this.options.maxReceiveMessageSize;
    await this.observe({
      methodPath,
      statusCode: response.status,
      headers: headersRecord,
      body: bytes,
      durationMs,
      success,
      request: signedReq,
    });

    if (bytes.length > this.options.maxReceiveMessageSize) {
      throw new InvariantError("resource_exhausted", `response body exceeds ${this.options.maxReceiveMessageSize} byte limit`);
    }
    if (!response.ok) {
      throw httpError(response.status, bytes);
    }
    return bytes;
  }

  private async headers(request: OutboundHTTPRequest): Promise<Record<string, string>> {
    const headers: Record<string, string> = {
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
  readonly responseBody: string;
  body: string;
  private readonly template: PathTemplate;

  constructor(method: string, pattern: string, body: string, responseBody = "") {
    this.method = method.toUpperCase();
    this.pattern = pattern;
    this.body = body;
    this.responseBody = responseBody;
    this.template = PathTemplate.parse(pattern);
  }

  resolveFields(descriptor: DescMessage): void {
    for (const segment of this.template.segments) {
      if (segment.field) {
        segment.field = jsonFieldPath(descriptor, segment.field);
      }
    }
    if (this.body && this.body !== "*") {
      this.body = jsonFieldPath(descriptor, this.body);
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
    return new HTTPClientBinding(patternValue.kind || "POST", patternValue.path || `/${serviceName}/${methodName}`, httpRule.body || "", httpRule.responseBody || "");
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
  binding.resolveFields(tool.inputDesc);
  return async (request) => {
    const args = toJson(tool.inputDesc, request, { registry: server.parsed.registry }) as Record<string, unknown>;
    const built = binding.build(args, connection.baseUrl);
    const bytes = await connection.send(methodPath, binding.method, built.url, built.body);
    const payload = bytes.length === 0 ? {} : JSON.parse(Buffer.from(bytes).toString("utf8"));
    return fromJson(tool.outputDesc, responseBody(payload, binding.responseBody) as JsonValue, { registry: server.parsed.registry });
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
    const parts = pattern.replace(/^\/+|\/+$/g, "").split("/").filter(Boolean);
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
  if (!selector) {
    return payload;
  }
  if (typeof payload !== "object" || payload === null) {
    return {};
  }
  return getNested(payload as Record<string, unknown>, selector) ?? {};
}

function httpError(status: number, body: Uint8Array): InvariantError {
  const text = Buffer.from(body).toString("utf8");
  try {
    const payload = JSON.parse(text) as { code?: string; message?: string; details?: unknown[]; error?: { code?: string; message?: string; details?: unknown[] } };
    const err = payload.error ?? payload;
    if (err.code || err.message) {
      return new InvariantError(normalizeCode(err.code) ?? codeFromHttpStatus(status), err.message ?? `HTTP ${status}`, err.details ?? []);
    }
  } catch {
    // Fall through to the generic HTTP error.
  }
  return new InvariantError(codeFromHttpStatus(status), text || `HTTP ${status}`);
}

function normalizeCode(code: string | undefined) {
  if (!code) {
    return undefined;
  }
  return code.toLowerCase() as InvariantError["code"];
}
