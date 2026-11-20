# Next.js App Router

Invariant's TypeScript runtime serves its HTTP projection from WHATWG Fetch
hosts through `Server.fetchHandler()`. This guide mounts MCP and Connect in a
Next.js App Router application on the Node.js runtime, authenticates callers
with OAuth bearer tokens, and passes the verified caller to handlers as Connect
context values. The Next.js behavior it describes was verified against Next.js
16.4.6.

Next.js is not an Invariant dependency, and Invariant has no Next-specific
export or option. The handler is a plain `(request: Request) =>
Promise<Response>` function; the application owns its route files, token
verification, authorization policy, and OAuth metadata.

## Adopt an existing Next.js app

Start with one operation and define its request and response in protobuf. An
existing OpenAPI document can bootstrap a proto for review. Generate TypeScript
bindings and a descriptor image from the reviewed contract. The app's business
logic goes in the typed service implementation. [OpenAPI import](openapi-import.md)

| Entry point | How it reaches the operation |
| --- | --- |
| Existing REST clients | The existing Next route maps its HTTP request and response to the generated types. |
| Remote CLI or stdio MCP | A small bridge loads the descriptor and calls the deployed app over HTTP. Canonical Connect works across all four Invariant runtimes. |
| Remote MCP | Agents connect to the app's `/mcp` endpoint. |
| Local CLI or stdio MCP | A Node process registers the same typed implementation and runs it without starting Next.js. |

