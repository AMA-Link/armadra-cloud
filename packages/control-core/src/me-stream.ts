/**
 * `/v1/me/stream`（契约 §6，协议包 §5.7）的控制面一侧：票据与事件扇出。WebSocket 本身由装配方
 * 托管（Go 中继在 `go/internal/personal`，Workers 中继在 `ControlDO`）。
 *
 * - 票据：`POST /v1/me/stream/ticket`（会话）→ 30 秒、一次性，只在内存，存哈希；
 * - 事件：owner 收全部源的；访客只收链接指向的那个源的，以及发给它那条链接的 `accessRevoked`；
 * - 源上下线来自中继的在线事件，只转发仍在登记里的源。
 */
import type { MeStreamEvent } from "@armadra/platform-protocol/cloud-api";
import { SESSION } from "@armadra/platform-protocol/identity-vectors";

import type { ControlEvent, EventAudience } from "./events.ts";
import type { ControlContext } from "./ports.ts";
import { randomToken, secretHash } from "./secrets.ts";
import { isLiveDevice, type Caller } from "./sessions.ts";

const MAX_TICKETS = 1_000;

export interface MeStreamCore {
  ticket(caller: Caller): { ticket: string; expiresAtMs: number };
  /** 一次性兑换：票不在、过期或设备已失效答 null。 */
  redeem(ticket: string | undefined): Promise<Caller | null>;
  /** 设备仍有效（未登出、未撤销、未过期）。 */
  stillValid(caller: Caller): Promise<boolean>;
  /** 订阅这个调用方能看到的事件；答退订。 */
  attach(caller: Caller, send: (event: MeStreamEvent) => void): () => void;
  clear(): void;
}

export function createMeStreamCore(context: ControlContext): MeStreamCore {
  const { tenantId, ports } = context;
  const tickets = new Map<string, { caller: Caller; expiresAtMs: number }>();

  const sweep = () => {
    const nowMs = context.now();
    for (const [key, entry] of tickets)
      if (entry.expiresAtMs <= nowMs) tickets.delete(key);
  };

  /** 这个调用方能看到这个源吗（访客只看链接指向的那个）。 */
  const sees = async (
    caller: Caller,
    sourceId: string | undefined,
  ): Promise<boolean> => {
    if (caller.role === "owner") return true;
    const link = caller.linkId
      ? await ports.links.get(tenantId, caller.linkId)
      : null;
    return link !== null && link.sourceId === sourceId;
  };

  const stillValid = async (caller: Caller): Promise<boolean> => {
    const device = await ports.devices.get(tenantId, caller.deviceId);
    return device !== null && isLiveDevice(device, context.now());
  };

  return {
    ticket(caller) {
      sweep();
      if (tickets.size >= MAX_TICKETS) tickets.clear();
      const ticket = randomToken(32);
      const expiresAtMs = context.now() + SESSION.wsTicketTtlMs;
      tickets.set(secretHash("ticket", ticket), { caller, expiresAtMs });
      return { ticket, expiresAtMs };
    },

    async redeem(ticket) {
      const key = ticket ? secretHash("ticket", ticket) : "";
      const entry = tickets.get(key);
      tickets.delete(key);
      if (!entry || entry.expiresAtMs <= context.now()) return null;
      return (await stillValid(entry.caller)) ? entry.caller : null;
    },

    stillValid,

    attach(caller, send) {
      const deliver = (event: ControlEvent, audience?: EventAudience) => {
        if (audience) {
          if (caller.linkId === audience.linkId) send(event);
          return;
        }
        if (caller.role === "owner") {
          send(event);
          return;
        }
        sees(caller, event.sourceId).then(
          (visible) => {
            if (visible) send(event);
          },
          () => {},
        );
      };
      const offBus = context.subscribe(deliver);
      const offPresence = context.presence.subscribe((presence) => {
        ports.sources.get(tenantId, presence.sourceId).then(
          (source) => {
            if (!source || source.revokedAtMs !== null) return;
            if (presence.type === "sourceOnline")
              deliver({
                type: "sourceOnline",
                sourceId: presence.sourceId,
                sinceMs: presence.atMs,
                relayOrigin: context.signer.issuer,
              });
            else
              deliver({
                type: "sourceOffline",
                sourceId: presence.sourceId,
                atMs: presence.atMs,
              });
          },
          () => {},
        );
      });
      return () => {
        offBus();
        offPresence();
      };
    },

    clear() {
      tickets.clear();
    },
  };
}
