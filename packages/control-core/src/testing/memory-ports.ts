/**
 * 测试夹具：端口的内存实现（多租户，按 `tenantId` 隔离）与一套可直接跑服务的装配。
 *
 * 语义与端口约定一致：`update` 在副本上执行 `fn`，抛错不写；读方法答副本。它是参照实现：
 * `ports-contract.ts` 与 `services-contract.ts` 两套合同测试对它和每个存储实现（SQLite……）各跑一遍。
 */
import { createEventBus, type ControlEvent } from "../events.ts";
import type {
  AccountRecord,
  ConnectionFilter,
  ControlContext,
  ControlLimitKind,
  ControlPorts,
  DeviceRecord,
  LimitResult,
  LinkRecord,
  PasswordPolicy,
  PresenceEvent,
  RegistrationTokenRecord,
  SigningKeyRecord,
  SourceRecord,
  TenantSettings,
} from "../ports.ts";
import { sameHex } from "../secrets.ts";
import { isStaleDevice } from "../sessions.ts";
import { generateSigningKey, Signer } from "../signer.ts";
import { guestPrefix } from "../subjects.ts";

interface TenantState {
  settings: TenantSettings;
  accounts: AccountRecord[];
  devices: DeviceRecord[];
  sources: SourceRecord[];
  registrations: (RegistrationTokenRecord & { accountId: string })[];
  links: LinkRecord[];
}

const copy = <T>(value: T): T => structuredClone(value);

/** 原子读-改-写：在副本上跑，成功才换回去。 */
function mutate<T>(
  tenants: Map<string, TenantState>,
  tenantId: string,
  fn: (draft: TenantState) => T,
): T {
  const draft = copy(tenant(tenants, tenantId));
  const result = fn(draft);
  tenants.set(tenantId, draft);
  return result;
}

function tenant(
  tenants: Map<string, TenantState>,
  tenantId: string,
): TenantState {
  const found = tenants.get(tenantId);
  if (!found) throw new Error(`没有这个租户：${tenantId}`);
  return found;
}

/** 端口 + 测试装配用的两个入口（合同测试要求存储实现也提供）。 */
export interface MemoryPorts extends ControlPorts {
  /** 建一个租户（测试装配用）。 */
  addTenant(tenantId: string, settings: TenantSettings): void;
  /** 直接放一个账号（测试装配用）。 */
  addAccount(tenantId: string, account: AccountRecord): void;
}

export function createMemoryPorts(): MemoryPorts {
  const tenants = new Map<string, TenantState>();
  const read = (tenantId: string) => tenant(tenants, tenantId);
  const row = <R>(rows: R[], match: (row: R) => boolean) =>
    rows.find(match) ?? null;

  return {
    addTenant(tenantId, settings) {
      tenants.set(tenantId, {
        settings: copy(settings),
        accounts: [],
        devices: [],
        sources: [],
        registrations: [],
        links: [],
      });
    },
    addAccount(tenantId, account) {
      mutate(tenants, tenantId, (draft) => {
        draft.accounts.push(copy(account));
      });
    },

    accounts: {
      async findByName(tenantId, name) {
        return copy(row(read(tenantId).accounts, (a) => a.name === name));
      },
      async get(tenantId, accountId) {
        return copy(
          row(read(tenantId).accounts, (a) => a.accountId === accountId),
        );
      },
      async update(tenantId, accountId, fn) {
        return mutate(tenants, tenantId, (draft) =>
          fn(row(draft.accounts, (a) => a.accountId === accountId)),
        );
      },
      async resetPassword(tenantId, accountId, password, nowMs, options) {
        return mutate(tenants, tenantId, (draft) => {
          const account = row(draft.accounts, (a) => a.accountId === accountId);
          if (!account) return false;
          Object.assign(account, {
            password: copy(password),
            updatedAtMs: nowMs,
            failures: 0,
            lockedUntilMs: 0,
          });
          for (const device of draft.devices)
            if (
              device.subject === accountId &&
              device.revokedAtMs === null &&
              device.deviceId !== options?.keepDeviceId
            )
              device.revokedAtMs = nowMs;
          return true;
        });
      },
    },

    devices: {
      async get(tenantId, deviceId) {
        return copy(
          row(read(tenantId).devices, (d) => d.deviceId === deviceId),
        );
      },
      async listBySubject(tenantId, subject) {
        return copy(
          read(tenantId).devices.filter((d) => d.subject === subject),
        );
      },
      async insert(tenantId, device, nowMs) {
        mutate(tenants, tenantId, (draft) => {
          draft.devices = draft.devices.filter((d) => !isStaleDevice(d, nowMs));
          draft.devices.push(copy(device));
        });
      },
      async update(tenantId, deviceId, fn) {
        return mutate(tenants, tenantId, (draft) =>
          fn(row(draft.devices, (d) => d.deviceId === deviceId)),
        );
      },
      async touch(tenantId, seen) {
        mutate(tenants, tenantId, (draft) => {
          for (const device of draft.devices) {
            const at = seen.get(device.deviceId);
            if (at !== undefined && (device.lastSeenAtMs ?? 0) < at)
              device.lastSeenAtMs = at;
          }
        });
      },
    },

    sources: {
      async get(tenantId, sourceId) {
        return copy(
          row(read(tenantId).sources, (s) => s.sourceId === sourceId),
        );
      },
      async list(tenantId) {
        return copy(read(tenantId).sources);
      },
      async put(tenantId, sourceId, fn) {
        return mutate(tenants, tenantId, (draft) => {
          const record = fn(
            copy(row(draft.sources, (s) => s.sourceId === sourceId)),
          );
          draft.sources = draft.sources.filter((s) => s.sourceId !== sourceId);
          draft.sources.push(record);
          return copy(record);
        });
      },
      async update(tenantId, sourceId, fn) {
        return mutate(tenants, tenantId, (draft) =>
          fn(row(draft.sources, (s) => s.sourceId === sourceId)),
        );
      },
    },

    registrations: {
      async add(tenantId, token, options) {
        const live = read(tenantId).registrations.filter(
          (r) => r.expiresAtMs > options.nowMs,
        );
        if (live.length >= options.max) return false;
        mutate(tenants, tenantId, (draft) => {
          draft.registrations = [...live, copy(token)];
        });
        return true;
      },
      async consume(tenantId, tokenHash, nowMs) {
        return mutate(tenants, tenantId, (draft) => {
          const index = draft.registrations.findIndex(
            (r) => r.expiresAtMs > nowMs && sameHex(r.tokenHash, tokenHash),
          );
          if (index < 0) return null;
          const [hit] = draft.registrations.splice(index, 1);
          return hit ? { accountId: hit.accountId } : null;
        });
      },
    },

    links: {
      async get(tenantId, linkId) {
        return copy(row(read(tenantId).links, (l) => l.linkId === linkId));
      },
      async list(tenantId, filter = {}) {
        return copy(
          read(tenantId).links.filter(
            (l) =>
              filter.sourceId === undefined || l.sourceId === filter.sourceId,
          ),
        );
      },
      async insert(tenantId, link) {
        mutate(tenants, tenantId, (draft) => {
          draft.links.push(copy(link));
        });
      },
      async update(tenantId, linkId, fn) {
        return mutate(tenants, tenantId, (draft) =>
          fn(row(draft.links, (l) => l.linkId === linkId)),
        );
      },
      async revoke(tenantId, linkId, nowMs) {
        return mutate(tenants, tenantId, (draft) => {
          const link = row(
            draft.links,
            (l) => l.linkId === linkId && l.revokedAtMs === null,
          );
          if (!link) return null;
          link.revokedAtMs = nowMs;
          for (const device of draft.devices)
            if (
              device.subject.startsWith(guestPrefix(linkId)) &&
              device.revokedAtMs === null
            )
              device.revokedAtMs = nowMs;
          return link.sourceId;
        });
      },
    },

    tenants: {
      async get(tenantId) {
        return copy(read(tenantId).settings);
      },
    },
  };
}

