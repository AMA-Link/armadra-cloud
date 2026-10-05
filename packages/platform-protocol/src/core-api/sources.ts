/**
 * core 契约 §33：客户端源表与远程服务 `/api/sources/*`（owner 专用）。凭据只在
 * SecretStore，响应里只有 `hasCredentials`。
 */
import { z } from "zod";

import { hex32Schema } from "../tunnel/handshake.ts";

export const clientSourceKindSchema = z.enum([
  "local",
  "direct",
  "relayed",
  "hosted",
]);
export const remoteServiceKindSchema = z.enum(["personal", "saas"]);
export const viaSchema = z.enum(["direct", "relayed"]);

export const clientSourceSchema = z.object({
  sourceId: hex32Schema,
  kind: clientSourceKindSchema,
  label: z.string(),
  baseUrl: z.string(),
  relayOrigin: z.string(),
  fingerprint: z.string(),
  cloudIssuer: z.string(),
  principalHint: z.string(),
  addedAtMs: z.number().int(),
  lastOkAtMs: z.number().int(),
  orderIndex: z.number().int(),
  hasCredentials: z.boolean(),
});

export const remoteServiceSchema = z.object({
  serviceId: z.string(),
  kind: remoteServiceKindSchema,
  issuer: z.string(),
  label: z.string(),
  accountHint: z.string(),
  fingerprint: z.string(),
  addedAtMs: z.number().int(),
  lastOkAtMs: z.number().int(),
  /** 本机是否登记到它。 */
  registered: z.boolean(),
  hasCredentials: z.boolean(),
});

/** 远程服务 `me.sources` 的一行，加上本机是否已挂载。 */
export const remoteSourceSummarySchema = z.object({
  sourceId: hex32Schema,
  name: z.string(),
  kind: z.enum(["desktop", "server", "hosted"]),
  online: z.boolean(),
  lastSeenAtMs: z.number().int().nullable(),
  owner: z.boolean(),
  via: z.enum(["owner", "link", "org"]),
  relayOrigin: z.string(),
  coreVersion: z.string(),
  mounted: z.boolean(),
});

export const sourcesListOutputSchema = z.object({
  sources: z.array(clientSourceSchema),
  remotes: z.array(remoteServiceSchema),
});

export const sourcesAddDirectInputSchema = z.object({
  pairLink: z.string().optional(),
  origin: z.string().optional(),
  code: z.string().optional(),
  fingerprint: z.string().optional(),
  label: z.string().optional(),
});

export const sourcesUpdateInputSchema = z.object({
  sourceId: hex32Schema,
  label: z.string().optional(),
  orderIndex: z.number().int().optional(),
  baseUrl: z.string().optional(),
  relayOrigin: z.string().optional(),
});

export const sourceIdInputSchema = z.object({ sourceId: hex32Schema });

export const sourcesSessionInputSchema = z.object({
  sourceId: hex32Schema,
  via: viaSchema.optional(),
});
export const sourcesSessionOutputSchema = z.object({
  accessToken: z.string(),
  accessExpiresAtMs: z.number().int(),
  httpBase: z.string(),
  wsBase: z.string(),
  via: viaSchema,
  relayToken: z.string().optional(),
  relayTokenExpiresAtMs: z.number().int().optional(),
});

export const remoteAddInputSchema = z.discriminatedUnion("kind", [
  z.object({
    kind: z.literal("personal"),
    issuer: z.string(),
    account: z.string(),
    password: z.string(),
    label: z.string().optional(),
    fingerprint: z.string().optional(),
  }),
  z.object({
    kind: z.literal("saas"),
    issuer: z.string(),
    label: z.string().optional(),
  }),
]);

export const remoteAddOutputSchema = z.object({
  remote: remoteServiceSchema,
  next: z.union([
    z.literal("ready"),
    z.object({
      deviceCode: z.object({
        userCode: z.string(),
        verificationUrl: z.string(),
        expiresAtMs: z.number().int(),
      }),
    }),
  ]),
});

export const serviceIdInputSchema = z.object({ serviceId: z.string() });
export const remoteDevicePollOutputSchema = z.object({
  status: z.enum(["pending", "ready", "denied", "expired"]),
});
export const remoteSourcesOutputSchema = z.object({
  sources: z.array(remoteSourceSummarySchema),
});
export const mountInputSchema = z.object({
  serviceId: z.string(),
  sourceId: hex32Schema,
  label: z.string().optional(),
});
export const remoteSessionOutputSchema = z.object({
  accessToken: z.string(),
  accessExpiresAtMs: z.number().int(),
  issuer: z.string(),
  capabilities: z.array(z.string()),
});

export type ClientSource = z.infer<typeof clientSourceSchema>;
export type RemoteService = z.infer<typeof remoteServiceSchema>;
export type RemoteSourceSummary = z.infer<typeof remoteSourceSummarySchema>;
