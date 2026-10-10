/**
 * saas 模式的两件外部往来（中继设计 §2.8、§6；cloud-api §17）：
 *
 * - JWKS：从 cloud 的 `/.well-known/jwks.json` 拉（内核的 `JwksCache` 再缓存、`kid` 不认识时重取）；
 * - 在线事件：经 `relay.nodePresence`（`POST <cloud>/v1/relay/nodes/cf/presence`，节点令牌）报给
 *   cloud，检查周期里重复报 `sourceOnline` 续期（`ttlMs` = 3 个周期）。
 *
 * 没配节点令牌就不报（cloud 那边看不到这个节点上的源在线，客户端照样能连）。
 */
import { JWKS_WELL_KNOWN } from "@armadra/platform-protocol";
import {
  jwkSetSchema,
  type JwkSet,
} from "@armadra/platform-protocol/assertion";
import type { PresenceEvent } from "@armadra/relay-kernel/control/types";

import type { RelayConfig } from "./env.ts";

export async function cloudJwks(cloudIssuer: string): Promise<JwkSet> {
  const response = await fetch(`${cloudIssuer}${JWKS_WELL_KNOWN}`, {
    headers: { accept: "application/json" },
  });
  if (!response.ok) throw new Error(`jwks ${response.status}`);
  return jwkSetSchema.parse(await response.json());
}

export async function reportPresence(
  config: RelayConfig,
  nodeToken: string | undefined,
  event: PresenceEvent,
): Promise<void> {
  if (!nodeToken || !config.cloudIssuer) return;
  const body = {
    node: event.node,
    ttlMs: Math.min(600_000, Math.max(10_000, 3 * config.alarmMs)),
    events: [
      event.type === "sourceOnline"
        ? {
            type: event.type,
            sourceId: event.sourceId,
            tunnelId: event.tunnelId,
            coreVersion: event.coreVersion,
            atMs: event.atMs,
          }
        : {
            type: event.type,
            sourceId: event.sourceId,
            tunnelId: event.tunnelId,
            atMs: event.atMs,
          },
    ],
  };
  const response = await fetch(
    `${config.cloudIssuer}/v1/relay/nodes/${event.node}/presence`,
    {
      method: "POST",
      headers: {
        authorization: `Bearer ${nodeToken}`,
        "content-type": "application/json",
      },
      body: JSON.stringify(body),
    },
  );
  if (!response.ok) throw new Error(`presence ${response.status}`);
}
