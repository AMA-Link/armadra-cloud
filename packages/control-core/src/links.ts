/**
 * 分享链接与访客（中继设计 §6.5，平台设计 §17.3）。
 *
 * - `create` 答 `secret`（32 字节 base64url），存储只存哈希。分享出去的链接是
 *   `https://<中继>/j/<linkId>#<secret>.<core 邀请令牌>`：秘密只在 `#` 片段里，浏览器不会把
 *   它发给任何服务器；中继自己拼的 `url` 不带片段。
 * - `accept { secret }`：按 IP 限流（`link.accept.ip`，20 次 / 分钟）→ 人机挑战（scope 含
 *   `links.accept` 时，cloud-api §16）→ 链接在、未撤销 → 秘密
 *   比对 → 未过期、次数未满 → `uses++` → 建访客设备（`guest:<linkId>:<随机>`，30 天）→ 签
 *   带 `link` 声明的断言与中继令牌。
 * - `update { label }`：只改备注（落地页与访客的源名用它）；已撤销的也能改，不动秘密、次数与期限。
 * - `revoke`：撤销链接与它名下的全部访客设备，断开访客经中继的 WS。
 * - `scope`（0.3.0 起，SaaS 设计 §3.3 (b)）：工作空间 / 会话 / 只读。访客的断言带 `scp`，
 *   只读时中继令牌带 `ro: true`（边缘不据它过滤，读写由 core 判定）。缺省 = 整台、按角色（旧行为）。
 */
import type {
  ChallengeToken,
  DeviceInput,
  LinkScope,
  LinkSummary,
} from "@armadra/platform-protocol/cloud-api";
import { SESSION } from "@armadra/platform-protocol/identity-vectors";

import { ControlError } from "./errors.ts";
import type { ControlContext, ControlPorts, LinkRecord } from "./ports.ts";
import { randomHex, randomToken, sameHex, secretHash } from "./secrets.ts";
import { createDevice, sessionFor, type Caller } from "./sessions.ts";
import { requireOwner, type Sources } from "./sources.ts";
import { guestPrefix } from "./subjects.ts";

/** 链接最长有效期：30 天（与访客的刷新令牌同寿）。 */
export const LINK_MAX_TTL_MS = SESSION.refreshTtlMs;

/**
 * 规范化分享范围：空的答 undefined（整台）；`sessionId` 要有 `workspaceId`；会话范围总是只读，
 * 显式 `readOnly: false` 答 `bad_request`；`readOnly: false` 等同缺省，不存。
 */
export function normalizeLinkScope(
  scope: LinkScope | undefined,
): LinkScope | undefined {
  if (!scope) return undefined;
  const { workspaceId, sessionId, readOnly } = scope;
  if (sessionId !== undefined) {
    if (workspaceId === undefined || readOnly === false)
      throw new ControlError("bad_request");
    return { workspaceId, sessionId, readOnly: true };
  }
  const out: LinkScope = {};
  if (workspaceId !== undefined) out.workspaceId = workspaceId;
  if (readOnly === true) out.readOnly = true;
  return Object.keys(out).length === 0 ? undefined : out;
}

export function linkUrl(issuer: string, linkId: string): string {
  return `${issuer}/j/${linkId}`;
}

function toSummary(issuer: string, link: LinkRecord): LinkSummary {
  return {
    linkId: link.linkId,
    kind: "source_invite",
    label: link.label,
    role: link.role,
    sourceId: link.sourceId,
    ...(link.scope ? { scope: { ...link.scope } } : {}),
    url: linkUrl(issuer, link.linkId),
    uses: link.uses,
    maxUses: link.maxUses,
    expiresAtMs: link.expiresAtMs,
    createdAtMs: link.createdAtMs,
    revokedAtMs: link.revokedAtMs,
  };
}

type LinkPorts = Pick<ControlPorts, "links" | "tenants">;

/** 撤销链接与它的访客设备；答链接指向的源，没有这条有效链接答 null。 */
export function revokeLink(
  context: { tenantId: string; ports: Pick<ControlPorts, "links"> },
  linkId: string,
  nowMs: number,
): Promise<string | null> {
  return context.ports.links.revoke(context.tenantId, linkId, nowMs);
}

/** 全部链接（可按源过滤），按创建顺序。 */
export async function listLinks(
  context: { tenantId: string; ports: LinkPorts },
  sourceId?: string,
): Promise<LinkSummary[]> {
  const { tenantId, ports } = context;
  const issuer = (await ports.tenants.get(tenantId)).issuer;
  const links = await ports.links.list(
    tenantId,
    sourceId === undefined ? {} : { sourceId },
  );
  return links.map((link) => toSummary(issuer, link));
}

