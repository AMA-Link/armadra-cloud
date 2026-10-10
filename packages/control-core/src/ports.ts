/**
 * 控制面的端口（ports）：服务只经这些接口读写状态，不碰文件、数据库或缓存。
 *
 * - personal：`apps/relay/src/personal/ports.ts` 用状态文件实现全部端口；
 * - saas：端口的 PostgreSQL / Redis 实现（后续包）。
 *
 * 约定：
 * - 存储端口的方法第一个参数是 `tenantId`（personal 恒为 {@link DEFAULT_TENANT}）；
 * - 方法都是异步的；`update` 类方法是**原子的读-改-写**：`fn` 在一份副本上同步执行，
 *   抛错则什么也不写，没改任何东西也不写；
 * - 签名钥（{@link KeyStore}）同步读：签发在热路径上，实现自己缓存。
 */
import type {
  DevicePlatform,
  Ed25519PublicJwk,
} from "@armadra/platform-protocol/assertion";
import type {
  LinkScope,
  sourceKindSchema,
} from "@armadra/platform-protocol/cloud-api";
import type { z } from "zod";

import type { ChallengeGate } from "./challenge.ts";
import type { EventBus } from "./events.ts";
import type { Signer } from "./signer.ts";

/** personal 唯一的租户。 */
export const DEFAULT_TENANT = "default";

// ------------------------------------------------------------ 记录

export interface PasswordRecord {
  kdf: "scrypt";
  cost: number;
  block: number;
  parallel: number;
  length: number;
  salt: string;
  hash: string;
}

export interface Lockout {
  failures: number;
  lockedUntilMs: number;
  lastFailureAtMs: number;
}

export interface AccountRecord extends Lockout {
  /** 不透明的 `sub`（personal `acct:<账号名>`，saas uuid）。 */
  accountId: string;
  /** 登录名。 */
  name: string;
  password: PasswordRecord;
  createdAtMs: number;
  updatedAtMs: number;
}

export interface SigningKeyRecord {
  kid: string;
  /** PKCS#8 DER 的 base64。 */
  privateKeyPkcs8: string;
  publicJwk: Ed25519PublicJwk;
  activeFromMs: number;
  retiredAtMs: number | null;
}

export type SourceKind = z.infer<typeof sourceKindSchema>;

export interface SourceRecord {
  sourceId: string;
  name: string;
  kind: SourceKind;
  publicJwk: Ed25519PublicJwk;
  coreVersion: string;
  capabilities: string[];
  registeredAtMs: number;
  lastSeenAtMs: number | null;
  revokedAtMs: number | null;
  trustedOrigins: string[];
}

export interface RegistrationTokenRecord {
  tokenHash: string;
  expiresAtMs: number;
}

export interface DeviceRecord {
  deviceId: string;
  /** `acct:<账号名>` 或 `guest:<linkId>:<随机>`。 */
  subject: string;
  platform: DevicePlatform;
  name: string;
  refreshHash: string;
  /** 上一个刷新令牌的哈希：它再出现就是重放，撤销整台设备。 */
  previousRefreshHash: string | null;
  rotation: number;
  createdAtMs: number;
  expiresAtMs: number;
  revokedAtMs: number | null;
  lastSeenAtMs: number | null;
}

export interface LinkRecord {
  linkId: string;
  sourceId: string;
  invitationId: string;
  label: string;
  role: string;
  secretHash: string;
  maxUses: number | null;
  uses: number;
  createdAtMs: number;
  expiresAtMs: number;
  revokedAtMs: number | null;
  /** 分享范围（0.3.0 起）；整台时不出现。已规范化（见 `links.ts` 的 `normalizeLinkScope`）。 */
  scope?: LinkScope;
}

/** 租户级设置。 */
export interface TenantSettings {
  /** 断言 / 令牌的 `iss`，也是页面来源与中继地址的根。 */
  issuer: string;
  allowGuests: boolean;
  trustedOrigins: string[];
}

// ------------------------------------------------------------ 存储端口

export interface AccountStore {
  findByName(tenantId: string, name: string): Promise<AccountRecord | null>;
  get(tenantId: string, accountId: string): Promise<AccountRecord | null>;
  /** 账号不存在时 `fn` 拿到 null。 */
  update<T>(
    tenantId: string,
    accountId: string,
    fn: (draft: AccountRecord | null) => T,
  ): Promise<T>;
  /**
   * 原子地：换口令记录、清锁，并撤销 `sub = accountId` 的全部未撤销设备（访客不受影响）。
   * 账号不存在答 false。
   */
  resetPassword(
    tenantId: string,
    accountId: string,
    password: PasswordRecord,
    nowMs: number,
  ): Promise<boolean>;
}

export interface DeviceStore {
  get(tenantId: string, deviceId: string): Promise<DeviceRecord | null>;
  listBySubject(tenantId: string, subject: string): Promise<DeviceRecord[]>;
  /** 新增一台；实现可以顺手清掉 `isStaleDevice` 为真的旧行。 */
  insert(tenantId: string, device: DeviceRecord, nowMs: number): Promise<void>;
  update<T>(
    tenantId: string,
    deviceId: string,
    fn: (draft: DeviceRecord | null) => T,
  ): Promise<T>;
  /** 批量记「最近使用」（只往后推）。 */
  touch(tenantId: string, seen: ReadonlyMap<string, number>): Promise<void>;
}

