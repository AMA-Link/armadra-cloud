/**
 * 握手 `armadra-e2e/1`（设计 §4.4）：「临时 X25519 + HKDF 转录链 + Ed25519 身份签名」，
 * 三条消息，之后两个方向各一把 AES-256-GCM 记录密钥。
 *
 * ```text
 * h0 = SHA-256("armadra-e2e/1" || sourceId)
 * m1 客户端 → core   ver | flags | e_c | nonce_c | pinHint            h1 = SHA-256(h0 || m1)
 *    dh = X25519(e, e')                                               h2 = SHA-256(h1 || e_s)
 *    k_hs_s2c / k_hs_c2s = HKDF(dh, salt = h2, "… hs s2c" / "… hs c2s")
 * m2 core → 客户端   e_s | AEAD(k_hs_s2c, 0, aad = h2,
 *                            coreIdentityPub | Ed25519(core, "… core" || h2 || coreIdentityPub))
 *                                                                     h3 = SHA-256(h2 || c2)
 * m3 客户端 → core   AEAD(k_hs_c2s, 0, aad = h3,
 *                         devicePub | Ed25519(dev, "… device" || h3 || devicePub)
 *                         | platform | name | proof?)                  h4 = SHA-256(h3 || c3)
 *    proof = HMAC-SHA256(HKDF(secret, "", "… proof"), h3)          （只在首次配对 / 访客加入）
 * 记录密钥  HKDF(dh, salt = h4, "… c2s" / "… s2c", 36) = key 32 | nonce salt 4
 * SAS      HKDF(dh, salt = h4, "… sas", 4) 取 u32 BE mod 10⁶，补零到 6 位
 * ```
 *
 * 前向保密来自临时钥；身份靠签名，签名输入带上了 `h`，`h` 绑定 sourceId、两把临时钥与两把
 * 身份钥（经密文），所以中间人两侧的 SAS 必然不同。握手密钥各只用一次，nonce 固定为零。
 *
 * 这个模块与记录层、复用层分开：日后换成别的握手只动这里与 `fixtures/e2e/`。
 */
import { hex32Schema } from "../tunnel/handshake.ts";
import { utf8 } from "../internal/bytes.ts";
import {
  E2E_FLAGS,
  E2E_LABELS,
  E2E_PROTOCOL,
  E2E_VERSION,
  LIMITS_E2E,
} from "./constants.ts";
import {
  decodeDevice,
  decodeHello,
  deviceIdOf,
  encodeDevice,
  encodeHello,
  KEY_BYTES,
  M2_BYTES,
  NONCE_C_BYTES,
  PIN_HINT_BYTES,
  pinHint,
  SIGNATURE_BYTES,
  type E2eHello,
} from "./messages.ts";
import type { DevicePlatform } from "../assertion/claims.ts";
import {
  aeadOpen,
  aeadSeal,
  concatBytes,
  constantTimeEqual,
  E2eError,
  ed25519Verify,
  generateX25519,
  hkdf,
  hmacSha256,
  hmacSha256Verify,
  importAeadKey,
  label,
  randomBytes,
  sha256,
  x25519,
  type E2eSigner,
  type X25519KeyPair,
} from "./primitives.ts";

/** 一个方向的记录密钥：AES-256-GCM 密钥 32 字节 + nonce 前缀 4 字节。 */
export interface E2eDirectionSecret {
  key: Uint8Array;
  salt: Uint8Array;
}

/** 握手结果：两端算出的完全相同。 */
export interface E2eSession {
  /** 最终转录哈希 h4。 */
  transcript: Uint8Array;
  clientToCore: E2eDirectionSecret;
  coreToClient: E2eDirectionSecret;
  /** 6 位十进制校验码，两端各自显示、由人核对（本机确认）。 */
  sas: string;
  /** m1 开了填充：两个方向的记录都填充到 64 字节的倍数。 */
  padding: boolean;
  /** core 的 Ed25519 身份公钥。 */
  coreIdentity: Uint8Array;
  /** 客户端设备的 Ed25519 公钥。 */
  devicePublicKey: Uint8Array;
}

const ZERO_NONCE = new Uint8Array(12);
const EMPTY = new Uint8Array(0);

