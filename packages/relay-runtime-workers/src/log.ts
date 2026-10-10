/**
 * Workers 的日志：一行一个 JSON 写到 `console.log`（`observability.logs` 收集）。字段规则与 Node
 * 中继相同：中继内核的白名单（`relayLog`）只留元数据，不记路径、头、令牌、正文。
 */
import {
  createLogger,
  type Logger,
  type LogLevel,
} from "@armadra/cloud-shared/log";
import { relayLog, type RelayLog } from "@armadra/relay-kernel/log";

export function workersLogger(name: string, level: LogLevel): Logger {
  return createLogger({
    name,
    level,
    // eslint-disable-next-line no-console -- Workers 的日志出口就是 console
    write: (line) => console.log(line),
  });
}

export function workersRelayLog(name: string, level: LogLevel): RelayLog {
  return relayLog(workersLogger(name, level));
}
