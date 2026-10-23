import { z } from "zod";

import { jwkSetSchema } from "../../assertion/claims.ts";
import { errors } from "../../errors/registry.ts";
import { base, SINCE_0_1 } from "../meta.ts";
import {
  challengeProviderSchema,
  challengeScopeSchema,
  empty,
  protocolVersionSchema,
} from "../schemas.ts";

/** capabilities 只增不删。personal 报 `auth.password`、`auth.password-change`、`links.source-invite`、`links.update`、`links.scope`、`me.stream`。 */
export const PLATFORM_CAPABILITIES = [
  "auth.password",
  "auth.passkey",
  "auth.totp",
  "auth.oauth",
  "auth.device-code",
  "orgs",
  "links.source-invite",
  "links.org-join",
  /** `links.update`（改备注，0.2.0 起）。 */
  "links.update",
  /** `links.create` 收 `scope`（工作空间 / 会话 / 只读，0.3.0 起）；不报的中继会丢掉 `scope`。 */
  "links.scope",
  "sources.hosted",
  "billing",
  "me.stream",
  /** 认 Turnstile 挑战令牌（cloud-api §16，0.3.5 起）；要求哪些 procedure 带看 `platform.info.challenge`。 */
  "challenge.turnstile",
  /** `auth.changePassword`（会话 + 旧口令改口令，cloud-api §18，0.3.10 起）。 */
  "auth.password-change",
  /** `me.traffic`（中继流量统计，cloud-api §19，0.5.0 起）。 */
  "me.traffic",
  // 协议 1.4（cloud-api §21 起）：SaaS 控制面实现了才报；personal 都不报。
  "teams",
  "invitations",
  "groups",
  "grants",
  "access-requests",
  "audit",
  "api-tokens",
  "links.team-join",
  "auth.email-verify",
  "auth.password-reset",
  "me.spaces",
  "trust",
  "status",
] as const;

export const platformModeSchema = z.enum(["saas", "personal"]);

/** 服务端名称的上限（与 core 源表 `label` 相同）。 */
export const PLATFORM_NAME_MAX = 128;

/**
 * 服务端要求的挑战（cloud-api §16，0.3.5 起）：`scope` 里的 procedure 必须带 `challenge`
 * 令牌；`siteKey` 不是秘密，页面与手机拿它渲染挑战组件。不报 = 不要求。
 */
export const platformChallengeSchema = z.object({
  provider: challengeProviderSchema,
  siteKey: z.string().min(1).max(256),
  /**
   * 要求挑战的 procedure。已知值：auth.login、links.accept、auth.changePassword、auth.register、
   * auth.passwordResetStart；以后可能再加，客户端解析时忽略不认识的项（cloud-api §16），
   * 所以这里是字符串数组，解析后只留已知项。服务端配置侧仍用严格枚举（`challengeScopeSchema`）。
   */
  scope: z
    .array(
      z
        .string()
        .describe(
          "已知值：auth.login、links.accept、auth.changePassword、auth.register、auth.passwordResetStart",
        ),
    )
    .min(1)
    .overwrite((items) =>
      items.filter((item) => challengeScopeSchema.safeParse(item).success),
    ),
});

export const platformInfoSchema = z.object({
  mode: platformModeSchema,
  /**
   * 服务端名称（0.3.1 起，可选）：客户端添加这个服务时用它作缺省显示名，用户改名后
   * 以本地名为准。personal 取 `--name` / `RELAY_NAME` / 状态文件 `settings.displayName`，
   * 都没有时是 `--host`；saas 预留。旧客户端忽略它，旧中继不报时客户端用地址兜底。
   */
  name: z.string().min(1).max(PLATFORM_NAME_MAX).optional(),
  issuer: z.url(),
  protocol: protocolVersionSchema,
  capabilities: z.array(z.string()),
  relay: z.object({ addressing: z.enum(["subdomain", "path"]) }),
  webApp: z.url().nullable(),
  /** 1.4 补充：是否开放注册（运营方事故开关关闭时为 false，注册页据此显示关闭态）；personal 恒为 true；旧服务端不报，缺省 = 开放。 */
  registrationsOpen: z.boolean().optional(),
  /** 要求挑战时才有（0.3.5 起）；旧客户端忽略它，此时服务端答 `challenge_required`。 */
  challenge: z
    .preprocess((value) => {
      // scope 过滤掉不认识的项后为空：当作服务端没有要求挑战，不让整个 platform.info 解析失败。
      if (typeof value !== "object" || value === null) return value;
      const { scope } = value as { scope?: unknown };
      if (!Array.isArray(scope)) return value;
      const known = scope.filter(
        (item) => challengeScopeSchema.safeParse(item).success,
      );
      return scope.length > 0 && known.length === 0 ? undefined : value;
    }, platformChallengeSchema.optional())
    .optional(),
});

export const platformHealthSchema = z.object({
  ok: z.literal(true),
  version: z.string(),
  mode: platformModeSchema,
});

const anonymous = { scope: ["anonymous"], since: SINCE_0_1 } as const;

export const platform = {
  info: base
    .route({ method: "GET", path: "/.well-known/armadra-platform" })
    .meta(anonymous)
    .input(empty)
    .output(platformInfoSchema),
  jwks: base
    .route({ method: "GET", path: "/.well-known/jwks.json" })
    .meta(anonymous)
    .input(empty)
    .output(jwkSetSchema),
  health: base
    .route({ method: "GET", path: "/health" })
    .meta(anonymous)
    .errors(errors.pick("unavailable"))
    .input(empty)
    .output(platformHealthSchema),
};
