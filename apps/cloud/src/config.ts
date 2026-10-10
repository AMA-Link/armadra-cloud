/** 控制面环境变量（控制面设计 §9）。C0-1 只解析起服务与探活要用到的那几项。 */
import {
  listenSchema,
  logLevelSchema,
  parseConfig,
  type Env,
} from "@armadra/cloud-shared/config";
import { defaultMigrationsDir } from "@armadra/control-store-postgres/migrate";
import { z } from "zod";

/** PostgreSQL 迁移只有一个目录：`@armadra/control-store-postgres` 的 `migrations/`（单文件旁的 `pg-migrations/`）。 */
const DEFAULT_MIGRATIONS_DIR = defaultMigrationsDir();

const serveSchema = z.object({
  ARMADRA_CLOUD_ISSUER: z.url(),
  ARMADRA_CLOUD_APP_ORIGIN: z.url(),
  ARMADRA_CLOUD_LISTEN: listenSchema.default({ host: "0.0.0.0", port: 8100 }),
  ARMADRA_CLOUD_DATABASE_URL: z.url(),
  ARMADRA_CLOUD_REDIS_URL: z.url(),
  ARMADRA_CLOUD_MASTER_KEY: z
    .string()
    .regex(/^[0-9a-f]{64}$/i, "须为 32 字节 hex")
    .optional(),
  ARMADRA_CLOUD_RELAY_DOMAIN: z.string().optional(),
  ARMADRA_CLOUD_LOG_LEVEL: logLevelSchema,
});

const migrateSchema = z.object({
  ARMADRA_CLOUD_DATABASE_URL: z.url(),
  ARMADRA_CLOUD_MIGRATIONS_DIR: z.string().default(DEFAULT_MIGRATIONS_DIR),
  ARMADRA_CLOUD_LOG_LEVEL: logLevelSchema,
});

export interface CloudConfig {
  issuer: string;
  appOrigin: string;
  listen: { host: string; port: number };
  databaseUrl: string;
  redisUrl: string;
  relayDomain: string | null;
  logLevel: "debug" | "info" | "warn" | "error";
}

export function loadServeConfig(env: Env): CloudConfig {
  const raw = parseConfig(serveSchema, env);
  return {
    issuer: raw.ARMADRA_CLOUD_ISSUER,
    appOrigin: raw.ARMADRA_CLOUD_APP_ORIGIN,
    listen: raw.ARMADRA_CLOUD_LISTEN,
    databaseUrl: raw.ARMADRA_CLOUD_DATABASE_URL,
    redisUrl: raw.ARMADRA_CLOUD_REDIS_URL,
    relayDomain: raw.ARMADRA_CLOUD_RELAY_DOMAIN ?? null,
    logLevel: raw.ARMADRA_CLOUD_LOG_LEVEL,
  };
}

export interface MigrateConfig {
  databaseUrl: string;
  migrationsDir: string;
  logLevel: "debug" | "info" | "warn" | "error";
}

export function loadMigrateConfig(env: Env): MigrateConfig {
  const raw = parseConfig(migrateSchema, env);
  return {
    databaseUrl: raw.ARMADRA_CLOUD_DATABASE_URL,
    migrationsDir: raw.ARMADRA_CLOUD_MIGRATIONS_DIR,
    logLevel: raw.ARMADRA_CLOUD_LOG_LEVEL,
  };
}
