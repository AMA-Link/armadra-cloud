/**
 * 撤销即时生效（中继设计 §2.7、§4.2）：`SourceDO` 把「没撤销」的判断与可信来源短缓存起来，
 * 所以 `ControlDO` 里任何会让令牌失效或改来源的写入（撤销源 / 设备 / 链接、改口令、源的可信
 * 来源变了）都要立即通知各源的对象作废缓存。这里给端口包一层：写完、确实变了才通知。
 *
 * 刷新令牌的轮换、最近在线时间这类不影响判断的写不通知（免得每 15 分钟叫醒一遍所有源）。
 */
import type { ControlPorts } from "@armadra/control-core/ports";

const revokedOf = (record: { revokedAtMs: number | null } | null) =>
  record?.revokedAtMs ?? null;

export function watchRevocations(
  ports: ControlPorts,
  changed: () => void,
): ControlPorts {
  return {
    ...ports,
    accounts: {
      ...ports.accounts,
      async resetPassword(tenantId, accountId, password, nowMs, options) {
        const done = await ports.accounts.resetPassword(
          tenantId,
          accountId,
          password,
          nowMs,
          options,
        );
        if (done) changed();
        return done;
      },
    },
    devices: {
      ...ports.devices,
      async update(tenantId, deviceId, fn) {
        let revoked = false;
        const result = await ports.devices.update(
          tenantId,
          deviceId,
          (draft) => {
            const before = revokedOf(draft);
            const out = fn(draft);
            if (revokedOf(draft) !== before) revoked = true;
            return out;
          },
        );
        if (revoked) changed();
        return result;
      },
    },
    sources: {
      ...ports.sources,
      async put(tenantId, sourceId, fn) {
        let touched = false;
        const result = await ports.sources.put(
          tenantId,
          sourceId,
          (existing) => {
            const next = fn(existing);
            touched =
              revokedOf(existing) !== revokedOf(next) ||
              JSON.stringify(existing?.trustedOrigins ?? null) !==
                JSON.stringify(next.trustedOrigins);
            return next;
          },
        );
        if (touched) changed();
        return result;
      },
      async update(tenantId, sourceId, fn) {
        let touched = false;
        const result = await ports.sources.update(
          tenantId,
          sourceId,
          (draft) => {
            const before = revokedOf(draft);
            const origins = JSON.stringify(draft?.trustedOrigins ?? null);
            const out = fn(draft);
            touched =
              revokedOf(draft) !== before ||
              JSON.stringify(draft?.trustedOrigins ?? null) !== origins;
            return out;
          },
        );
        if (touched) changed();
        return result;
      },
    },
    links: {
      ...ports.links,
      async update(tenantId, linkId, fn) {
        let revoked = false;
        const result = await ports.links.update(tenantId, linkId, (draft) => {
          const before = revokedOf(draft);
          const out = fn(draft);
          if (revokedOf(draft) !== before) revoked = true;
          return out;
        });
        if (revoked) changed();
        return result;
      },
      async revoke(tenantId, linkId, nowMs) {
        const sourceId = await ports.links.revoke(tenantId, linkId, nowMs);
        if (sourceId !== null) changed();
        return sourceId;
      },
    },
  };
}