function transcriptStart(sourceId: string): Promise<Uint8Array> {
  if (!hex32Schema.safeParse(sourceId).success)
    throw new E2eError("bad_message", "sourceId 须为 32 位小写 hex");
  return sha256(label(E2E_PROTOCOL), utf8(sourceId));
}

function signingInput(
  which: string,
  h: Uint8Array,
  publicKey: Uint8Array,
): Uint8Array {
  return concatBytes(label(which), h, publicKey);
}

async function handshakeKey(dh: Uint8Array, h2: Uint8Array, info: string) {
  return importAeadKey(await hkdf(dh, h2, info, 32));
}

async function proofKey(secret: Uint8Array): Promise<Uint8Array> {
  if (secret.length < 16)
    throw new E2eError("bad_message", "配对秘密至少 16 字节");
  return hkdf(secret, EMPTY, E2E_LABELS.proof, 32);
}

async function direction(
  dh: Uint8Array,
  h4: Uint8Array,
  info: string,
): Promise<E2eDirectionSecret> {
  const bytes = await hkdf(dh, h4, info, 36);
  return { key: bytes.slice(0, 32), salt: bytes.slice(32) };
}

async function deriveSession(
  dh: Uint8Array,
  h4: Uint8Array,
  flags: number,
  coreIdentity: Uint8Array,
  devicePublicKey: Uint8Array,
): Promise<E2eSession> {
  const sasBytes = await hkdf(dh, h4, E2E_LABELS.sas, 4);
  const sasValue =
    new DataView(sasBytes.buffer, sasBytes.byteOffset, 4).getUint32(0, false) %
    1_000_000;
  return {
    transcript: h4,
    clientToCore: await direction(dh, h4, E2E_LABELS.clientToCore),
    coreToClient: await direction(dh, h4, E2E_LABELS.coreToClient),
    sas: String(sasValue).padStart(6, "0"),
    padding: (flags & E2E_FLAGS.padding) !== 0,
    coreIdentity,
    devicePublicKey,
  };
}

function signatureLength(signature: Uint8Array): Uint8Array {
  if (signature.length !== SIGNATURE_BYTES)
    throw new E2eError("signature_invalid", "签名方没有答 64 字节");
  return signature;
}

// ------------------------------------------------------------------ 客户端

export interface ClientHandshakeOptions {
  sourceId: string;
  /** 钉住的 core 身份公钥；没有（首次 TOFU）时 m2 里的公钥由调用方记录。 */
  pinnedIdentity?: Uint8Array | null;
  /** {@link E2E_FLAGS} 的组合，缺省 0。 */
  flags?: number;
  /** 只给测试向量：固定临时钥与 nonce。 */
  ephemeral?: X25519KeyPair;
  nonce?: Uint8Array;
}

export interface ClientFinishOptions {
  device: E2eSigner;
  platform: DevicePlatform;
  /** 设备显示名，UTF-8 不超过 64 字节。 */
  name: string;
  /** 首次配对的秘密或访客邀请令牌（字节）；有就带 `proof`。 */
  proofSecret?: Uint8Array;
}

/**
 * 客户端一侧：`start` 产出 m1 → `readCore(m2)` 验 core 身份 → `finish` 产出 m3 与会话密钥。
 * 任何一步抛 {@link E2eError} 都应当关闭通道，不重试同一个对象。
 */
export class ClientHandshake {
  readonly m1: Uint8Array;
  readonly flags: number;
  private step: "m2" | "m3" | "done" = "m2";
  private dh: Uint8Array | null = null;
  private h2: Uint8Array | null = null;
  private h3: Uint8Array | null = null;
  private identity: Uint8Array | null = null;
  private readonly ephemeral: X25519KeyPair;
  private readonly pinned: Uint8Array | null;
  private readonly h1: Uint8Array;

  private constructor(
    ephemeral: X25519KeyPair,
    pinned: Uint8Array | null,
    h1: Uint8Array,
    m1: Uint8Array,
    flags: number,
  ) {
    this.ephemeral = ephemeral;
    this.pinned = pinned;
    this.h1 = h1;
    this.m1 = m1;
    this.flags = flags;
  }

