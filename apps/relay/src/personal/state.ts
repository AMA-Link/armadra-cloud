/**
 * `<data-dir>/state.json`（中继设计 §6.1）：个人中转唯一的状态文件。
 *
 * 只存账号、口令哈希、签名钥、源 / 设备 / 链接登记——不存任何消息内容、令牌明文或口令。
 * 心跳、在线、限流只在内存。
 *
 * - **原子写**：先写同目录临时文件（0600、fsync），再改名覆盖；断电时留下的是上一份完整文件。
 * - **多写者**：`serve` 与 `passwd` / `rotate-key` / `links revoke` 可能同时动它。每次写都在
 *   `state.lock` 目录锁里「读盘 → 有新修订就先合入 → 改 → 写回 revision+1」；`serve` 另外每秒
 *   看一眼文件，别人改过就重读（口令、密钥、撤销即时生效）。
 * - **不认识的文件拒绝启动**：解析失败、schema 不符、版本未知一律报错退出，绝不自动重建。
 */
import {
  chmodSync,
  closeSync,
  existsSync,
  fsyncSync,
  mkdirSync,
  openSync,
  readFileSync,
  renameSync,
  rmdirSync,
  rmSync,
  statSync,
  unwatchFile,
  watchFile,
  writeSync,
} from "node:fs";
import { join } from "node:path";

import { randomHex } from "@armadra/control-core/secrets";
import {
  devicePlatformSchema,
  ed25519PublicJwkSchema,
} from "@armadra/platform-protocol/assertion";
import {
  PLATFORM_NAME_MAX,
  linkScopeSchema,
  sourceKindSchema,
} from "@armadra/platform-protocol/cloud-api";
import { z } from "zod";

export const STATE_VERSION = 1;
export const STATE_FILE = "state.json";
const LOCK_DIR = "state.lock";
const LOCK_WAIT_MS = 5_000;
const LOCK_STALE_MS = 15_000;

const hex32 = z.string().regex(/^[0-9a-f]{32}$/);
const nullableMs = z.number().int().nullable();

export const passwordRecordSchema = z.object({
  kdf: z.literal("scrypt"),
  cost: z.number().int().positive(),
  block: z.number().int().positive(),
  parallel: z.number().int().positive(),
  length: z.number().int().positive(),
  salt: z.string().min(1),
  hash: z.string().min(1),
});

const accountSchema = z.object({
  name: z.string().min(1).max(64),
  password: passwordRecordSchema,
  createdAtMs: z.number().int(),
  updatedAtMs: z.number().int(),
  failures: z.number().int().nonnegative(),
  lockedUntilMs: z.number().int().nonnegative(),
  lastFailureAtMs: z.number().int().nonnegative(),
});

const signingKeySchema = z.object({
  kid: z.string().min(1),
  /** PKCS#8 DER 的 base64。 */
  privateKeyPkcs8: z.string().min(1),
  publicJwk: ed25519PublicJwkSchema,
  activeFromMs: z.number().int(),
  retiredAtMs: nullableMs,
});

const sourceSchema = z.object({
  sourceId: hex32,
  name: z.string(),
  kind: sourceKindSchema,
  publicJwk: ed25519PublicJwkSchema,
  coreVersion: z.string(),
  capabilities: z.array(z.string()),
  registeredAtMs: z.number().int(),
  lastSeenAtMs: nullableMs,
  revokedAtMs: nullableMs,
  trustedOrigins: z.array(z.string()),
});

const registrationTokenSchema = z.object({
  tokenHash: z.string().regex(/^[0-9a-f]{64}$/),
  expiresAtMs: z.number().int(),
});

const deviceSchema = z.object({
  deviceId: hex32,
  /** `acct:<账号名>` 或 `guest:<linkId>:<随机>`。 */
  subject: z.string().min(1),
  platform: devicePlatformSchema,
  name: z.string(),
  refreshHash: z.string().regex(/^[0-9a-f]{64}$/),
  /** 上一个刷新令牌的哈希：它再出现就是重放，撤销整台设备。 */
  previousRefreshHash: z
    .string()
    .regex(/^[0-9a-f]{64}$/)
    .nullable(),
  rotation: z.number().int().positive(),
  createdAtMs: z.number().int(),
  expiresAtMs: z.number().int(),
  revokedAtMs: nullableMs,
  lastSeenAtMs: nullableMs,
});

