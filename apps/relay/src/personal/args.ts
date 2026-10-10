/**
 * `armadra-relay personal <子命令>` 的参数（中继设计 §6、§7，平台设计 §17.8）。参数解析不用库。
 * 命令行优先，其次环境变量（`RELAY_*`），最后缺省值。
 */
import {
  parseListen,
  type Env,
  type ListenAddress,
} from "@armadra/cloud-shared/config";
import { resolve } from "node:path";

import { PLATFORM_NAME_MAX } from "@armadra/platform-protocol/cloud-api";
import {
  createClientIp,
  parseProxyList,
  type TrustedProxyConfig,
} from "@armadra/relay-runtime-node/client-ip";

import { STORE_KINDS, type StoreKind } from "./store-types.ts";

export const TLS_MODES = ["self-signed", "acme", "file", "plain"] as const;
export type TlsMode = (typeof TLS_MODES)[number];

export const ACCOUNT_NAME = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;

export class UsageError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "UsageError";
  }
}

export function isLoopback(host: string): boolean {
  const literal =
    host.startsWith("[") && host.endsWith("]") ? host.slice(1, -1) : host;
  return (
    literal === "localhost" ||
    literal === "::1" ||
    /^127\.\d+\.\d+\.\d+$/.test(literal)
  );
}

/** `--name value` 对；`positional` 收不带 `--` 的参数（`links revoke <id>`）。 */
export function parseFlags(
  argv: readonly string[],
  known: ReadonlySet<string>,
  options: { positional?: number; booleans?: ReadonlySet<string> } = {},
): { values: Map<string, string>; positional: string[] } {
  const values = new Map<string, string>();
  const positional: string[] = [];
  for (let index = 0; index < argv.length; index += 1) {
    const flag = argv[index] ?? "";
    if (!flag.startsWith("--")) {
      if (positional.length >= (options.positional ?? 0))
        throw new UsageError(`多余的参数：${flag}`);
      positional.push(flag);
      continue;
    }
    const name = flag.slice(2);
    if (!known.has(name)) throw new UsageError(`未知选项：${flag}`);
    if (options.booleans?.has(name)) {
      values.set(name, "1");
      continue;
    }
    const value = argv[index + 1];
    if (value === undefined || (value.startsWith("--") && value !== "-"))
      throw new UsageError(`${flag} 缺少取值`);
    values.set(name, value);
    index += 1;
  }
  return { values, positional };
}

function listenOf(text: string, flag: string): ListenAddress {
  try {
    return parseListen(text);
  } catch {
    throw new UsageError(`${flag} 不是一个地址：${text}`);
  }
}

function originOf(text: string): string {
  let url: URL;
  try {
    url = new URL(text);
  } catch {
    throw new UsageError(`--public-origin 不是一个地址：${text}`);
  }
  if (url.protocol !== "https:" && url.protocol !== "http:")
    throw new UsageError(`--public-origin 只能是 https 或 http：${text}`);
  return url.origin;
}

/** personal 的 issuer：`https://<host>:<port>`（443 时省略端口）。 */
export function defaultIssuer(host: string, port: number): string {
  const name = host.includes(":") && !host.startsWith("[") ? `[${host}]` : host;
  return port === 443 ? `https://${name}` : `https://${name}:${port}`;
}

// ------------------------------------------------------------ 公共

interface Common {
  dataDir: string;
}

function dataDirOf(values: Map<string, string>, env: Env, cwd: string): string {
  const dataDir = values.get("data-dir") ?? env.RELAY_DATA_DIR;
  if (!dataDir) throw new UsageError("缺少 --data-dir（或 RELAY_DATA_DIR）");
  return resolve(cwd, dataDir);
}

function accountOf(values: Map<string, string>, env: Env): string | undefined {
  const account = values.get("account") ?? env.RELAY_PERSONAL_ACCOUNT;
  if (account !== undefined && !ACCOUNT_NAME.test(account))
    throw new UsageError(
      `--account 只能是字母、数字与 . _ -，1–64 个字符：${account}`,
    );
  return account;
}

function passwordFileOf(
  values: Map<string, string>,
  env: Env,
  cwd: string,
): string | undefined {
  const file = values.get("password-file") ?? env.RELAY_PERSONAL_PASSWORD_FILE;
  if (file === undefined || file === "-") return file;
  return resolve(cwd, file);
}

