/**
 * 隧道握手（协议包设计 §3.1）：四条文本帧（JSON），`ready` 之后只允许二进制帧。
 *
 *   1. core → relay  hello      带控制面签的隧道令牌与 nonce
 *   2. relay → core  challenge  relay 已验隧道令牌，回 nonce2 与自己的节点名
 *   3. core → relay  auth       源私钥对 {@link challengeSigningInput} 的 Ed25519 签名
 *   4. relay → core  ready | reject
 *
 * schema 非 strict：更高 minor 加的字段被忽略。签名与验签在各仓用自己的 Ed25519
 * 实现（`node:crypto`），这里只给签名输入。
 */
import { z } from "zod";

import { utf8 } from "../internal/bytes.ts";

export const hex32Schema = z
  .string()
  .regex(/^[0-9a-f]{32}$/, "须为 32 位小写 hex");
/** 16 字节的 base64url（无填充）= 22 个字符。 */
export const nonceSchema = z
  .string()
  .regex(/^[A-Za-z0-9_-]{22}$/, "须为 16 字节 base64url");
/** Ed25519 签名 64 字节的 base64url = 86 个字符。 */
export const signatureSchema = z
  .string()
  .regex(/^[A-Za-z0-9_-]{86}$/, "须为 64 字节 base64url");

export const protocolVersionSchema = z.object({
  major: z.number().int().nonnegative(),
  minor: z.number().int().nonnegative(),
});

export const helloSchema = z.object({
  type: z.literal("hello"),
  protocol: protocolVersionSchema,
  sourceId: hex32Schema,
  coreVersion: z.string().min(1).max(64),
  capabilities: z.array(z.string()),
  tunnelToken: z.string().min(1),
  nonce: nonceSchema,
});

export const challengeSchema = z.object({
  type: z.literal("challenge"),
  nonce2: nonceSchema,
  relayNode: z.string().min(1).max(64),
});

export const authSchema = z.object({
  type: z.literal("auth"),
  sig: signatureSchema,
});

export const tunnelLimitsSchema = z.object({
  maxStreams: z.number().int().positive(),
  streamWindow: z.number().int().positive(),
  tunnelWindow: z.number().int().positive(),
  maxFrameBytes: z.number().int().positive(),
});

export const readySchema = z.object({
  type: z.literal("ready"),
  tunnelId: hex32Schema,
  limits: tunnelLimitsSchema,
  heartbeatMs: z.number().int().positive(),
  protocol: protocolVersionSchema,
});

export const TUNNEL_REJECT_CODES = [
  "protocol_unsupported",
  "tunnel_token_invalid",
  "tunnel_token_expired",
  "signature_invalid",
  "source_revoked",
  "rate_limited",
  /** 同源新隧道替换旧隧道时发给旧的，关闭码 4491。 */
  "replaced",
] as const;

export const tunnelRejectCodeSchema = z.enum(TUNNEL_REJECT_CODES);
export type TunnelRejectCode = z.infer<typeof tunnelRejectCodeSchema>;

export const rejectSchema = z.object({
  type: z.literal("reject"),
  code: tunnelRejectCodeSchema,
  message: z.string(),
});

/** core 发出的握手消息。 */
export const tunnelClientMessageSchema = z.discriminatedUnion("type", [
  helloSchema,
  authSchema,
]);
/** relay 发出的握手消息。 */
export const tunnelServerMessageSchema = z.discriminatedUnion("type", [
  challengeSchema,
  readySchema,
  rejectSchema,
]);

export type Hello = z.infer<typeof helloSchema>;
export type Challenge = z.infer<typeof challengeSchema>;
export type Auth = z.infer<typeof authSchema>;
export type Ready = z.infer<typeof readySchema>;
export type Reject = z.infer<typeof rejectSchema>;
export type TunnelClientMessage = z.infer<typeof tunnelClientMessageSchema>;
export type TunnelServerMessage = z.infer<typeof tunnelServerMessageSchema>;

/** `utf8(sourceId + "\n" + nonce + "\n" + nonce2 + "\n" + relayNode)`：auth 签的就是它。 */
export function challengeSigningInput(input: {
  sourceId: string;
  nonce: string;
  nonce2: string;
  relayNode: string;
}): Uint8Array {
  return utf8(
    `${input.sourceId}\n${input.nonce}\n${input.nonce2}\n${input.relayNode}`,
  );
}