const linkSchema = z.object({
  linkId: z.string().regex(/^[0-9a-f]{16}$/),
  sourceId: hex32,
  invitationId: z.string().min(1),
  label: z.string(),
  role: z.string(),
  secretHash: z.string().regex(/^[0-9a-f]{64}$/),
  maxUses: z.number().int().positive().nullable(),
  uses: z.number().int().nonnegative(),
  createdAtMs: z.number().int(),
  expiresAtMs: z.number().int(),
  revokedAtMs: nullableMs,
  /** 分享范围（0.3.0 起）；整台时不写。旧文件没有这个键，照常读。 */
  scope: linkScopeSchema.optional(),
});

const rateLimitValue = z.number().int().min(1).max(1_000_000);

export const stateSchema = z.object({
  version: z.literal(STATE_VERSION),
  /** 每次写入加一；用来判断别的进程是否改过文件。 */
  revision: z.number().int().nonnegative(),
  issuer: z.string().min(1),
  account: accountSchema,
  signingKeys: z.array(signingKeySchema).min(1),
  sources: z.array(sourceSchema),
  registrationTokens: z.array(registrationTokenSchema),
  devices: z.array(deviceSchema),
  links: z.array(linkSchema),
  settings: z.object({
    allowGuests: z.boolean(),
    trustedOrigins: z.array(z.string()),
    /** 边缘限流额度（次 / 分钟），都可省略；命令行与环境变量优先于它。 */
    rateLimits: z
      .object({
        anonymous: rateLimitValue.optional(),
        preflight: rateLimitValue.optional(),
        token: rateLimitValue.optional(),
        account: rateLimitValue.optional(),
      })
      .optional(),
    /**
     * 服务端名称（`platform.info.name`，0.3.1 起）；命令行与环境变量优先于它，都没有时
     * 用 `--host`。旧文件没有这个键，照常读。
     */
    displayName: z.string().trim().min(1).max(PLATFORM_NAME_MAX).optional(),
    /** 受信代理（见 `http/client-ip.ts`）；命令行与环境变量优先于它。缺省不信任任何代理。 */
    trustedProxy: z
      .object({
        proxies: z.array(z.string().min(1).max(64)).max(64),
        prefer: z.enum(["forwarded-for", "real-ip"]).optional(),
      })
      .optional(),
  }),
});

/** 各行与控制核端口的记录同形（`ports.ts` 两个方向都赋值，类型检查守住）。 */
export type State = z.infer<typeof stateSchema>;

export type StateErrorCode =
  | "not_initialized"
  | "exists"
  | "invalid"
  | "locked";

/** 状态文件的问题：CLI 打印 message 并以 64 / 1 退出；不带文件内容。 */
export class StateError extends Error {
  readonly code: StateErrorCode;
  constructor(code: StateErrorCode, message: string) {
    super(message);
    this.name = "StateError";
    this.code = code;
  }
}

export function statePath(dataDir: string): string {
  return join(dataDir, STATE_FILE);
}

function parseState(text: string): State {
  let raw: unknown;
  try {
    raw = JSON.parse(text);
  } catch {
    throw new StateError("invalid", "状态文件不是合法的 JSON，拒绝启动");
  }
  const version = (raw as { version?: unknown } | null)?.version;
  if (version !== STATE_VERSION) {
    throw new StateError(
      "invalid",
      `状态文件版本不认识：${String(version)}，拒绝启动`,
    );
  }
  const parsed = stateSchema.safeParse(raw);
  if (!parsed.success)
    throw new StateError("invalid", "状态文件字段不符，拒绝启动");
  return parsed.data;
}

function readState(file: string): State {
  let text: string;
  try {
    text = readFileSync(file, "utf8");
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT")
      throw new StateError(
        "not_initialized",
        "还没有初始化：先运行 armadra-relay personal init",
      );
    throw error;
  }
  return parseState(text);
}

function writeAtomic(file: string, state: State, exclusive = false): void {
  const payload = `${JSON.stringify(state, null, 2)}\n`;
  const temporary = `${file}.${process.pid}.${randomHex(4)}.tmp`;
  const fd = openSync(temporary, "wx", 0o600);
  try {
    writeSync(fd, payload);
    fsyncSync(fd);
  } finally {
    closeSync(fd);
  }
  chmodSync(temporary, 0o600);
  if (exclusive && existsSync(file)) {
    rmSync(temporary, { force: true });
    throw new StateError("exists", "已经初始化过：状态文件已存在");
  }
  renameSync(temporary, file);
}

