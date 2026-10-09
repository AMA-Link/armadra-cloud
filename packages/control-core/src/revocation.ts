/**
 * 撤销靠 `jti` 的形状即时生效、重启后仍然有效：
 *
 * - 隧道令牌 `t.<sourceId>.<随机>`：源被撤销或不在登记里 → 撤销；
 * - 中继令牌 `r.<deviceId>.<sourceId>.<随机>`：设备被撤销 / 过期、源被撤销，或访客的链接被撤销
 *   → 撤销；
 * - 访问令牌 `a.<deviceId>.<随机>`：同设备（校验在 sessions.ts）。
 */
import type { ControlPorts } from "./ports.ts";
import { isLiveDevice } from "./sessions.ts";
import { guestLinkId } from "./subjects.ts";

export async function isRevokedJti(
  context: {
    tenantId: string;
    ports: Pick<ControlPorts, "devices" | "sources" | "links">;
  },
  jti: string,
  nowMs: number,
): Promise<boolean> {
  const { tenantId, ports } = context;
  const [kind, id, sourceId] = jti.split(".");
  if (kind === "t") {
    const source = id ? await ports.sources.get(tenantId, id) : null;
    return !source || source.revokedAtMs !== null;
  }
  if (kind === "r" || kind === "a") {
    const device = id ? await ports.devices.get(tenantId, id) : null;
    if (!device || !isLiveDevice(device, nowMs)) return true;
    if (kind === "r") {
      const source = sourceId
        ? await ports.sources.get(tenantId, sourceId)
        : null;
      if (!source || source.revokedAtMs !== null) return true;
    }
    const linkId = guestLinkId(device.subject);
    if (linkId === null) return false;
    const link = await ports.links.get(tenantId, linkId);
    return !link || link.revokedAtMs !== null;
  }
  return false;
}