export interface SourceStore {
  get(tenantId: string, sourceId: string): Promise<SourceRecord | null>;
  list(tenantId: string): Promise<SourceRecord[]>;
  /** 原子替换：`fn` 拿到现有记录（或 null）答新记录。 */
  put(
    tenantId: string,
    sourceId: string,
    fn: (existing: SourceRecord | null) => SourceRecord,
  ): Promise<SourceRecord>;
  update<T>(
    tenantId: string,
    sourceId: string,
    fn: (draft: SourceRecord | null) => T,
  ): Promise<T>;
}

export interface RegistrationTokenStore {
  /**
   * 清掉过期的；同时有效的已满 `max` 条答 false（什么也不写），否则加入答 true。
   * `accountId` 是签发的账号（源登记后归它）。
   */
  add(
    tenantId: string,
    token: RegistrationTokenRecord & { accountId: string },
    options: { nowMs: number; max: number },
  ): Promise<boolean>;
  /** 一次性：命中未过期的同哈希就删掉，答签发它的账号；没命中答 null。 */
  consume(
    tenantId: string,
    tokenHash: string,
    nowMs: number,
  ): Promise<{ accountId: string } | null>;
}

export interface LinkStore {
  get(tenantId: string, linkId: string): Promise<LinkRecord | null>;
  /** 按创建顺序。 */
  list(tenantId: string, filter?: { sourceId?: string }): Promise<LinkRecord[]>;
  insert(tenantId: string, link: LinkRecord): Promise<void>;
  update<T>(
    tenantId: string,
    linkId: string,
    fn: (draft: LinkRecord | null) => T,
  ): Promise<T>;
  /**
   * 原子地：撤销链接与它名下的全部访客设备（`sub` 以 `guest:<linkId>:` 开头）。
   * 答链接指向的源；没有这条有效链接答 null。
   */
  revoke(
    tenantId: string,
    linkId: string,
    nowMs: number,
  ): Promise<string | null>;
}

export interface TenantStore {
  get(tenantId: string): Promise<TenantSettings>;
}

/** 签名钥（同步读，实现自己缓存）。 */
export interface KeyStore {
  issuer(): string;
  signingKeys(): readonly SigningKeyRecord[];
}

export interface ControlPorts {
  accounts: AccountStore;
  devices: DeviceStore;
  sources: SourceStore;
  registrations: RegistrationTokenStore;
  links: LinkStore;
  tenants: TenantStore;
}

// ------------------------------------------------------------ 中继侧端口

export interface LimitResult {
  allowed: boolean;
  retryAfterMs?: number;
}

/** 控制面用到的限流种类（与中继内核的 `LimitKind` 同名）。 */
export type ControlLimitKind = "login.ip" | "link.accept.ip";

export interface RateLimiter {
  hit(kind: ControlLimitKind, key: string): Promise<LimitResult>;
}

export type PresenceEvent =
  | {
      type: "sourceOnline";
      sourceId: string;
      coreVersion: string;
      atMs: number;
    }
  | { type: "sourceOffline"; sourceId: string; atMs: number };

export interface PresenceBus {
  subscribe(fn: (event: PresenceEvent) => void): () => void;
}

/** 要断开的客户端连接：按源、或按中继令牌 `sub` 的前缀（访客 `guest:<linkId>:`）。 */
export interface ConnectionFilter {
  sourceId?: string;
  subjectPrefix?: string;
}

/** 控制面对中继的依赖：查在线、关隧道、断开客户端。 */
export interface RelayPort {
  route(sourceId: string): Promise<{ coreVersion: string } | null>;
  closeTunnel(sourceId: string): void;
  disconnect(filter: ConnectionFilter): void;
}

export type PasswordRule =
  | "password_too_short"
  | "password_too_long"
  | "password_contains_name"
  | "password_too_common";

/** 口令策略：答第一条不过的规则，全过答 `undefined`。 */
export type PasswordPolicy = (
  password: string,
  names: readonly string[],
) => PasswordRule | undefined;

/** 服务的装配：一个租户的端口 + 签发 + 时钟 + 事件。 */
export interface ControlContext extends EventBus {
  tenantId: string;
  ports: ControlPorts;
  signer: Signer;
  limits: RateLimiter;
  presence: PresenceBus;
  relay: RelayPort;
  passwordPolicy: PasswordPolicy;
  now: () => number;
  /**
   * 源连隧道的节点地址（`sources.heartbeatHint` 的 `nodes[].url`，cloud-api §17）；缺省是
   * issuer 的 `wss://…/t/v1`。Workers 中继答 `…/t/v1?src=<sourceId>`，把隧道路由到源的对象。
   */
  tunnelUrl?: (sourceId: string) => string;
  /**
   * 人机挑战（cloud-api §16）：配置了 siteKey 与 secret 才有；没有 = 不要求挑战、`platform.info`
   * 不报 `challenge`。
   */
  challenge?: ChallengeGate;
}