The example keeps `GET /api/greet?name=Ada` as ordinary HTTP and adds a
canonical Connect mount for remote clients. The app owns the legacy path and
JSON shape, so the RPC in this example needs no `google.api.http` annotation.
If the deployed app only exposes REST, a Go, Python, or TypeScript bridge can
use explicit HTTP bindings to call those routes. See [agent access](#give-an-agent-access).

## What the handler serves

`server.fetchHandler(options)` freezes the server's configuration, so register
services, interceptors, filters, limits, and the metadata mapper first. It
returns a function that declares exactly one `Request` parameter and never
rejects. App Router's generated route types accept that shape for a `GET`,
`POST`, or `DELETE` export. Next.js also passes `{ params }` as a second
argument, which the handler ignores.

The Fetch host shares one HTTP core with the Node host from `httpHandler()`, so
both answer with the same limits, deadlines, cancellation, MCP framing, and
error envelopes. After its mount prefix, the handler serves:

| Path after the prefix | Response |
| --- | --- |
| `GET /healthz`, `GET /readyz` | `200 {"status":"ok"}`, without calling the hook |
| `GET /`, `GET /__invariant/tools` | The tool catalog |
| `GET /__invariant/descriptor.binpb` | The descriptor image |
| `/mcp` | MCP over `POST`; `405` with `Allow: POST` for other methods; `403` with any `Origin` header |
| `POST /{package.Service}/{Method}` | The Connect route |
| Anything else | `404` `not_found` |

The probes answer `GET` only. Next.js serves `HEAD` through the `GET` export,
and Invariant treats a `HEAD` probe like any other in-mount request, so it
reaches the hook; configure health checks with `GET`.

`requestPathPrefix` defaults to `""`. Otherwise it is a literal path such as
`/api/rpc`, in URL pathname form with no trailing slash or empty segment.
Prefixes with query strings, fragments, or dot segments are invalid.
An invalid value throws `TypeError` when the handler is built, which
fails `next build` because the build evaluates route modules. The handler
matches the raw pathname of `request.url` segment by segment, without
percent-decoding or slash normalization. A pathname equal to the prefix maps to
`/`; a pathname outside it, such as `/api/rpcx`, receives `404` without calling
the hook. `context.url` keeps the full request URL, prefix included.

Next.js removes a configured `basePath` from `request.url` before the route
handler runs, so the prefix never includes it. MCP is served only where the
path left after the prefix is `/mcp`: `app/mcp/route.ts` needs no prefix, and
an `app/api/mcp/route.ts` mount would use `requestPathPrefix: "/api"`.

## Install

Invariant packages are distributed only from Git, as the
[install section](../README.md#install) describes. Install v0.17.0 or later for `Server.fetchHandler`:

```bash
npm install --allow-git=root "github:jim-technologies/invariantprotocol#v0.17.0"
```

For reproducible production builds, replace the tag with a full commit revision
using npm's normal Git syntax. npm's `allow-git` setting is `all` by default;
`root` allows only the Git dependencies declared in the project's own
`package.json`. A project `.npmrc` keeps that policy for later `npm install`
and `npm ci` runs:

```ini
allow-git=root
```

The application also depends on `@connectrpc/connect` and `@bufbuild/protobuf`,
with `@bufbuild/buf` (the Buf CLI) and `@bufbuild/protoc-gen-es` as development
dependencies; the example below uses `jose` to verify tokens. Keep
`@connectrpc/connect` within Invariant's `^2.2.0` range. Hook errors and context
values still work when the application resolves a second copy, because
`ConnectError` recognizes errors from any copy and context values are keyed by
symbols, but aligned ranges avoid bundling two.

## Generate code from the descriptor image

Build the descriptor image, then generate Protobuf-ES bindings from that exact
image, as the [build contract](../README.md#build-contract) describes. Buf keeps
source comments by default, and they become MCP tool descriptions. Inside a
Next.js application, generate TypeScript with protoc-gen-es's default import
extension: Turbopack cannot resolve generated imports that name `.js` files
backed by `.ts` sources. Protobuf-ES ships bindings only for the well-known
types, so `include_imports` also generates the files your protos import, such
as `google/api/annotations.proto` or `buf/validate/validate.proto`.

```yaml
# buf.yaml
version: v2
modules:
  - path: proto
```

```yaml
# buf.gen.yaml
version: v2
plugins:
  - local: protoc-gen-es
    out: src/gen
    opt: target=ts
    include_imports: true
```

`next build` evaluates route modules in worker processes, and the server below
reads `descriptor.binpb` when its module loads, so generate before building or
starting the development server. `npm run` puts `node_modules/.bin` on `PATH`,
so the scripts find the locally installed `buf` and `protoc-gen-es`:

```json
{
  "scripts": {
    "generate": "buf build -o descriptor.binpb && buf generate descriptor.binpb",
    "dev": "npm run generate && next dev",
    "build": "npm run generate && next build",
    "start": "next start"
  }
}
```

The example service:

```proto
// proto/greet/v1/greet.proto
syntax = "proto3";

package greet.v1;

// Greets callers.
service GreetService {
  // Greet a person by name.
  rpc Greet(GreetRequest) returns (GreetResponse);
  // Stream three numbered greetings.
  rpc StreamGreet(GreetRequest) returns (stream GreetResponse);
}

// Who to greet.
message GreetRequest {
  // Person to greet.
  string name = 1;
}

// A greeting.
message GreetResponse {
  // Rendered greeting.
  string message = 1;
}
```

The paths below assume a `src/` directory and the `@/*` to `./src/*` import
alias in `tsconfig.json`.

## Build the server once

```ts
// src/lib/invariant.ts
import "server-only";
import { readFileSync } from "node:fs";
import path from "node:path";
import { authorize, bearer } from "./auth";
import { createGreetServer } from "./greet";

// The literal cwd-relative read is traced into standalone output, so the
// deployment reads the same image the bindings were generated from.
const descriptor = readFileSync(path.join(process.cwd(), "descriptor.binpb"));
export const server = createGreetServer(descriptor);
server.use(authorize);
export const authenticateApi = bearer("API_AUDIENCE");

export const mcp = server.fetchHandler({
  contextValues: bearer("MCP_RESOURCE", "MCP_RESOURCE_METADATA_URL"),
});

export const rpc = server.fetchHandler({
  requestPathPrefix: "/api/rpc",
  contextValues: authenticateApi,
});
```

```ts
// src/lib/greet.ts
import type { ServiceImpl } from "@connectrpc/connect";
import { Server } from "@jim-technologies/invariant-protocol";
import { GreetService } from "../gen/greet/v1/greet_pb";
import { principalKey } from "./auth";

const greet: ServiceImpl<typeof GreetService> = {
  async greet(request, context) {
    const subject = context.values.get(principalKey)?.subject;
    return { message: `Hi ${request.name}, from ${subject}` };
  },
  async *streamGreet(request, context) {
    for (let i = 1; i <= 3 && !context.signal.aborted; i++) {
      yield { message: `#${i}: hi ${request.name}` };
      await new Promise((resolve) => setTimeout(resolve, 300));
    }
  },
};