  static async start(
    options: ClientHandshakeOptions,
  ): Promise<ClientHandshake> {
    const h0 = await transcriptStart(options.sourceId);
    const ephemeral = options.ephemeral ?? (await generateX25519());
    const pinned = options.pinnedIdentity ?? null;
    if (pinned && pinned.length !== KEY_BYTES)
      throw new E2eError("bad_message", "钉住的身份公钥须为 32 字节");
    const flags = options.flags ?? 0;
    const hello: E2eHello = {
      version: E2E_VERSION,
      flags,
      ephemeral: ephemeral.publicKey,
      nonce: options.nonce ?? randomBytes(NONCE_C_BYTES),
      pinHint: pinned ? await pinHint(pinned) : new Uint8Array(PIN_HINT_BYTES),
    };
    const m1 = encodeHello(hello);
    return new ClientHandshake(
      ephemeral,
      pinned,
      await sha256(h0, m1),
      m1,
      flags,
    );
  }

  /** 读 m2：答 core 的身份公钥（已验签；钉住了就已比对）。 */
  async readCore(m2: Uint8Array): Promise<{ coreIdentity: Uint8Array }> {
    if (this.step !== "m2") throw new E2eError("state", "m2 已经读过");
    this.step = "done";
    if (m2.length !== M2_BYTES)
      throw new E2eError("bad_message", `m2 应为 ${M2_BYTES} 字节`);
    const ephemeralCore = m2.slice(0, KEY_BYTES);
    const sealed = m2.slice(KEY_BYTES);
    const dh = await x25519(this.ephemeral.privateKey, ephemeralCore);
    const h2 = await sha256(this.h1, ephemeralCore);
    const key = await handshakeKey(dh, h2, E2E_LABELS.handshakeCoreToClient);
    const plaintext = await aeadOpen(key, ZERO_NONCE, h2, sealed);
    const coreIdentity = plaintext.slice(0, KEY_BYTES);
    const signature = plaintext.slice(KEY_BYTES);
    if (this.pinned && !constantTimeEqual(coreIdentity, this.pinned))
      throw new E2eError("identity_mismatch", "core 身份公钥与钉住的不同");
    const ok = await ed25519Verify(
      coreIdentity,
      signature,
      signingInput(E2E_LABELS.coreSignature, h2, coreIdentity),
    );
    if (!ok) throw new E2eError("signature_invalid", "core 的转录签名不对");
    this.dh = dh;
    this.h2 = h2;
    this.h3 = await sha256(h2, sealed);
    this.identity = coreIdentity;
    this.step = "m3";
    return { coreIdentity };
  }

  /** 写 m3：设备签转录（可选带 `proof`），答 m3 与会话密钥。 */
  async finish(
    options: ClientFinishOptions,
  ): Promise<{ m3: Uint8Array; session: E2eSession }> {
    if (
      this.step !== "m3" ||
      !this.dh ||
      !this.h2 ||
      !this.h3 ||
      !this.identity
    )
      throw new E2eError("state", "还没读 m2");
    this.step = "done";
    const devicePub = options.device.publicKey;
    if (devicePub.length !== KEY_BYTES)
      throw new E2eError("bad_message", "设备公钥须为 32 字节");
    const signature = signatureLength(
      await options.device.sign(
        signingInput(E2E_LABELS.deviceSignature, this.h3, devicePub),
      ),
    );
    const proof = options.proofSecret
      ? await hmacSha256(await proofKey(options.proofSecret), this.h3)
      : null;
    const plaintext = encodeDevice({
      publicKey: devicePub,
      signature,
      platform: options.platform,
      name: options.name,
      proof,
    });
    const key = await handshakeKey(
      this.dh,
      this.h2,
      E2E_LABELS.handshakeClientToCore,
    );
    const m3 = await aeadSeal(key, ZERO_NONCE, this.h3, plaintext);
    const h4 = await sha256(this.h3, m3);
    const session = await deriveSession(
      this.dh,
      h4,
      this.flags,
      this.identity,
      devicePub,
    );
    this.dh.fill(0);
    return { m3, session };
  }
}

// ------------------------------------------------------------------ core

export interface CoreHandshakeOptions {
  sourceId: string;
  /** core 的身份签名方（现有源钥）。 */
  identity: E2eSigner;
  /** 只给测试向量：固定临时钥。 */
  ephemeral?: X25519KeyPair;
}

/** m3 读出来的设备。 */
export interface E2eDevice {
  publicKey: Uint8Array;
  /** 公钥 SHA-256 前 16 字节的 hex（`trusted_devices.device_id`）。 */
  deviceId: string;
  platform: DevicePlatform;
  name: string;
}

