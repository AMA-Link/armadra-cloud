/**
 * 人机挑战（cloud-api §16，中继设计 §4.3）：登录（与可选的接受链接）之前先过 Cloudflare Turnstile。
 *
 * - 只有装配方给了 {@link ChallengeGate}（配置了 siteKey **与** secret）才要求；不给 = 不要求、
 *   `platform.info` 不报 `challenge`。
 * - 校验经端口 {@link ChallengeVerifier} 注入：控制核不发 HTTP，Node 中继与 Workers 中继各给一个
 *   调 siteverify 的实现（请求体与答复的解读在这里，两边共用）。
 * - 次序（由调用方保证）：按 IP 限流先计 → 挑战 → 口令 / 链接秘密；挑战失败不计入登录失败次数。
 * - 令牌一次性：同一个令牌在 300 秒内再出现，不问提供方直接答 `challenge_invalid`（提供方自己
 *   也拒绝重放，这里只是省一次往返）。只记令牌的哈希，不记明文。
 * - 提供方没答、答了 5xx 或答不成 JSON：同一个幂等键重试一次，仍不行答 `challenge_invalid`
 *   （客户端换一个新令牌重来）。
 */
import { createHash, randomUUID } from "node:crypto";

import type {
  ChallengeToken,
  challengeScopeSchema,
  platformChallengeSchema,
} from "@armadra/platform-protocol/cloud-api";
import { z } from "zod";

import { ControlError } from "./errors.ts";

export type ChallengeScope = z.infer<typeof challengeScopeSchema>;
export type PlatformChallenge = z.infer<typeof platformChallengeSchema>;

/** Turnstile 的服务端校验地址。 */
export const TURNSTILE_SITEVERIFY_URL =
  "https://challenges.cloudflare.com/turnstile/v0/siteverify";

/** 令牌有效期（Turnstile：300 秒），也是本地防重放表的保留时长。 */
export const CHALLENGE_TOKEN_TTL_MS = 300_000;

/** 校验请求的超时。 */
export const SITEVERIFY_TIMEOUT_MS = 5_000;

/** 挑战能要求的 procedure；personal 缺省只要求登录（§16：core 接受链接时没有挑战组件）。 */
export const CHALLENGE_SCOPES: readonly ChallengeScope[] = [
  "auth.login",
  "links.accept",
];
export const DEFAULT_CHALLENGE_SCOPE: readonly ChallengeScope[] = [
  "auth.login",
];

export interface ChallengeVerdict {
  ok: boolean;
  /**
   * 不通过的原因：提供方的 `error-codes`（如 `timeout-or-duplicate`、`invalid-input-response`），
   * 或本地的 `network` / `bad-response` / `replayed`。不含令牌与 secret。
   */
  reasons: string[];
  /** 提供方没给出结论（网络错、5xx、答复不成形）：可以用同一个幂等键再问一次。 */
  retryable?: boolean;
}

/** 提供方校验端口（Node 与 Workers 各给实现）。 */
export interface ChallengeVerifier {
  verify(input: {
    token: string;
    remoteIp?: string;
    idempotencyKey: string;
  }): Promise<ChallengeVerdict>;
}

/** siteverify 的请求体（`application/x-www-form-urlencoded`）。 */
export function siteverifyBody(input: {
  secret: string;
  token: string;
  remoteIp?: string;
  idempotencyKey: string;
}): URLSearchParams {
  const body = new URLSearchParams();
  body.set("secret", input.secret);
  body.set("response", input.token);
  if (input.remoteIp && input.remoteIp !== "unknown")
    body.set("remoteip", input.remoteIp);
  body.set("idempotency_key", input.idempotencyKey);
  return body;
}

const siteverifyAnswerSchema = z.object({
  success: z.boolean(),
  "error-codes": z.array(z.string()).optional(),
});

