import { Code, ConnectError, createHandlerContext } from "@connectrpc/connect";

export const MAX_NODE_TIMER_DELAY_MS = 2_147_483_647;

export function monotonicDeadlineAfter(timeoutMs: number): number {
  return performance.now() + timeoutMs;
}

export function remainingDeadlineMs(deadlineAt: number): number {
  return deadlineAt - performance.now();
}

export function createDeadlineHandlerContext(
  init: Parameters<typeof createHandlerContext>[0],
): ReturnType<typeof createHandlerContext> {
  if (init.timeoutMs === undefined || init.timeoutMs <= MAX_NODE_TIMER_DELAY_MS) {
    return createHandlerContext(init);
  }

  const deadlineAt = monotonicDeadlineAfter(init.timeoutMs);
  const deadline = new AbortController();
  const cleanupDeadline = scheduleAbsoluteDeadline(deadlineAt, () => {
    deadline.abort(new ConnectError("the operation timed out", Code.DeadlineExceeded));
  });
  const requestSignal =
    init.requestSignal === undefined ? deadline.signal : AbortSignal.any([init.requestSignal, deadline.signal]);
  const context = createHandlerContext({
    ...init,
    timeoutMs: undefined,
    requestSignal,
  });
  const abort = context.abort.bind(context);
  return Object.assign(context, {
    timeoutMs: () => remainingDeadlineMs(deadlineAt),
    abort(reason?: unknown) {
      cleanupDeadline();
      abort(reason);
    },
  });
}

export function scheduleAbsoluteDeadline(deadlineAt: number | undefined, expire: () => void): () => void {
  if (deadlineAt === undefined) {
    return () => undefined;
  }

  let active = true;
  let timer: ReturnType<typeof setTimeout> | undefined;
  const schedule = () => {
    if (!active) {
      return;
    }
    const remaining = remainingDeadlineMs(deadlineAt);
    if (!(remaining > 0)) {
      active = false;
      expire();
      return;
    }
    timer = setTimeout(schedule, Math.min(remaining, MAX_NODE_TIMER_DELAY_MS));
  };
  schedule();

  return () => {
    active = false;
    if (timer !== undefined) {
      clearTimeout(timer);
    }
  };
}
