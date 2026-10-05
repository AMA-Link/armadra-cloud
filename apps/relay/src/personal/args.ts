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

// ------------------------------------------------------------ init

export interface PersonalInitOptions extends Common {
  account: string;
  /** 口令文件；`-` 是标准输入；不给时在终端上交互读。 */
  passwordFile?: string;
  host: string;
  port: number;
  tls: TlsMode;
  publicOrigin?: string;
}

const INIT_FLAGS = new Set([
  "data-dir",
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
  return {
    dataDir: dataDirOf(values, env, cwd),
    account,
    ...(passwordFile ? { passwordFile } : {}),
    host,
    port: portOf(values, env),
    tls: tlsOf(values, env),
    ...(publicText ? { publicOrigin: originOf(publicText) } : {}),
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
}

const SERVE_FLAGS = new Set([
  "data-dir",
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
  return {
    dataDir,
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
