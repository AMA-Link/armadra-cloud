/**
 * 隧道握手状态机（中继设计 §3.1，协议包 §3.1）：hello → challenge → auth → ready。
 *
 * 1. `hello`：协议 major 相同（否则 `reject protocol_unsupported` + 4409）；隧道令牌
 *    验签与声明（`auth/tunnel-token.ts`），`sub === hello.sourceId`；之后按 sourceId
 *    限流 `tunnel.connect.src`（`reject rate_limited` + 4429）。令牌错 → reject + 4490。
 * 2. 发 `challenge { nonce2, relayNode }`。
 * 3. `auth.sig` 用 `cnf.jwk` 验 `sourceId\nnonce\nnonce2\nrelayNode`；错 → `reject
 *    signature_invalid` + 4490。
 * 4. 通过后由 `tunnel/server.ts` 处理同源替换、发 `ready`、写路由。
 *
 * 任何格式错（不是 JSON、不符 schema、顺序不对）→ 不发 reject，直接关 4400。
 * 本类不碰套接字与定时器：输入一条文本消息，输出下一步动作；超时由调用方计时。
 */
import { randomBytes, verify, type KeyObject } from "node:crypto";

import { PROTOCOL_VERSION } from "@armadra/platform-protocol";
import type { TunnelTokenClaims } from "@armadra/platform-protocol/assertion";
import {
  authSchema,
  challengeSigningInput,
  CLOSE,
  helloSchema,
  type Challenge,
  type Hello,
  type Reject,
  type TunnelRejectCode,
} from "@armadra/platform-protocol/tunnel";

import type { AuthContext } from "../auth/context.ts";
import { verifyTunnelToken } from "../auth/tunnel-token.ts";
import type { ControlPlane } from "../control/types.ts";

export type HandshakeStep =
  | { kind: "send"; message: Challenge }
  | { kind: "reject"; message: Reject; closeCode: number }
  | { kind: "fail"; closeCode: number; reason: string }
  | { kind: "ready"; hello: Hello; claims: TunnelTokenClaims };

export interface HandshakeDeps {
  auth: AuthContext;
  limits: ControlPlane["limits"];
  relayNode: string;
  /** 16 字节随机数的 base64url；测试注入固定值。 */
  nonce?: () => string;
}

const REJECT_MESSAGES: Record<TunnelRejectCode, string> = {
  protocol_unsupported: "protocol major not supported",
  tunnel_token_invalid: "tunnel token invalid",
  tunnel_token_expired: "tunnel token expired",
  signature_invalid: "signature invalid",
  source_revoked: "source revoked",
  rate_limited: "too many tunnel connections",
  replaced: "replaced by a newer tunnel",
};

const REJECT_CLOSE: Record<TunnelRejectCode, number> = {
  protocol_unsupported: CLOSE.protocolUnsupported,
  tunnel_token_invalid: CLOSE.tunnelAuthFailed,
  tunnel_token_expired: CLOSE.tunnelAuthFailed,
  signature_invalid: CLOSE.tunnelAuthFailed,
  source_revoked: CLOSE.tunnelAuthFailed,
  rate_limited: CLOSE.limitReached,
  replaced: CLOSE.tunnelReplaced,
};

export function rejectStep(reject: TunnelRejectCode): HandshakeStep {
  return {
    kind: "reject",
    message: { type: "reject", code: reject, message: REJECT_MESSAGES[reject] },
    closeCode: REJECT_CLOSE[reject],
  };
}

const fail = (reason: string): HandshakeStep => ({
  kind: "fail",
  closeCode: CLOSE.badFrame,
  reason,
});

function parseJson(text: string): unknown {
  try {
    return JSON.parse(text);
  } catch {
    return undefined;
  }
}

export class Handshake {
  state: "hello" | "auth" | "done" = "hello";
  private hello: Hello | undefined;
  private claims: TunnelTokenClaims | undefined;
  private sourceKey: KeyObject | undefined;
  private challenge: Challenge | undefined;

  private readonly deps: HandshakeDeps;

  constructor(deps: HandshakeDeps) {
    this.deps = deps;
  }

  /** 握手是否到了 `auth` 之后（用于指标里区分阶段）。 */
  get sourceId(): string | undefined {
    return this.hello?.sourceId;
  }

  async onText(text: string): Promise<HandshakeStep> {
    const value = parseJson(text);
    if (this.state === "hello") return this.onHello(value);
    if (this.state === "auth") return this.onAuth(value);
    return fail("unexpected_text");
  }

  private async onHello(value: unknown): Promise<HandshakeStep> {
    const parsed = helloSchema.safeParse(value);
    if (!parsed.success) return this.done(fail("bad_hello"));
    const hello = parsed.data;
    if (hello.protocol.major !== PROTOCOL_VERSION.major)
      return this.done(rejectStep("protocol_unsupported"));
    const token = await verifyTunnelToken(
      this.deps.auth,
      hello.tunnelToken,
      hello.sourceId,
    );
    if (!token.ok) return this.done(rejectStep(token.reject));
    const limit = await this.deps.limits.hit(
      "tunnel.connect.src",
      hello.sourceId,
    );
    if (!limit.allowed) return this.done(rejectStep("rate_limited"));
    this.hello = hello;
    this.claims = token.claims;
    this.sourceKey = token.sourceKey;
    this.challenge = {
      type: "challenge",
      nonce2: (this.deps.nonce ?? randomNonce)(),
      relayNode: this.deps.relayNode,
    };
    this.state = "auth";
    return { kind: "send", message: this.challenge };
  }

  private onAuth(value: unknown): HandshakeStep {
    const parsed = authSchema.safeParse(value);
    if (!parsed.success) return this.done(fail("bad_auth"));
    const hello = this.hello as Hello;
    const challenge = this.challenge as Challenge;
    const input = challengeSigningInput({
      sourceId: hello.sourceId,
      nonce: hello.nonce,
      nonce2: challenge.nonce2,
      relayNode: challenge.relayNode,
    });
    let valid = false;
    try {
      valid = verify(
        null,
        input,
        this.sourceKey as KeyObject,
        Buffer.from(parsed.data.sig, "base64url"),
      );
    } catch {
      valid = false;
    }
    if (!valid) return this.done(rejectStep("signature_invalid"));
    return this.done({
      kind: "ready",
      hello,
      claims: this.claims as TunnelTokenClaims,
    });
  }

  private done(step: HandshakeStep): HandshakeStep {
    this.state = "done";
    return step;
  }
}

export function randomNonce(): string {
  return randomBytes(16).toString("base64url");
}
