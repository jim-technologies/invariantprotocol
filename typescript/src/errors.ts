export type Code =
  | "cancelled"
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

  constructor(code: Code, message: string, details: unknown[] = []) {
    super(message);
    this.name = "InvariantError";
    this.code = code;
    this.details = details;
  }

  toPayload(): { code: Code; message: string; details: unknown[] } {
    return { code: this.code, message: this.message, details: this.details };
  }
}

export function asInvariantError(err: unknown): InvariantError {
  if (err instanceof InvariantError) {
    return err;
  }
  if (err instanceof Error) {
    return new InvariantError("internal", err.message);
  }
  return new InvariantError("internal", String(err));
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
    case "cancelled":
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
      return "invalid_argument";
    case 401:
      return "unauthenticated";
    case 403:
      return "permission_denied";
    case 404:
      return "not_found";
    case 409:
      return "aborted";
    case 412:
      return "failed_precondition";
    case 413:
    case 429:
      return "resource_exhausted";
    case 499:
      return "cancelled";
    case 501:
      return "unimplemented";
    case 503:
      return "unavailable";
    case 504:
      return "deadline_exceeded";
    default:
      return status >= 500 ? "internal" : "unknown";
  }
}

export function grpcStatusFor(code: Code): number {
  switch (code) {
    case "cancelled":
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
      return "cancelled";
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
