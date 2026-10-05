/**
 * compact JWS 的拆与装（协议包设计 §4.1）。不含签名与验签：两仓各用 `node:crypto`
 * 的 Ed25519（`sign(null, signingInput, key)` / `verify(null, signingInput, key, sig)`）；
 * 浏览器侧永远不验签，只拿着用。
 */
import { z } from "zod";

import {
  fromBase64Url,
  fromUtf8,
  toBase64Url,
  utf8,
} from "../internal/bytes.ts";

export type JwsErrorCode = "jws_malformed" | "jws_alg_unsupported";

export class JwsError extends Error {
  readonly code: JwsErrorCode;
  constructor(code: JwsErrorCode, message: string) {
    super(message);
    this.name = "JwsError";
    this.code = code;
  }
}

export const jwsHeaderSchema = z.object({
  alg: z.literal("EdDSA"),
  kid: z.string().min(1),
  typ: z.string().optional(),
});

export type JwsHeader = z.infer<typeof jwsHeaderSchema>;

export interface CompactJws {
  header: JwsHeader;
  payload: Record<string, unknown>;
  /** `utf8(encodedHeader + "." + encodedPayload)`：验签的输入。 */
  signingInput: Uint8Array;
  signature: Uint8Array;
}

const ED25519_SIGNATURE_BYTES = 64;

function decodeJsonObject(
  segment: string,
  what: string,
): Record<string, unknown> {
  let value: unknown;
  try {
    value = JSON.parse(fromUtf8(fromBase64Url(segment)));
  } catch {
    throw new JwsError("jws_malformed", `${what} 不是 base64url 编码的 JSON`);
  }
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new JwsError("jws_malformed", `${what} 不是 JSON 对象`);
  }
  return value as Record<string, unknown>;
}

/** 拆一个 compact JWS；不验签。格式错抛 `jws_malformed`，`alg` 不是 EdDSA 抛 `jws_alg_unsupported`。 */
export function splitCompactJws(token: string): CompactJws {
  const parts = token.split(".");
  if (parts.length !== 3 || parts.some((part) => part.length === 0)) {
    throw new JwsError("jws_malformed", "不是三段式 compact JWS");
  }
  const [encodedHeader, encodedPayload, encodedSignature] = parts as [
    string,
    string,
    string,
  ];
  const rawHeader = decodeJsonObject(encodedHeader, "头");
  if (typeof rawHeader.alg !== "string")
    throw new JwsError("jws_malformed", "头缺 alg");
  if (rawHeader.alg !== "EdDSA") {
    throw new JwsError("jws_alg_unsupported", "只接受 alg = EdDSA");
  }
  const header = jwsHeaderSchema.safeParse(rawHeader);
  if (!header.success)
    throw new JwsError("jws_malformed", "头缺 kid 或字段类型不对");
  const payload = decodeJsonObject(encodedPayload, "载荷");
  let signature: Uint8Array;
  try {
    signature = fromBase64Url(encodedSignature);
  } catch {
    throw new JwsError("jws_malformed", "签名不是 base64url");
  }
  if (signature.length !== ED25519_SIGNATURE_BYTES) {
    throw new JwsError("jws_malformed", "Ed25519 签名应为 64 字节");
  }
  return {
    header: header.data,
    payload,
    signingInput: utf8(`${encodedHeader}.${encodedPayload}`),
    signature,
  };
}

/** 序列化头与载荷（`JSON.stringify` 的键序即调用方给的键序）。 */
export function buildSigningInput(
  header: object,
  payload: object,
): { signingInput: Uint8Array; encodedHeader: string; encodedPayload: string } {
  const encodedHeader = toBase64Url(utf8(JSON.stringify(header)));
  const encodedPayload = toBase64Url(utf8(JSON.stringify(payload)));
  return {
    signingInput: utf8(`${encodedHeader}.${encodedPayload}`),
    encodedHeader,
    encodedPayload,
  };
}

export function joinCompactJws(
  encodedHeader: string,
  encodedPayload: string,
  signature: Uint8Array,
): string {
  return `${encodedHeader}.${encodedPayload}.${toBase64Url(signature)}`;
}
