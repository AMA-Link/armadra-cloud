/**
 * 控制核端口的状态文件实现（personal）。只有一个租户（`DEFAULT_TENANT`），全部端口落在同一份
 * `state.json`：每个写方法是一次 `StateStore.update`（目录锁内读盘 → 改副本 → 校验写回），
 * `fn` 抛错不写、没改不写；读方法读内存里的那份（`serve` 每秒看一眼文件，别人改过就重读）。
 *
 * 方法虽然是 async，读写都在调用的那一刻同步完成。
 */
import type {
  AccountRecord,
  ControlPorts,
  DeviceRecord,
  KeyStore,
  LinkRecord,
  SourceRecord,
} from "@armadra/control-core/ports";
import { sameHex } from "@armadra/control-core/secrets";
import { isStaleDevice } from "@armadra/control-core/sessions";
import { guestPrefix, ownerSubject } from "@armadra/control-core/subjects";

import type { State, StateStore } from "./state.ts";

/** 返回副本：调用方改它不会动到内存里的状态。 */
const copy = <T>(value: T): T => structuredClone(value);

export function stateKeys(store: StateStore): KeyStore {
  return {
    issuer: () => store.current.issuer,
    signingKeys: () => store.current.signingKeys,
  };
}

function updateRow<R extends object, T>(
  store: StateStore,
  pick: (draft: State) => R[],
  match: (row: R) => boolean,
  fn: (draft: R | null) => T,
): T {
  return store.update((draft) => fn(pick(draft).find(match) ?? null));
}

/** 状态文件里的账号没有 id：它就是 `acct:<账号名>`。 */
function withId(account: State["account"]): AccountRecord {
  return { ...copy(account), accountId: ownerSubject(account.name) };
}

export function createStatePorts(store: StateStore): ControlPorts {
  const owner = (state: State, accountId: string) =>
    accountId === ownerSubject(state.account.name) ? state.account : null;
  return {
    accounts: {
      async findByName(_tenantId, name) {
        const account = store.current.account;
        return account.name === name ? withId(account) : null;
      },
      async get(_tenantId, accountId) {
        const account = owner(store.current, accountId);
        return account ? withId(account) : null;
      },
      async update(_tenantId, accountId, fn) {
        return store.update((draft) => {
          const account = owner(draft, accountId);
          if (!account) return fn(null);
          const view = withId(account);
          const result = fn(view);
          // 写回除 id 以外的字段（id 由账号名决定，不落盘）。
          const { accountId: _id, ...fields } = view;
          Object.assign(account, fields);
          return result;
        });
      },
      async resetPassword(_tenantId, accountId, password, nowMs, options) {
        return store.update((draft) => {
          const account = owner(draft, accountId);
          if (!account) return false;
          account.password = copy(password);
          account.updatedAtMs = nowMs;
          account.failures = 0;
          account.lockedUntilMs = 0;
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
      async get(_tenantId, deviceId) {
        const device = store.current.devices.find(
          (row) => row.deviceId === deviceId,
        );
        return device ? copy(device) : null;
      },
      async listBySubject(_tenantId, subject) {
        return copy(
          store.current.devices.filter((row) => row.subject === subject),
        );
      },
      async insert(_tenantId, device: DeviceRecord, nowMs) {
        store.update((draft) => {
          // 顺手清掉早已失效的设备行，状态文件不无限长。
          draft.devices = draft.devices.filter(
            (row) => !isStaleDevice(row, nowMs),
          );
          draft.devices.push(copy(device));
        });
      },
      async update(_tenantId, deviceId, fn) {
        return updateRow(
          store,
          (draft) => draft.devices,
          (row) => row.deviceId === deviceId,
          fn,
        );
      },
      async touch(_tenantId, seen) {
        store.update((draft) => {
          for (const device of draft.devices) {
            const at = seen.get(device.deviceId);
            if (at !== undefined && (device.lastSeenAtMs ?? 0) < at)
              device.lastSeenAtMs = at;
          }
        });
      },
    },

    sources: {
      async get(_tenantId, sourceId) {
        const source = store.current.sources.find(
          (row) => row.sourceId === sourceId,
        );
        return source ? copy(source) : null;
      },
      async list() {
        return copy(store.current.sources);
      },
      async put(_tenantId, sourceId, fn) {
        return store.update((draft) => {
          const existing =
            draft.sources.find((row) => row.sourceId === sourceId) ?? null;
          const record: SourceRecord = fn(existing ? copy(existing) : null);
          draft.sources = draft.sources.filter(
            (row) => row.sourceId !== sourceId,
          );
          draft.sources.push(record);
          return copy(record);
        });
      },
      async update(_tenantId, sourceId, fn) {
        return updateRow(
          store,
          (draft) => draft.sources,
          (row) => row.sourceId === sourceId,
          fn,
        );
      },
    },

    registrations: {
      async add(_tenantId, token, options) {
        const full = Symbol("full");
        try {
          store.update((draft) => {
            draft.registrationTokens = draft.registrationTokens.filter(
              (row) => row.expiresAtMs > options.nowMs,
            );
            if (draft.registrationTokens.length >= options.max) throw full;
            // 只有一个账号：签发人不必落盘。
            draft.registrationTokens.push({
              tokenHash: token.tokenHash,
              expiresAtMs: token.expiresAtMs,
            });
          });
          return true;
        } catch (error) {
          if (error === full) return false;
          throw error;
        }
      },
      async consume(_tenantId, tokenHash, nowMs) {
        return store.update((draft) => {
          const index = draft.registrationTokens.findIndex(
            (row) =>
              row.expiresAtMs > nowMs && sameHex(row.tokenHash, tokenHash),
          );
          if (index < 0) return null;
          draft.registrationTokens.splice(index, 1);
          return { accountId: ownerSubject(draft.account.name) };
        });
      },
    },

    links: {
      async get(_tenantId, linkId) {
        const link = store.current.links.find((row) => row.linkId === linkId);
        return link ? copy(link) : null;
      },
      async list(_tenantId, filter = {}) {
        return copy(
          store.current.links.filter(
            (row) =>
              filter.sourceId === undefined || row.sourceId === filter.sourceId,
          ),
        );
      },
      async insert(_tenantId, link: LinkRecord) {
        store.update((draft) => {
          draft.links.push(copy(link));
        });
      },
      async update(_tenantId, linkId, fn) {
        return updateRow(
          store,
          (draft) => draft.links,
          (row) => row.linkId === linkId,
          fn,
        );
      },
      async revoke(_tenantId, linkId, nowMs) {
        return store.update((draft) => {
          const link = draft.links.find(
            (row) => row.linkId === linkId && row.revokedAtMs === null,
          );
          if (!link) return null;
          link.revokedAtMs = nowMs;
          const prefix = guestPrefix(linkId);
          for (const device of draft.devices)
            if (
              device.subject.startsWith(prefix) &&
              device.revokedAtMs === null
            )
              device.revokedAtMs = nowMs;
          return link.sourceId;
        });
      },
    },

    tenants: {
      async get() {
        const state = store.current;
        return {
          issuer: state.issuer,
          allowGuests: state.settings.allowGuests,
          trustedOrigins: [...state.settings.trustedOrigins],
        };
      },
    },
  };
}