export function createGreetServer(descriptor: Uint8Array): Server {
  const server = Server.fromBytes(descriptor);
  server.register(GreetService, greet);
  return server;
}
```

Next.js evaluates the module once per server process, and every route that
imports it shares the instance. `next build` also evaluates it in worker
processes, so construction must stay free of side effects: it reads the
descriptor and registers services, but opens no ports and makes no network
calls, and the token verifier reads its configuration per request. No
`next.config` setting is required.

`import "server-only"` turns an accidental import from a Client Component into
a build error. Next.js resolves that import itself, so the `server-only`
package need not be installed. Keep it in the Next.js entry point so the
shared registration module can also run in a plain Node process.

Do not call `server.grpcServer()` in the Next.js process: when Turbopack
bundles Invariant, `@grpc/reflection` reads its `.proto` files through a
rewritten `__dirname` and fails. Serve native gRPC from a separate Node.js
process built from the same descriptor image.

## Route files

```ts
// src/app/mcp/route.ts: https://app.example.com/mcp
import { mcp } from "@/lib/invariant";

export { mcp as DELETE, mcp as GET, mcp as POST };
```

```ts
// src/app/api/rpc/[...path]/route.ts: POST /api/rpc/greet.v1.GreetService/Greet
import { rpc } from "@/lib/invariant";

export { rpc as GET, rpc as POST };
```

The MCP route exports `GET` and `DELETE` so that Invariant answers them: an
authenticated caller receives `405` with `Allow: POST`, which the official MCP
TypeScript SDK reads as "no standalone stream". Next.js answers `OPTIONS`
itself, without calling the hook, with `204` and an `Allow` header that lists
the route's exported methods plus `HEAD` and `OPTIONS`, and serves `HEAD`
through the `GET` export. Any other method a route does not export receives
Next.js's own `405`, which carries no `Allow` header.

The catch-all route receives the full original path in `request.url`; dotted
segments such as `greet.v1.GreetService` and the `__invariant` segment route to
it normally. A required catch-all does not match `/api/rpc` itself, so read the
catalog at `/api/rpc/__invariant/tools` and the probes at `/api/rpc/healthz`
and `/api/rpc/readyz`; an optional catch-all, `[[...path]]`, would also match
the bare prefix. Each mount serves every Invariant route below its prefix
behind its own hook, so `POST /api/rpc/mcp` also speaks MCP to callers holding
an API token.

No route segment config is needed. Next.js runs route handlers on the Node.js
runtime by default, and with `cacheComponents` enabled, exporting `runtime` or
`dynamic` fails the build. Invariant reads every request, so its `GET` routes
are never prerendered. The Edge runtime is not supported.

## Keep an existing REST route

The route file still owns its old HTTP contract. This example accepts a
`name` query parameter and returns the legacy `greeting` property. Generated
schemas give the adapter a typed request and response, and `Server.invoke`
runs the same registered RPC and interceptors as Connect and MCP. The route
passes the verified caller and the request's cancellation signal to dispatch.

```ts
// src/app/api/greet/route.ts
import { create, isMessage } from "@bufbuild/protobuf";
import { Code, ConnectError, createHandlerContext } from "@connectrpc/connect";
import { GreetRequestSchema, GreetResponseSchema, GreetService } from "@/gen/greet/v1/greet_pb";
import { authenticateApi, server } from "@/lib/invariant";

