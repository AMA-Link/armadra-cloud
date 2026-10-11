/**
 * 端到端通道用到的密码学原语：全部来自 WebCrypto（`globalThis.crypto.subtle`，Node 24、
 * 浏览器、WKWebView、Workers 都有），本文件只做参数固定与字节转换，不实现任何算法。
 *
 * 只有这个文件碰 `crypto.subtle`；握手与记录层经它调用。
 */
import { fromBase64Url } from "../internal/bytes.ts";

export type E2eErrorCode =
  /** 运行环境没有 WebCrypto，或缺 X25519 / Ed25519。 */
  | "crypto_unavailable"
  /** 握手消息长度、版本字节、标志位或字段不合法。 */
  | "bad_message"
  /** 版本字节不是 1。 */
  | "version_unsupported"
  /** m1 的 `pinHint` 与 core 的身份公钥不符（core 侧以 `RST refused` 关闭）。 */
  | "pin_mismatch"
  /** m2 里的 core 身份公钥与钉住的不同。 */
  | "identity_mismatch"
  /** Ed25519 签名不对。 */
  | "signature_invalid"
  /** AEAD 解密失败：密钥不对、被篡改、重放或乱序。 */
  | "decrypt_failed"
  /** X25519 得到全零共享秘密（对端给了小阶点）。 */
  | "weak_key"
  /** 记录或握手消息超过长度上限。 */
  | "record_too_large"
  /** 记录明文的条目结构不对。 */
  | "bad_record"
  /** 这个方向的记录数到了 2³²：必须重建通道。 */
  | "record_limit"
  /** 调用顺序不对（例如没读 m2 就写 m3）。 */
  | "state";

export class E2eError extends Error {
  readonly code: E2eErrorCode;
  constructor(code: E2eErrorCode, message: string) {
    super(message);
    this.name = "E2eError";
    this.code = code;
  }
}

/**
 * WebCrypto 的类型从全局 `crypto` 推出来：包不带 DOM lib，也不 import `node:crypto`，
 * 页面（DOM lib）与 Node（`@types/node`）各自的声明都对得上。
 */
type Subtle = typeof globalThis.crypto.subtle;
export type CryptoKey = Awaited<ReturnType<Subtle["importKey"]>>;
type KeyUsage = Parameters<Subtle["importKey"]>[4][number];

function subtle(): Subtle {
  const value = globalThis.crypto?.subtle;
  if (!value) throw new E2eError("crypto_unavailable", "没有 WebCrypto");
  return value;
}

/** WebCrypto 只收以 `ArrayBuffer` 为底的视图；共享内存或别的底先复制一份。 */
function view(bytes: Uint8Array): Uint8Array<ArrayBuffer> {
  return bytes.buffer instanceof ArrayBuffer
    ? (bytes as Uint8Array<ArrayBuffer>)
    : new Uint8Array(bytes);
}

const encoder = new TextEncoder();

export function concatBytes(...parts: readonly Uint8Array[]): Uint8Array {
  const out = new Uint8Array(parts.reduce((sum, part) => sum + part.length, 0));
  let at = 0;
  for (const part of parts) {
    out.set(part, at);
    at += part.length;
  }
  return out;
}

export function label(text: string): Uint8Array {
  return encoder.encode(text);
}

export function randomBytes(length: number): Uint8Array {
  const out = new Uint8Array(length);
  globalThis.crypto.getRandomValues(out);
  return out;
}

export async function sha256(
  ...parts: readonly Uint8Array[]
): Promise<Uint8Array> {
  return new Uint8Array(
    await subtle().digest("SHA-256", view(concatBytes(...parts))),
  );
}

/** HKDF-SHA256（RFC 5869）：`salt` 为空时按 RFC 取全零。 */
export async function hkdf(
  ikm: Uint8Array,
  salt: Uint8Array,
  info: string,
  length: number,
): Promise<Uint8Array> {
  const key = await subtle().importKey("raw", view(ikm), "HKDF", false, [
    "deriveBits",
  ]);
  return new Uint8Array(
    await subtle().deriveBits(
      { name: "HKDF", hash: "SHA-256", salt: view(salt), info: label(info) },
      key,
      length * 8,
    ),
  );
}

