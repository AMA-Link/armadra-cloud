/**
 * core 契约 §31 的其余几条与 §32 的可观察面：撤销登记、状态、绑定、可信来源、
 * 事件流的 `cloud.tunnel`。
 */
import { z } from "zod";

import { ed25519PublicJwkSchema } from "../assertion/claims.ts";
import { hex32Schema } from "../tunnel/handshake.ts";
import { TUNNEL_STATES, tunnelStatusSchema } from "./cloud-register.ts";

/** `DELETE /api/identity/cloud/register`。 */
export const cloudRevokeInputSchema = z.object({ issuer: z.string() });
export const cloudRevokeOutputSchema = z.object({});

/** `GET /api/identity/cloud`。 */
export const cloudStatusOutputSchema = z.object({
  registrations: z.array(
    z.object({
      issuer: z.string(),
      mode: z.enum(["saas", "personal"]),
      label: z.string().optional(),
      jwksFetchedAtMs: z.number().int().nullable(),
      trustedOrigins: z.array(z.string()),
      relayOrigins: z.array(z.string()),
      registeredAtMs: z.number().int(),
      tunnel: tunnelStatusSchema,
    }),
  ),
  sourcePublicKey: ed25519PublicJwkSchema,
  sourceId: hex32Schema,
});

/** `POST /api/identity/cloud/bind`：把断言的 `sub` 映射到当前 principal。 */
export const cloudBindInputSchema = z.object({ assertion: z.string() });
export const cloudBindOutputSchema = z.object({ bound: z.literal(true) });

/** `PUT /api/identity/cloud/{issuer}/trusted-origins`。 */
export const cloudTrustedOriginsInputSchema = z.object({
  issuer: z.string(),
  origins: z.array(z.string()),
});
export const cloudTrustedOriginsOutputSchema = z.object({
  origins: z.array(z.string()),
});

/** §32：事件流事件 `cloud.tunnel`。 */
export const CLOUD_TUNNEL_EVENT = "cloud.tunnel";
export const cloudTunnelEventSchema = z.object({
  issuer: z.string(),
  state: z.enum(TUNNEL_STATES),
});

export type CloudStatusOutput = z.infer<typeof cloudStatusOutputSchema>;
export type CloudTunnelEvent = z.infer<typeof cloudTunnelEventSchema>;
