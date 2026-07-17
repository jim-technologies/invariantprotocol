import { defineConfig } from "vitest/config";

export default defineConfig({
  test: {
    include: ["tests/**/*.test.ts"],
    coverage: {
      provider: "v8",
      include: ["src/**/*.ts"],
      exclude: ["src/gen/**"],
      reporter: ["text"],
      thresholds: {
        statements: 85,
        branches: 70,
        functions: 90,
        lines: 85,
      },
    },
  },
});