function portOf(values: Map<string, string>, env: Env): number {
  const text = values.get("port") ?? env.RELAY_PORT ?? "8443";
  const port = Number(text);
  if (!Number.isInteger(port) || port < 1 || port > 65_535)
    throw new UsageError(`--port 不是端口：${text}`);
  return port;
}

function tlsOf(values: Map<string, string>, env: Env): TlsMode {
  const tls = (values.get("tls") ?? env.RELAY_TLS ?? "self-signed") as TlsMode;
  if (!TLS_MODES.includes(tls))
    throw new UsageError(`--tls 只能是 ${TLS_MODES.join(" | ")}：${tls}`);
  return tls;
}

/** `--store` / `RELAY_STORE`；不给时由数据目录里已有的文件决定（新装 sqlite）。 */
function storeOf(values: Map<string, string>, env: Env): StoreKind | undefined {
  const text = values.get("store") ?? env.RELAY_STORE;
  if (text === undefined) return undefined;
  if (!(STORE_KINDS as readonly string[]).includes(text))
    throw new UsageError(`--store 只能是 ${STORE_KINDS.join(" | ")}：${text}`);
  return text as StoreKind;
}

// ------------------------------------------------------------ init

export interface PersonalInitOptions extends Common {
  account: string;
  /** 口令文件；`-` 是标准输入；不给时在终端上交互读。 */
  passwordFile?: string;
  host: string;
  port: number;
  tls: TlsMode;
  publicOrigin?: string;
  /** 缺省 sqlite。 */
  store?: StoreKind;
}

const INIT_FLAGS = new Set([
  "data-dir",
  "store",
  "account",
  "password-file",
  "host",
  "port",
  "tls",
  "public-origin",
]);

export function parsePersonalInit(
  argv: readonly string[],
  env: Env,
  cwd = process.cwd(),
): PersonalInitOptions {
  const { values } = parseFlags(argv, INIT_FLAGS);
  const account = accountOf(values, env);
  if (!account)
    throw new UsageError("缺少 --account（或 RELAY_PERSONAL_ACCOUNT）");
  const host = values.get("host") ?? env.RELAY_HOST;
  if (!host)
    throw new UsageError("缺少 --host：公网 IP 或域名（或 RELAY_HOST）");
  const publicText = values.get("public-origin") ?? env.RELAY_PUBLIC_ORIGIN;
  const passwordFile = passwordFileOf(values, env, cwd);
  const store = storeOf(values, env);
  return {
    dataDir: dataDirOf(values, env, cwd),
    account,
    ...(passwordFile ? { passwordFile } : {}),
    host,
    port: portOf(values, env),
    tls: tlsOf(values, env),
    ...(publicText ? { publicOrigin: originOf(publicText) } : {}),
    ...(store ? { store } : {}),
  };
}

// ------------------------------------------------------------ serve

export interface AcmeArgs {
  email: string;
  directory?: string;
  caBundle?: string;
  httpListen: ListenAddress;
}

export interface PersonalServeOptions extends Common {
  /** `--store` / `RELAY_STORE`；不给时按数据目录里已有的文件（新装 sqlite）。 */
  store?: StoreKind;
  /** 对外的 IP 或域名（issuer 与自签证书的 SAN 用它）。 */
  host: string;
  port: number;
  listen: ListenAddress;
  tls: TlsMode;
  logLevel: "debug" | "info" | "warn" | "error";
  /** 对外来源；缺省 `https://<host>:<port>`。 */
  publicOrigin?: string;
  /** 首次自动 init 用（`RELAY_PERSONAL_ACCOUNT` + `RELAY_PERSONAL_PASSWORD_FILE`）。 */
  account?: string;
  passwordFile?: string;
  /** Armadra `apps/web` 的构建产物（`--web-root` / `RELAY_WEB_ROOT`）。 */
  webRoot?: string;
  tlsCert?: string;
  tlsKey?: string;
  acme?: AcmeArgs;
  /** 指标单独监听（`--metrics-listen` / `RELAY_METRICS_LISTEN`）；缺省不开。 */
  metricsListen?: ListenAddress;
  /** 指标要求的 Bearer 令牌（`RELAY_METRICS_TOKEN`，只走环境变量）。 */
  metricsToken?: string;
  /** 边缘限流额度（次 / 分钟）；没给的项取状态文件 `settings.rateLimits`，再取缺省。 */
  rateLimits?: RateLimitOptions;
  /** 受信代理（`--trusted-proxy` / `RELAY_TRUSTED_PROXY`）；没给取状态文件 `settings.trustedProxy`。 */
  trustedProxy?: TrustedProxyConfig;
  /**
   * 服务端名称（`--name` / `RELAY_NAME`，`platform.info.name`）；没给取状态文件
   * `settings.displayName`，再取 `host`。
   */
  name?: string;
}

