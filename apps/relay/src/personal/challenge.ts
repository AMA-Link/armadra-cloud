/**
 * Node 中继的挑战校验（cloud-api §16，`--challenge turnstile`）：用 Node 的 `fetch` 调 Turnstile 的
 * siteverify。请求体与答复的解读在控制核（`@armadra/control-core/challenge`），与 Workers 中继同一段。
 *
 * secret 只从文件读（`--turnstile-secret-file`），只在请求体里经过，不进日志与响应。
 */
import { readFileSync } from "node:fs";

import {
  readSiteverify,
  siteverifyBody,
  SITEVERIFY_TIMEOUT_MS,
  TURNSTILE_SITEVERIFY_URL,
  type ChallengeVerifier,
} from "@armadra/control-core/challenge";

export function createTurnstileVerifier(options: {
  secret: string;
  /** 测试用：假 siteverify 的地址。 */
  endpoint?: string;
  timeoutMs?: number;
}): ChallengeVerifier {
  const endpoint = options.endpoint ?? TURNSTILE_SITEVERIFY_URL;
  const timeoutMs = options.timeoutMs ?? SITEVERIFY_TIMEOUT_MS;
  return {
    async verify(input) {
      let response: Response;
      try {
        response = await fetch(endpoint, {
          method: "POST",
          body: siteverifyBody({ secret: options.secret, ...input }),
          signal: AbortSignal.timeout(timeoutMs),
        });
      } catch {
        return { ok: false, reasons: ["network"], retryable: true };
      }
      let body: unknown = null;
      try {
        body = await response.json();
      } catch {
        body = null;
      }
      return readSiteverify(response.status, body);
    },
  };
}

/** 读 secret 文件（去首尾空白）；空文件当作没配，抛错交给启动失败。 */
export function readTurnstileSecret(file: string): string {
  const secret = readFileSync(file, "utf8").trim();
  if (secret === "") throw new Error(`Turnstile secret 文件是空的：${file}`);
  return secret;
}
