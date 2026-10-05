/**
 * core 契约 §31 里 cloud 侧要知道的那一条：`POST /api/identity/cloud/login`。
 * 访客或账号持源访问断言换 core 自己的原生会话。Armadra 的
 * `packages/shared/src/contract/cloud.ts` 从这里 import 同一份 schema。
 */
import { z } from "zod";

/** 与 core 契约 §3 / §10 的原生会话同形（页面 `identitySessionSchema`）。 */
export const nativeSessionSchema = z.object({
  hostId: z.string(),
  device: z.object({
    deviceId: z.string(),
    principalId: z.string().default(""),
    displayName: z.string().default(""),
    role: z.string().default("owner"),
    createdAtUnixMs: z.number().default(0),
    revision: z.number().default(0),
  }),
  scopes: z
    .array(
      z.object({
        permission: z.string(),
        workspaceId: z.string().default(""),
        executionHostId: z.string().default(""),
      }),
    )
    .default([]),
  expiresAtUnixMs: z.number().default(0),
  csrfToken: z.string().optional(),
  native: z
    .object({ accessToken: z.string(), refreshToken: z.string() })
    .optional(),
});

export const cloudLoginInputSchema = z.object({
  assertion: z.string(),
  invitationToken: z.string().optional(),
});

export const cloudLoginOutputSchema = z.object({
  session: nativeSessionSchema,
  principal: z.object({
    principalId: z.string(),
    kind: z.enum(["owner", "member"]),
    displayName: z.string(),
  }),
  created: z.boolean(),
});

export type NativeSession = z.infer<typeof nativeSessionSchema>;
export type CloudLoginInput = z.infer<typeof cloudLoginInputSchema>;
export type CloudLoginOutput = z.infer<typeof cloudLoginOutputSchema>;
