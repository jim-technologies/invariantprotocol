import { create } from "@bufbuild/protobuf";
import { pathToString } from "@bufbuild/protobuf/reflect";
import { createValidator } from "@bufbuild/protovalidate";
import { Code, ConnectError, type Interceptor, type StreamRequest } from "@connectrpc/connect";

import { BadRequestSchema } from "./gen/google/rpc/error_details_pb.js";

/**
 * Validate every unary or streaming request message with Protovalidate.
 * A single Connect-ES interceptor is sufficient for both cardinalities.
 */
export function validation(): Interceptor {
  const validator = createValidator();
  return (next) => async (request) => {
    if (!request.stream) {
      validateMessage(
        validator,
        request.method.input,
        request.message,
        request.method.parent.typeName,
        request.method.name,
      );
      return next(request);
    }
    return next({
      ...request,
      message: validatedMessages(validator, request),
    });
  };
}

function validateMessage(
  validator: ReturnType<typeof createValidator>,
  schema: Parameters<ReturnType<typeof createValidator>["validate"]>[0],
  message: Parameters<ReturnType<typeof createValidator>["validate"]>[1],
  service: string,
  method: string,
): void {
  const result = validator.validate(schema, message);
  if (result.kind === "valid") {
    return;
  }
  if (result.kind === "error") {
    throw new ConnectError(
      `/${service}/${method}: protovalidate could not evaluate the request: ${result.error.message}`,
      Code.Internal,
      undefined,
      undefined,
      result.error,
    );
  }

  const fieldViolations = result.violations.map((violation) => ({
    field: pathToString(violation.field),
    description: violation.message,
  }));
  const detail = create(BadRequestSchema, { fieldViolations });
  const messageText = fieldViolations.map((violation) => `${violation.field}: ${violation.description}`).join("; ");
  throw new ConnectError(messageText || result.error.message, Code.InvalidArgument, undefined, [
    { desc: BadRequestSchema, value: detail },
  ]);
}

async function* validatedMessages(validator: ReturnType<typeof createValidator>, request: StreamRequest) {
  for await (const message of request.message) {
    validateMessage(validator, request.method.input, message, request.method.parent.typeName, request.method.name);
    yield message;
  }
}
