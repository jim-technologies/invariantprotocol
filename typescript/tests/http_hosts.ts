import { createServer, type Server as HttpServer, type IncomingMessage, type ServerResponse } from "node:http";
import type { AddressInfo } from "node:net";
import { setFlagsFromString } from "node:v8";
import { runInNewContext } from "node:vm";

import type { FetchHandlerOptions, HttpHandlerOptions, Server } from "../src/index.js";

export type Host = {
  baseUrl: string;
  /** Every promise a handler returned, in arrival order. */
  handled: Promise<unknown>[];
  close(): Promise<void>;
};

/** A node:http server around Server.httpHandler. */
export async function startNodeHost(server: Server, options?: HttpHandlerOptions): Promise<Host> {
  const handler = server.httpHandler(options);
  const handled: Promise<unknown>[] = [];
  const http = createServer((req, res) => {
    handled.push(handler(req, res));
  });
  return listen(http, handled);
}

/** Next.js 16.4.6 aborts a route's request signal with this error when the response closes early. */
export class ResponseAborted extends Error {
  override name = "ResponseAborted";
}

/**
 * A node:http bridge around Server.fetchHandler that reproduces the Next.js
 * 16.4.6 Node runtime adapter: the Request streams the IncomingMessage with
 * duplex "half", its signal aborts with ResponseAborted when the response
 * closes before it finished, and the Response body is piped with backpressure
 * and cancelled through the same signal.
 */
export async function startFetchHost(server: Server, options?: FetchHandlerOptions): Promise<Host> {
  const handler = server.fetchHandler(options);
  const handled: Promise<unknown>[] = [];
  const http = createServer((req, res) => {
    const source = new AbortController();
    res.once("close", () => {
      if (!res.writableFinished) {
        source.abort(new ResponseAborted());
      }
    });
    // The Request is built inline so the bridge keeps no reference to it.
    const response = handler(nextRequest(req, source.signal));
    handled.push(response);
    void response.then((value) => pipeToNodeResponse(value, res, source.signal));
  });
  return listen(http, handled);
}

/** Runs a full garbage collection without changing how Vitest starts Node. */
export function forceGc(): void {
  setFlagsFromString("--expose-gc");
  (runInNewContext("gc") as () => void)();
}

function nextRequest(req: IncomingMessage, signal: AbortSignal): Request {
  const headers = new Headers();
  for (let i = 0; i < req.rawHeaders.length; i += 2) {
    headers.append(req.rawHeaders[i]!, req.rawHeaders[i + 1]!);
  }
  const hasBody = req.method !== "GET" && req.method !== "HEAD";
  return new Request(`http://${req.headers.host ?? "localhost"}${req.url ?? "/"}`, {
    method: req.method,
    headers,
    signal,
    duplex: "half",
    body: hasBody ? (req as unknown as BodyInit) : null,
  } as RequestInit);
}

async function pipeToNodeResponse(response: Response, res: ServerResponse, signal: AbortSignal): Promise<void> {
  if (res.destroyed) {
    return;
  }
  res.statusCode = response.status;
  response.headers.forEach((value, key) => {
    res.appendHeader(key, value);
  });
  if (response.body === null) {
    res.end();
    return;
  }
  const writer = new WritableStream<Uint8Array>({
    async write(chunk) {
      if (!res.write(chunk)) {
        await new Promise<void>((resolve) => {
          const resume = () => {
            res.off("drain", resume);
            res.off("close", resume);
            resolve();
          };
          res.on("drain", resume);
          res.on("close", resume);
        });
      }
    },
    close() {
      res.end();
    },
    abort() {
      res.destroy();
    },
  });
  await response.body.pipeTo(writer, { signal }).catch(() => res.destroy());
}

async function listen(http: HttpServer, handled: Promise<unknown>[]): Promise<Host> {
  await new Promise<void>((resolve) => http.listen(0, "127.0.0.1", resolve));
  const { port } = http.address() as AddressInfo;
  return {
    baseUrl: `http://127.0.0.1:${port}`,
    handled,
    close: () =>
      new Promise<void>((resolve) => {
        http.closeAllConnections();
        http.close(() => resolve());
      }),
  };
}