async function hmacKey(key: Uint8Array, usage: "sign" | "verify") {
  return subtle().importKey(
    "raw",
    view(key),
    { name: "HMAC", hash: "SHA-256" },
    false,
    [usage],
  );
}

export async function hmacSha256(
  key: Uint8Array,
  data: Uint8Array,
): Promise<Uint8Array> {
  return new Uint8Array(
    await subtle().sign("HMAC", await hmacKey(key, "sign"), view(data)),
  );
}

/** 常数时间比较（交给 WebCrypto 的 HMAC 校验）。 */
export async function hmacSha256Verify(
  key: Uint8Array,
  data: Uint8Array,
  mac: Uint8Array,
): Promise<boolean> {
  return subtle().verify(
    "HMAC",
    await hmacKey(key, "verify"),
    view(mac),
    view(data),
  );
}

// ------------------------------------------------------------------ AES-256-GCM

export type AeadKey = CryptoKey;

export async function importAeadKey(raw: Uint8Array): Promise<AeadKey> {
  if (raw.length !== 32)
    throw new E2eError("crypto_unavailable", "AES-256-GCM 密钥必须是 32 字节");
  return subtle().importKey("raw", view(raw), "AES-GCM", false, [
    "encrypt",
    "decrypt",
  ]);
}

export async function aeadSeal(
  key: AeadKey,
  nonce: Uint8Array,
  aad: Uint8Array,
  plaintext: Uint8Array,
): Promise<Uint8Array> {
  return new Uint8Array(
    await subtle().encrypt(
      {
        name: "AES-GCM",
        iv: view(nonce),
        additionalData: view(aad),
        tagLength: 128,
      },
      key,
      view(plaintext),
    ),
  );
}

export async function aeadOpen(
  key: AeadKey,
  nonce: Uint8Array,
  aad: Uint8Array,
  ciphertext: Uint8Array,
): Promise<Uint8Array> {
  try {
    return new Uint8Array(
      await subtle().decrypt(
        {
          name: "AES-GCM",
          iv: view(nonce),
          additionalData: view(aad),
          tagLength: 128,
        },
        key,
        view(ciphertext),
      ),
    );
  } catch {
    throw new E2eError("decrypt_failed", "AEAD 解密失败");
  }
}

// ------------------------------------------------------------------ X25519

/** 临时密钥对：公钥是 32 字节 raw，私钥不可导出。 */
export interface X25519KeyPair {
  publicKey: Uint8Array;
  privateKey: CryptoKey;
}

export async function generateX25519(): Promise<X25519KeyPair> {
  const pair = (await subtle().generateKey({ name: "X25519" }, false, [
    "deriveBits",
  ])) as { publicKey: CryptoKey; privateKey: CryptoKey };
  return {
    publicKey: new Uint8Array(await subtle().exportKey("raw", pair.publicKey)),
    privateKey: pair.privateKey,
  };
}

const PKCS8_PREFIX = {
  X25519: [
    0x30, 0x2e, 0x02, 0x01, 0x00, 0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x6e,
    0x04, 0x22, 0x04, 0x20,
  ],
  Ed25519: [
    0x30, 0x2e, 0x02, 0x01, 0x00, 0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x70,
    0x04, 0x22, 0x04, 0x20,
  ],
} as const;

/** 32 字节种子 → 私钥（PKCS#8）与 raw 公钥。只给测试向量与持有原始种子的一方用。 */
async function fromSeed(
  algorithm: "X25519" | "Ed25519",
  seed: Uint8Array,
  usages: KeyUsage[],
): Promise<{ privateKey: CryptoKey; publicKey: Uint8Array }> {
  if (seed.length !== 32)
    throw new E2eError("bad_message", `${algorithm} 种子必须是 32 字节`);
  const pkcs8 = concatBytes(Uint8Array.from(PKCS8_PREFIX[algorithm]), seed);
  const extractable = await subtle().importKey(
    "pkcs8",
    view(pkcs8),
    { name: algorithm },
    true,
    usages,
  );
  const jwk = await subtle().exportKey("jwk", extractable);
  const privateKey = await subtle().importKey(
    "pkcs8",
    view(pkcs8),
    { name: algorithm },
    false,
    usages,
  );
  pkcs8.fill(0);
  return { privateKey, publicKey: fromBase64Url(jwk.x ?? "") };
}

