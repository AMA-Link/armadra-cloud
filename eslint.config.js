// ESLint（flat config）。规则与 Armadra 根配置同源，裁掉了 React 相关部分；
// 格式交给 Prettier（eslint-config-prettier 放最后）。
//
// error 只给代码边界（AGENTS.md「代码边界」）；其余推荐规则 warn，只报不阻断。
import js from "@eslint/js";
import prettier from "eslint-config-prettier";
import globals from "globals";
import tseslint from "typescript-eslint";

/** 把一组共享配置里的 error 统一降为 warn。 */
function warnOnly(configs) {
  return configs.map((config) => {
    if (!config.rules) return config;
    const rules = {};
    for (const [name, value] of Object.entries(config.rules)) {
      if (value === "error" || value === 2) rules[name] = "warn";
      else if (Array.isArray(value) && (value[0] === "error" || value[0] === 2))
        rules[name] = ["warn", ...value.slice(1)];
      else rules[name] = value;
    }
    return { ...config, rules };
  });
}

const TS = ["**/*.{ts,mts,cts}"];
const JS = ["**/*.{js,mjs,cjs}"];

// ------------------------------------------------------------ import 边界

/** `@orpc/*` 只能出现在门面里（AGENTS.md）；门面文件在下方单独放开。 */
const orpcPattern = {
  group: ["@orpc/*"],
  message:
    "@orpc/* 只能出现在 packages/platform-protocol/src/cloud-api/、packages/control-core/src/router.ts、tools/contract/。",
};

/** 与 Armadra 的往来只经协议包与容器镜像，不 import 它的源码。 */
const armadraSourcePattern = {
  group: [
    "@armadra/shared",
    "@armadra/shared/*",
    "@armadra/desktop",
    "@armadra/desktop/*",
    "@armadra/web",
    "@armadra/web/*",
    "@armadra/server",
    "@armadra/server/*",
  ],
  message:
    "不得 import Armadra 仓的源码；只经 @armadra/platform-protocol 与容器镜像往来。",
};

/** 中继的 TS 包（relay-kernel、relay-runtime-workers）不得 import apps/cloud，也不直接碰数据库驱动。 */
const relayPatterns = [
  {
    group: ["@armadra/cloud", "@armadra/cloud/*"],
    message: "中继的包不得 import apps/cloud；两者只共享 packages/*。",
  },
  {
    regex: "^(\\.\\./)+cloud/",
    message: "中继的包不得 import apps/cloud；两者只共享 packages/*。",
  },
  {
    group: ["postgres", "@armadra/cloud-shared/db"],
    message:
      "中继的 TS 包不直接碰数据库驱动：PostgreSQL 只经 @armadra/control-store-postgres。",
  },
];

/** PostgreSQL 存储只实现控制核的端口。 */
const controlStorePostgresPatterns = [
  {
    group: [
      "redis",
      "ws",
      "@armadra/cloud-shared/redis",
      "@armadra/cloud-shared/http",
      "@armadra/control-store-sqlite",
      "@armadra/control-store-sqlite/*",
      "@armadra/relay-kernel",
      "@armadra/relay-kernel/*",
      "@armadra/relay-runtime-workers",
      "@armadra/relay-runtime-workers/*",
      "@armadra/cloud",
      "@armadra/cloud/*",
    ],
    message:
      "control-store-postgres 只实现控制核的端口：不带 Redis、网络、中继与别的存储实现。",
  },
];

/** 中继内核（personal 与 saas 共用的数据面）：不 import 控制核，不带 SaaS 专有依赖。 */
const kernelPatterns = [
  {
    group: ["@armadra/control-core", "@armadra/control-core/*"],
    message:
      "relay-kernel 不得 import control-core；控制面经 ControlPlane 接口接进来。",
  },
  {
    group: ["redis", "@armadra/cloud-shared/redis"],
    message:
      "relay-kernel 不带 SaaS 专有依赖：Redis 实现在 Go 中继（go/internal/control/redis）。",
  },
];

/**
 * 内核运行时无关（Workers 版用）：只许 `node:crypto` / `node:buffer`；`ws`、
 * `node:http` / `net` / `stream` / `fs` 与 HTTP 外壳不进内核。
 * 只管源码，测试照常可以用 Node 起服务器。
 */
const kernelPortablePatterns = [
  {
    regex: "^node:(?!(crypto|buffer)$)",
    message:
      "relay-kernel 运行时无关：只许 node:crypto / node:buffer，其余 Node 专有部分不进内核。",
  },
  {
    group: ["ws", "@armadra/cloud-shared/http"],
    message: "relay-kernel 运行时无关：WebSocket 与 HTTP 外壳不进内核。",
  },
];

/**
 * Workers 运行时：跑在 workerd 上，只许 `node:crypto` / `node:buffer`（`nodejs_compat` 原生支持）；
 * 不带 `ws`、HTTP 外壳、Node 运行时与 PostgreSQL / Redis，`node:sqlite` 版存储的打开与迁移读目录
 * 也不能进来（DO SQLite 只复用端口、行映射与迁移对账）。
 */