export interface RateLimitOptions {
  /** 无令牌或令牌无效的请求，按 IP。 */
  anonymous?: number;
  /** CORS 预检，按 IP，单独的桶。 */
  preflight?: number;
  /** 带有效令牌的请求，按令牌。 */
  token?: number;
  /** 带有效令牌的请求，按账号（该账号全部令牌之和）。 */
  account?: number;
}

const RATE_LIMIT_FLAGS = [
  ["anonymous", "limit-anonymous", "RELAY_LIMIT_ANONYMOUS"],
  ["preflight", "limit-preflight", "RELAY_LIMIT_PREFLIGHT"],
  ["token", "limit-token", "RELAY_LIMIT_TOKEN"],
  ["account", "limit-account", "RELAY_LIMIT_ACCOUNT"],
] as const;

function rateLimitsOf(
  values: Map<string, string>,
  env: Env,
): RateLimitOptions | undefined {
  const limits: RateLimitOptions = {};
  for (const [key, flag, variable] of RATE_LIMIT_FLAGS) {
    const text = values.get(flag) ?? env[variable];
    if (text === undefined) continue;
    const value = Number(text);
    if (!Number.isInteger(value) || value < 1 || value > 1_000_000)
      throw new UsageError(`--${flag} 要是 1 到 1000000 的整数：${text}`);
    limits[key] = value;
  }
  return Object.keys(limits).length > 0 ? limits : undefined;
}

function trustedProxyOf(
  values: Map<string, string>,
  env: Env,
): TrustedProxyConfig | undefined {
  const text = values.get("trusted-proxy") ?? env.RELAY_TRUSTED_PROXY;
  const preferText =
    values.get("trusted-proxy-prefer") ?? env.RELAY_TRUSTED_PROXY_PREFER;
  if (
    preferText !== undefined &&
    preferText !== "forwarded-for" &&
    preferText !== "real-ip"
  )
    throw new UsageError(
      `--trusted-proxy-prefer 只能是 forwarded-for 或 real-ip：${preferText}`,
    );
  if (text === undefined) {
    if (preferText !== undefined)
      throw new UsageError(
        "--trusted-proxy-prefer 要和 --trusted-proxy 一起给",
      );
    return undefined;
  }
  const proxies = parseProxyList(text);
  try {
    createClientIp({ proxies });
  } catch (error) {
    throw new UsageError(
      `--trusted-proxy ${error instanceof Error ? error.message : String(error)}`,
    );
  }
  return { proxies, ...(preferText ? { prefer: preferText } : {}) };
}

function nameOf(values: Map<string, string>, env: Env): string | undefined {
  // 环境变量为空当作没给（compose 缺省把它传成空串）；命令行给了空串则拒绝。
  const text = values.get("name") ?? (env.RELAY_NAME || undefined);
  if (text === undefined) return undefined;
  const name = text.trim();
  if (name.length < 1 || name.length > PLATFORM_NAME_MAX)
    throw new UsageError(
      `--name 要是 1 到 ${PLATFORM_NAME_MAX} 个字符：${text}`,
    );
  return name;
}

const SERVE_FLAGS = new Set([
  "data-dir",
  "store",
  "host",
  "port",
  "tls",
  "listen",
  "log-level",
  "metrics-listen",
  "public-origin",
  "account",
  "password-file",
  "web-root",
  "tls-cert",
  "tls-key",
  "acme-email",
  "acme-directory",
  "acme-ca-bundle",
  "acme-http-listen",
  "limit-anonymous",
  "limit-preflight",
  "limit-token",
  "limit-account",
  "trusted-proxy",
  "trusted-proxy-prefer",
  "name",
]);