export async function GET(request: Request) {
  try {
    const contextValues = await authenticateApi(request);
    const name = new URL(request.url).searchParams.get("name");
    if (!name) {
      return Response.json({ error: "name is required" }, { status: 400 });
    }
    const input = create(GreetRequestSchema, { name });
    const context = createHandlerContext({
      service: GreetService,
      method: GreetService.method.greet,
      protocolName: "rest",
      requestMethod: request.method,
      url: request.url,
      requestSignal: request.signal,
      contextValues,
    });
    try {
      const output = await server.invoke("greet.v1.GreetService.Greet", input, context);
      if (!isMessage(output, GreetResponseSchema)) {
        throw new Error("unexpected greeting response");
      }
      return Response.json({ greeting: output.message });
    } finally {
      context.abort();
    }
  } catch (error) {
    const failure = ConnectError.from(error, Code.Internal);
    if (failure.code === Code.Unauthenticated) {
      const challenge = failure.metadata.get("www-authenticate");
      return Response.json({ error: failure.rawMessage }, {
        status: 401,
        headers: challenge ? { "www-authenticate": challenge } : undefined,
      });
    }
    if (failure.code === Code.PermissionDenied) {
      return Response.json({ error: failure.rawMessage }, { status: 403 });
    }
    return Response.json({ error: "internal server error" }, { status: 500 });
  }
}
```

```bash
curl -fsS -H "Authorization: Bearer $APP_API_TOKEN" \
  'https://app.example.com/api/greet?name=Ada'
```

For JSON request bodies that already match ProtoJSON, use Protobuf-ES
`fromJsonString` with the generated request schema and map malformed input to
the route's existing client error. If the body has a different shape, decode
it with the app's existing parser and map its fields explicitly. Keep the
route's own status codes, headers, body limits, and timeout policy at that
boundary. For responses that already match ProtoJSON, `server.toJson` can
serialize the RPC result using the descriptor registry.

## Authenticate in the `contextValues` hook

Request headers remain untrusted. Handlers and interceptors see only what the
HTTP metadata mapper forwards, by default the tracing and request-ID headers
and never `authorization` or an identity key, and nothing copies context
values back into headers or metadata. The hook is the application's enclosing
middleware. It verifies credentials from the raw `Request` (a `NextRequest`
under Next.js) and returns the trusted values:

```ts
// src/lib/auth.ts
import {
  Code,
  ConnectError,
  createContextKey,
  createContextValues,
  type Interceptor,
} from "@connectrpc/connect";
import { createRemoteJWKSet, type JWTPayload, jwtVerify } from "jose";

export type Principal = { subject: string; scopes: string[] };
export const principalKey = createContextKey<Principal | undefined>(undefined, {
  description: "verified caller",
});

function env(name: string): string {
  const value = process.env[name];
  if (!value) throw new Error(`${name} is not configured`);
  return value;
}

let jwks: ReturnType<typeof createRemoteJWKSet> | undefined;

function challenge(metadata?: string, error?: "invalid_token"): ConnectError {
  const params = [
    ...(metadata ? [`resource_metadata="${metadata}"`] : []),
    'scope="greet"',
    ...(error ? [`error="${error}"`] : []),
  ];
  const message = "a valid bearer token is required";
  return new ConnectError(message, Code.Unauthenticated, {
    "www-authenticate": `Bearer ${params.join(", ")}`,
  });
}

// One hook per mount. Each verifies its own audience, so a token issued for
// the API cannot call /mcp, and an MCP token cannot call the API.
export function bearer(
  audienceVar: "MCP_RESOURCE" | "API_AUDIENCE",
  metadataVar?: "MCP_RESOURCE_METADATA_URL",
) {
  return (request: Request) =>
    verifyBearer(request.headers.get("authorization") ?? "", audienceVar, metadataVar);
}

