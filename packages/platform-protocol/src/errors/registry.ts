/**
 * 错误码注册表（协议包设计 §7）：snake_case，两仓都从这里取，不允许本地字面量。
 * 线上形状永远 `{ code, message, requestId?, details? }`；HTTP 状态码查本表。
 * 只增不删：删码或改状态码是 major 变更。
 */
export const ERRORS = {
  // 通用
  bad_request: { status: 400 },
  unauthenticated: { status: 401 },
  forbidden: { status: 403 },
  not_found: { status: 404 },
  conflict: { status: 409 },
  rate_limited: { status: 429 },
  internal: { status: 500 },
  unavailable: { status: 503 },
  /** 预留的接口（SaaS 服务端，总计划 §12.2）：路由在、实现未落地。 */
  not_implemented: { status: 501 },
  // 账号
  email_taken: { status: 409 },
  credentials_invalid: { status: 401 },
  account_locked: { status: 429 },
  account_disabled: { status: 403 },
  email_unverified: { status: 403 },
  session_expired: { status: 401 },
  session_revoked: { status: 401 },
  mfa_required: { status: 401 },
  mfa_invalid: { status: 401 },
  mfa_expired: { status: 410 },
  // 口令策略：与 core 的策略码同名（core 契约 §18.1）
  password_too_short: { status: 400 },
  password_too_long: { status: 400 },
  password_too_common: { status: 400 },
  password_contains_name: { status: 400 },
  password_breached: { status: 400 },
  device_code_pending: { status: 428 },
  device_code_expired: { status: 410 },
  device_code_denied: { status: 403 },
  // 源
  registration_token_invalid: { status: 401 },
  source_taken: { status: 409 },
  source_revoked: { status: 410 },
  source_offline: { status: 503 },
  source_access_denied: { status: 403 },
  protocol_unsupported: { status: 426 },
  // 链接 / 组织
  link_invalid: { status: 404 },
  link_expired: { status: 410 },
  link_exhausted: { status: 410 },
  link_secret_invalid: { status: 403 },
  org_role_insufficient: { status: 403 },
  org_slug_taken: { status: 409 },
  // 中继
  relay_token_invalid: { status: 401 },
  relay_source_mismatch: { status: 403 },
  limit_reached: { status: 429 },
  // core §31–§33（core 自己答；登记在这里是为了两仓用同一份表）
  cloud_not_registered: { status: 401 },
  cloud_account_unlinked: { status: 401 },
  cloud_assertion_invalid: { status: 401 },
  cloud_assertion_replayed: { status: 401 },
  cloud_already_registered: { status: 409 },
  cloud_issuer_mismatch: { status: 400 },
  invitation_invalid: { status: 401 },
  source_unreachable: { status: 502 },
  source_unauthorized: { status: 401 },
  fingerprint_mismatch: { status: 400 },
} as const satisfies Record<string, { status: number }>;

export type ErrorCode = keyof typeof ERRORS;

export const ERROR_CODES = Object.keys(ERRORS) as ErrorCode[];

export function isErrorCode(value: unknown): value is ErrorCode {
  return typeof value === "string" && Object.hasOwn(ERRORS, value);
}

export function errorStatus(code: ErrorCode): number {
  return ERRORS[code].status;
}

/** 给 `oc.errors()` 用：挑出一组码，各带自己的状态码。 */
export const errors = {
  pick<const C extends ErrorCode>(
    ...codes: C[]
  ): { [K in C]: { status: (typeof ERRORS)[K]["status"] } } {
    const picked = {} as Record<string, { status: number }>;
    for (const code of codes) {
      if (!isErrorCode(code))
        throw new Error(`未登记的错误码：${String(code)}`);
      picked[code] = { status: ERRORS[code].status };
    }
    return picked as { [K in C]: { status: (typeof ERRORS)[K]["status"] } };
  },
};