const workersRuntimePatterns = [
  {
    regex: "^node:(?!(crypto|buffer)$)",
    message:
      "relay-runtime-workers 跑在 workerd 上：只许 node:crypto / node:buffer。",
  },
  {
    regex: "^(ws|postgres|redis)$",
    message:
      "relay-runtime-workers 不带 ws 与 PostgreSQL / Redis：客户端 WS 由 workerd 终止，存储是 DO SQLite。",
  },
  {
    group: [
      "@armadra/cloud-shared/db",
      "@armadra/cloud-shared/redis",
      "@armadra/cloud-shared/http",
      "@armadra/cloud-shared/config",
      "@armadra/control-store-sqlite/open",
      "@armadra/control-store-sqlite/migrate",
      "@armadra/cloud",
      "@armadra/cloud/*",
    ],
    message:
      "relay-runtime-workers 只装配内核、控制核与可移植的 SQLite 端口；Node 专有的部分不进 Worker。",
  },
];

/** 控制核只经端口读写状态，且与传输无关。 */
const controlCorePatterns = [
  {
    group: [
      "postgres",
      "redis",
      "@armadra/cloud-shared/db",
      "@armadra/cloud-shared/redis",
    ],
    message:
      "control-core 只经端口读写状态；PostgreSQL / Redis 实现放在 app 的适配器里。",
  },
  {
    regex: "^node:fs(/promises)?$",
    message: "control-core 只经端口读写状态，不碰文件系统。",
  },
  {
    group: [
      "ws",
      "node:http",
      "node:https",
      "node:net",
      "@armadra/cloud-shared/http",
    ],
    message: "control-core 与传输无关：HTTP / WebSocket 外壳在 app 里。",
  },
  {
    group: [
      "@armadra/relay-kernel",
      "@armadra/relay-kernel/*",
      "@armadra/cloud",
      "@armadra/cloud/*",
    ],
    message: "control-core 不 import 中继内核与 app。",
  },
];

/** 协议包只含 schema、类型、纯编解码与常量：不 import node:*（fixtures 子路径除外）。 */
const nodeBuiltinPattern = {
  regex: "^node:",
  message:
    "协议包要能在页面与手机里直接用，不得 import node:*（只有 src/fixtures/ 例外）。",
};

const restrictImports = (...patterns) => ({
  "no-restricted-imports": ["error", { patterns }],
});

export default tseslint.config(
  {
    ignores: [
      "**/node_modules/**",
      "**/dist/**",
      "**/out/**",
      "**/coverage/**",
      "**/*.d.ts",
      ".pnpm-store/**",
      ".data/**",
      "docs/**",
    ],
  },
  { linterOptions: { reportUnusedDisableDirectives: "off" } },

  ...warnOnly([js.configs.recommended]),
  ...warnOnly(tseslint.configs.recommended),
  {
    files: [...TS, ...JS],
    languageOptions: {
      ecmaVersion: 2023,
      sourceType: "module",
      globals: { ...globals.node },
    },
    rules: {
      "@typescript-eslint/no-unused-vars": [
        "warn",
        {
          argsIgnorePattern: "^_",
          varsIgnorePattern: "^_",
          caughtErrors: "none",
        },
      ],
      ...restrictImports(orpcPattern, armadraSourcePattern),
    },
  },
  {
    files: ["packages/relay-kernel/**/*.ts"],
    rules: restrictImports(
      orpcPattern,
      armadraSourcePattern,
      ...relayPatterns,
      ...kernelPatterns,
    ),
  },
  {
    files: ["packages/control-store-postgres/src/**/*.ts"],
    ignores: ["packages/control-store-postgres/src/**/*.test.ts"],
    rules: restrictImports(
      orpcPattern,
      armadraSourcePattern,
      ...controlStorePostgresPatterns,
    ),
  },
  {
    files: ["packages/relay-kernel/src/**/*.ts"],
    ignores: ["packages/relay-kernel/src/**/*.test.ts"],
    rules: restrictImports(
      orpcPattern,
      armadraSourcePattern,
      ...relayPatterns,
      ...kernelPatterns,
      ...kernelPortablePatterns,
    ),
  },
  {
    files: [
      "packages/relay-runtime-workers/src/**/*.ts",
      "apps/relay-workers/src/**/*.ts",
    ],
    rules: restrictImports(
      orpcPattern,
      armadraSourcePattern,
      ...workersRuntimePatterns,
    ),
  },
  {
    files: ["packages/control-core/src/**/*.ts"],
    ignores: ["packages/control-core/src/**/*.test.ts"],
    rules: restrictImports(
      orpcPattern,
      armadraSourcePattern,
      ...controlCorePatterns,
    ),
  },
  {
    files: ["packages/platform-protocol/src/**/*.ts"],
    ignores: ["packages/platform-protocol/src/fixtures/**"],
    rules: restrictImports(
      orpcPattern,
      armadraSourcePattern,
      nodeBuiltinPattern,
    ),
  },
  {
    files: ["packages/platform-protocol/src/cloud-api/**/*.ts"],
    rules: restrictImports(armadraSourcePattern, nodeBuiltinPattern),
  },
  {
    files: ["tools/contract/**"],
    rules: restrictImports(armadraSourcePattern),
  },
  {
    files: ["packages/control-core/src/router.ts"],
    rules: restrictImports(armadraSourcePattern, ...controlCorePatterns),
  },

  prettier,
);