export async function verifyBearer(
  header: string,
  audienceVar: "MCP_RESOURCE" | "API_AUDIENCE",
  metadataVar?: "MCP_RESOURCE_METADATA_URL",
) {
  const metadata = metadataVar ? env(metadataVar) : undefined;
  const issuer = env("OAUTH_ISSUER");
  const audience = env(audienceVar);
  const token = /^Bearer (.+)$/i.exec(header)?.[1];
  if (!token) throw challenge(metadata);
  jwks ??= createRemoteJWKSet(new URL(env("OAUTH_JWKS_URL")));
  let payload: JWTPayload;
  try {
    ({ payload } = await jwtVerify(token, jwks, { issuer, audience }));
  } catch {
    throw challenge(metadata, "invalid_token");
  }
  const { sub, scope } = payload;
  if (typeof sub !== "string") throw challenge(metadata, "invalid_token");
  const scopes = typeof scope === "string" ? scope.split(" ") : [];
  // A new ContextValues every time: Invariant fails a reused one with 500.
  return createContextValues().set(principalKey, { subject: sub, scopes });
}

// Application policy, run once per call on every projection.
export const authorize: Interceptor = (next) => async (req) => {
  const principal = req.contextValues.get(principalKey);
  if (!principal) {
    throw new ConnectError("authentication required", Code.Unauthenticated);
  }
  if (!principal.scopes.includes("greet")) {
    throw new ConnectError("missing the greet scope", Code.PermissionDenied);
  }
  return next(req);
};
```

The hook contract is the same on the Fetch and Node hosts:

- It runs exactly once for every in-mount request except `GET /healthz` and
  `GET /readyz`: the catalog, the descriptor, Connect routes, unknown paths,
  and every `/mcp` request, whatever its final status. It never runs for a path
  outside the prefix, or for stdio MCP, the CLI, native gRPC, or
  `Server.invoke`.
- It runs before any body byte is read, before the `/mcp` `Origin`, method,
  `Accept`, `Content-Type`, and `MCP-Protocol-Version` checks, and before the
  `Connect-Timeout-Ms` deadline starts. An unauthenticated caller therefore
  receives `401` where it would otherwise receive `403`, `405`, `406`, or
  `415`, and the deadline covers body reading, decoding, and the handler, but
  not the hook. The hook must not consume the body. On the Fetch host, a hook
  that needs it, such as one verifying a request signature, can read
  `request.clone()`, but that read takes in the whole upload before Invariant's
  byte limit applies, so the hook must bound it.
- It must return a `ContextValues` created for this request. Handlers read it
  as `context.values` and shared interceptors as `req.contextValues`, on
  Connect unary and streaming routes and on MCP `tools/call`. Returning the
  same instance for a second request fails that request with `500`
  `internal`, because a shared instance would carry one caller's values, and
  Invariant's own deadline state, into another request. A result that is not a
  `ContextValues` also fails with `500` `internal`. Without a hook, every
  request receives a new, empty `ContextValues`.
- A thrown or rejected `ConnectError` or `InvariantError` becomes the response,
  rendered as a handler's error would be. Its code selects the HTTP status
  (`unauthenticated` is `401`, `permission_denied` is `403`,
  `resource_exhausted` is `429`, `unavailable` is `503`, and a code outside
  Invariant's `Code` becomes `unknown`, which is `500`), the body is the
  canonical `{"code","message","details"}` envelope, keeping only details in
  Connect's `{"type","value"}` form, and its metadata becomes response headers,
  except Connect's protocol headers (such as `content-type`, `content-length`,
  `date`, and `connect-*` or `grpc-*` names) and hop-by-hop headers such as
  `connection` and `transfer-encoding`. On `/mcp` too, hook failures are HTTP
  responses, never JSON-RPC errors.
- As in Connect's own handlers, an error that a Connect client parsed from an
  upstream response relays its code, message, and details but none of its
  metadata, which holds the upstream's response headers. A client error that
  Connect derives from a bare HTTP status, such as a gateway's `503`, still
  carries those headers, so a hook that calls another service should catch its
  failures and throw an error of its own.
- Any other thrown value becomes `500` `internal` carrying the error's message.
  Map verification failures to `Code.Unauthenticated`, and keep secrets and
  internal hostnames out of error messages.
- If the client disconnects while the hook is pending, the request is not
  dispatched.

MCP `initialize` and `tools/list` never pass through interceptors, so the
hook's `401` with `WWW-Authenticate` is what lets an MCP client discover the
authorization server on its first request. A scope that every call needs can
also be required in the hook: a `permission_denied` error carrying
`WWW-Authenticate: Bearer error="insufficient_scope", scope="..."` produces a
`403` with that challenge. The official MCP TypeScript SDK (1.32.2) answers it
by requesting the advertised scope only in its interactive authorization-code
flow; its client-credentials provider requests just the scope it was
configured with, so configure machine clients with every scope the hook and
interceptors require.

Per-tool policy belongs in an interceptor registered with `server.use`, which
runs once per call on every projection; over MCP its denials are `tools/call`
results with `isError: true` at HTTP status `200`. `tools/list` and the catalog
show the same tools to every caller that passes the hook.

## OAuth metadata and audiences

MCP clients locate the authorization server through RFC 9728 protected
resource metadata. They follow the challenge's `resource_metadata` first and
fall back to the well-known URL derived from the MCP path, so the application
serves the document there:

```ts
// src/app/.well-known/oauth-protected-resource/mcp/route.ts
import { connection } from "next/server";

