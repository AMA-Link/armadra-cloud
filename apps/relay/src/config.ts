/** saas 模式的环境变量（中继设计 §7）。只解析起服务与探活要用到的那几项；personal 的参数在 personal/args.ts。 */
import {
  flagSchema,
  listenSchema,
  logLevelSchema,
  parseConfig,
  type Env,
} from "@armadra/cloud-shared/config";
import { hostname } from "node:os";
import { z } from "zod";

const saasSchema = z.object({
  RELAY_LISTEN: listenSchema.default({ host: "0.0.0.0", port: 8443 }),
  RELAY_NODE_ID: z
    .string()
    .regex(/^[A-Za-z0-9._-]{1,64}$/)
    .optional(),
  RELAY_DOMAIN: z.string().min(1),
  RELAY_CLOUD_ISSUER: z.url(),
  RELAY_REDIS_URL: z.url(),
  RELAY_ALLOW_INSECURE_TUNNEL: flagSchema,
  RELAY_LOG_LEVEL: logLevelSchema,
});

export interface SaasRelayConfig {
  mode: "saas";
  listen: { host: string; port: number };
  nodeId: string;
  domain: string;
  cloudIssuer: string;
  redisUrl: string;
  allowInsecureTunnel: boolean;
  logLevel: "debug" | "info" | "warn" | "error";
}

export function loadSaasConfig(env: Env): SaasRelayConfig {
  const raw = parseConfig(saasSchema, env);
  return {
    mode: "saas",
    listen: raw.RELAY_LISTEN,
    nodeId:
      raw.RELAY_NODE_ID ??
      hostname()
        .replace(/[^A-Za-z0-9._-]/g, "-")
        .slice(0, 64),
    domain: raw.RELAY_DOMAIN,
    cloudIssuer: raw.RELAY_CLOUD_ISSUER,
    redisUrl: raw.RELAY_REDIS_URL,
    allowInsecureTunnel: raw.RELAY_ALLOW_INSECURE_TUNNEL,
    logLevel: raw.RELAY_LOG_LEVEL,
  };
}
