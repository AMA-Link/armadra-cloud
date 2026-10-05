/**
 * core 契约 §31 / §32 里 cloud 侧要知道的形状：`POST /api/identity/cloud/register`
 * 与隧道状态（`cloud.tunnel` 事件、`GET …/cloud/status`）。
 */
import { z } from "zod";

export const TUNNEL_STATES = [
  "disabled",
  "connecting",
  "authenticating",
  "ready",
  "draining",
  "backoff",
] as const;

export const tunnelStatusSchema = z.object({
  state: z.enum(TUNNEL_STATES),
  node: z.string().nullable(),
  since: z.number().nullable(),
  streams: z.number(),
  lastError: z.object({ code: z.string(), message: z.string() }).nullable(),
});

export const cloudRegisterInputSchema = z.object({
  issuer: z.url(),
  registrationToken: z.string(),
  label: z.string().optional(),
});

export const cloudRegisterOutputSchema = z.object({
  issuer: z.string(),
  sourceId: z.string(),
  relayOrigins: z.array(z.string()),
  trustedOrigins: z.array(z.string()),
  tunnel: tunnelStatusSchema,
});

export type TunnelState = (typeof TUNNEL_STATES)[number];
export type TunnelStatus = z.infer<typeof tunnelStatusSchema>;
export type CloudRegisterInput = z.infer<typeof cloudRegisterInputSchema>;
export type CloudRegisterOutput = z.infer<typeof cloudRegisterOutputSchema>;
