import { strict as assert } from "node:assert";
import { spawn, type ChildProcess } from "node:child_process";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { createFileRegistry, fromBinary } from "@bufbuild/protobuf";
import { FileDescriptorSetSchema } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-node";

const root = fileURLToPath(new URL("../..", import.meta.url));
const descriptor = fromBinary(
  FileDescriptorSetSchema,
  readFileSync(new URL("../../python/tests/proto/descriptor.binpb", import.meta.url)),
);
const resolvedService = createFileRegistry(descriptor).getService("greet.v1.GreetService");
assert.ok(resolvedService, "shared descriptor is missing greet.v1.GreetService");
const greetService = resolvedService as typeof import("./gen/greet_pb.js").GreetService;

const goServer = process.env.INVARIANT_CONNECT_INTEROP_GO;
const rustServer = process.env.INVARIANT_CONNECT_INTEROP_RUST;
if (!goServer || !rustServer) {
  throw new Error("run this interoperability check through `make connect-interop`");
}

const startupTimeoutMs = 30_000;
const shutdownTimeoutMs = 5_000;
const rpcTimeoutMs = 5_000;

type Runtime = {
  name: string;
  command: string;
  args: string[];
  cwd: string;
};

const runtimes: Runtime[] = [
  {
    name: "Go",
    command: goServer,
    args: [],
    cwd: root,
  },
  {
    name: "Python",
    command: "uv",
    args: ["run", "--locked", "--no-sync", "python", "tests/connect_interop_server.py"],
    cwd: fileURLToPath(new URL("../../python", import.meta.url)),
  },
  {
    name: "Rust",
    command: rustServer,
    args: [],
    cwd: fileURLToPath(new URL("../../rust", import.meta.url)),
  },
];

type RunningServer = {
  child: ChildProcess;
  baseUrl: string;
  closed: Promise<unknown>;
};

function terminate(child: ChildProcess, signal: NodeJS.Signals): void {
  if (child.pid === undefined) {
    return;
  }
  try {
    process.kill(-child.pid, signal);
  } catch (error) {
    if (!(error instanceof Error && "code" in error && error.code === "ESRCH")) {
      throw error;
    }
  }
}

async function start(runtime: Runtime): Promise<RunningServer> {
  const child = spawn(runtime.command, runtime.args, {
    cwd: runtime.cwd,
    detached: true,
    env: process.env,
    stdio: ["ignore", "pipe", "pipe"],
  });
  const closed = new Promise<void>((resolve) => {
    child.once("close", () => resolve());
  });
  child.stdout?.setEncoding("utf8");
  child.stderr?.setEncoding("utf8");

  let stdout = "";
  let stderr = "";
  child.stdout?.on("data", (chunk: string) => {
    stdout += chunk;
  });
  child.stderr?.on("data", (chunk: string) => {
    stderr += chunk;
  });

  const ready = new Promise<string>((resolve, reject) => {
    const timer = setInterval(() => {
      const match = stdout.match(/^http:\/\/127\.0\.0\.1:\d+$/m);
      if (match) {
        clearInterval(timer);
        resolve(match[0]);
      }
    }, 10);
    timer.unref();

    child.once("error", (error) => {
      clearInterval(timer);
      reject(error);
    });
    child.once("exit", (code, signal) => {
      clearInterval(timer);
      reject(new Error(`${runtime.name} server exited before readiness (code=${code}, signal=${signal})\n${stderr}`));
    });
  });

  try {
    const baseUrl = await Promise.race([
      ready,
      new Promise<never>((_, reject) => {
        const timer = setTimeout(() => {
          reject(new Error(`${runtime.name} server did not start within ${startupTimeoutMs}ms\n${stderr}`));
        }, startupTimeoutMs);
        timer.unref();
      }),
    ]);
    return { child, baseUrl, closed };
  } catch (error) {
    terminate(child, "SIGKILL");
    throw error;
  }
}

async function stop(server: RunningServer): Promise<void> {
  terminate(server.child, "SIGTERM");
  const timedOut = new Promise<"timeout">((resolve) => {
    const timer = setTimeout(() => resolve("timeout"), shutdownTimeoutMs);
    timer.unref();
  });
  if ((await Promise.race([server.closed, timedOut])) === "timeout") {
    terminate(server.child, "SIGKILL");
    await server.closed;
  }
}

async function verify(runtime: Runtime, baseUrl: string, useBinaryFormat: boolean): Promise<void> {
  const client = createClient(
    greetService,
    createConnectTransport({
      baseUrl,
      httpVersion: "1.1",
      useBinaryFormat,
      defaultTimeoutMs: rpcTimeoutMs,
      acceptCompression: [],
    }),
  );

  const unary = await client.greet({ name: "Unary" });
  assert.equal(unary.message, "Hi Unary");

  const streamed: string[] = [];
  for await (const response of client.streamGreet({ name: "Stream", count: 2 })) {
    streamed.push(response.message);
  }
  assert.deepEqual(streamed, ["Hi Stream #0", "Hi Stream #1"]);

  await assert.rejects(
    client.greet({ name: "error" }),
    (error: unknown) =>
      error instanceof ConnectError && error.code === Code.FailedPrecondition && error.rawMessage === "interop status",
  );

  console.log(`Connect-ES ${useBinaryFormat ? "binary" : "JSON"} passed against ${runtime.name}`);
}

for (const runtime of runtimes) {
  const server = await start(runtime);
  try {
    await verify(runtime, server.baseUrl, false);
    await verify(runtime, server.baseUrl, true);
  } finally {
    await stop(server);
  }
}
