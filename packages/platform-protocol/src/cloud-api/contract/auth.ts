import { z } from "zod";

import { errors } from "../../errors/registry.ts";
import {
  base,
  SINCE_0_1,
  SINCE_0_3_10,
  SINCE_0_10,
  SINCE_0_15,
} from "../meta.ts";
import { deviceRequestSchema } from "../saas-schemas.ts";
import {
  challengeTokenSchema,
  deviceInputSchema,
  empty,
  cookieSessionResultSchema,
  mfaChallengeResultSchema,
  sessionResultSchema,
  sessionSchema,
} from "../schemas.ts";

const anonymous = { scope: ["anonymous"], since: SINCE_0_1 } as const;
const session = { scope: ["session"], since: SINCE_0_1 } as const;

const passwordErrors = errors.pick(
  "password_too_short",
  "password_too_long",
  "password_too_common",
  "password_contains_name",
  "password_breached",
);

/** 与 core 契约 §18.2 同形：`options` 是 WebAuthn 的 `…OptionsJSON`，`response` 是 `PublicKeyCredential.toJSON()`。 */
const webauthnJson = z.record(z.string(), z.unknown());

export const passkeySchema = z.object({
  credentialId: z.string(),
  label: z.string(),
  aaguid: z.string(),
  transports: z.array(z.string()),
  createdAtMs: z.number().int(),
});

export const deviceStartOutputSchema = z.object({
  deviceCode: z.string(),
  userCode: z.string().regex(/^[A-Z]{4}-[A-Z]{4}$/),
  verificationUrl: z.url(),
  expiresAtMs: z.number().int(),
  intervalMs: z.number().int().positive(),
});

export const devicePollOutputSchema = z.discriminatedUnion("status", [
  z.object({ status: z.literal("pending") }),
  z.object({ status: z.literal("approved"), session: sessionSchema }),
  z.object({ status: z.literal("denied") }),
  z.object({ status: z.literal("expired") }),
]);

/** 1.4 起的 OAuth 提供方（M2；账号绑定要先验证邮箱）。 */
export const OAUTH_PROVIDERS = ["github", "google"] as const;
export const oauthProviderSchema = z.enum(OAUTH_PROVIDERS);

/** 授权地址：`oauthStart`、`stepUpOAuthStart`、`oauthLinkStart` 共用。 */
export const oauthStartOutputSchema = z.object({
  authorizationUrl: z.url(),
  state: z.string(),
  expiresAtMs: z.number().int(),
});

/** 回调带回的授权结果：`auth.stepUp` 的 `oauth` 分支用。 */
export const oauthStepUpInputSchema = z.object({
  provider: oauthProviderSchema,
  state: z.string(),
  code: z.string(),
});

/** 绑定成功后的一条第三方身份（不含任何令牌）。 */
export const linkedProviderSchema = z.object({
  provider: oauthProviderSchema,
  /** 提供方的登录名（GitHub 的 login）；没有则缺省。 */
  login: z.string().optional(),
  email: z.string(),
  linkedAtMs: z.number().int(),
});

const since15 = { scope: ["session"], since: SINCE_0_15 } as const;

const since10 = (scope: "anonymous" | "session") =>
  ({ scope: [scope], since: SINCE_0_10 }) as const;
const tokenErrors = errors.pick(
  "email_token_invalid",
  "email_token_expired",
  "rate_limited",
);

