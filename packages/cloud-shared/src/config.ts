/**
 * 环境变量解析：每个 app 用 zod 描述自己的变量表，这里只负责解析、汇总错误与
 * 退出码。配置错误一律以 64（EX_USAGE）退出，消息里只出现变量名，不回显取值
 * （取值里可能有口令与连接串）。
 */
import { z } from "zod";

export const CONFIG_EXIT_CODE = 64;

export class ConfigError extends Error {
  readonly issues: readonly string[];
  constructor(issues: readonly string[]) {
    super(`配置无效：\n${issues.map((issue) => `  - ${issue}`).join("\n")}`);
    this.name = "ConfigError";
    this.issues = issues;
  }
}

export type Env = Readonly<Record<string, string | undefined>>;

/** 把空字符串当作没设：compose 里 `FOO=` 与不写效果相同。 */
function withoutBlanks(env: Env): Record<string, string> {
  const result: Record<string, string> = {};
  for (const [key, value] of Object.entries(env)) {
    if (value !== undefined && value.trim() !== "") result[key] = value;
  }
  return result;
}

export function parseConfig<T extends z.ZodType>(
  schema: T,
  env: Env,
): z.infer<T> {
  const result = schema.safeParse(withoutBlanks(env));
  if (result.success) return result.data;
  const issues = result.error.issues.map((issue) => {
    const key = issue.path.join(".") || "(root)";
    return `${key}：${issue.message}`;
  });
  throw new ConfigError(issues);
}

export interface ListenAddress {
  host: string;
  port: number;
}

/** `host:port`，IPv6 写成 `[::1]:8100`。 */
export function parseListen(value: string): ListenAddress {
  const match = /^(?:\[([^\]]+)\]|([^:]+)):(\d{1,5})$/.exec(value);
  const port = match ? Number(match[3]) : Number.NaN;
  if (!match || !Number.isInteger(port) || port < 0 || port > 65_535) {
    throw new Error(`不是合法的 host:port：${value}`);
  }
  return { host: match[1] ?? match[2] ?? "", port };
}

export const listenSchema = z.string().transform((value, context) => {
  try {
    return parseListen(value);
  } catch (error) {
    context.addIssue({ code: "custom", message: (error as Error).message });
    return z.NEVER;
  }
});

export const urlSchema = z.url();

export const logLevelSchema = z
  .enum(["debug", "info", "warn", "error"])
  .default("info");

/** `0/1/true/false`，缺省 false。 */
export const flagSchema = z
  .enum(["0", "1", "true", "false"])
  .default("0")
  .transform((value) => value === "1" || value === "true");