export interface MemoryHarness {
  context: ControlContext;
  ports: MemoryPorts;
  clock: { now: number };
  events: ControlEvent[];
  disconnects: ConnectionFilter[];
  closedTunnels: string[];
  /** 当作在线的源（`relay.route` 答它们）。 */
  online: Map<string, string>;
  presence: { publish(event: PresenceEvent): void };
}

/** 一个租户、一把签名钥、不限流（除非给了 `limit`）、可控时钟的装配。 */
export function memoryHarness(
  options: {
    tenantId?: string;
    issuer?: string;
    nowMs?: number;
    allowGuests?: boolean;
    passwordPolicy?: PasswordPolicy;
    limit?: (kind: ControlLimitKind, key: string) => LimitResult;
    /** 换一个存储实现（缺省内存参照）；合同测试用。 */
    ports?: MemoryPorts;
  } = {},
): MemoryHarness {
  const tenantId = options.tenantId ?? "t1";
  const issuer = options.issuer ?? "https://cloud.armadra.test";
  const clock = { now: options.nowMs ?? Date.UTC(2026, 9, 10, 12) };
  const now = () => clock.now;
  const ports = options.ports ?? createMemoryPorts();
  ports.addTenant(tenantId, {
    issuer,
    allowGuests: options.allowGuests ?? true,
    trustedOrigins: [],
  });
  const keys: SigningKeyRecord[] = [generateSigningKey("k1", clock.now)];
  const signer = new Signer(
    { issuer: () => issuer, signingKeys: () => keys },
    now,
  );
  const events: ControlEvent[] = [];
  const disconnects: ConnectionFilter[] = [];
  const closedTunnels: string[] = [];
  const online = new Map<string, string>();
  const presenceListeners = new Set<(event: PresenceEvent) => void>();
  const bus = createEventBus();
  bus.subscribe((event) => events.push(event));
  const context: ControlContext = {
    tenantId,
    ports,
    signer,
    limits: {
      hit: async (kind, key) => options.limit?.(kind, key) ?? { allowed: true },
    },
    presence: {
      subscribe(fn) {
        presenceListeners.add(fn);
        return () => presenceListeners.delete(fn);
      },
    },
    relay: {
      route: async (sourceId) => {
        const coreVersion = online.get(sourceId);
        return coreVersion === undefined ? null : { coreVersion };
      },
      closeTunnel: (sourceId) => closedTunnels.push(sourceId),
      disconnect: (filter) => disconnects.push(filter),
    },
    passwordPolicy: options.passwordPolicy ?? (() => undefined),
    now,
    ...bus,
  };
  return {
    context,
    ports,
    clock,
    events,
    disconnects,
    closedTunnels,
    online,
    presence: {
      publish(event) {
        for (const listener of [...presenceListeners]) listener(event);
      },
    },
  };
}