export function parsePersonalServe(
  argv: readonly string[],
  env: Env,
  cwd = process.cwd(),
): PersonalServeOptions {
  const { values } = parseFlags(argv, SERVE_FLAGS);
  const dataDir = dataDirOf(values, env, cwd);
  const host = values.get("host") ?? env.RELAY_HOST;
  if (!host)
    throw new UsageError("缺少 --host：公网 IP 或域名（或 RELAY_HOST）");
  const port = portOf(values, env);
  const tls = tlsOf(values, env);
  const listen = listenOf(
    values.get("listen") ??
      env.RELAY_LISTEN ??
      `${isLoopback(host) ? "127.0.0.1" : "0.0.0.0"}:${port}`,
    "--listen",
  );
  if (tls === "plain" && !isLoopback(listen.host))
    throw new UsageError(
      `--tls plain 只允许监听回环地址（给同机的反向代理用）：${listen.host}`,
    );
  const logLevel = (values.get("log-level") ??
    env.RELAY_LOG_LEVEL ??
    "info") as PersonalServeOptions["logLevel"];
  if (!["debug", "info", "warn", "error"].includes(logLevel))
    throw new UsageError(`--log-level 不合法：${logLevel}`);
  const metricsText = values.get("metrics-listen") ?? env.RELAY_METRICS_LISTEN;
  const publicText = values.get("public-origin") ?? env.RELAY_PUBLIC_ORIGIN;
  const tlsCert = values.get("tls-cert") ?? env.RELAY_TLS_CERT;
  const tlsKey = values.get("tls-key") ?? env.RELAY_TLS_KEY;
  if (tls === "file" && (!tlsCert || !tlsKey))
    throw new UsageError("--tls file 要同时给 --tls-cert 与 --tls-key");
  let acme: AcmeArgs | undefined;
  if (tls === "acme") {
    const email = values.get("acme-email") ?? env.RELAY_ACME_EMAIL;
    if (!email) throw new UsageError("--tls acme 要给 --acme-email");
    const directory = values.get("acme-directory") ?? env.RELAY_ACME_DIRECTORY;
    const caBundle = values.get("acme-ca-bundle") ?? env.RELAY_ACME_CA_BUNDLE;
    acme = {
      email,
      ...(directory ? { directory } : {}),
      ...(caBundle ? { caBundle: resolve(cwd, caBundle) } : {}),
      httpListen: listenOf(
        values.get("acme-http-listen") ??
          env.RELAY_ACME_HTTP_LISTEN ??
          "0.0.0.0:80",
        "--acme-http-listen",
      ),
    };
  }
  const account = accountOf(values, env);
  const passwordFile = passwordFileOf(values, env, cwd);
  const webRoot = values.get("web-root") ?? env.RELAY_WEB_ROOT;
  const rateLimits = rateLimitsOf(values, env);
  const trustedProxy = trustedProxyOf(values, env);
  const name = nameOf(values, env);
  const store = storeOf(values, env);
  return {
    dataDir,
    ...(store ? { store } : {}),
    host,
    port,
    listen,
    tls,
    logLevel,
    ...(publicText ? { publicOrigin: originOf(publicText) } : {}),
    ...(account ? { account } : {}),
    ...(passwordFile ? { passwordFile } : {}),
    ...(webRoot ? { webRoot: resolve(cwd, webRoot) } : {}),
    ...(tls === "file" && tlsCert && tlsKey
      ? { tlsCert: resolve(cwd, tlsCert), tlsKey: resolve(cwd, tlsKey) }
      : {}),
    ...(acme ? { acme } : {}),
    ...(metricsText
      ? { metricsListen: listenOf(metricsText, "--metrics-listen") }
      : {}),
    ...(env.RELAY_METRICS_TOKEN
      ? { metricsToken: env.RELAY_METRICS_TOKEN }
      : {}),
    ...(rateLimits ? { rateLimits } : {}),
    ...(trustedProxy ? { trustedProxy } : {}),
    ...(name ? { name } : {}),
  };
}

// ------------------------------------------------------------ 其余子命令

export function parseDataDirOnly(
  argv: readonly string[],
  env: Env,
  cwd = process.cwd(),
): Common {
  const { values } = parseFlags(argv, new Set(["data-dir"]));
  return { dataDir: dataDirOf(values, env, cwd) };
}