/** 解读 siteverify 的答复（HTTP 状态 + JSON 体）。 */
export function readSiteverify(
  status: number,
  body: unknown,
): ChallengeVerdict {
  if (status >= 500)
    return { ok: false, reasons: ["network"], retryable: true };
  const parsed = siteverifyAnswerSchema.safeParse(body);
  if (!parsed.success)
    return { ok: false, reasons: ["bad-response"], retryable: status !== 400 };
  if (parsed.data.success) return { ok: true, reasons: [] };
  const reasons = (parsed.data["error-codes"] ?? []).slice(0, 8);
  return {
    ok: false,
    reasons: reasons.length > 0 ? reasons : ["rejected"],
    // 提供方自己的内部错误可以重试；其余是令牌本身的问题。
    retryable: reasons.includes("internal-error"),
  };
}

/** 逗号分隔的 scope（配置里用）；有不认识的或为空就抛。 */
export function parseChallengeScope(text: string): ChallengeScope[] {
  const items = text
    .split(",")
    .map((item) => item.trim())
    .filter(Boolean);
  if (items.length === 0) throw new Error("挑战范围为空");
  for (const item of items)
    if (!(CHALLENGE_SCOPES as readonly string[]).includes(item))
      throw new Error(`挑战范围不认识：${item}`);
  return [...new Set(items)] as ChallengeScope[];
}

export interface ChallengeGate {
  /** `platform.info.challenge`。 */
  info(): PlatformChallenge;
  /**
   * 这条 procedure 要挑战就校验；不通过抛 `challenge_required` / `challenge_invalid`。
   * 调用方在按 IP 限流之后、口令或秘密之前调。
   */
  check(
    procedure: ChallengeScope,
    challenge: ChallengeToken | undefined,
    ip: string,
  ): Promise<void>;
}

export interface ChallengeGateOptions {
  siteKey: string;
  scope?: readonly ChallengeScope[];
  verifier: ChallengeVerifier;
  now: () => number;
  /** 不通过时记一笔（只给原因，不给令牌）。 */
  onRejected?: (procedure: ChallengeScope, reasons: string[]) => void;
}

/** 本地防重放表的上限（超了先清过期的，仍满就整表清空：提供方仍会拒绝重放）。 */
const MAX_SEEN = 10_000;

const tokenHash = (token: string) =>
  createHash("sha256")
    .update(`armadra/relay/v1/challenge\u0000${token}`, "utf8")
    .digest("hex");

export function createChallengeGate(
  options: ChallengeGateOptions,
): ChallengeGate {
  const siteKey = options.siteKey.trim();
  if (siteKey === "") throw new Error("挑战缺 siteKey");
  const scope = [...(options.scope ?? DEFAULT_CHALLENGE_SCOPE)];
  if (scope.length === 0) throw new Error("挑战范围为空");
  /** 用过的令牌哈希 → 过期时刻。 */
  const seen = new Map<string, number>();

  const remember = (hash: string, nowMs: number) => {
    if (seen.size >= MAX_SEEN)
      for (const [key, until] of seen) if (until <= nowMs) seen.delete(key);
    if (seen.size >= MAX_SEEN) seen.clear();
    seen.set(hash, nowMs + CHALLENGE_TOKEN_TTL_MS);
  };

  const reject = (procedure: ChallengeScope, reasons: string[]): never => {
    options.onRejected?.(procedure, reasons);
    throw new ControlError("challenge_invalid");
  };

  return {
    info: () => ({ provider: "turnstile", siteKey, scope: [...scope] }),
    async check(procedure, challenge, ip) {
      if (!scope.includes(procedure)) return;
      if (!challenge) throw new ControlError("challenge_required");
      if (challenge.provider !== "turnstile")
        return reject(procedure, ["provider"]);
      const nowMs = options.now();
      const hash = tokenHash(challenge.token);
      const until = seen.get(hash);
      if (until !== undefined && until > nowMs)
        return reject(procedure, ["replayed"]);
      // 先占位：并发的同一令牌只有一个去问提供方。
      remember(hash, nowMs);
      const request = {
        token: challenge.token,
        ...(ip ? { remoteIp: ip } : {}),
        idempotencyKey: randomUUID(),
      };
      const ask = async (): Promise<ChallengeVerdict> => {
        try {
          return await options.verifier.verify(request);
        } catch {
          return { ok: false, reasons: ["network"], retryable: true };
        }
      };
      let verdict = await ask();
      if (!verdict.ok && verdict.retryable) verdict = await ask();
      if (!verdict.ok) reject(procedure, verdict.reasons);
    },
  };
}
