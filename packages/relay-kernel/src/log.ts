/**
 * 中继日志（中继设计 §1、平台设计 §8.3）：**只记元数据**——时间、源、隧道、流、字节数、
 * 关闭码、结果。不记正文、不记路径与 query（query 里可能有票据）、不记任何头、不记令牌。
 *
 * 做法是白名单：字段名在 {@link RelayLogFields} 里才写出，其余在运行时丢掉；
 * 调用方就算把整个对象塞进来，也只剩白名单里的标量。
 */
import type { LogFields, Logger } from "@armadra/cloud-shared/log";

export interface RelayLogFields {
  sourceId?: string;
  tunnelId?: string;
  streamId?: number;
  node?: string;
  kind?: "http" | "ws" | "e2e";
  result?: string;
  code?: number | string;
  reason?: string;
  bytesIn?: number;
  bytesOut?: number;
  durationMs?: number;
  tunnels?: number;
  streams?: number;
  mode?: string;
  url?: string;
  listen?: string;
  error?: string;
}

const ALLOWED: ReadonlySet<string> = new Set<keyof RelayLogFields>([
  "sourceId",
  "tunnelId",
  "streamId",
  "node",
  "kind",
  "result",
  "code",
  "reason",
  "bytesIn",
  "bytesOut",
  "durationMs",
  "tunnels",
  "streams",
  "mode",
  "url",
  "listen",
  "error",
]);

export interface RelayLog {
  debug(message: string, fields?: RelayLogFields): void;
  info(message: string, fields?: RelayLogFields): void;
  warn(message: string, fields?: RelayLogFields): void;
  error(message: string, fields?: RelayLogFields): void;
}

function pick(fields: RelayLogFields | undefined): LogFields | undefined {
  if (!fields) return undefined;
  const out: LogFields = {};
  for (const [key, value] of Object.entries(fields)) {
    if (!ALLOWED.has(key)) continue;
    if (
      typeof value === "string" ||
      typeof value === "number" ||
      typeof value === "boolean"
    )
      out[key] = value;
  }
  return out;
}

export function relayLog(base: Logger): RelayLog {
  return {
    debug: (message, fields) => base.debug(message, pick(fields)),
    info: (message, fields) => base.info(message, pick(fields)),
    warn: (message, fields) => base.warn(message, pick(fields)),
    error: (message, fields) => base.error(message, pick(fields)),
  };
}

/** 错误只记类名（`Error.name`），不记 message：message 里可能带着对端给的内容。 */
export function errorName(error: unknown): string {
  return error instanceof Error ? error.name : typeof error;
}
