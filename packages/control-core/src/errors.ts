/**
 * 控制面服务抛的错误：只带注册表里的码。HTTP 层按码查状态码，`message` 是固定英文短句，
 * 界面按码取 i18n 文案。
 */
import type { ErrorCode } from "@armadra/platform-protocol/errors";

export class ControlError extends Error {
  readonly code: ErrorCode;
  readonly retryAfterMs?: number;
  constructor(code: ErrorCode, options: { retryAfterMs?: number } = {}) {
    super(code);
    this.name = "ControlError";
    this.code = code;
    if (options.retryAfterMs !== undefined)
      this.retryAfterMs = Math.max(0, Math.ceil(options.retryAfterMs));
  }
}
