import {
  create,
  fromJson,
  type DescMessage,
  type JsonValue,
  type MessageShape,
  toJson,
} from "@bufbuild/protobuf";

import { ParsedDescriptor, type ServiceInfo } from "./descriptor.js";
import { failedPrecondition, InvariantError, notFound } from "./errors.js";
import {
  buildGrpcServer as buildGrpcProjection,
  grpcClientForService,
  grpcProxyHandler,
  serveGrpc as serveGrpcProjection,
  type GrpcConnectOptions,
} from "./grpc.js";
import { httpHandler as buildHttpHandler, serveHttp as serveHttpProjection } from "./http.js";
import {
  clientBindingForMethod,
  HTTPConnection,
  httpProxyHandler,
  httpRulesByMethodPath,
  type ConnectHttpOptions,
} from "./http_client.js";
import { SchemaGenerator, type JsonSchema } from "./schema.js";

export type HandlerContext = unknown;
export type UnaryHandler = (request: MessageShape<DescMessage>, context: HandlerContext) => Promise<unknown>;
export type StreamHandler = (
  request: MessageShape<DescMessage>,
  context: HandlerContext,
) => AsyncIterable<unknown>;
export type UnaryNext = (request: MessageShape<DescMessage>, context: HandlerContext) => Promise<unknown>;
export type StreamNext = (
  request: MessageShape<DescMessage>,
  context: HandlerContext,
) => AsyncIterable<unknown>;

export type ServerCallInfo = {
  fullMethod: string;
};

export type UnaryInterceptor = (
  request: MessageShape<DescMessage>,
  context: HandlerContext,
  info: ServerCallInfo,
  next: UnaryNext,
) => Promise<unknown>;

export type StreamInterceptor = (
  request: MessageShape<DescMessage>,
  context: HandlerContext,
  info: ServerCallInfo,
  next: StreamNext,
) => AsyncIterable<unknown>;

export type Tool = {
  name: string;
  description: string;
  inputSchema: JsonSchema;
  handler: UnaryHandler | StreamHandler;
  inputType: string;
  outputType: string;
  serviceFullName: string;
  methodName: string;
  serverStreaming: boolean;
  inputDesc: DescMessage;
  outputDesc: DescMessage;
};

export type ToolCatalogEntry = {
  name: string;
  description: string;
  inputSchema: JsonSchema;
  _meta?: Record<string, unknown>;
};

export const SERVER_NAME = "invariant-protocol";
export const SERVER_VERSION = "0.6.0";

export class Server {
  readonly name = SERVER_NAME;
  readonly version = SERVER_VERSION;
  readonly parsed: ParsedDescriptor;
  readonly schemaGen: SchemaGenerator;
  readonly tools = new Map<string, Tool>();

  private readonly interceptors: UnaryInterceptor[] = [];
  private readonly streamInterceptors: StreamInterceptor[] = [];
  private readonly grpcClients: Array<{ close: () => void }> = [];
  private includes: string[] = [];
  private excludes: string[] = [];
  private httpMaxUnaryRequest = 16 * 1024 * 1024;
  private connectStreamMaxRequest = 16 * 1024 * 1024;

  private constructor(parsed: ParsedDescriptor) {
    this.parsed = parsed;
    this.schemaGen = new SchemaGenerator(parsed);
  }

  static fromDescriptor(path: string): Server {
    return new Server(ParsedDescriptor.fromFile(path));
  }

  static fromBytes(bytes: Uint8Array): Server {
    return new Server(ParsedDescriptor.fromBytes(bytes));
  }

  include(...patterns: string[]): void {
    this.includes.push(...patterns);
  }

  exclude(...patterns: string[]): void {
    this.excludes.push(...patterns);
  }

  use(interceptor: UnaryInterceptor): void {
    if (!isAsyncFunction(interceptor)) {
      throw new TypeError("Unary interceptors must be async functions.");
    }
    this.interceptors.push(interceptor);
  }

  useStream(interceptor: StreamInterceptor): void {
    if (!isAsyncGeneratorFunction(interceptor)) {
      throw new TypeError("Stream interceptors must be async generator functions.");
    }
    this.streamInterceptors.push(interceptor);
  }

