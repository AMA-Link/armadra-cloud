/**
 * Worker 的绑定与配置（`wrangler.jsonc` 的 `vars`、secret 与绑定）。配置一次读齐、校验，坏了
 * 整个 Worker 答 503 `unavailable`（不带配置内容）。secret 的值只在这里经过，不进日志与响应。
 */
import type { ControlDO } from "./control-do.ts";
import type { SourceDO } from "./source-do.ts";

export interface RelayEnv {
  /** `personal`（缺省）或 `saas`（不叫 `MODE`：本地工具链常把它设成 `test` / `development`）。 */
  RELAY_MODE?: string;
  /** 对外地址（personal 的 issuer，也是 `/app/` 与隧道节点的地址），如 `https://relay.armadra.com`。 */
  RELAY_ISSUER?: string;
  /** 服务端名称（`platform.info.name`）；缺省取租户设置，再缺省取 issuer 的主机名。 */
  RELAY_NAME?: string;
  /** 隧道心跳（`ready.heartbeatMs`），毫秒，10_000–60_000，缺省 45_000。 */
  RELAY_HEARTBEAT_MS?: string;
  /** 检查周期（alarm），毫秒，10_000–600_000，缺省 60_000（Workers Free 建议 120_000）。 */
  RELAY_ALARM_MS?: string;
  /** 客户端 WS 两个方向都没有字节多久就断开，毫秒，缺省 120_000。 */
  RELAY_CLIENT_IDLE_MS?: string;
  /** 日配额：请求数（UTC 自然日），缺省 2_000_000；0 = 不限。 */
  RELAY_DAILY_REQUEST_BUDGET?: string;
  /** 日配额：隧道载荷字节，缺省 50 GB；0 = 不限。 */
  RELAY_DAILY_TUNNEL_BYTES_BUDGET?: string;
  /** 首次初始化时的可信来源（逗号分隔，之后以租户设置为准）。 */
  RELAY_TRUSTED_ORIGINS?: string;
  /** ControlDO 第一次创建时的位置提示（`apac`、`wnam`…），缺省 `apac`。 */
  RELAY_CONTROL_LOCATION?: string;
  /** saas：控制面的 issuer（JWKS 与节点回报都找它）。 */
  RELAY_CLOUD_ISSUER?: string;
  /** saas：源地址的域（`<sourceId>.src.<域>`）；不给就用路径寻址 `/s/<id>/`。 */
  RELAY_DOMAIN?: string;
  RELAY_LOG_LEVEL?: string;
  // ------------------------------------------------------------ secret
  /** 签名私钥的主密钥：64 位十六进制（32 字节），personal 必需。 */
  RELAY_MASTER_KEY?: string;
  /** `/metrics` 的 Bearer 令牌；不配就不开 `/metrics`。 */
  RELAY_METRICS_TOKEN?: string;
  /** 首次初始化的账号名与口令（初始化完成后删掉这两个 secret）。 */
  RELAY_PERSONAL_ACCOUNT?: string;
  RELAY_PERSONAL_PASSWORD?: string;
  /** saas：节点令牌（`relay.nodePresence`）。 */
  RELAY_NODE_TOKEN?: string;
  // ------------------------------------------------------------ 绑定
  SOURCE: DurableObjectNamespace<SourceDO>;
  CONTROL: DurableObjectNamespace<ControlDO>;
  /** Armadra `apps/web` 的构建产物（Static Assets）；没有就答自有页面。 */
  ASSETS?: Fetcher;
  /** Rate Limiting 绑定（按 IP，§4.2）；没配的闸不生效。 */
  LIMIT_LOGIN?: RateLimit;
  LIMIT_LINK_ACCEPT?: RateLimit;
  LIMIT_REGISTER?: RateLimit;
  LIMIT_TUNNEL?: RateLimit;
  LIMIT_MEDIA?: RateLimit;
  LIMIT_ANONYMOUS?: RateLimit;
  LIMIT_PREFLIGHT?: RateLimit;
}

export type RelayMode = "personal" | "saas";

export interface RelayConfig {
  mode: RelayMode;
  /** 对外地址（无尾斜杠）。 */
  issuer: string;
  name: string | null;
  heartbeatMs: number;
  alarmMs: number;
  clientIdleMs: number;
  dailyRequestBudget: number;
  dailyTunnelBytesBudget: number;
  trustedOrigins: string[];
  controlLocation: DurableObjectLocationHint;
  cloudIssuer: string | null;
  domain: string | null;
  logLevel: "debug" | "info" | "warn" | "error";
}

export class ConfigError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "ConfigError";
  }
}

const LOCATIONS = new Set<string>([
  "wnam",
  "enam",
  "sam",
  "weur",
  "eeur",
  "apac",
  "apac-ne",
  "apac-se",
  "oc",
  "afr",
  "me",
]);

/** 缺省心跳（cloud-api §17）：45 秒，用户可改到 10–60 秒。 */
export const DEFAULT_HEARTBEAT_MS = 45_000;
export const DEFAULT_ALARM_MS = 60_000;
export const DEFAULT_CLIENT_IDLE_MS = 120_000;
export const DEFAULT_DAILY_REQUEST_BUDGET = 2_000_000;
export const DEFAULT_DAILY_TUNNEL_BYTES_BUDGET = 50 * 1024 ** 3;