export function createLinks(context: ControlContext, sources: Sources) {
  const { tenantId, ports } = context;

  async function liveSource(sourceId: string) {
    const source = await ports.sources.get(tenantId, sourceId);
    return source && source.revokedAtMs === null ? source : null;
  }

  return {
    async create(
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
            scope?: LinkScope;
          }
        | { kind: "org_join" },
    ) {
      requireOwner(caller);
      if (input.kind !== "source_invite") throw new ControlError("bad_request");
      const scope = normalizeLinkScope(input.scope);
      const nowMs = context.now();
      if (
        input.expiresAtMs <= nowMs ||
        input.expiresAtMs > nowMs + LINK_MAX_TTL_MS
      )
        throw new ControlError("bad_request");
      const linkId = randomHex(8);
      const secret = randomToken(32);
      const settings = await ports.tenants.get(tenantId);
      if (!settings.allowGuests) throw new ControlError("forbidden");
      if (!(await liveSource(input.sourceId)))
        throw new ControlError("source_access_denied");
      await ports.links.insert(tenantId, {
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
        ...(scope ? { scope } : {}),
      });
      return {
        linkId,
        url: linkUrl(settings.issuer, linkId),
        secret,
        expiresAtMs: input.expiresAtMs,
      };
    },

    async get(linkId: string) {
      const link = await ports.links.get(tenantId, linkId);
      const source =
        link && link.revokedAtMs === null
          ? await liveSource(link.sourceId)
          : null;
      if (!link || link.revokedAtMs !== null || !source)
        throw new ControlError("link_invalid");
      return {
        kind: "source_invite" as const,
        label: link.label,
        role: link.role,
        sourceName: source.name,
        ...(link.scope ? { scope: { ...link.scope } } : {}),
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
        challenge?: ChallengeToken;
      },
      ip: string,
    ) {
      const limited = await context.limits.hit("link.accept.ip", ip);
      if (!limited.allowed)
        throw new ControlError("rate_limited", {
          retryAfterMs: limited.retryAfterMs,
        });
      await context.challenge?.check("links.accept", input.challenge, ip);
      const nowMs = context.now();
      const device = input.device ?? {
        platform: "browser" as const,
        name: "guest",
      };
      const found = await ports.links.get(tenantId, input.linkId);
      const source = found ? await liveSource(found.sourceId) : null;
      const settings = await ports.tenants.get(tenantId);
      if (
        !found ||
        found.revokedAtMs !== null ||
        !source ||
        !settings.allowGuests
      )
        throw new ControlError("link_invalid");
      if (
        !input.secret ||
        !sameHex(secretHash("link", input.secret), found.secretHash)
      )
        throw new ControlError("link_secret_invalid");
      // 次数与期限在原子的读-改-写里再核一遍，并发的 accept 不会超额。
      const link = await ports.links.update(tenantId, input.linkId, (draft) => {
        if (!draft || draft.revokedAtMs !== null)
          throw new ControlError("link_invalid");
        if (draft.expiresAtMs <= nowMs) throw new ControlError("link_expired");
        if (draft.maxUses !== null && draft.uses >= draft.maxUses)
          throw new ControlError("link_exhausted");
        draft.uses += 1;
        return { ...draft };
      });
      const created = await createDevice(context, {
        subject: `${guestPrefix(link.linkId)}${randomHex(4)}`,
        device,
        nowMs,
        ttlMs: Math.min(SESSION.refreshTtlMs, LINK_MAX_TTL_MS),
      });
      const granted = await sources.grant(source, created.device, {
        name: link.label || undefined,
        link: { linkId: link.linkId, invitationId: link.invitationId },
        ...(link.scope ? { scope: link.scope } : {}),
      });
      context.emit({
        type: "linkUsed",
        linkId: link.linkId,
        sourceId: source.sourceId,
        by: { guest: true },
        atMs: nowMs,
      });
      return {
        sourceId: source.sourceId,
        relayOrigin: granted.relayOrigin,
        relayBaseUrl: granted.relayBaseUrl,
        assertion: granted.assertion,
        relayToken: granted.relayToken,
        guestSession: await sessionFor(
          context,
          created.device,
          created.refreshToken,
        ),
      };
    },

    async list(caller: Caller, sourceId?: string): Promise<LinkSummary[]> {
      requireOwner(caller);
      return listLinks(context, sourceId);
    },

    async update(caller: Caller, input: { linkId: string; label: string }) {
      requireOwner(caller);
      const label = input.label.trim();
      const link = await ports.links.update(tenantId, input.linkId, (draft) => {
        if (!draft) throw new ControlError("not_found");
        draft.label = label;
        return { ...draft };
      });
      return toSummary((await ports.tenants.get(tenantId)).issuer, link);
    },

    async revoke(caller: Caller, linkId: string): Promise<void> {
      requireOwner(caller);
      const sourceId = await revokeLink(context, linkId, context.now());
      if (sourceId === null) throw new ControlError("not_found");
      context.relay.disconnect({ subjectPrefix: guestPrefix(linkId) });
      context.emit({ type: "accessRevoked", sourceId }, { linkId });
    },
  };
}