export async function GET() {
  await connection(); // read deployment configuration per request
  return Response.json({
    resource: process.env.MCP_RESOURCE,
    authorization_servers: [process.env.OAUTH_ISSUER],
    scopes_supported: ["greet"],
    bearer_methods_supported: ["header"],
  });
}
```

With a `basePath`, this route is served below it, which is not the well-known
URL derived from the MCP URL. Clients then find the document only through the
challenge's `resource_metadata`, so point `MCP_RESOURCE_METADATA_URL` at the
`basePath`-prefixed route.

`await connection()` keeps the route dynamic. With `cacheComponents` enabled,
Next.js otherwise prerenders a `GET` handler that reads nothing from the
request, fixing the configuration seen at build time.

The example reads its configuration from these environment variables:

```bash
OAUTH_ISSUER=https://auth.example.com
OAUTH_JWKS_URL=https://auth.example.com/.well-known/jwks.json
MCP_RESOURCE=https://app.example.com/mcp
MCP_RESOURCE_METADATA_URL=https://app.example.com/.well-known/oauth-protected-resource/mcp
API_AUDIENCE=https://app.example.com/api/rpc
```

MCP clients name the `MCP_RESOURCE` URL as the RFC 8707 resource when they
request a token, and each mount accepts only tokens issued for its own
audience, so a token for one mount cannot be replayed on the other. Build
these URLs from configuration, never from the request: under `next start`,
`request.url` carries the server's own host and port rather than the public
origin, and its scheme follows a client-sent `X-Forwarded-Proto`.

Configure MCP clients with exactly `https://app.example.com/mcp`. Next.js
answers `/mcp/` with a `308` redirect to `/mcp`. Keep the default
`trailingSlash: false`: with `trailingSlash: true`, Next.js redirects `/mcp`,
every Connect method path, the catalog, and the probes to trailing-slash paths
that Invariant does not route. A client that follows the redirect then
receives `404` if it passes the hook and the hook's `401` otherwise; a
Connect-ES client on Node.js does not follow it and fails with `unknown`
(`HTTP 308`). The probes stop matching. Next.js exempts `.well-known` paths,
and paths whose last segment contains a dot, such as
`__invariant/descriptor.binpb`, from that redirect.
The official MCP TypeScript SDK also rejects metadata whose `resource` does not
match the origin and path it connects to.

