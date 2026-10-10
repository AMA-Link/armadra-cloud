/**
 * `personal serve` 的控制面（`--control memory|redis|postgres`，Node 中继存储设计 §5.2）：
 *
 * - `memory`（缺省）：内核的内存实现，单节点时就是正确的；
 * - `redis` / `postgres`：多节点，路由互见、在线事件与关隧道广播、撤销与速率限流各节点共用。
 *
 * 后两者的客户端与实现**按需动态 import**：缺省装配的静态 import 图里没有 Redis / PostgreSQL 驱动
 * （`boundaries.test.ts` 守）。`--control postgres` 先把库迁移到最新（与 `--store postgres` 同一份
 * 迁移与账本，对不上拒绝启动）。连接串含口令：日志只记种类与节点名。
 */
import { createMemoryControlPlane } from "@armadra/relay-kernel/control/memory";
import type { LimitKind, LimitRule } from "@armadra/relay-kernel/control/types";
import type { JwkSet } from "@armadra/relay-kernel/control/types";
import type {
  ClusterControlOptions,
  ClusterControlPlane,
  ControlKind,
} from "@armadra/relay-runtime-node/control/types";

import { StateError } from "./state.ts";
import type { DatabaseOptions } from "./store-types.ts";

/** 启动时连 Redis 最多等多久。 */
const CONNECT_TIMEOUT_MS = 5_000;

export interface ControlInput {
  kind: ControlKind;
  nodeId: string;
  issuer: string;
  jwks: () => JwkSet | Promise<JwkSet>;
  revoked: (jti: string) => Promise<boolean>;
  limits: Partial<Record<LimitKind, LimitRule>>;
  now: () => number;
  database?: DatabaseOptions;
  redisUrl?: string;
  onError: (error: unknown, operation: string) => void;
}

export interface PersonalControl {
  control: ClusterControlPlane;
  /** `/health` 的就绪探针（多节点时探外部服务）。 */
  probes: Record<string, () => Promise<void>>;
}

export async function createControl(
  input: ControlInput,
): Promise<PersonalControl> {
  const base: ClusterControlOptions = {
    mode: "personal",
    issuer: input.issuer,
    jwks: input.jwks,
    nodeId: input.nodeId,
    revoked: input.revoked,
    limits: input.limits,
    now: input.now,
    onError: input.onError,
  };
  if (input.kind === "memory") {
    const memory = createMemoryControlPlane(base);
    return { control: { ...memory, close: async () => {} }, probes: {} };
  }
  if (input.kind === "redis") {
    const { createRedis, pingRedis } = await import(
      "@armadra/cloud-shared/redis"
    );
    const { createRedisControlPlane } = await import(
      "@armadra/relay-runtime-node/control/redis"
    );
    const redis = createRedis(input.redisUrl ?? "");
    try {
      // 客户端断线自己重连、`connect()` 连不上会一直等：启动时只等 {@link CONNECT_TIMEOUT_MS}。
      let timer: NodeJS.Timeout | undefined;
      await Promise.race([
        redis.connect(),
        new Promise((_resolve, reject) => {
          timer = setTimeout(
            () =>
              reject(Object.assign(new Error("timeout"), { code: "timeout" })),
            CONNECT_TIMEOUT_MS,
          );
        }),
      ]).finally(() => clearTimeout(timer));
      const control = await createRedisControlPlane({ ...base, redis });
      return {
        control: {
          ...control,
          async close() {
            await control.close();
            redis.destroy();
          },
        },
        probes: { redis: () => pingRedis(redis) },
      };
    } catch (error) {
      redis.destroy();
      throw new StateError(
        "invalid",
        `Redis 连不上：${(error as { code?: string }).code ?? "unknown"}`,
      );
    }
  }
  const { connectPostgres, ensureSchema } = await import(
    "@armadra/control-store-postgres/open"
  );
  const { migratePostgres } = await import(
    "@armadra/control-store-postgres/migrate"
  );
  const { PostgresStoreError } = await import(
    "@armadra/control-store-postgres/errors"
  );
  const { createPostgresControlPlane } = await import(
    "@armadra/relay-runtime-node/control/postgres"
  );
  const database = input.database as DatabaseOptions;
  const sql = connectPostgres({
    url: database.url,
    ...(database.schema ? { schema: database.schema } : {}),
    max: 5,
    applicationName: `armadra-relay-${input.nodeId}`.slice(0, 63),
  });
  try {
    await ensureSchema(sql, database.schema);
    await migratePostgres(sql);
    const control = await createPostgresControlPlane({ ...base, sql });
    return {
      control: {
        ...control,
        async close() {
          await control.close();
          await sql.end({ timeout: 5 });
        },
      },
      probes: {
        database: async () => {
          await sql`SELECT 1`;
        },
      },
    };
  } catch (error) {
    await sql.end({ timeout: 5 });
    if (error instanceof PostgresStoreError)
      throw new StateError("invalid", error.message);
    if (error instanceof Error && error.name === "MissingSchemaError")
      throw new StateError("invalid", error.message);
    throw new StateError(
      "invalid",
      `PostgreSQL 连不上：${(error as { code?: string }).code ?? "unknown"}`,
    );
  }
}