export const auth = {
  register: base
    .route({ method: "POST", path: "/v1/auth/register" })
    .meta(anonymous)
    .errors({
      ...errors.pick(
        "email_taken",
        "rate_limited",
        "registrations_paused",
        "challenge_required",
        "challenge_invalid",
        "forbidden",
      ),
      ...passwordErrors,
    })
    .input(
      z.object({
        email: z.email(),
        password: z.string(),
        displayName: z.string().min(1).max(128),
        device: deviceInputSchema,
        /** 挑战令牌（cloud-api §16）：`platform.info.challenge.scope` 含 `auth.register` 时必带。 */
        challenge: challengeTokenSchema.optional(),
      }),
    )
    .output(sessionResultSchema),
  login: base
    .route({ method: "POST", path: "/v1/auth/login" })
    .meta(anonymous)
    .errors(
      errors.pick(
        "credentials_invalid",
        "account_locked",
        "account_disabled",
        "rate_limited",
        "challenge_required",
        "challenge_invalid",
        "forbidden",
        "operator_mfa_required",
      ),
    )
    .input(
      z.object({
        /** 邮箱，或 personal 模式的账号名。 */
        account: z.string().min(1),
        password: z.string(),
        device: deviceInputSchema,
        /** 挑战令牌（cloud-api §16，0.3.5 起）：`platform.info.challenge.scope` 含 `auth.login` 时必带。 */
        challenge: challengeTokenSchema.optional(),
        /**
         * 1.4 补充：要一个运营范围的 Cookie 会话（独立的 `__Host-armadra-ops` Cookie，寿命 1 小时，
         * 不覆盖普通会话，cloud-api §1 / §28）。只对开了两步验证的运营账号签出：不是运营账号答
         * `403 forbidden`，是运营账号但没开两步验证答 `403 operator_mfa_required`；走两步验证时
         * 范围随挑战带到 `auth.mfaVerify`。
         */
        scope: z.literal("operator").optional(),
      }),
    )
    .output(z.union([cookieSessionResultSchema, mfaChallengeResultSchema])),
  mfaVerify: base
    .route({ method: "POST", path: "/v1/auth/mfa/verify" })
    .meta(anonymous)
    .errors(errors.pick("mfa_invalid", "mfa_expired", "account_locked"))
    .input(z.object({ challengeId: z.string(), code: z.string() }))
    .output(cookieSessionResultSchema),
  refresh: base
    .route({ method: "POST", path: "/v1/auth/refresh" })
    .meta(anonymous)
    .errors(errors.pick("session_expired", "session_revoked", "forbidden"))
    .input(
      z.object({
        /**
         * 1.4 补充：可省。Cookie 会话（网页控制台）的刷新凭据在 `__Host-armadra-cloud` /
         * `__Host-armadra-ops` Cookie 里，不带；Bearer 客户端（原生、核心）照旧必带。
         * personal 中继实际上仍要求带：缺了答 `session_revoked`（cloud-api §1 / §2）。
         */
        refreshToken: z.string().optional(),
        /**
         * 1.4 补充：不带作用于普通控制台会话（`__Host-armadra-cloud`），带 `"operator"`
         * 作用于运营会话（`__Host-armadra-ops`）；各自要对应的 CSRF 值。personal 答 `403 forbidden`。
         */
        scope: z.literal("operator").optional(),
      }),
    )
    .output(cookieSessionResultSchema),
  /**
   * 在已有会话上再验证（1.4 补充，cloud-api §2）：口令、两步验证码、通行密钥断言至少给一项；
   * 通过后这个会话在 `elevatedUntilMs`（10 分钟）前算「已提权」，敏感操作（注销账号、转移所有权）据此放行。
   * 0.15.0 起可带 `oauth`：用已绑定的提供方再走一次授权（先 `auth.stepUpOAuthStart`），换出的
   * `(provider, subject)` 必须是本账号已绑定的身份，state 的目的、账号、设备与浏览器随机数都要对得上，
   * 否则 `401 credentials_invalid`（cloud-api §29）。
   */
  stepUp: base
    .route({ method: "POST", path: "/v1/auth/step-up" })
    .meta(session)
    .errors(
      errors.pick(
        "credentials_invalid",
        "mfa_invalid",
        "rate_limited",
        "bad_request",
      ),
    )
    .input(
      z
        .object({
          password: z.string().optional(),
          totpCode: z.string().optional(),
          passkeyAssertion: webauthnJson.optional(),
          oauth: oauthStepUpInputSchema.optional(),
        })
        .refine(
          (value) =>
            value.password !== undefined ||
            value.totpCode !== undefined ||
            value.passkeyAssertion !== undefined ||
            value.oauth !== undefined,
          {
            message: "password、totpCode、passkeyAssertion、oauth 至少给一项",
          },
        ),
    )
    .output(z.object({ elevatedUntilMs: z.number().int() })),
  /**
   * 提权用的授权地址（0.15.0 起，cloud-api §29）：`redirectUri` 规则同 `auth.oauthStart`；
   * state 的目的是 `reauth`，绑定账号、设备与浏览器随机数。能力 `auth.step-up-oauth`。
   */
  stepUpOAuthStart: base
    .route({ method: "POST", path: "/v1/auth/step-up/oauth/{provider}/start" })
    .meta(since15)
    .errors(errors.pick("rate_limited", "bad_request", "not_found"))
    .input(
      z.object({
        provider: oauthProviderSchema,
        redirectUri: z.url().max(2048),
      }),
    )
    .output(oauthStartOutputSchema),
  logout: base
    .route({ method: "POST", path: "/v1/auth/logout" })
    .meta(session)
    .errors(errors.pick("forbidden"))
    .input(
      z.object({
        /** 1.4 补充：同 `auth.refresh` 的 `scope`——不带登出普通控制台会话，带 `"operator"` 登出运营会话。 */
        scope: z.literal("operator").optional(),
      }),
    )
    .output(empty),
  deviceStart: base
    .route({ method: "POST", path: "/v1/auth/device/start" })
    .meta(anonymous)
    .errors(errors.pick("rate_limited"))
    .input(z.object({ device: deviceInputSchema }))
    .output(deviceStartOutputSchema),
  devicePoll: base
    .route({ method: "POST", path: "/v1/auth/device/poll" })
    .meta(anonymous)
    .errors(errors.pick("rate_limited"))
    .input(z.object({ deviceCode: z.string() }))
    .output(devicePollOutputSchema),
  deviceApprove: base
    .route({ method: "POST", path: "/v1/auth/device/approve" })
    .meta(session)
    .errors(errors.pick("device_code_expired", "not_found"))
    .input(z.object({ userCode: z.string(), approve: z.boolean() }))
    .output(empty),
  /** 设备授权页要显示发起设备的平台与名字（设计 §11.5），再决定批准还是拒绝。 */
  deviceGet: base
    .route({ method: "GET", path: "/v1/auth/device" })
    .meta(since10("session"))
    .errors(errors.pick("device_code_expired", "not_found", "rate_limited"))
    .input(z.object({ userCode: z.string() }))
    .output(deviceRequestSchema),
  /** 拒绝后发起设备的 `devicePoll` 答 `denied`。 */
  deviceDeny: base
    .route({ method: "POST", path: "/v1/auth/device/deny" })
    .meta(since10("session"))
    .errors(errors.pick("device_code_expired", "not_found"))
    .input(z.object({ userCode: z.string() }))
    .output(empty),
  passkeyRegisterOptions: base
    .route({ method: "POST", path: "/v1/auth/passkey/register/options" })
    .meta(session)
    .input(z.object({ label: z.string().max(64).optional() }))
    .output(z.object({ challengeId: z.string(), options: webauthnJson })),
  passkeyRegister: base
    .route({ method: "POST", path: "/v1/auth/passkey/register" })
    .meta(session)
    .input(
      z.object({
        challengeId: z.string(),
        response: webauthnJson,
        label: z.string().max(64).optional(),
      }),
    )
    .output(passkeySchema),
  passkeyLoginOptions: base
    .route({ method: "POST", path: "/v1/auth/passkey/login/options" })
    .meta(anonymous)
    .errors(errors.pick("rate_limited"))
    .input(empty)
    .output(z.object({ challengeId: z.string(), options: webauthnJson })),
  passkeyLogin: base
    .route({ method: "POST", path: "/v1/auth/passkey/login" })
    .meta(anonymous)
    .errors(errors.pick("credentials_invalid", "rate_limited"))
    .input(
      z.object({
        challengeId: z.string(),
        response: webauthnJson,
        device: deviceInputSchema,
      }),
    )
    .output(sessionResultSchema),
  totpEnroll: base
    .route({ method: "POST", path: "/v1/auth/totp/enroll" })
    .meta(session)
    .errors(errors.pick("conflict"))
    .input(empty)
    .output(z.object({ secret: z.string(), otpauthUri: z.string() })),
  totpConfirm: base
    .route({ method: "POST", path: "/v1/auth/totp/confirm" })
    .meta(session)
    .errors(errors.pick("mfa_invalid"))
    .input(z.object({ code: z.string() }))
    .output(z.object({ recoveryCodes: z.array(z.string()) })),
  totpDisable: base
    .route({ method: "POST", path: "/v1/auth/totp/disable" })
    .meta(session)
    .errors(errors.pick("mfa_invalid"))
    .input(z.object({ code: z.string() }))
    .output(z.object({ disabled: z.literal(true) })),
  /**
   * 改口令（cloud-api §18，0.3.10 起）：要有效会话与旧口令；新口令过同一套策略。成功后撤销账号
   * 本人除当前设备以外的全部设备（访客不受影响），当前设备的刷新令牌也换新，答当前设备的新会话。
   */
  changePassword: base
    .route({ method: "POST", path: "/v1/auth/password" })
    .meta({ scope: ["session"], since: SINCE_0_3_10 })
    .errors({
      ...errors.pick(
        "credentials_invalid",
        "account_locked",
        "rate_limited",
        "forbidden",
        "challenge_required",
        "challenge_invalid",
      ),
      ...passwordErrors,
    })
    .input(
      z.object({
        password: z.string(),
        newPassword: z.string(),
        /** 挑战令牌：`platform.info.challenge.scope` 含 `auth.changePassword` 时必带。 */
        challenge: challengeTokenSchema.optional(),
      }),
    )
    .output(sessionResultSchema),
  recoveryCodesRotate: base
    .route({ method: "POST", path: "/v1/auth/recovery-codes/rotate" })
    .meta(session)
    .errors(errors.pick("mfa_invalid"))
    .input(z.object({ code: z.string() }))
    .output(z.object({ recoveryCodes: z.array(z.string()) })),
  /**
   * 邮箱验证与改邮箱确认（cloud-api §29，1.4 起）：邮件里的一次性令牌；未验证的账号可以登录，
   * 但不能登记主机、签链接、被授予。令牌 24 小时有效、只能用一次。
   */
  emailVerify: base
    .route({ method: "POST", path: "/v1/auth/email/verify" })
    .meta(since10("anonymous"))
    .errors(tokenErrors)
    .input(z.object({ token: z.string().min(1) }))
    .output(z.object({ email: z.string(), emailVerified: z.literal(true) })),
  /** 重发验证邮件（按账号限流）。 */
  emailVerifyResend: base
    .route({ method: "POST", path: "/v1/auth/email/resend" })
    .meta(since10("session"))
    .errors(errors.pick("rate_limited", "conflict"))
    .input(empty)
    .output(empty),
  /** 总是答成功（不泄露邮箱是否注册）；邮件里的链接 30 分钟有效。 */
  passwordResetStart: base
    .route({ method: "POST", path: "/v1/auth/password-reset/start" })
    .meta(since10("anonymous"))
    .errors(
      errors.pick("rate_limited", "challenge_required", "challenge_invalid"),
    )
    .input(
      z.object({
        email: z.email(),
        /** 挑战令牌（cloud-api §16）：`platform.info.challenge.scope` 含 `auth.passwordResetStart` 时必带。 */
        challenge: challengeTokenSchema.optional(),
      }),
    )
    .output(empty),
  /** 成功后撤销该账号全部设备，要重新登录（不答会话）。 */
  passwordResetFinish: base
    .route({ method: "POST", path: "/v1/auth/password-reset/finish" })
    .meta(since10("anonymous"))
    .errors({ ...tokenErrors, ...passwordErrors })
    .input(z.object({ token: z.string().min(1), newPassword: z.string() }))
    .output(empty),
  /**
   * 第三方登录（cloud-api §29）：答授权地址，页面或系统浏览器跳过去，回来后 `oauthFinish`。
   * `redirectUri` 必须严格等于控制台来源 + `/login/oauth/<provider>`，否则 `400 bad_request`；
   * 没配的提供方答 `404 not_found`。Cookie 传输（带控制台 `Origin` 且不带 `Authorization`）时
   * 另下发短命 Cookie `__Host-armadra-oauth`（浏览器绑定随机数），Bearer 传输不发。
   */
  oauthStart: base
    .route({ method: "POST", path: "/v1/auth/oauth/{provider}/start" })
    .meta(since10("anonymous"))
    .errors(errors.pick("rate_limited", "bad_request", "not_found"))
    .input(
      z.object({
        provider: oauthProviderSchema,
        redirectUri: z.url().max(2048),
      }),
    )
    .output(oauthStartOutputSchema),
  /**
   * 用回调的 `code` 换会话（cloud-api §29）：`state` 一次性、10 分钟；Cookie 传输时 `__Host-armadra-oauth` 要与
   * 发起时的一致。已绑定则登录；未绑定且本地没有同邮箱账号则建账号（邮箱已验证、无口令）；本地有同邮箱账号时，
   * 服务端关了口令登录（`auth.password` 不在能力里）且那个账号没有任何第三方身份、也不是运营账号时才绑定并登录（0.15.0 起收紧），
   * 否则 `409 conflict`（`details.reason = "account_exists"`），用户用原有方式登录后在设置里显式绑定。
   * 提供方给的邮箱未验证答 `403 email_unverified`。Cookie 会话多带 `csrfToken`。
   */
  oauthFinish: base
    .route({ method: "POST", path: "/v1/auth/oauth/{provider}/finish" })
    .meta(since10("anonymous"))
    .errors(
      errors.pick(
        "credentials_invalid",
        "email_unverified",
        "account_disabled",
        "rate_limited",
        "conflict",
        "registrations_paused",
      ),
    )
    .input(
      z.object({
        provider: oauthProviderSchema,
        state: z.string(),
        code: z.string(),
        device: deviceInputSchema,
      }),
    )
    .output(z.union([cookieSessionResultSchema, mfaChallengeResultSchema])),
  /**
   * 绑定第三方登录的授权地址（0.15.0 起，cloud-api §29）：会话必须已提权，否则 `401 mfa_required`；
   * 按账号限流（每小时 10 次）。state 的目的是 `link`，绑定账号、设备与浏览器随机数。能力 `auth.oauth-link`。
   */
  oauthLinkStart: base
    .route({ method: "POST", path: "/v1/auth/oauth/{provider}/link/start" })
    .meta(since15)
    .errors(
      errors.pick("mfa_required", "rate_limited", "bad_request", "not_found"),
    )
    .input(
      z.object({
        provider: oauthProviderSchema,
        redirectUri: z.url().max(2048),
      }),
    )
    .output(oauthStartOutputSchema),
  /**
   * 完成绑定（0.15.0 起）：提供方邮箱必须已验证（`403 email_unverified`），但不要求等于账号邮箱，
   * 也不改账号邮箱与显示名。身份已属别的账号答 `409 conflict`（`details.reason = "identity_linked"`，不透露是谁），
   * 本账号该提供方已有身份答 `409 conflict`（`details.reason = "provider_linked"`）。
   */
  oauthLinkFinish: base
    .route({ method: "POST", path: "/v1/auth/oauth/{provider}/link/finish" })
    .meta(since15)
    .errors(
      errors.pick(
        "mfa_required",
        "credentials_invalid",
        "email_unverified",
        "conflict",
        "rate_limited",
      ),
    )
    .input(
      z.object({
        provider: oauthProviderSchema,
        state: z.string(),
        code: z.string(),
      }),
    )
    .output(linkedProviderSchema),
  /**
   * 解绑第三方登录；会话必须已提权（0.15.0 起，否则 `401 mfa_required`）。「至少留一种登录方式」只算此刻能用的：
   * 服务端已配置的提供方，加上已设口令且口令登录开着；运营的口令加 TOTP 只进运营会话，不算。
   */
  oauthUnlink: base
    .route({ method: "DELETE", path: "/v1/auth/oauth/{provider}" })
    .meta(since10("session"))
    .errors(errors.pick("not_found", "conflict", "mfa_required"))
    .input(z.object({ provider: oauthProviderSchema }))
    .output(empty),
};
