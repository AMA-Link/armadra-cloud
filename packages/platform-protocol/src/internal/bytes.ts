/**
 * 字节工具：UTF-8 与 base64url。不用 `Buffer`（页面与手机里没有），也不用
 * `atob` / `btoa`（只吃 Latin-1 字符串，转换容易出错）。
 */
const encoder = new TextEncoder();
const decoder = new TextDecoder("utf-8", { fatal: true });

export function utf8(text: string): Uint8Array {
  return encoder.encode(text);
}

/** 严格解码：不是合法 UTF-8 就抛。 */
export function fromUtf8(bytes: Uint8Array): string {
  return decoder.decode(bytes);
}

const ALPHABET =
  "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";
const LOOKUP = new Map([...ALPHABET].map((char, index) => [char, index]));

/** base64url，无填充（RFC 4648 §5，JWS 的写法）。 */
export function toBase64Url(bytes: Uint8Array): string {
  let out = "";
  let index = 0;
  for (; index + 2 < bytes.length; index += 3) {
    const n =
      ((bytes[index] ?? 0) << 16) |
      ((bytes[index + 1] ?? 0) << 8) |
      (bytes[index + 2] ?? 0);
    out +=
      ALPHABET[(n >> 18) & 63]! +
      ALPHABET[(n >> 12) & 63]! +
      ALPHABET[(n >> 6) & 63]! +
      ALPHABET[n & 63]!;
  }
  const rest = bytes.length - index;
  if (rest === 1) {
    const n = (bytes[index] ?? 0) << 16;
    out += ALPHABET[(n >> 18) & 63]! + ALPHABET[(n >> 12) & 63]!;
  } else if (rest === 2) {
    const n = ((bytes[index] ?? 0) << 16) | ((bytes[index + 1] ?? 0) << 8);
    out +=
      ALPHABET[(n >> 18) & 63]! +
      ALPHABET[(n >> 12) & 63]! +
      ALPHABET[(n >> 6) & 63]!;
  }
  return out;
}

/** 严格解码：只收 base64url 字母表、无填充、无多余位；否则抛。 */
export function fromBase64Url(text: string): Uint8Array {
  if (text.length % 4 === 1) throw new Error("base64url 长度不合法");
  const out = new Uint8Array(Math.floor((text.length * 3) / 4));
  let bits = 0;
  let value = 0;
  let at = 0;
  for (const char of text) {
    const digit = LOOKUP.get(char);
    if (digit === undefined) throw new Error("base64url 含非法字符");
    value = (value << 6) | digit;
    bits += 6;
    if (bits >= 8) {
      bits -= 8;
      out[at++] = (value >> bits) & 0xff;
    }
  }
  if ((value & ((1 << bits) - 1)) !== 0)
    throw new Error("base64url 末尾有多余的位");
  return out;
}

export function bytesEqual(left: Uint8Array, right: Uint8Array): boolean {
  if (left.length !== right.length) return false;
  for (let index = 0; index < left.length; index += 1) {
    if (left[index] !== right[index]) return false;
  }
  return true;
}
