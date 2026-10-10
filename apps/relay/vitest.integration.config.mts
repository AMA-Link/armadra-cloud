import { defineConfig } from "vitest/config";

/**
 * 集成：要真的 PostgreSQL 与 Redis（pnpm dev:up 或 CI 服务容器）。`test/external/` 是多节点与
 * 存储迁移；按存储参数化的三个单测文件在设了 `ARMADRA_CLOUD_TEST_DATABASE_URL` 时多跑一遍 postgres。
 */
export default defineConfig({
  test: {
    include: [
      "test/external/**/*.test.ts",
      "src/personal/accounts.test.ts",
      "src/personal/sources.test.ts",
      "src/personal/serve.test.ts",
    ],
    environment: "node",
    testTimeout: 30_000,
    hookTimeout: 30_000,
  },
});
