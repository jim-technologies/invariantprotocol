import { type ContextValues, createContextValues } from "@connectrpc/connect";

import { clientDisconnected, createHttpCore } from "./http_core.js";
import type { Server } from "./server.js";

export type FetchHandlerOptions = {
  /**
   * The literal path the host mounts the handler at, such as "/api/rpc" for a
   * Next.js app/api/rpc/[...path] route. Defaults to "". A prefix must have no
   * trailing slash or empty segment and must already be in URL pathname form.
   * An invalid prefix throws a TypeError when the handler is built.
   */
  requestPathPrefix?: string;
  /**
   * Trusted values for one request, such as a verified principal. Called once
   * per request except GET /healthz and GET /readyz, before any body byte is
   * read and before the Connect-Timeout-Ms deadline starts. Return a new
   * ContextValues for every request and leave the body unread; throw a
   * ConnectError or InvariantError to reject the request with its status.
   */
  contextValues?: (request: Request) => ContextValues | Promise<ContextValues>;
};

// Undici links request.signal to the host's signal through a WeakRef, so an
// unreachable in-flight Request would silently lose a later disconnect.
const inFlight = new Set<Request>();

export function fetchHandler(
  server: Server,
  options: FetchHandlerOptions = {},
): (request: Request) => Promise<Response> {
  const prefix = options.requestPathPrefix ?? "";
  if (!/^(\/[^/]+)*$/.test(prefix) || (prefix !== "" && new URL(`http://localhost${prefix}`).pathname !== prefix)) {
    throw new TypeError(
      `requestPathPrefix must be "" or a URL pathname such as "/api/rpc" without a trailing slash, got ${JSON.stringify(prefix)}`,
    );
  }
  const core = createHttpCore(server, prefix);
  return async function invariantFetchHandler(request: Request): Promise<Response> {
    inFlight.add(request);
    const disconnect = new AbortController();
    let released = false;
    const release = () => {
      if (!released) {
        released = true;
        inFlight.delete(request);
        request.signal.removeEventListener("abort", abort);
      }
    };
    const abort = () => {
      disconnect.abort(clientDisconnected());
      release();
    };
    if (request.signal.aborted) {
      abort();
    } else {
      request.signal.addEventListener("abort", abort, { once: true });
    }
    const response = await core(
      {
        httpVersion: "",
        method: request.method,
        url: request.url,
        header: request.headers,
        body: requestBody(request),
        signal: disconnect.signal,
      },
      () => (options.contextValues ? options.contextValues(request) : createContextValues()),
    );
    if (response.body instanceof Uint8Array) {
      release();
      return new Response(response.body, { status: response.status, headers: response.header });
    }
    const iterator = response.body[Symbol.asyncIterator]();
    let cancelled = false;
    const body = new ReadableStream<Uint8Array>(
      {
        async pull(controller) {
          try {
            const chunk = await iterator.next();
            if (cancelled) {
              return;
            }
            if (chunk.done) {
              release();
              controller.close();
            } else {
              controller.enqueue(chunk.value);
            }
          } catch (e) {
            release();
            controller.error(e);
          }
        },
        async cancel() {
          cancelled = true;
          release();
          await iterator.return?.();
        },
      },
      // Pull only when the host reads, so the handler never runs ahead.
      { highWaterMark: 0 },
    );
    return new Response(body, { status: response.status, headers: response.header });
  };
}

/**
 * Reads the request body lazily, after the hook, which may have replaced
 * request.body by calling request.clone(). Stopping early discards some of the
 * rest rather than cancelling it: under Next.js a cancel destroys the Node
 * request before the response is sent, and a partly read body left in place
 * blocks its keep-alive connection until the host's keep-alive timeout.
 */
function requestBody(request: Request): AsyncIterable<Uint8Array> {
  return {
    [Symbol.asyncIterator]() {
      let reader: ReadableStreamDefaultReader<Uint8Array> | undefined;
      const stop = async (): Promise<IteratorResult<Uint8Array>> => {
        if (reader !== undefined) {
          void discard(reader);
        }
        return { done: true, value: undefined };
      };
      return {
        async next(): Promise<IteratorResult<Uint8Array>> {
          if (reader === undefined) {
            if (request.body === null) {
              return { done: true, value: undefined };
            }
            reader = request.body.getReader();
          }
          const chunk = await reader.read();
          return chunk.done ? { done: true, value: undefined } : { done: false, value: chunk.value };
        },
        return: stop,
        throw: stop,
      };
    },
  };
}

/**
 * The Fetch analogue of Node's req.resume(), bounded like connect-node's
 * read-ahead. A request.clone() is a tee branch that queues every chunk read
 * here, so draining a long upload would buffer all of it whenever nothing
 * reads the clone; a longer remainder stays unread until the host closes the
 * idle connection.
 */
async function discard(reader: ReadableStreamDefaultReader<Uint8Array>): Promise<void> {
  try {
    for (let left = 256 * 1024; left > 0; ) {
      const chunk = await reader.read();
      if (chunk.done) {
        break;
      }
      left -= chunk.value.byteLength;
    }
  } catch {
    // The client went away, or the host cancelled the body.
  } finally {
    reader.releaseLock();
  }
}
