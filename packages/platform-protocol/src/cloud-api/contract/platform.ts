import { z } from "zod";

import { jwkSetSchema } from "../../assertion/claims.ts";
import { errors } from "../../errors/registry.ts";
import { base, SINCE_0_1 } from "../meta.ts";
import { empty, protocolVersionSchema } from "../schemas.ts";

/** capabilities 只增不删。personal 报 `auth.password`、`auth.device-code`、`links.source-invite`、`me.stream`。 */
export const PLATFORM_CAPABILITIES = [
  "auth.password",
  "auth.passkey",
  "auth.totp",
  "auth.oauth",
  "auth.device-code",
  "orgs",
  "links.source-invite",
  "links.org-join",
  "sources.hosted",
  "billing",
  "me.stream",
] as const;

export const platformModeSchema = z.enum(["saas", "personal"]);

export const platformInfoSchema = z.object({
  mode: platformModeSchema,
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