export async function x25519FromSeed(seed: Uint8Array): Promise<X25519KeyPair> {
  return fromSeed("X25519", seed, ["deriveBits"]);
}

/** X25519 共享秘密；全零（对端给了小阶点）抛 `weak_key`。 */
export async function x25519(
  privateKey: CryptoKey,
  peerPublic: Uint8Array,
): Promise<Uint8Array> {
  if (peerPublic.length !== 32)
    throw new E2eError("bad_message", "X25519 公钥必须是 32 字节");
  let shared: Uint8Array;
  try {
    const peer = await subtle().importKey(
      "raw",
      view(peerPublic),
      { name: "X25519" },
      false,
      [],
    );
    shared = new Uint8Array(
      await subtle().deriveBits(
        { name: "X25519", public: peer },
        privateKey,
        256,
      ),
    );
  } catch {
    throw new E2eError("weak_key", "X25519 计算失败");
  }
  if (shared.every((byte) => byte === 0))
    throw new E2eError("weak_key", "X25519 共享秘密全零");
  return shared;
}

// ------------------------------------------------------------------ Ed25519

/**
 * Ed25519 签名方：公钥 32 字节 raw；签名可以在别处做（桌面经本机 core 签、手机钥匙串里
 * 不可导出的钥），握手只要求 `sign` 答 64 字节。
 */
export interface E2eSigner {
  readonly publicKey: Uint8Array;
  sign(data: Uint8Array): Promise<Uint8Array>;
}

/** 用 WebCrypto 的 Ed25519 私钥（可以不可导出）构造签名方。 */
export function ed25519Signer(
  privateKey: CryptoKey,
  publicKey: Uint8Array,
): E2eSigner {
  return {
    publicKey,
    async sign(data) {
      return new Uint8Array(
        await subtle().sign("Ed25519", privateKey, view(data)),
      );
    },
  };
}

export async function ed25519SignerFromSeed(
  seed: Uint8Array,
): Promise<E2eSigner> {
  const { privateKey, publicKey } = await fromSeed("Ed25519", seed, ["sign"]);
  return ed25519Signer(privateKey, publicKey);
}

export async function ed25519Verify(
  publicKey: Uint8Array,
  signature: Uint8Array,
  data: Uint8Array,
): Promise<boolean> {
  if (publicKey.length !== 32 || signature.length !== 64) return false;
  try {
    const key = await subtle().importKey(
      "raw",
      view(publicKey),
      { name: "Ed25519" },
      false,
      ["verify"],
    );
    return await subtle().verify("Ed25519", key, view(signature), view(data));
  } catch {
    return false;
  }
}

// ------------------------------------------------------------------ 编码

export function toHex(bytes: Uint8Array): string {
  let out = "";
  for (const byte of bytes) out += byte.toString(16).padStart(2, "0");
  return out;
}

export function fromHex(text: string): Uint8Array {
  if (!/^(?:[0-9a-f]{2})*$/.test(text))
    throw new E2eError("bad_message", "不是小写 hex");
  return Uint8Array.from(text.match(/../g) ?? [], (pair) =>
    Number.parseInt(pair, 16),
  );
}

/** 常数时间比较（长度不同直接 false，长度不是秘密）。 */
export function constantTimeEqual(
  left: Uint8Array,
  right: Uint8Array,
): boolean {
  if (left.length !== right.length) return false;
  let diff = 0;
  for (let index = 0; index < left.length; index += 1)
    diff |= (left[index] as number) ^ (right[index] as number);
  return diff === 0;
}