function integer(
  value: string | undefined,
  name: string,
  fallback: number,
  min: number,
  max: number,
): number {
  if (value === undefined || value.trim() === "") return fallback;
  if (!/^\d+$/.test(value.trim())) throw new ConfigError(`${name} 不是整数`);
  const parsed = Number(value.trim());
  if (parsed < min || parsed > max)
    throw new ConfigError(`${name} 超出范围：${min}–${max}`);
  return parsed;
}

/** `https://host[:port]` 形式的来源（无路径、无尾斜杠）；不是就抛。 */
export function originOf(value: string, name: string): string {
  let url: URL;
  try {
    url = new URL(value);
  } catch {
    throw new ConfigError(`${name} 不是地址`);
  }
  if (url.protocol !== "https:" && url.protocol !== "http:")
    throw new ConfigError(`${name} 要是 http(s) 地址`);
  if (url.pathname !== "/" || url.search || url.hash || url.username)
    throw new ConfigError(`${name} 只能是来源（不带路径）`);
  return url.origin;
}

export function readConfig(env: RelayEnv): RelayConfig {
  const mode = (env.RELAY_MODE ?? "personal").trim() as RelayMode;
  if (mode !== "personal" && mode !== "saas")
    throw new ConfigError("RELAY_MODE 只能是 personal 或 saas");
  if (!env.RELAY_ISSUER) throw new ConfigError("缺少 RELAY_ISSUER");
  const issuer = originOf(env.RELAY_ISSUER, "RELAY_ISSUER");
  const cloudIssuer = env.RELAY_CLOUD_ISSUER
    ? originOf(env.RELAY_CLOUD_ISSUER, "RELAY_CLOUD_ISSUER")
    : null;
  if (mode === "saas" && !cloudIssuer)
    throw new ConfigError("saas 模式要 RELAY_CLOUD_ISSUER");
  const location = (env.RELAY_CONTROL_LOCATION ?? "apac").trim();
  if (!LOCATIONS.has(location))
    throw new ConfigError("RELAY_CONTROL_LOCATION 不认识");
  const level = (env.RELAY_LOG_LEVEL ?? "info").trim();
  if (!["debug", "info", "warn", "error"].includes(level))
    throw new ConfigError("RELAY_LOG_LEVEL 不认识");
  const domain = env.RELAY_DOMAIN?.trim().toLowerCase() || null;
  if (domain !== null && !/^[a-z0-9.-]+$/.test(domain))
    throw new ConfigError("RELAY_DOMAIN 不是域名");
  const trustedOrigins = (env.RELAY_TRUSTED_ORIGINS ?? "")
    .split(",")
    .map((item) => item.trim())
    .filter(Boolean)
    .map((item) => originOf(item, "RELAY_TRUSTED_ORIGINS"));
  return {
    mode,
    issuer,
    name: env.RELAY_NAME?.trim() || null,
    heartbeatMs: integer(
      env.RELAY_HEARTBEAT_MS,
      "RELAY_HEARTBEAT_MS",
      DEFAULT_HEARTBEAT_MS,
      10_000,
      60_000,
    ),
    alarmMs: integer(
      env.RELAY_ALARM_MS,
      "RELAY_ALARM_MS",
      DEFAULT_ALARM_MS,
      1_000,
      600_000,
    ),
    clientIdleMs: integer(
      env.RELAY_CLIENT_IDLE_MS,
      "RELAY_CLIENT_IDLE_MS",
      DEFAULT_CLIENT_IDLE_MS,
      1_000,
      3_600_000,
    ),
    dailyRequestBudget: integer(
      env.RELAY_DAILY_REQUEST_BUDGET,
      "RELAY_DAILY_REQUEST_BUDGET",
      DEFAULT_DAILY_REQUEST_BUDGET,
      0,
      Number.MAX_SAFE_INTEGER,
    ),
    dailyTunnelBytesBudget: integer(
      env.RELAY_DAILY_TUNNEL_BYTES_BUDGET,
      "RELAY_DAILY_TUNNEL_BYTES_BUDGET",
      DEFAULT_DAILY_TUNNEL_BYTES_BUDGET,
      0,
      Number.MAX_SAFE_INTEGER,
    ),
    trustedOrigins,
    controlLocation: location as DurableObjectLocationHint,
    cloudIssuer,
    domain,
    logLevel: level as RelayConfig["logLevel"],
  };
}

/** 隧道节点地址（cloud-api §17）：issuer 的 `wss://…/t/v1?src=<sourceId>`。 */
export function tunnelNodeUrl(issuer: string, sourceId: string): string {
  return `${issuer.replace(/^http/, "ws")}/t/v1?src=${sourceId}`;
}

/** 两种对象的名字：一源一个 `SourceDO`，一租户一个 `ControlDO`（personal 恒 `tenant:default`）。 */
export const sourceObjectName = (sourceId: string) => `src:${sourceId}`;
export const CONTROL_OBJECT_NAME = "tenant:default";
