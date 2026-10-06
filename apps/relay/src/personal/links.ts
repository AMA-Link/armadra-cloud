/**
 * 分享链接与访客（中继设计 §6.5，平台设计 §17.3）。
 *
 * - `create` 答 `secret`（32 字节 base64url），状态文件只存哈希。分享出去的链接是
 *   `https://<中继>/j/<linkId>#<secret>.<core 邀请令牌>`：秘密只在 `#` 片段里，浏览器不会把
 *   它发给任何服务器；中继自己拼的 `url` 不带片段。
 * - `accept { secret }`：按 IP 限流（`link.accept.ip`，20 次 / 分钟）→ 链接在、未撤销 → 秘密
 *   比对 → 未过期、次数未满 → `uses++` → 建访客设备（`guest:<linkId>:<随机>`，30 天）→ 签
 *   带 `link` 声明的断言与中继令牌。
 * - `update { label }`：只改备注（落地页与访客的源名用它）；已撤销的也能改，不动秘密、次数与期限。
 * - `revoke`：撤销链接与它名下的全部访客设备，断开访客经中继的 WS。
 */
import type {
  DeviceInput,
  LinkSummary,
} from "@armadra/platform-protocol/cloud-api";
import { SESSION } from "@armadra/platform-protocol/identity-vectors";

import { guestPrefix, PersonalError, type PersonalContext } from "./context.ts";
import { randomHex, randomToken, sameHex, secretHash } from "./secrets.ts";
import { createDevice, sessionFor, type Caller } from "./sessions.ts";
import { requireOwner, type Sources } from "./sources.ts";
import type { LinkRecord, State } from "./state.ts";

/** 链接最长有效期：30 天（与访客的刷新令牌同寿）。 */
export const LINK_MAX_TTL_MS = SESSION.refreshTtlMs;

export function linkUrl(issuer: string, linkId: string): string {
  return `${issuer}/j/${linkId}`;
}

function toSummary(state: State, link: LinkRecord): LinkSummary {
  return {
    linkId: link.linkId,
    kind: "source_invite",
    label: link.label,
    role: link.role,
    sourceId: link.sourceId,
    url: linkUrl(state.issuer, link.linkId),
    uses: link.uses,
    maxUses: link.maxUses,
    expiresAtMs: link.expiresAtMs,
    createdAtMs: link.createdAtMs,
    revokedAtMs: link.revokedAtMs,
  };
}

/** 在状态副本上撤销链接与它的访客设备；答是否找到。 */
export function revokeLinkIn(
  draft: State,
  linkId: string,
  nowMs: number,
): boolean {
  const link = draft.links.find(
    (row) => row.linkId === linkId && row.revokedAtMs === null,
  );
  if (!link) return false;
  link.revokedAtMs = nowMs;
  const prefix = guestPrefix(linkId);
  for (const device of draft.devices)
    if (device.subject.startsWith(prefix) && device.revokedAtMs === null)
      device.revokedAtMs = nowMs;
  return true;
}

export function listLinks(state: State, sourceId?: string): LinkSummary[] {
  return state.links
    .filter((link) => sourceId === undefined || link.sourceId === sourceId)
    .map((link) => toSummary(state, link));
}

