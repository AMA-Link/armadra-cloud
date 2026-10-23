/**
 * 个人中转状态文件（schema v1）的 zod 结构，只给 make-fixtures.mjs 生成夹具用。
 * 运行时代码已是 Go（go/internal/store/statefile/schema.go 逐条对应这里）；TypeScript 版个人中转
 * 删除后，夹具生成只留这一份最小的结构与 snapshot.ts 的 stateToSnapshot。
 */
import {
  devicePlatformSchema,
  ed25519PublicJwkSchema,
} from "../../../packages/platform-protocol/src/assertion/index.ts";
import {
  PLATFORM_NAME_MAX,
  linkScopeSchema,
  sourceKindSchema,
} from "../../../packages/platform-protocol/src/cloud-api/index.ts";
import { z } from "zod";

export const STATE_VERSION = 1;

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
