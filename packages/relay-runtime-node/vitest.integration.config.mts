import { defineConfig } from "vitest/config";

/** 多节点控制面：要真的 PostgreSQL 与 Redis（pnpm dev:up 或 CI 服务容器）。 */
export default defineConfig({
  test: {
    include: ["test/external/**/*.test.ts"],
    environment: "node",
    testTimeout: 30_000,
    hookTimeout: 30_000,
  },
});