  setMaxUnaryRequestBytes(n: number): void {
    this.httpMaxUnaryRequest = n > 0 ? n : 16 * 1024 * 1024;
  }

  setMaxStreamRequestBytes(n: number): void {
    this.connectStreamMaxRequest = n > 0 ? n : 16 * 1024 * 1024;
  }

  maxUnaryRequestBytes(): number {
    return this.httpMaxUnaryRequest;
  }

  maxStreamRequestBytes(): number {
    return this.connectStreamMaxRequest;
  }

  register(servicer: object, serviceName?: string): void {
    const services = serviceName ? this.serviceByName(serviceName) : this.matchServicer(servicer);

    for (const [svcFullName, svc] of services) {
      for (const [methodName, method] of svc.methods) {
        if (method.clientStreaming || !this.shouldInclude(svcFullName, methodName)) {
          continue;
        }

        const handler = (servicer as Record<string, unknown>)[methodName];
        if (typeof handler !== "function") {
          continue;
        }

        if (method.serverStreaming) {
          if (!isAsyncGeneratorFunction(handler)) {
            throw new TypeError(
              `${servicer.constructor.name}.${methodName} is server-streaming and must be an async generator.`,
            );
          }
        } else if (!isAsyncFunction(handler)) {
          throw new TypeError(`${servicer.constructor.name}.${methodName} must be an async function.`);
        }

        const inputDesc = this.parsed.getMessage(method.inputType);
        const outputDesc = this.parsed.getMessage(method.outputType);
        if (!inputDesc || !outputDesc) {
          throw new InvariantError("internal", `missing message descriptor for ${svc.name}.${methodName}`);
        }

        this.addTool({
          name: `${svc.name}.${methodName}`,
          description: method.comment || `${svc.name}.${methodName}`,
          inputSchema: this.schemaGen.messageToSchema(method.inputType),
          handler: handler.bind(servicer) as UnaryHandler | StreamHandler,
          inputType: method.inputType,
          outputType: method.outputType,
          serviceFullName: svcFullName,
          methodName,
          serverStreaming: method.serverStreaming,
          inputDesc,
          outputDesc,
        });
      }
    }
  }

  connect(address: string, options: GrpcConnectOptions = {}): void {
    const services = options.serviceName ? this.serviceByName(options.serviceName) : this.parsed.services;

    for (const [svcFullName, svc] of services) {
      const proxyTools: Tool[] = [];
      for (const [methodName, method] of svc.methods) {
        if (method.clientStreaming || method.serverStreaming || !this.shouldInclude(svcFullName, methodName)) {
          continue;
        }
        const inputDesc = this.parsed.getMessage(method.inputType);
        const outputDesc = this.parsed.getMessage(method.outputType);
        if (!inputDesc || !outputDesc) {
          throw new InvariantError("internal", `missing message descriptor for ${svc.name}.${methodName}`);
        }
        proxyTools.push({
          name: `${svc.name}.${methodName}`,
          description: method.comment || `${svc.name}.${methodName}`,
          inputSchema: this.schemaGen.messageToSchema(method.inputType),
          handler: async () => {
            throw new InvariantError("internal", "gRPC proxy handler not initialized");
          },
          inputType: method.inputType,
          outputType: method.outputType,
          serviceFullName: svcFullName,
          methodName,
          serverStreaming: false,
          inputDesc,
          outputDesc,
        });
      }

      if (proxyTools.length === 0) {
        continue;
      }

      const client = grpcClientForService(address, svcFullName, proxyTools, options);
      this.grpcClients.push(client);
      for (const tool of proxyTools) {
        tool.handler = grpcProxyHandler(client, tool.methodName, tool);
        this.addTool(tool);
      }
    }
  }

