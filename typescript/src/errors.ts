import { Code as ConnectCode, ConnectError } from "@connectrpc/connect";

export type Code =
  | "canceled"
  | "unknown"
  | "invalid_argument"
  | "deadline_exceeded"
  | "not_found"
  | "already_exists"
  | "permission_denied"
  | "resource_exhausted"
  | "failed_precondition"
  | "aborted"
  | "out_of_range"
  | "unimplemented"
  | "internal"
  | "unavailable"
  | "data_loss"
  | "unauthenticated";

export class InvariantError extends Error {
  readonly code: Code;
  readonly details: unknown[];
  readonly metadata: Headers;

  constructor(code: Code, message: string, details: unknown[] = [], metadata: HeadersInit = {}) {
    super(message);
    this.name = "InvariantError";
    this.code = code;
    this.details = details;
    this.metadata = new Headers(metadata);
  }

  toPayload(): { code: Code; message: string; details: unknown[] } {
    return { code: this.code, message: this.message, details: this.details };
  }
}

export function asInvariantError(err: unknown): InvariantError {
  if (err instanceof InvariantError) {
    return err;
  }
  if (err instanceof ConnectError) {
    return new InvariantError(
      codeFromConnectCode(err.code),
      err.rawMessage,
      connectDetailsForPayload(err),
      err.metadata,
    );
  }
  if (err instanceof Error) {
    return new InvariantError("internal", err.message);
  }
  return new InvariantError("internal", String(err));
}

/** Convert an Invariant status to Connect's canonical status representation. */
export function toConnectError(err: unknown): ConnectError {
  if (err instanceof ConnectError) {
    return err;
  }
  if (err instanceof InvariantError) {
    const connect = new ConnectError(err.message, connectCodeFor(err.code), err.metadata);
    connect.details = connectDetailsFromPayload(err.details);
    return connect;
  }
  return ConnectError.from(err, ConnectCode.Internal);
}

/**
 * Preserve intentional RPC statuses, but classify an unexpected handler
 * failure as Internal and include the canonical gRPC method for triage.
 */
export function normalizeHandlerError(err: unknown, fullMethod: string): InvariantError | ConnectError {
  if (err instanceof InvariantError || err instanceof ConnectError) {
    return err;
  }
  if (err instanceof Error && (err.name === "AbortError" || err.name === "TimeoutError")) {
    return ConnectError.from(err);
  }
  const message = err instanceof Error ? err.message : String(err);
  return new ConnectError(`${fullMethod}: ${message}`, ConnectCode.Internal, undefined, undefined, err);
}

export function connectCodeFor(code: Code): ConnectCode {
  return grpcStatusFor(code) as ConnectCode;
}

export function codeFromConnectCode(code: ConnectCode): Code {
  return codeFromGrpcStatus(code);
}

export function invalidArgument(message: string): InvariantError {
  return new InvariantError("invalid_argument", message);
}

export function notFound(message: string): InvariantError {
  return new InvariantError("not_found", message);
}

export function failedPrecondition(message: string): InvariantError {
  return new InvariantError("failed_precondition", message);
}

export function httpStatusFor(code: Code): number {
  switch (code) {
    case "canceled":
      return 499;
    case "invalid_argument":
    case "out_of_range":
      return 400;
    case "deadline_exceeded":
      return 504;
    case "not_found":
      return 404;
    case "already_exists":
    case "aborted":
      return 409;
    case "permission_denied":
      return 403;
    case "resource_exhausted":
      return 429;
    case "failed_precondition":
      return 400;
    case "unimplemented":
      return 501;
    case "unavailable":
      return 503;
    case "unauthenticated":
      return 401;
    case "unknown":
    case "internal":
    case "data_loss":
    default:
      return 500;
  }
}

export function codeFromHttpStatus(status: number): Code {
  switch (status) {
    case 400:
      return "internal";
    case 401:
      return "unauthenticated";
    case 403:
      return "permission_denied";
    case 404:
      return "unimplemented";
    case 429:
    case 502:
    case 503:
    case 504:
      return "unavailable";
    default:
      return "unknown";
  }
}

export function grpcStatusFor(code: Code): number {
  switch (code) {
    case "canceled":
      return 1;
    case "unknown":
      return 2;
    case "invalid_argument":
      return 3;
    case "deadline_exceeded":
      return 4;
    case "not_found":
      return 5;
    case "already_exists":
      return 6;
    case "permission_denied":
      return 7;
    case "resource_exhausted":
      return 8;
    case "failed_precondition":
      return 9;
    case "aborted":
      return 10;
    case "out_of_range":
      return 11;
    case "unimplemented":
      return 12;
    case "internal":
      return 13;
    case "unavailable":
      return 14;
    case "data_loss":
      return 15;
    case "unauthenticated":
      return 16;
  }
}

export function codeFromGrpcStatus(status: number | undefined): Code {
  switch (status) {
    case 1:
      return "canceled";
    case 2:
      return "unknown";
    case 3:
      return "invalid_argument";
    case 4:
      return "deadline_exceeded";
    case 5:
      return "not_found";
    case 6:
      return "already_exists";
    case 7:
      return "permission_denied";
    case 8:
      return "resource_exhausted";
    case 9:
      return "failed_precondition";
    case 10:
      return "aborted";
    case 11:
      return "out_of_range";
    case 12:
      return "unimplemented";
    case 13:
      return "internal";
    case 14:
      return "unavailable";
    case 15:
      return "data_loss";
    case 16:
      return "unauthenticated";
    default:
      return "unknown";
  }
}

function connectDetailsForPayload(error: ConnectError): unknown[] {
  return error.details.map((detail) => {
    if ("desc" in detail) {
      return { type: detail.desc.typeName };
    }
    return { type: detail.type, value: Buffer.from(detail.value).toString("base64") };
  });
}

function connectDetailsFromPayload(details: readonly unknown[]): ConnectError["details"] {
  const out: ConnectError["details"] = [];
  for (const detail of details) {
    if (typeof detail !== "object" || detail === null || Array.isArray(detail)) {
      continue;
    }
    if ("desc" in detail && "value" in detail) {
      out.push(detail as ConnectError["details"][number]);
      continue;
    }
    const payload = detail as { type?: unknown; value?: unknown; debug?: unknown };
    if (typeof payload.type !== "string" || typeof payload.value !== "string") {
      continue;
    }
    const incoming: { type: string; value: Uint8Array; debug?: unknown } = {
      type: payload.type,
      value: Buffer.from(payload.value, "base64"),
    };
    if (payload.debug !== undefined) {
      incoming.debug = payload.debug;
    }
    out.push(incoming as ConnectError["details"][number]);
  }
  return out;
}