## Platform notes

- **Proxy.** Keep Invariant's routes out of the `proxy.ts` matcher. For a
  matched request, Next.js buffers the body in memory and passes on only its
  first `experimental.proxyClientMaxBodySize` bytes (10 MB by default),
  logging a warning rather than failing the request, so a larger body would
  reach Invariant truncated. Next.js also recommends only optimistic checks in
  a proxy; credentials belong in the hook.
- **Compression.** `next start` gzips `application/json` and
  `application/connect+json` responses for clients that send
  `Accept-Encoding`, and Connect server-streaming frames still arrive as they
  are produced. Invariant itself negotiates no Connect compression; set
  `compress: false` to turn Next's off. A reverse proxy such as nginx must not
  buffer streaming responses; Next.js's self-hosting guide disables nginx
  buffering with `X-Accel-Buffering: no`.
- **Execution limits.** Deployment platforms can read a route's
  `export const maxDuration` (in seconds) from the build output and stop
  longer executions. Size it for the longest server stream or MCP tool call; it
  remains valid with `cacheComponents`.
- **Cancellation.** When the client disconnects, Next.js aborts
  `request.signal` with a `ResponseAborted` error. Invariant aborts the
  handler's `context.signal` with
  `ConnectError("client disconnected", Code.Canceled)` whatever the host's
  reason. A Connect server-stream body has no read-ahead buffer: the handler is
  asked for its next message only when the host reads one. An MCP call over
  HTTP is cancelled only by closing its connection or by its
  `Connect-Timeout-Ms` deadline: each `POST /mcp` is stateless, so a later
  `notifications/cancelled` cannot reach it.
- **MCP clients.** Invariant serves the stateless, JSON-only MCP `2025-11-25`
  tool-server subset. Clients list both `application/json` and
  `text/event-stream` in `Accept` and send `MCP-Protocol-Version: 2025-11-25`
  after initialization, as the official SDKs do. A `/mcp` request that carries
  an `Origin` header receives `403` once it passes the hook, so browser pages,
  including this application's own, cannot call `/mcp` directly. A client
  pinned to a later MCP revision cannot connect; the default negotiation of
  the official TypeScript SDK (1.32.2 and the 2.4.0 client) and Python SDK
  (2.3.0) does.

## Give an agent access

**Remote MCP.** Configure the agent's MCP client with the URL
`https://app.example.com/mcp` and a bearer token issued for `MCP_RESOURCE`
with every scope the hook and interceptors require. A client that implements
MCP authorization can instead start from the `401` challenge. Every unary and
server-streaming method is a tool; a stream's messages become the content
blocks of one result. With the official TypeScript SDK:

```ts
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";

const client = new Client({ name: "agent", version: "1.0.0" });
await client.connect(
  new StreamableHTTPClientTransport(new URL("https://app.example.com/mcp"), {
    requestInit: {
      headers: { authorization: `Bearer ${process.env.APP_MCP_TOKEN}` },
    },
  }),
);
const { tools } = await client.listTools();
```

**CLI or stdio MCP over HTTP.** Use a bridge process when the app is already
deployed. It builds a `Server` from the deployment's
descriptor image, registers the Connect routes as a remote backend with
`connectHttp`, and projects them as stdio MCP or the CLI, with no generated
code and no copy of the application:

```js
// bridge.mjs
import { readFileSync } from "node:fs";
import { runCli, Server, serveMcpStdio } from "@jim-technologies/invariant-protocol";

const descriptor = readFileSync(new URL("./descriptor.binpb", import.meta.url));
const server = Server.fromBytes(descriptor);
server.connectHttp("https://app.example.com/api/rpc", {
  auth: () => ({ authorization: `Bearer ${process.env.APP_API_TOKEN}` }),
});

if (process.argv[2] === "mcp") {
  await serveMcpStdio(server);
} else {
  try {
    console.log(await runCli(server, process.argv.slice(2)));
  } catch (error) {
    console.error(error instanceof Error ? error.message : String(error));
    process.exitCode = 1;
  }
}
```