  connectHttp(baseUrl: string, options: ConnectHttpOptions = {}): void {
    const services = options.serviceName ? this.serviceByName(options.serviceName) : this.parsed.services;
    const rules = httpRulesByMethodPath(this);
    const connection = new HTTPConnection(baseUrl, options);

    for (const [svcFullName, svc] of services) {
      for (const [methodName, method] of svc.methods) {
        if (method.clientStreaming || method.serverStreaming || !this.shouldInclude(svcFullName, methodName)) {
          continue;
        }
        const inputDesc = this.parsed.getMessage(method.inputType);
        const outputDesc = this.parsed.getMessage(method.outputType);
        if (!inputDesc || !outputDesc) {
          throw new InvariantError("internal", `missing message descriptor for ${svc.name}.${methodName}`);
        }

        const methodPath = `/${svcFullName}/${methodName}`;
        const binding = clientBindingForMethod(rules.get(methodPath), svcFullName, methodName);
        const tool: Tool = {
          name: `${svc.name}.${methodName}`,
          description: method.comment || `${svc.name}.${methodName}`,
          inputSchema: this.schemaGen.messageToSchema(method.inputType),
          handler: async () => {
            throw new InvariantError("internal", "HTTP proxy handler not initialized");
          },
          inputType: method.inputType,
          outputType: method.outputType,
          serviceFullName: svcFullName,
          methodName,
          serverStreaming: false,
          inputDesc,
          outputDesc,
        };
        tool.handler = httpProxyHandler(this, connection, binding, tool, methodPath);
        this.addTool(tool);
      }
    }
  }

  async invoke(toolName: string, request: unknown, context: HandlerContext = undefined): Promise<MessageShape<DescMessage>> {
    const tool = this.tools.get(toolName);
    if (!tool) {
      throw notFound(`Unknown tool '${toolName}'. Available: ${JSON.stringify([...this.tools.keys()].sort())}`);
    }
    if (tool.serverStreaming) {
      throw failedPrecondition(`Tool '${toolName}' is server-streaming; use invokeStream`);
    }
    return this.invokeTool(tool, this.coerceMessage(tool.inputDesc, request), context);
  }

  async *invokeStream(
    toolName: string,
    request: unknown,
    context: HandlerContext = undefined,
  ): AsyncIterable<MessageShape<DescMessage>> {
    const tool = this.tools.get(toolName);
    if (!tool) {
      throw notFound(`Unknown tool '${toolName}'. Available: ${JSON.stringify([...this.tools.keys()].sort())}`);
    }
    if (!tool.serverStreaming) {
      throw failedPrecondition(`Tool '${toolName}' is unary; use invoke`);
    }
    yield* this.invokeStreamTool(tool, this.coerceMessage(tool.inputDesc, request), context);
  }

  toolCatalog(): ToolCatalogEntry[] {
    return [...this.tools.values()]
      .sort((a, b) => a.name.localeCompare(b.name))
      .map((tool) => {
        const entry: ToolCatalogEntry = {
          name: tool.name,
          description: tool.description,
          inputSchema: tool.inputSchema,
        };
        if (tool.serverStreaming) {
          entry._meta = { streaming: true };
        }
        return entry;
      });
  }

  httpHandler(): ReturnType<typeof buildHttpHandler> {
    return buildHttpHandler(this);
  }

  serveHttp(port: number, host?: string) {
    return serveHttpProjection(this, port, host);
  }

  buildGrpcServer() {
    return buildGrpcProjection(this);
  }

  serveGrpc(port: number, host?: string) {
    return serveGrpcProjection(this, port, host);
  }

  async stop(): Promise<void> {
    for (const client of this.grpcClients.splice(0)) {
      client.close();
    }
  }

  toJson(tool: Tool, message: MessageShape<DescMessage>): JsonValue {
    return toJson(tool.outputDesc, message, { useProtoFieldName: true, registry: this.parsed.registry });
  }

  coerceMessage(desc: DescMessage, value: unknown): MessageShape<DescMessage> {
    if (isMessageFor(desc, value)) {
      return value as MessageShape<DescMessage>;
    }
    try {
      return fromJson(desc, (value ?? {}) as JsonValue, { registry: this.parsed.registry });
    } catch {
      return create(desc, (value ?? {}) as never);
    }
  }

  async invokeTool(
    tool: Tool,
    request: MessageShape<DescMessage>,
    context: HandlerContext,
  ): Promise<MessageShape<DescMessage>> {
    const info = { fullMethod: `/${tool.serviceFullName}/${tool.methodName}` };
    const handler = tool.handler as UnaryHandler;
    const response = await this.chainedInvoke(request, context, info, handler);
    return this.coerceMessage(tool.outputDesc, response);
  }

