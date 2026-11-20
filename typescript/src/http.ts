import { createServer, type Server as HttpServer, type IncomingMessage, type ServerResponse } from "node:http";

import { Code as ConnectCode, ConnectError, type ContextValues, createContextValues } from "@connectrpc/connect";
import { universalResponseToNodeResponse } from "@connectrpc/connect-node";

import { InvariantError } from "./errors.js";
import { clientDisconnected, createHttpCore, type HttpCore } from "./http_core.js";
import type { Server } from "./server.js";

export { CONNECT_STREAM_JSON, CONNECT_STREAM_PROTO, PROTO_CONTENT_TYPE } from "./http_core.js";

export type HttpHandlerOptions = {
  /**
   * Trusted values for one request, such as a verified principal. Called once
   * per request except GET /healthz and GET /readyz, before any body byte is
   * read and before the Connect-Timeout-Ms deadline starts. Return a new
   * ContextValues for every request and leave the body unread; throw a
   * ConnectError or InvariantError to reject the request with its status.
   */
  contextValues?: (request: IncomingMessage) => ContextValues | Promise<ContextValues>;
};

/** The returned promise settles when the response closes and never rejects. */
export function httpHandler(
  server: Server,
  options: HttpHandlerOptions = {},
): (req: IncomingMessage, res: ServerResponse) => Promise<void> {
  const core = createHttpCore(server);
  return (req, res) => {
    const disconnect = new AbortController();
    const closed = new Promise<void>((resolve) => {
      const onClose = () => {
        // An HTTP/2 stream reset also finishes the writable side, without end().
        if (res.writableEnded && res.writableFinished) {
          // Discard any unread request remainder so a keep-alive connection
          // can parse its next request.
          req.resume();
        } else {
          disconnect.abort(clientDisconnected());
        }
        resolve();
      };
      if (res.closed) {
        onClose();
      } else {
        res.once("close", onClose);
      }
    });
    void respond(core, req, res, disconnect.signal, options).catch(() => res.destroy());
    return closed;
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

async function respond(
  core: HttpCore,
  req: IncomingMessage,
  res: ServerResponse,
  signal: AbortSignal,
  options: HttpHandlerOptions,
): Promise<void> {
  // Only the path and query come from the client; an absolute-form or "//"
  // target cannot choose context.url's origin. HTTP/2 also admits an opaque
  // :path such as "foo:@attacker.example/" under a non-http :scheme; it has no
  // absolute path and would extend the origin below, so it closes like a
  // target that URL rejects.
  const target = new URL(req.url ?? "/", "http://localhost");
  if (!target.pathname.startsWith("/")) {
    throw new TypeError("request target has no absolute path");
  }
  const response = await core(
    {
      httpVersion: req.httpVersion,
      method: req.method ?? "GET",
      url: `http://localhost${target.pathname}${target.search}`,
      header: requestHeaders(req),
      body: { [Symbol.asyncIterator]: () => requestBody(req) },
      signal,
    },
    () => (options.contextValues ? options.contextValues(req) : createContextValues()),
  );
  if (signal.aborted) {
    return;
  }
  // node:http2 ends a HEAD stream with its headers and fails a later body
  // write, after which the response never closes.
  const head = req.method === "HEAD";
  // connect-node's writer never settles after a successful end, so nothing
  // waits on it; a streamed body releases itself.
  universalResponseToNodeResponse(
    {
      status: response.status,
      header: response.header,
      body: head ? undefined : response.body instanceof Uint8Array ? oneChunk(response.body) : response.body,
    },
    res,
  ).catch((error: unknown) => {
    const reason = ConnectError.from(error);
    if (signal.aborted || reason.code === ConnectCode.Aborted) {
      return;
    }
    if (!res.headersSent) {
      try {
        sendInternalError(res, reason.rawMessage, head);
        return;
      } catch {
        // Close the connection instead.
      }
    }
    res.destroy();
  });
}

/**
 * Reads the request body. Stopping early discards the rest rather than
 * destroying the request, because the response still has to reach the client:
 * a keep-alive connection can then parse its next request, and an HTTP/2
 * client can finish sending so that its stream closes.
 */
async function* requestBody(req: IncomingMessage): AsyncGenerator<Uint8Array> {
  try {
    yield* req.iterator({ destroyOnReturn: false });
  } finally {
    req.resume();
  }
}

async function* oneChunk(bytes: Uint8Array): AsyncIterable<Uint8Array> {
  if (bytes.byteLength > 0) {
    yield bytes;
  }
}

/**
 * Answers 500 when Node rejects a response header before anything is written,
 * such as a value with a control character, which Fetch Headers accept.
 */
function sendInternalError(res: ServerResponse, message: string, head: boolean): void {
  // node:http2 keeps the headers it accepted before the one it rejected.
  for (const name of res.getHeaderNames()) {
    res.removeHeader(name);
  }
  const body = Buffer.from(JSON.stringify(new InvariantError("internal", message).toPayload()));
  res.writeHead(500, { "content-type": "application/json", "content-length": body.byteLength });
  res.end(head ? undefined : body);
}

/**
 * Keeps every raw header line, as a Fetch Request does, so a repeated
 * singleton such as Content-Type is seen joined rather than first-wins. Like
 * connect-node, it skips the pseudo-headers of a node:http2 compat request.
 */
function requestHeaders(req: IncomingMessage): Headers {
  const headers = new Headers();
  for (let i = 0; i < req.rawHeaders.length; i += 2) {
    const name = req.rawHeaders[i];
    const value = req.rawHeaders[i + 1];
    if (name !== undefined && value !== undefined && !name.startsWith(":")) {
      headers.append(name, value);
    }
  }
  return headers;
}