function sleepSync(ms: number): void {
  Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms);
}

/**
 * 目录锁：`mkdir` 是原子的。超过 {@link LOCK_STALE_MS} 的锁视为崩溃遗留。答释放函数（等不到就
 * `locked`）；跨 await 的持锁（迁到 PostgreSQL）直接用它。
 */
export function acquireLock(dataDir: string): () => void {
  const lock = join(dataDir, LOCK_DIR);
  const deadline = Date.now() + LOCK_WAIT_MS;
  for (;;) {
    try {
      mkdirSync(lock, { mode: 0o700 });
      break;
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "EEXIST") throw error;
      try {
        if (Date.now() - statSync(lock).mtimeMs > LOCK_STALE_MS) {
          rmdirSync(lock);
          continue;
        }
      } catch {
        continue;
      }
      if (Date.now() > deadline)
        throw new StateError("locked", "状态文件被另一个进程占用");
      sleepSync(10);
    }
  }
  return () => {
    try {
      rmdirSync(lock);
    } catch {
      // 锁已被当作遗留清掉：不影响这次写入。
    }
  };
}

export function withLock<T>(dataDir: string, fn: () => T): T {
  const release = acquireLock(dataDir);
  try {
    return fn();
  } finally {
    release();
  }
}

export class StateStore {
  readonly dataDir: string;
  private state: State;
  private watching = false;

  private constructor(dataDir: string, state: State) {
    this.dataDir = dataDir;
    this.state = state;
  }

  /** 读已有的状态文件；没有 → `not_initialized`，坏的 → `invalid`。 */
  static open(dataDir: string): StateStore {
    return new StateStore(dataDir, readState(statePath(dataDir)));
  }

  /** 首次写入（`init`）；已有文件 → `exists`，不覆盖。 */
  static create(dataDir: string, state: State): StateStore {
    mkdirSync(dataDir, { recursive: true, mode: 0o700 });
    const file = statePath(dataDir);
    if (existsSync(file))
      throw new StateError("exists", "已经初始化过：状态文件已存在");
    const checked = stateSchema.parse(state);
    withLock(dataDir, () => writeAtomic(file, checked, true));
    return new StateStore(dataDir, checked);
  }

  get current(): State {
    return this.state;
  }

  /**
   * 读-改-写：锁内重读盘上的文件（别的进程可能改过），在副本上执行 `fn`，校验后写回。
   * `fn` 抛错时什么也不写。
   */
  update<T>(fn: (draft: State) => T): T {
    return withLock(this.dataDir, () => {
      const file = statePath(this.dataDir);
      const onDisk = readState(file);
      const base = onDisk.revision >= this.state.revision ? onDisk : this.state;
      const draft = structuredClone(base);
      const result = fn(draft);
      if (JSON.stringify(draft) === JSON.stringify(base)) {
        // 没改任何东西（比如只是查了一下就拒绝）：不写盘。
        this.state = base;
        return result;
      }
      draft.revision = base.revision + 1;
      const checked = stateSchema.parse(draft);
      writeAtomic(file, checked);
      this.state = checked;
      return result;
    });
  }

  /** 盘上的修订更新就重读；答是否重读了。坏文件保留内存里的那份并抛出。 */
  reload(): boolean {
    const onDisk = readState(statePath(this.dataDir));
    if (onDisk.revision <= this.state.revision) return false;
    this.state = onDisk;
    return true;
  }

  /** 每 `intervalMs` 看一次文件，别的进程改过就重读并回调。 */
  watch(
    onReload: () => void,
    onError: (error: unknown) => void,
    intervalMs = 1000,
  ): () => void {
    const file = statePath(this.dataDir);
    if (this.watching) return () => {};
    this.watching = true;
    const listener = () => {
      try {
        if (this.reload()) onReload();
      } catch (error) {
        onError(error);
      }
    };
    watchFile(file, { interval: intervalMs, persistent: false }, listener);
    return () => {
      this.watching = false;
      unwatchFile(file, listener);
    };
  }
}