```bash
curl -fsS -H "Authorization: Bearer $APP_API_TOKEN" \
  -o descriptor.binpb https://app.example.com/api/rpc/__invariant/descriptor.binpb
node bridge.mjs --help
node bridge.mjs greet.v1.GreetService Greet -r '{"name":"Ada"}'
node bridge.mjs mcp
```

The bridge's token is issued for `API_AUDIENCE`, and the deployment's hook and
interceptors still decide every call. Downloading the descriptor image from
the deployment keeps the bridge's methods in step with it. The image lists
every method, so if the deployment narrows its catalog with `include` or
`exclude`, run the bridge with the same patterns in `INVARIANT_INCLUDE` or
`INVARIANT_EXCLUDE`.

Register the bridge with an agent as a stdio MCP server that runs
`node /path/to/bridge.mjs mcp` with an absolute path, and set `APP_API_TOKEN`
in that server's own environment entry. Agents start stdio servers from their
own working directory, and the official MCP TypeScript SDK (1.32.2) passes a
stdio server only a few variables from the agent's environment, such as `HOME`
and `PATH`, so a token exported only in the agent's shell never reaches the
bridge. In the common `mcpServers` configuration:

```json
{
  "mcpServers": {
    "app": {
      "command": "node",
      "args": ["/path/to/bridge.mjs", "mcp"],
      "env": { "APP_API_TOKEN": "<token for API_AUDIENCE>" }
    }
  }
}
```

The bridge forwards unary methods only. Server-streaming methods remain
available over remote MCP and direct local registration.

Go, Python, and TypeScript HTTP bridges use the primary `google.api.http`
binding when a method has one. Point an annotated bridge at the origin
serving those REST paths, and check that its request and response mappings
match the existing JSON contract. For portable Connect access, use the
unannotated contract shown here and keep the legacy translations in Next route
files. Rust's remote HTTP bridge uses canonical Connect paths.

**CLI or stdio MCP in-process.** If the operator can run the app's code
and access its dependencies, register the same implementation directly.
This entry point uses the same scope check as the HTTP paths. It verifies the
process's API credential for each call and supplies the caller as a context
value before authorization. It starts no Next.js server and opens no HTTP port.

```ts
// src/local.mts
import { readFileSync } from "node:fs";
import { runCli, serveMcpStdio } from "@jim-technologies/invariant-protocol";
import { authorize, principalKey, verifyBearer } from "./lib/auth";
import { createGreetServer } from "./lib/greet";

const descriptor = readFileSync(new URL("../descriptor.binpb", import.meta.url));
const server = createGreetServer(descriptor);
server.use((next) => async (req) => {
  const token = process.env.APP_API_TOKEN;
  const values = await verifyBearer(token ? `Bearer ${token}` : "", "API_AUDIENCE");
  req.contextValues.set(principalKey, values.get(principalKey));
  return next(req);
});
server.use(authorize);

if (process.argv[2] === "mcp") {
  await serveMcpStdio(server);
} else {
  try {
    console.log(await runCli(server, process.argv.slice(2)));
  } catch (error) {
    console.error(error instanceof Error ? error.message : String(error));
    process.exitCode = 1;
  }
}
```

Run this from the app root with its TypeScript runner, for example
`tsx` installed as an app development dependency:

```bash
npm install --save-dev tsx
npm run generate
npm exec -- tsx src/local.mts greet.v1.GreetService Greet -r '{"name":"Ada"}'
npm exec -- tsx src/local.mts mcp
```

The local process needs the app's runtime configuration and credentials,
including the OAuth issuer and JWKS URL in this example. For an agent that
starts stdio MCP from a different directory, configure the app's absolute
`node_modules/.bin/tsx` path as the command, the absolute path to
`src/local.mts` and `mcp` as arguments, and supply the process's environment.
The script finds `descriptor.binpb` relative to its own file.
