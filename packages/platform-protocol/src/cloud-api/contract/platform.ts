import { z } from "zod";

import { jwkSetSchema } from "../../assertion/claims.ts";
import { errors } from "../../errors/registry.ts";
import { base, SINCE_0_1 } from "../meta.ts";
import { empty, protocolVersionSchema } from "../schemas.ts";

/** capabilities 只增不删。personal 报 `auth.password`、`links.source-invite`、`links.update`、`links.scope`、`me.stream`。 */
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
] as const;

export const platformModeSchema = z.enum(["saas", "personal"]);

/** 服务端名称的上限（与 core 源表 `label` 相同）。 */
export const PLATFORM_NAME_MAX = 128;

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