export function createLinks(context: PersonalContext, sources: Sources) {
  return {
    create(
      caller: Caller,
      input:
        | {
            kind: "source_invite";
            sourceId: string;
            invitationId: string;
            label: string;
            role: string;
            expiresAtMs: number;
            maxUses?: number;
          }
        | { kind: "org_join" },
    ) {
      requireOwner(caller);
      if (input.kind !== "source_invite")
        throw new PersonalError("bad_request");
      const nowMs = context.now();
      if (
        input.expiresAtMs <= nowMs ||
        input.expiresAtMs > nowMs + LINK_MAX_TTL_MS
      )
        throw new PersonalError("bad_request");
      const linkId = randomHex(8);
      const secret = randomToken(32);
      const state = context.store.update((draft) => {
        if (!draft.settings.allowGuests) throw new PersonalError("forbidden");
        const source = draft.sources.find(
          (row) => row.sourceId === input.sourceId && row.revokedAtMs === null,
        );
        if (!source) throw new PersonalError("source_access_denied");
        draft.links.push({
          linkId,
          sourceId: input.sourceId,
          invitationId: input.invitationId,
          label: input.label,
          role: input.role,
          secretHash: secretHash("link", secret),
          maxUses: input.maxUses ?? null,
          uses: 0,
          createdAtMs: nowMs,
          expiresAtMs: input.expiresAtMs,
          revokedAtMs: null,
        });
        return draft;
      });
      return {
        linkId,
        url: linkUrl(state.issuer, linkId),
        secret,
        expiresAtMs: input.expiresAtMs,
      };
    },

    get(linkId: string) {
      const state = context.store.current;
      const link = state.links.find(
        (row) => row.linkId === linkId && row.revokedAtMs === null,
      );
      const source = link
        ? state.sources.find(
            (row) => row.sourceId === link.sourceId && row.revokedAtMs === null,
          )
        : undefined;
      if (!link || !source) throw new PersonalError("link_invalid");
      return {
        kind: "source_invite" as const,
        label: link.label,
        role: link.role,
        sourceName: source.name,
        expiresAtMs: link.expiresAtMs,
        exhausted: link.maxUses !== null && link.uses >= link.maxUses,
        requiresAccount: false,
      };
    },

    async accept(
      input: {
        linkId: string;
        secret?: string;
        device?: Pick<DeviceInput, "platform" | "name">;
      },
      ip: string,
    ) {
      const limited = await context.control.limits.hit("link.accept.ip", ip);
      if (!limited.allowed)
        throw new PersonalError("rate_limited", {
          retryAfterMs: limited.retryAfterMs,
        });
      const nowMs = context.now();
      const device = input.device ?? {
        platform: "browser" as const,
        name: "guest",
      };
      const accepted = context.store.update((draft) => {
        const link = draft.links.find((row) => row.linkId === input.linkId);
        const source = link
          ? draft.sources.find(
              (row) =>
                row.sourceId === link.sourceId && row.revokedAtMs === null,
            )
          : undefined;
        if (
          !link ||
          link.revokedAtMs !== null ||
          !source ||
          !draft.settings.allowGuests
        )
          throw new PersonalError("link_invalid");
        if (
          !input.secret ||
          !sameHex(secretHash("link", input.secret), link.secretHash)
        )
          throw new PersonalError("link_secret_invalid");
        if (link.expiresAtMs <= nowMs) throw new PersonalError("link_expired");
        if (link.maxUses !== null && link.uses >= link.maxUses)
          throw new PersonalError("link_exhausted");
        link.uses += 1;
        const created = createDevice(draft, {
          subject: `${guestPrefix(link.linkId)}${randomHex(4)}`,
          device,
          nowMs,
          ttlMs: Math.min(SESSION.refreshTtlMs, LINK_MAX_TTL_MS),
        });
        return { ...created, link: { ...link }, source: { ...source } };
      });
      const granted = await sources.grant(accepted.source, accepted.device, {
        name: accepted.link.label || undefined,
        link: {
          linkId: accepted.link.linkId,
          invitationId: accepted.link.invitationId,
        },
      });
      context.emit({
        type: "linkUsed",
        linkId: accepted.link.linkId,
        sourceId: accepted.source.sourceId,
        by: { guest: true },
        atMs: nowMs,
      });
      return {
        sourceId: accepted.source.sourceId,
        relayOrigin: granted.relayOrigin,
        relayBaseUrl: granted.relayBaseUrl,
        assertion: granted.assertion,
        relayToken: granted.relayToken,
        guestSession: sessionFor(
          context,
          accepted.device,
          accepted.refreshToken,
        ),
      };
    },

    list(caller: Caller, sourceId?: string): LinkSummary[] {
      requireOwner(caller);
      return listLinks(context.store.current, sourceId);
    },

    update(caller: Caller, input: { linkId: string; label: string }) {
      requireOwner(caller);
      const label = input.label.trim();
      const state = context.store.update((draft) => {
        const link = draft.links.find((row) => row.linkId === input.linkId);
        if (!link) throw new PersonalError("not_found");
        link.label = label;
        return draft;
      });
      const link = state.links.find((row) => row.linkId === input.linkId);
      if (!link) throw new PersonalError("not_found");
      return toSummary(state, link);
    },

    revoke(caller: Caller, linkId: string): void {
      requireOwner(caller);
      const nowMs = context.now();
      const sourceId = context.store.update((draft) => {
        const link = draft.links.find((row) => row.linkId === linkId);
        if (!link || !revokeLinkIn(draft, linkId, nowMs))
          throw new PersonalError("not_found");
        return link.sourceId;
      });
      context.disconnect({ subjectPrefix: guestPrefix(linkId) });
      context.emit({ type: "accessRevoked", sourceId }, { linkId });
    },
  };
}
