/**
 * Worker 入口（中继设计 §2.1）：无状态，每请求一次，按路径分派。
 *
 * ```text
 * /s/<id>/…（saas 配了域时 <id>.src.<域>/…）→ SourceDO(src:<id>)   源地址：HTTP 转发 / WS / 媒体票 / 预检
 * /t/v1?src=<id>                          → SourceDO(src:<id>)   隧道 WebSocket（Hibernation）
 * /v1/*、/.well-known/*、/v1/me/stream     → ControlDO(tenant:default)（personal；saas 答 404）
 * /health、/ready、/metrics                → Worker 自己答（/metrics 要 RELAY_METRICS_TOKEN）
 * /、/app/*、/j/<linkId>、根下的产物文件     → Static Assets（没有产物答自有页面）
 * ```
 *
 * 指向某个源的请求先于一切：源自己也有 `/health`，不能被中继的同名端点截走。闸（Rate Limiting
 * 绑定）在进对象之前判，省一次对象请求。
 */
import { createHash, timingSafeEqual } from "node:crypto";

import {
  PROTOCOL_HEADER,
  protocolHeaderValue,
  TUNNEL_PATH,
} from "@armadra/platform-protocol";
import { ME_STREAM_PATH } from "@armadra/platform-protocol/cloud-api";
import {
  hostAddressing,
  pathAddressing,
} from "@armadra/relay-kernel/edge/addressing";

import manifest from "../package.json" with { type: "json" };
import { isApiPath } from "./api.ts";
import {
  CONTROL_OBJECT_NAME,
  ConfigError,
  readConfig,
  sourceObjectName,
  type RelayConfig,
  type RelayEnv,
} from "./env.ts";
import {
  apiGate,
  edgeGate,
  GATE_PERIOD_S,
  passGate,
  type GateName,
} from "./gates.ts";
import { workersLogger } from "./log.ts";
import { servePages } from "./pages.ts";
import { jsonResponse, relayError } from "./responses.ts";

export const RELAY_WORKERS_VERSION: string = manifest.version;
/** saas 模式的配额计数用一个单独的对象（没有租户）。 */
const BUDGET_OBJECT_NAME = "budget";
const SOURCE_ID = /^[0-9a-f]{32}$/;

function controlStub(env: RelayEnv, config: RelayConfig) {
  return env.CONTROL.get(
    env.CONTROL.idFromName(
      config.mode === "personal" ? CONTROL_OBJECT_NAME : BUDGET_OBJECT_NAME,
    ),
    { locationHint: config.controlLocation },
  );
}

function sourceStub(env: RelayEnv, sourceId: string) {
  return env.SOURCE.get(env.SOURCE.idFromName(sourceObjectName(sourceId)));
}

function limited(gate: GateName): Response {
  return relayError("rate_limited", {
    retryAfterMs: GATE_PERIOD_S[gate] * 1000,
  });
}

function sameSecret(given: string, expected: string): boolean {
  const digest = (value: string) => createHash("sha256").update(value).digest();
  return timingSafeEqual(digest(given), digest(expected));
}

export async function handleRequest(
  request: Request,
  env: RelayEnv,
): Promise<Response> {
  let config: RelayConfig;
  try {
    config = readConfig(env);
  } catch (error) {
    if (error instanceof ConfigError) {
      workersLogger("relay", "warn").error("配置不对，拒绝服务", {
        reason: error.message,
      });
      return relayError("unavailable");
    }
    throw error;
  }
  const url = new URL(request.url);
  const path = url.pathname;
  const clientIp = request.headers.get("cf-connecting-ip") ?? "0.0.0.0";

  const addressing =
    config.mode === "saas" && config.domain
      ? hostAddressing(config.domain)
      : pathAddressing();
  const target = addressing.resolve({
    url: `${path}${url.search}`,
    host: request.headers.get("host") ?? url.host,
  });
  if (target) {
    const gate = edgeGate(request);
    if (!(await passGate(env, gate, clientIp)))
      return limited(gate as GateName);
    return sourceStub(env, target.sourceId).fetch(request);
  }

  if (path === TUNNEL_PATH) {
    const sourceId = url.searchParams.get("src") ?? "";
    if (!SOURCE_ID.test(sourceId)) return relayError("not_found");
    if (!(await passGate(env, "LIMIT_TUNNEL", clientIp)))
      return limited("LIMIT_TUNNEL");
    return sourceStub(env, sourceId).fetch(request);
  }

  const headers = { [PROTOCOL_HEADER]: protocolHeaderValue() };
  if (path === "/health" && request.method === "GET")
    return jsonResponse(
      200,
      {
        ok: true,
        mode: config.mode,
        node: "cf",
        version: RELAY_WORKERS_VERSION,
      },
      headers,
    );
  if (path === "/ready" && request.method === "GET") {
    const state = await controlStub(env, config)
      .ready()
      .catch(() => ({ ok: false, code: "unavailable" }));
    return jsonResponse(state.ok ? 200 : 503, state, headers);
  }
  if (path === "/metrics") {
    const token = env.RELAY_METRICS_TOKEN;
    if (!token || request.method !== "GET") return relayError("not_found");
    if (
      !sameSecret(request.headers.get("authorization") ?? "", `Bearer ${token}`)
    )
      return relayError("unauthenticated");
    return new Response(await controlStub(env, config).metrics(), {
      headers: {
        "content-type": "text/plain; version=0.0.4; charset=utf-8",
        "cache-control": "no-store",
      },
    });
  }

  if (isApiPath(path) || path === ME_STREAM_PATH) {
    if (config.mode !== "personal") return relayError("not_found");
    const gate = apiGate(request.method, path);
    if (!(await passGate(env, gate, clientIp)))
      return limited(gate as GateName);
    return controlStub(env, config).fetch(request);
  }

  return (
    (await servePages(
      request,
      env.ASSETS,
      config.challenge
        ? {
            embedOrigins: [config.issuer, ...config.trustedOrigins],
          }
        : undefined,
    )) ?? relayError("not_found")
  );
}
