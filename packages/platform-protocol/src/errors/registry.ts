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
  /** 从未发出（`orgs.*` 在 1.4 删除）；码只增不删，保留以防老客户端引用。 */
  org_role_insufficient: { status: 403 },
  org_slug_taken: { status: 409 },
  // 团队、邀请、转移、账号补充（cloud-api §21 起，1.4）
  /** 调用者在团队里的角色不够。 */
  team_role_insufficient: { status: 403 },
  /** 账号注销前要先转移名下团队的所有权（`me.delete`）。 */
  team_owner_required: { status: 409 },
  team_slug_taken: { status: 409 },
  /** `teams.delete` 时团队还有在线主机（0.11.0 起，只加不改）；`details.onlineHosts` 是在线主机数。 */
  team_hosts_online: { status: 409 },
  /** 团队设置不允许这个操作（成员登记主机、guest、匿名链接…）；`details.setting` 写是哪一项。 */
  team_policy_denied: { status: 403 },
  /** 团队被运营方停用：拒绝签断言与登记。 */
  team_suspended: { status: 403 },
  invitation_expired: { status: 410 },
  invitation_revoked: { status: 410 },
  /** 接受邀请的账号邮箱与被邀请的不一致。 */
  invitation_email_mismatch: { status: 403 },
  /** 这台主机已有一个未结束的转移。 */
  transfer_pending: { status: 409 },
  transfer_expired: { status: 410 },
  /** 邮件里的一次性令牌（验证邮箱、找回口令、改邮箱）无效或已用过。 */
  email_token_invalid: { status: 400 },
  email_token_expired: { status: 410 },
  /** 支付服务回调的签名对不上。 */
  webhook_signature_invalid: { status: 401 },
  // 控制台补充（cloud-api §31 起，1.4）
  /** 最后一个 owner 不能离开、降级或被移除，要先转移所有权。 */
  team_last_owner: { status: 409 },
  /** 团队在删除后的 30 天恢复期内，只有 owner 能 `teams.restore`。 */
  team_deleted: { status: 410 },
  member_exists: { status: 409 },
  /** 团队设置不允许 member 登记主机（`team_policy_denied` 的具体形式）。 */
  host_registration_disabled: { status: 403 },
  /** 团队设置不允许 guest。 */
  guests_disabled: { status: 403 },
  /** 团队设置不允许匿名接受分享链接（要先登录云账号）。 */
  anonymous_links_disabled: { status: 403 },
  /** 当前计划不含这项功能（与按数量计的 `limit_reached` 区分）。 */
  plan_feature_unavailable: { status: 403 },
  /** 背书要在已受信的设备上做。 */
  device_untrusted: { status: 403 },
  /** 链接被运营方冻结。 */
  link_frozen: { status: 403 },
  // 控制台缺口（cloud-api §2，1.4 补充）
  /** 运营方的事故开关关闭了注册（`platform.info.registrationsOpen = false`）。 */
  registrations_paused: { status: 503 },
  /** 运营账号没开两步验证，不能签出运营范围的会话（`auth.login` 的 `scope: "operator"`）。 */
  operator_mfa_required: { status: 403 },
  // 中继
  relay_token_invalid: { status: 401 },
  relay_source_mismatch: { status: 403 },
  limit_reached: { status: 429 },
  /** 源的 HTTP 响应不合法（状态行、头、定界或分块错），中继重置流并答 502（0.3.4 起）。 */
  bad_gateway: { status: 502 },
  /** 中继的存储打不开、认不出或迁移账本对不上：拒绝服务，绝不自动重建（0.3.5 起）。 */
  storage_unavailable: { status: 503 },
  /** 中继还没初始化（Workers 个人中转缺账号 / 口令 secret）（0.3.5 起）。 */
  not_initialized: { status: 503 },
  // 挑战（cloud-api §16，0.3.5 起）
  /** 服务端要求挑战令牌（`platform.info.challenge`）而请求没带。 */
  challenge_required: { status: 400 },
  /** 挑战令牌无效、过期或已用过。 */
  challenge_invalid: { status: 400 },
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
