/**
 * 握手三条消息的定长二进制布局（设计 §4.4）。字段顺序固定，解码后的结构再过 zod。
 *
 * ```text
 * m1（58 B，明文）       ver u8 | flags u8 | e_c 32 | nonce_c 16 | pinHint 8
 * m2（144 B）            e_s 32 | AES-256-GCM(k_hs_s2c, 0, aad = h2, coreIdentityPub 32 | sig_core 64) 112
 * m3（118–243 B）        AES-256-GCM(k_hs_c2s, 0, aad = h3, device)
 *   device 明文           devicePub 32 | sig_dev 64 | platformLen u8 | platform | nameLen u8 | name
 *                         | proofLen u8（0 或 32） | proof
 * ```
 *
 * 线上每条消息前面另有 u32 BE 长度（与记录同一种单元，见 `records.ts`）；转录哈希只吃消息体。
 */
import { z } from "zod";

import { devicePlatformSchema } from "../assertion/claims.ts";
import { fromUtf8, utf8 } from "../internal/bytes.ts";
import { E2E_FLAGS, E2E_VERSION, LIMITS_E2E } from "./constants.ts";
import { concatBytes, E2eError, sha256, toHex } from "./primitives.ts";

export const KEY_BYTES = 32;
export const NONCE_C_BYTES = 16;
export const PIN_HINT_BYTES = 8;
export const SIGNATURE_BYTES = 64;
export const PROOF_BYTES = 32;
export const M1_BYTES = 2 + KEY_BYTES + NONCE_C_BYTES + PIN_HINT_BYTES;
export const M2_BYTES =
  KEY_BYTES + KEY_BYTES + SIGNATURE_BYTES + LIMITS_E2E.tagBytes;

const KNOWN_FLAGS = Object.values(E2E_FLAGS).reduce((all, bit) => all | bit, 0);

const bytesOf = (length: number) =>
  z.custom<Uint8Array>(
    (value) => value instanceof Uint8Array && value.length === length,
    { message: `须为 ${length} 字节` },
  );

export const e2eHelloSchema = z.object({
  version: z.literal(E2E_VERSION),
  flags: z
    .number()
    .int()
    .min(0)
    .max(0xff)
    .refine((value) => (value & ~KNOWN_FLAGS) === 0, "未定义的标志位"),
  ephemeral: bytesOf(KEY_BYTES),
  nonce: bytesOf(NONCE_C_BYTES),
  pinHint: bytesOf(PIN_HINT_BYTES),
});
export type E2eHello = z.infer<typeof e2eHelloSchema>;

const boundedText = (max: number) =>
  z.string().refine((value) => utf8(value).length <= max, {
    message: `UTF-8 不超过 ${max} 字节`,
  });

export const e2eDeviceSchema = z.object({
  publicKey: bytesOf(KEY_BYTES),
  signature: bytesOf(SIGNATURE_BYTES),
  platform: devicePlatformSchema,
  name: boundedText(LIMITS_E2E.nameMaxBytes),
  proof: bytesOf(PROOF_BYTES).nullable(),
});
export type E2eDeviceMessage = z.infer<typeof e2eDeviceSchema>;

function invalid(message: string): never {
  throw new E2eError("bad_message", message);
}

function parse<T>(schema: z.ZodType<T>, value: unknown, what: string): T {
  const result = schema.safeParse(value);
  if (!result.success) invalid(`${what} 不合法`);
  return result.data;
}

export function encodeHello(hello: E2eHello): Uint8Array {
  const value = parse(e2eHelloSchema, hello, "m1");
  return concatBytes(
    Uint8Array.of(value.version, value.flags),
    value.ephemeral,
    value.nonce,
    value.pinHint,
  );
}

export function decodeHello(body: Uint8Array): E2eHello {
  if (body.length !== M1_BYTES) invalid(`m1 应为 ${M1_BYTES} 字节`);
  if (body[0] !== E2E_VERSION)
    throw new E2eError("version_unsupported", `不支持的版本 ${body[0]}`);
  let at = 2;
  const take = (length: number) => body.slice(at, (at += length));
  return parse(
    e2eHelloSchema,
    {
      version: body[0],
      flags: body[1],
      ephemeral: take(KEY_BYTES),
      nonce: take(NONCE_C_BYTES),
      pinHint: take(PIN_HINT_BYTES),
    },
    "m1",
  );
}

/** m3 的明文。 */
export function encodeDevice(device: E2eDeviceMessage): Uint8Array {
  const value = parse(e2eDeviceSchema, device, "m3");
  const platform = utf8(value.platform);
  const name = utf8(value.name);
  const proof = value.proof ?? new Uint8Array(0);
  return concatBytes(
    value.publicKey,
    value.signature,
    Uint8Array.of(platform.length),
    platform,
    Uint8Array.of(name.length),
    name,
    Uint8Array.of(proof.length),
    proof,
  );
}

export function decodeDevice(plaintext: Uint8Array): E2eDeviceMessage {
  let at = 0;
  const take = (length: number) => {
    if (at + length > plaintext.length) invalid("m3 截断");
    return plaintext.slice(at, (at += length));
  };
  const text = (max: number) => {
    const length = take(1)[0] as number;
    if (length > max) invalid("m3 文本过长");
    try {
      return fromUtf8(take(length));
    } catch {
      return invalid("m3 文本不是 UTF-8");
    }
  };
  const publicKey = take(KEY_BYTES);
  const signature = take(SIGNATURE_BYTES);
  const platform = text(LIMITS_E2E.platformMaxBytes);
  const name = text(LIMITS_E2E.nameMaxBytes);
  const proofLength = take(1)[0] as number;
  if (proofLength !== 0 && proofLength !== PROOF_BYTES)
    invalid("m3 的 proof 长度不对");
  const proof = proofLength === 0 ? null : take(PROOF_BYTES);
  if (at !== plaintext.length) invalid("m3 有多余字节");
  return parse(
    e2eDeviceSchema,
    { publicKey, signature, platform, name, proof },
    "m3",
  );
}

// ------------------------------------------------------------------ 身份标识

/** `pinHint`：身份公钥 SHA-256 的前 8 字节；没有钉扎时全零。 */
export async function pinHint(identityPub: Uint8Array): Promise<Uint8Array> {
  return (await sha256(identityPub)).slice(0, PIN_HINT_BYTES);
}

/** 设备 id：设备公钥 SHA-256 的前 16 字节，32 位小写 hex（`trusted_devices.device_id`）。 */
export async function deviceIdOf(devicePub: Uint8Array): Promise<string> {
  return toHex((await sha256(devicePub)).slice(0, 16));
}

const BASE32 = "abcdefghijklmnopqrstuvwxyz234567";

/** RFC 4648 base32，小写、无填充。 */
function base32(bytes: Uint8Array): string {
  let out = "";
  let bits = 0;
  let value = 0;
  for (const byte of bytes) {
    value = ((value << 8) | byte) & 0xffff;
    bits += 8;
    while (bits >= 5) {
      bits -= 5;
      out += BASE32[(value >> bits) & 31];
    }
  }
  if (bits > 0) out += BASE32[(value << (5 - bits)) & 31];
  return out;
}

/** 身份公钥指纹：`base32(SHA-256(pub))`，52 个字符；界面分组显示前 16 位（§4.9）。 */
export async function identityFingerprint(
  identityPub: Uint8Array,
): Promise<string> {
  return base32(await sha256(identityPub));
}
