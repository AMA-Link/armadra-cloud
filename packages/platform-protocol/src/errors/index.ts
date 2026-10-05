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
