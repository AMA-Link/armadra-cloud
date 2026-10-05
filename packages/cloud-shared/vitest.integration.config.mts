import { defineConfig } from "vitest/config";

/** 集成用例：需要真的 PostgreSQL 与 Redis（pnpm dev:up 或 CI 服务容器）。 */
export default defineConfig({
  test: {
    include: ["test/integration/**/*.test.ts"],
    environment: "node",
    testTimeout: 30_000,
  },
});
