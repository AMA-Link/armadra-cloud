/**
 * Workers 中继的挑战校验（cloud-api §16）：`ControlDO` 用 Workers 的 `fetch` 调 Turnstile 的
 * siteverify。请求体与答复的解读在控制核（`@armadra/control-core/challenge`），与 Node 中继同一段。
 *
 * secret 来自 `wrangler secret put RELAY_TURNSTILE_SECRET`，只在请求体里经过，不进日志与响应。
 */
import {
  readSiteverify,
  siteverifyBody,
  SITEVERIFY_TIMEOUT_MS,
  TURNSTILE_SITEVERIFY_URL,
  type ChallengeVerifier,
} from "@armadra/control-core/challenge";

export function createWorkersTurnstileVerifier(options: {
  secret: string;
  /** 测试用：换掉 `fetch`（假 siteverify）。 */
  fetch?: typeof fetch;
  timeoutMs?: number;
}): ChallengeVerifier {
  const send = options.fetch ?? ((input, init) => fetch(input, init));
  const timeoutMs = options.timeoutMs ?? SITEVERIFY_TIMEOUT_MS;
  return {
    async verify(input) {
      let response: Response;
      try {
        response = await send(TURNSTILE_SITEVERIFY_URL, {
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
