import { defineConfig } from "vitest/config";

/** 集成用例：需要真的 PostgreSQL（pnpm dev:up 或 CI 服务容器，`ARMADRA_CLOUD_TEST_DATABASE_URL`）。 */
export default defineConfig({
  test: {
    include: ["test/integration/**/*.test.ts"],
    environment: "node",
    testTimeout: 30_000,
    hookTimeout: 30_000,
  },
});