export interface CoreHandshakeResult {
  device: E2eDevice;
  session: E2eSession;
  /** m3 带了 `proof`。 */
  hasProof: boolean;
  /** 用配对秘密 / 邀请令牌核对 `proof`（常数时间）；没带 `proof` 答 false。 */
  verifyProof(secret: Uint8Array): Promise<boolean>;
}

/**
 * core 一侧：`accept(m1)` 校验 `pinHint` 并产出 m2 → `readDevice(m3)` 验设备签名，答会话密钥。
 * `pin_mismatch` 时 core 应以 `RST refused` 关闭，不发 m2（不泄露公钥）。
 */
export class CoreHandshake {
  readonly m2: Uint8Array;
  readonly hello: E2eHello;
  private step: "m3" | "done" = "m3";
  private readonly dh: Uint8Array;
  private readonly h2: Uint8Array;
  private readonly h3: Uint8Array;
  private readonly identity: Uint8Array;

  private constructor(
    dh: Uint8Array,
    h2: Uint8Array,
    h3: Uint8Array,
    identity: Uint8Array,
    m2: Uint8Array,
    hello: E2eHello,
  ) {
    this.dh = dh;
    this.h2 = h2;
    this.h3 = h3;
    this.identity = identity;
    this.m2 = m2;
    this.hello = hello;
  }

  static async accept(
    m1: Uint8Array,
    options: CoreHandshakeOptions,
  ): Promise<CoreHandshake> {
    const h0 = await transcriptStart(options.sourceId);
    const hello = decodeHello(m1);
    const identity = options.identity.publicKey;
    if (identity.length !== KEY_BYTES)
      throw new E2eError("bad_message", "身份公钥须为 32 字节");
    if (
      hello.pinHint.some((byte) => byte !== 0) &&
      !constantTimeEqual(hello.pinHint, await pinHint(identity))
    )
      throw new E2eError("pin_mismatch", "客户端钉住的不是这台 core");
    const h1 = await sha256(h0, m1);
    const ephemeral = options.ephemeral ?? (await generateX25519());
    const dh = await x25519(ephemeral.privateKey, hello.ephemeral);
    const h2 = await sha256(h1, ephemeral.publicKey);
    const signature = signatureLength(
      await options.identity.sign(
        signingInput(E2E_LABELS.coreSignature, h2, identity),
      ),
    );
    const key = await handshakeKey(dh, h2, E2E_LABELS.handshakeCoreToClient);
    const sealed = await aeadSeal(
      key,
      ZERO_NONCE,
      h2,
      concatBytes(identity, signature),
    );
    const m2 = concatBytes(ephemeral.publicKey, sealed);
    return new CoreHandshake(
      dh,
      h2,
      await sha256(h2, sealed),
      identity,
      m2,
      hello,
    );
  }

  async readDevice(m3: Uint8Array): Promise<CoreHandshakeResult> {
    if (this.step !== "m3") throw new E2eError("state", "m3 已经读过");
    this.step = "done";
    if (m3.length > LIMITS_E2E.maxHandshakeMessageBytes)
      throw new E2eError("record_too_large", "m3 过长");
    const key = await handshakeKey(
      this.dh,
      this.h2,
      E2E_LABELS.handshakeClientToCore,
    );
    const device = decodeDevice(await aeadOpen(key, ZERO_NONCE, this.h3, m3));
    const ok = await ed25519Verify(
      device.publicKey,
      device.signature,
      signingInput(E2E_LABELS.deviceSignature, this.h3, device.publicKey),
    );
    if (!ok) throw new E2eError("signature_invalid", "设备的转录签名不对");
    const h4 = await sha256(this.h3, m3);
    const session = await deriveSession(
      this.dh,
      h4,
      this.hello.flags,
      this.identity,
      device.publicKey,
    );
    this.dh.fill(0);
    const h3 = this.h3;
    const proof = device.proof;
    return {
      device: {
        publicKey: device.publicKey,
        deviceId: await deviceIdOf(device.publicKey),
        platform: device.platform,
        name: device.name,
      },
      session,
      hasProof: proof !== null,
      async verifyProof(secret) {
        if (!proof) return false;
        return hmacSha256Verify(await proofKey(secret), h3, proof);
      },
    };
  }
}
