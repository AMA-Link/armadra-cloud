/**
 * `armadra-relay personal serve` 的参数（中继设计 §7、平台设计 §17.8）。参数解析不用库。
 * 命令行优先，其次环境变量（`RELAY_DATA_DIR`、`RELAY_LISTEN`、`RELAY_TLS`），最后缺省值。
 */
import {
  parseListen,
  type Env,
  type ListenAddress,
} from "@armadra/cloud-shared/config";
import { resolve } from "node:path";

export const TLS_MODES = ["self-signed", "acme", "file", "plain"] as const;
export type TlsMode = (typeof TLS_MODES)[number];

/** R2 之前只有 `plain` 可用；其余模式在 R2 落地。 */
export const SUPPORTED_TLS_MODES: readonly TlsMode[] = ["plain"];

export interface PersonalServeOptions {
  dataDir: string;
  /** 对外的 IP 或域名（issuer 与自签证书的 SAN 用它）。 */
  host: string;
  port: number;
  listen: ListenAddress;
  tls: TlsMode;
  logLevel: "debug" | "info" | "warn" | "error";
  /** 指标单独监听（`--metrics-listen` / `RELAY_METRICS_LISTEN`）；缺省不开。 */
  metricsListen?: ListenAddress;
  /** 指标要求的 Bearer 令牌（`RELAY_METRICS_TOKEN`，只走环境变量）。 */
  metricsToken?: string;
}

export class UsageError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "UsageError";
  }
}

export function isLoopback(host: string): boolean {
  return (
    host === "localhost" || host === "::1" || /^127\.\d+\.\d+\.\d+$/.test(host)
  );
}

function flags(argv: readonly string[]): Map<string, string> {
  const values = new Map<string, string>();
  for (let index = 0; index < argv.length; index += 1) {
    const flag = argv[index] ?? "";
    if (!flag.startsWith("--")) throw new UsageError(`多余的参数：${flag}`);
    const value = argv[index + 1];
    if (value === undefined || value.startsWith("--"))
      throw new UsageError(`${flag} 缺少取值`);
    values.set(flag.slice(2), value);
    index += 1;
  }
  return values;
}

const KNOWN = new Set([
  "data-dir",
  "host",
  "port",
  "tls",
  "listen",
  "log-level",
  "metrics-listen",
]);

export function parsePersonalServe(
  argv: readonly string[],
  env: Env,
  cwd = process.cwd(),
): PersonalServeOptions {
  const values = flags(argv);
  for (const key of values.keys()) {
    if (!KNOWN.has(key)) throw new UsageError(`未知选项：--${key}`);
  }
  const dataDir = values.get("data-dir") ?? env.RELAY_DATA_DIR;
  if (!dataDir) throw new UsageError("缺少 --data-dir（或 RELAY_DATA_DIR）");
  const host = values.get("host") ?? env.RELAY_HOST;
  if (!host)
    throw new UsageError("缺少 --host：公网 IP 或域名（或 RELAY_HOST）");
  const portText = values.get("port") ?? "8443";
  const port = Number(portText);
  if (!Number.isInteger(port) || port < 1 || port > 65_535)
    throw new UsageError(`--port 不是端口：${portText}`);
  const tls = (values.get("tls") ?? env.RELAY_TLS ?? "self-signed") as TlsMode;
  if (!TLS_MODES.includes(tls))
    throw new UsageError(`--tls 只能是 ${TLS_MODES.join(" | ")}：${tls}`);
  const listenText =
    values.get("listen") ??
    env.RELAY_LISTEN ??
    `${isLoopback(host) ? "127.0.0.1" : "0.0.0.0"}:${port}`;
  let listen: ListenAddress;
  try {
    listen = parseListen(listenText);
  } catch {
    throw new UsageError(`--listen 不是一个地址：${listenText}`);
  }
  const logLevel = (values.get("log-level") ??
    env.RELAY_LOG_LEVEL ??
    "info") as PersonalServeOptions["logLevel"];
  if (!["debug", "info", "warn", "error"].includes(logLevel))
    throw new UsageError(`--log-level 不合法：${logLevel}`);
  const metricsText = values.get("metrics-listen") ?? env.RELAY_METRICS_LISTEN;
  let metricsListen: ListenAddress | undefined;
  if (metricsText) {
    try {
      metricsListen = parseListen(metricsText);
    } catch {
      throw new UsageError(`--metrics-listen 不是一个地址：${metricsText}`);
    }
  }
  return {
    dataDir: resolve(cwd, dataDir),
    host,
    port,
    listen,
    tls,
    logLevel,
    ...(metricsListen ? { metricsListen } : {}),
    ...(env.RELAY_METRICS_TOKEN
      ? { metricsToken: env.RELAY_METRICS_TOKEN }
      : {}),
  };
}
