import { cloudflareTest } from "@cloudflare/vitest-plugin";
import { defineConfig } from "vitest/config";

// 用仓库里的 wrangler.jsonc 起本地 workerd（不连 Cloudflare 账号）；secret 给测试值。
export default defineConfig({
  plugins: [
    cloudflareTest({
      wrangler: { configPath: "./wrangler.jsonc" },
      miniflare: {
        bindings: {
          RELAY_MASTER_KEY: "ab".repeat(32),
          RELAY_PERSONAL_ACCOUNT: "owner",
          RELAY_PERSONAL_PASSWORD: "plover-quartz-lantern-41",
        },
      },
    }),
  ],
  test: { include: ["test/**/*.test.ts"] },
});
