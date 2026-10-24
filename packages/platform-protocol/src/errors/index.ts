import { z } from "zod";

import { ERROR_CODES, type ErrorCode } from "./registry.ts";

export {
  ERROR_CODES,
  ERRORS,
  errors,
  errorStatus,
  isErrorCode,
  type ErrorCode,
} from "./registry.ts";

/** 线上错误形状。`code` 未知时也能解析（新版本服务端加的码），由调用方决定怎么显示。 */
export const wireErrorSchema = z.object({
  code: z.string().min(1),
  message: z.string(),
  requestId: z.string().optional(),
  details: z.unknown().optional(),
});

export type WireError = {
  code: ErrorCode;
  message: string;
  requestId?: string;
  details?: unknown;
};

export const errorCodeSchema = z.enum(
  ERROR_CODES as [ErrorCode, ...ErrorCode[]],
);

/**
 * 配额种类（cloud-api §21）：`limit_reached` 的 `details.quota` 与 `billing.usage` /
 * `teams.usage` 的 `metric` 共用这一张表。`teams` 是个人账号能建的团队数，`hosts` / `members` /
 * `guests` 是团队内的数量，`concurrent` 是同时连接数，`traffic` 是本月中继流量（字节）。
 */
export const QUOTAS = [
  "teams",
  "hosts",
  "members",
  "guests",
  "concurrent",
  "traffic",
] as const;
export type Quota = (typeof QUOTAS)[number];
export const quotaSchema = z.enum(QUOTAS);

/** `limit_reached` 的 `details`：哪个配额、当前用量与上限。 */
export const limitReachedDetailsSchema = z.object({
  quota: quotaSchema,
  used: z.number().nonnegative(),
  limit: z.number().nonnegative(),
});
export type LimitReachedDetails = z.infer<typeof limitReachedDetailsSchema>;

/** `team_hosts_online` 的 `details`：团队还有几台在线主机（0.11.0 起）。 */
export const teamHostsOnlineDetailsSchema = z.object({
  onlineHosts: z.number().int().positive(),
});
export type TeamHostsOnlineDetails = z.infer<
  typeof teamHostsOnlineDetailsSchema
>;