export function parsePasswd(
  argv: readonly string[],
  env: Env,
  cwd = process.cwd(),
): Common & { passwordFile?: string } {
  const { values } = parseFlags(argv, new Set(["data-dir", "password-file"]));
  const passwordFile = values.get("password-file");
  return {
    dataDir: dataDirOf(values, env, cwd),
    ...(passwordFile
      ? {
          passwordFile: passwordFile === "-" ? "-" : resolve(cwd, passwordFile),
        }
      : {}),
  };
}

export function parseLinks(
  argv: readonly string[],
  env: Env,
  cwd = process.cwd(),
):
  | (Common & { action: "list" })
  | (Common & { action: "revoke"; linkId: string }) {
  const [action, ...rest] = argv;
  if (action === "list") {
    const { values } = parseFlags(rest, new Set(["data-dir"]));
    return { action, dataDir: dataDirOf(values, env, cwd) };
  }
  if (action === "revoke") {
    const { values, positional } = parseFlags(rest, new Set(["data-dir"]), {
      positional: 1,
    });
    const linkId = positional[0];
    if (!linkId || !/^[0-9a-f]{16}$/.test(linkId))
      throw new UsageError(`links revoke 要一个链接 id：${linkId ?? ""}`);
    return { action, linkId, dataDir: dataDirOf(values, env, cwd) };
  }
  throw new UsageError(`links 只有 list 与 revoke：${action ?? ""}`);
}

// ------------------------------------------------------------ 存储

export function parseMigrate(
  argv: readonly string[],
  env: Env,
  cwd = process.cwd(),
): Common & { to: StoreKind } {
  const { values } = parseFlags(argv, new Set(["data-dir", "to"]));
  const to = values.get("to");
  if (to === undefined) throw new UsageError("缺少 --to：sqlite 或 file");
  if (!(STORE_KINDS as readonly string[]).includes(to))
    throw new UsageError(`--to 只能是 ${STORE_KINDS.join(" | ")}：${to}`);
  return { dataDir: dataDirOf(values, env, cwd), to: to as StoreKind };
}

export function parseBackup(
  argv: readonly string[],
  env: Env,
  cwd = process.cwd(),
): Common & { out: string } {
  const { values } = parseFlags(argv, new Set(["data-dir", "out"]));
  const out = values.get("out");
  if (!out) throw new UsageError("缺少 --out：备份文件的路径");
  return { dataDir: dataDirOf(values, env, cwd), out: resolve(cwd, out) };
}

export interface PersonalDoctorOptions extends Common {
  listen: ListenAddress;
  tls: TlsMode;
  /** 受信代理的原文（`--trusted-proxy` / `RELAY_TRUSTED_PROXY`）；没给时查存储里的。 */
  trustedProxy?: string;
  /** 自动初始化用过的口令文件（`--password-file` / `RELAY_PERSONAL_PASSWORD_FILE`）。 */
  passwordFile?: string;
}

const DOCTOR_FLAGS = new Set([
  "data-dir",
  "host",
  "port",
  "listen",
  "tls",
  "trusted-proxy",
  "password-file",
]);

/** `doctor` 收 `serve` 的同名参数与环境变量，只看不改；明文监听公网之类的问题报出来而不是拒绝。 */
export function parsePersonalDoctor(
  argv: readonly string[],
  env: Env,
  cwd = process.cwd(),
): PersonalDoctorOptions {
  const { values } = parseFlags(argv, DOCTOR_FLAGS);
  const host = values.get("host") ?? env.RELAY_HOST;
  const port = portOf(values, env);
  const listen = listenOf(
    values.get("listen") ??
      env.RELAY_LISTEN ??
      `${host && isLoopback(host) ? "127.0.0.1" : "0.0.0.0"}:${port}`,
    "--listen",
  );
  const trustedProxy = values.get("trusted-proxy") ?? env.RELAY_TRUSTED_PROXY;
  const passwordFile = passwordFileOf(values, env, cwd);
  return {
    dataDir: dataDirOf(values, env, cwd),
    listen,
    tls: tlsOf(values, env),
    ...(trustedProxy !== undefined ? { trustedProxy } : {}),
    ...(passwordFile && passwordFile !== "-" ? { passwordFile } : {}),
  };
}
