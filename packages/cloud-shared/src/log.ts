/**
 * 结构化日志：一行一个 JSON。调用方只能传显式字段——不提供「把请求整个记下来」
 * 的入口；正文、令牌、口令、终端输出永远不进日志（AGENTS.md）。
 */
export type LogLevel = "debug" | "info" | "warn" | "error";

const ORDER: Record<LogLevel, number> = {
  debug: 10,
  info: 20,
  warn: 30,
  error: 40,
};

export type LogFields = Record<
  string,
  string | number | boolean | null | undefined
>;

export interface Logger {
  debug(message: string, fields?: LogFields): void;
  info(message: string, fields?: LogFields): void;
  warn(message: string, fields?: LogFields): void;
  error(message: string, fields?: LogFields): void;
}

export interface LoggerOptions {
  name: string;
  level?: LogLevel;
  write?: (line: string) => void;
  now?: () => number;
}

export function createLogger(options: LoggerOptions): Logger {
  const threshold = ORDER[options.level ?? "info"];
  const write =
    options.write ?? ((line: string) => process.stdout.write(`${line}\n`));
  const now = options.now ?? Date.now;
  const emit = (level: LogLevel, message: string, fields?: LogFields) => {
    if (ORDER[level] < threshold) return;
    const record: Record<string, unknown> = {
      time: new Date(now()).toISOString(),
      level,
      name: options.name,
      msg: message,
    };
    for (const [key, value] of Object.entries(fields ?? {})) {
      if (value !== undefined && !(key in record)) record[key] = value;
    }
    write(JSON.stringify(record));
  };
  return {
    debug: (message, fields) => emit("debug", message, fields),
    info: (message, fields) => emit("info", message, fields),
    warn: (message, fields) => emit("warn", message, fields),
    error: (message, fields) => emit("error", message, fields),
  };
}