  async *invokeStreamTool(
    tool: Tool,
    request: MessageShape<DescMessage>,
    context: HandlerContext,
  ): AsyncIterable<MessageShape<DescMessage>> {
    const info = { fullMethod: `/${tool.serviceFullName}/${tool.methodName}` };
    const stream = this.chainedStream(request, context, info, tool.handler as StreamHandler);
    for await (const response of stream) {
      yield this.coerceMessage(tool.outputDesc, response);
    }
  }

  private addTool(tool: Tool): void {
    const existing = this.tools.get(tool.name);
    if (existing && existing.serviceFullName !== tool.serviceFullName) {
      throw new Error(
        `Tool name collision: ${tool.name} is registered by both ${existing.serviceFullName} and ${tool.serviceFullName}. Use include() to scope to one.`,
      );
    }
    this.tools.set(tool.name, tool);
  }

  private async chainedInvoke(
    request: MessageShape<DescMessage>,
    context: HandlerContext,
    info: ServerCallInfo,
    handler: UnaryHandler,
  ): Promise<unknown> {
    let current: UnaryNext = handler;
    for (const interceptor of [...this.interceptors].reverse()) {
      const next = current;
      current = (req, ctx) => interceptor(req, ctx, info, next);
    }
    return current(request, context);
  }

  private chainedStream(
    request: MessageShape<DescMessage>,
    context: HandlerContext,
    info: ServerCallInfo,
    handler: StreamHandler,
  ): AsyncIterable<unknown> {
    let current: StreamNext = handler;
    for (const interceptor of [...this.streamInterceptors].reverse()) {
      const next = current;
      current = (req, ctx) => interceptor(req, ctx, info, next);
    }
    return current(request, context);
  }

  private serviceByName(serviceName: string): Map<string, ServiceInfo> {
    const service = this.parsed.services.get(serviceName);
    if (!service) {
      throw new Error(`Service '${serviceName}' not found in descriptor. Available: ${JSON.stringify([...this.parsed.services.keys()])}`);
    }
    return new Map([[serviceName, service]]);
  }

  private matchServicer(servicer: object): Map<string, ServiceInfo> {
    const names = new Set(
      Object.getOwnPropertyNames(Object.getPrototypeOf(servicer)).filter((name) => {
        return name !== "constructor" && typeof (servicer as Record<string, unknown>)[name] === "function";
      }),
    );
    const matched = new Map<string, ServiceInfo>();
    for (const [svcFullName, svc] of this.parsed.services) {
      const rpcNames = [...svc.methods].filter(([, info]) => !info.clientStreaming).map(([name]) => name);
      if (rpcNames.some((name) => names.has(name))) {
        matched.set(svcFullName, svc);
      }
    }
    if (matched.size === 0) {
      throw new Error(`No matching service found for servicer. Available: ${JSON.stringify([...this.parsed.services.keys()])}`);
    }
    return matched;
  }

  private shouldInclude(serviceFullName: string, methodName: string): boolean {
    const fullPath = `${serviceFullName}.${methodName}`;
    const includes = [...this.includes, ...splitPatterns(process.env.INVARIANT_INCLUDE ?? "")];
    const excludes = [...this.excludes, ...splitPatterns(process.env.INVARIANT_EXCLUDE ?? "")];

    if (includes.length > 0 && !includes.some((pattern) => globMatch(pattern, fullPath))) {
      return false;
    }
    return !excludes.some((pattern) => globMatch(pattern, fullPath));
  }
}

function splitPatterns(value: string): string[] {
  return value
    .split(",")
    .map((part) => part.trim())
    .filter(Boolean);
}

function globMatch(pattern: string, value: string): boolean {
  const escaped = pattern.replace(/[.+^${}()|[\]\\]/g, "\\$&").replace(/\*/g, ".*");
  return new RegExp(`^${escaped}$`).test(value);
}

function isAsyncFunction(fn: unknown): boolean {
  return typeof fn === "function" && fn.constructor.name === "AsyncFunction";
}

function isAsyncGeneratorFunction(fn: unknown): boolean {
  return typeof fn === "function" && fn.constructor.name === "AsyncGeneratorFunction";
}

function isMessageFor(desc: DescMessage, value: unknown): boolean {
  return typeof value === "object" && value !== null && "$typeName" in value && (value as { $typeName?: string }).$typeName === desc.typeName;
}
