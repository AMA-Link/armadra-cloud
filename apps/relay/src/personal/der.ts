/**
 * 够用的 DER 写入器与 X.509 / PKCS#10 拼装（自签 CA、叶证书、ACME 的证书请求）。
 *
 * `node:crypto` 只生成密钥对、不签证书；与其加一个依赖，这里写几十行确定性的编码，
 * 产物用 `X509Certificate` 反向解析来测（`tls.test.ts`）。规则与 core Gateway 的本地 CA
 * 相同（ECDSA P-256、ecdsa-with-SHA256、SAN 里 IP 进 iPAddress、域名进 dNSName）。
 * 只写不读。
 */
import {
  createHash,
  createSign,
  randomBytes,
  type KeyObject,
} from "node:crypto";
import { isIPv4, isIPv6 } from "node:net";

export function tlv(tag: number, content: Buffer): Buffer {
  if (content.length < 0x80)
    return Buffer.concat([Buffer.from([tag, content.length]), content]);
  const bytes: number[] = [];
  for (let remaining = content.length; remaining > 0; remaining >>>= 8)
    bytes.unshift(remaining & 0xff);
  return Buffer.concat([
    Buffer.from([tag, 0x80 | bytes.length, ...bytes]),
    content,
  ]);
}

export const sequence = (...parts: Buffer[]) => tlv(0x30, Buffer.concat(parts));
export const set = (...parts: Buffer[]) => tlv(0x31, Buffer.concat(parts));
export const boolean = (value: boolean) =>
  tlv(0x01, Buffer.from([value ? 0xff : 0x00]));
export const octetString = (content: Buffer) => tlv(0x04, content);
export const bitString = (content: Buffer, unused = 0) =>
  tlv(0x03, Buffer.concat([Buffer.from([unused]), content]));
export const utf8String = (value: string) =>
  tlv(0x0c, Buffer.from(value, "utf8"));
export const explicit = (tag: number, content: Buffer) =>
  tlv(0xa0 | tag, content);
export const implicit = (tag: number, content: Buffer) =>
  tlv(0x80 | tag, content);

/** 非负整数；最高位是 1 时补 0x00。 */
export function integer(value: Buffer | number): Buffer {
  let bytes: Buffer;
  if (typeof value === "number") {
    const collected: number[] = [];
    for (
      let remaining = value;
      remaining > 0;
      remaining = Math.floor(remaining / 256)
    )
      collected.unshift(remaining & 0xff);
    bytes = Buffer.from(collected.length > 0 ? collected : [0]);
  } else {
    let start = 0;
    while (start < value.length - 1 && value[start] === 0) start += 1;
    bytes = value.subarray(start);
  }
  return tlv(
    0x02,
    (bytes[0] as number) & 0x80
      ? Buffer.concat([Buffer.from([0]), bytes])
      : bytes,
  );
}

export function oid(dotted: string): Buffer {
  const parts = dotted.split(".").map(Number);
  const bytes: number[] = [40 * (parts[0] as number) + (parts[1] as number)];
  for (const part of parts.slice(2)) {
    const chunk: number[] = [part & 0x7f];
    for (
      let remaining = Math.floor(part / 128);
      remaining > 0;
      remaining = Math.floor(remaining / 128)
    )
      chunk.unshift((remaining & 0x7f) | 0x80);
    bytes.push(...chunk);
  }
  return tlv(0x06, Buffer.from(bytes));
}

/** RFC 5280：2050 年前用 UTCTime。 */
export function utcTime(at: Date): Buffer {
  const pad = (value: number) => String(value).padStart(2, "0");
  const text =
    pad(at.getUTCFullYear() % 100) +
    pad(at.getUTCMonth() + 1) +
    pad(at.getUTCDate()) +
    pad(at.getUTCHours()) +
    pad(at.getUTCMinutes()) +
    pad(at.getUTCSeconds()) +
    "Z";
  return tlv(0x17, Buffer.from(text, "ascii"));
}

export function commonName(value: string): Buffer {
  return sequence(set(sequence(oid("2.5.4.3"), utf8String(value))));
}

export function extension(
  id: string,
  critical: boolean,
  value: Buffer,
): Buffer {
  return sequence(
    oid(id),
    ...(critical ? [boolean(true)] : []),
    octetString(value),
  );
}

function ipv6Bytes(value: string): Buffer {
  const [head, tail] = value.split("::");
  const parse = (part: string | undefined): number[] =>
    !part
      ? []
      : part.split(":").flatMap((group) => {
          const number = Number.parseInt(group, 16);
          return [(number >> 8) & 0xff, number & 0xff];
        });
  const left = parse(head);
  const right = tail === undefined ? [] : parse(tail);
  return Buffer.from([
    ...left,
    ...(new Array(16 - left.length - right.length).fill(0) as number[]),
    ...right,
  ]);
}

/** subjectAltName：IP 字面量进 `iPAddress [7]`，其余进 `dNSName [2]`。 */
export function subjectAltName(hosts: readonly string[]): Buffer {
  return sequence(
    ...hosts.map((host) => {
      const literal =
        host.startsWith("[") && host.endsWith("]") ? host.slice(1, -1) : host;
      if (isIPv4(literal))
        return implicit(7, Buffer.from(literal.split(".").map(Number)));
      if (isIPv6(literal)) return implicit(7, ipv6Bytes(literal));
      return implicit(2, Buffer.from(host, "ascii"));
    }),
  );
}

/** RFC 5280 §4.2.1.2 方法一：P-256 未压缩公钥（SPKI 末尾 65 字节）的 SHA-1。 */
export function subjectKeyId(spki: Buffer): Buffer {
  return createHash("sha1")
    .update(spki.subarray(spki.length - 65))
    .digest();
}

const ECDSA_SHA256 = "1.2.840.10045.4.3.2";

export function pem(label: string, der: Buffer): string {
  const body = der.toString("base64").replace(/(.{64})/g, "$1\n");
  return `-----BEGIN ${label}-----\n${body}${body.endsWith("\n") ? "" : "\n"}-----END ${label}-----\n`;
}

export function signCertificate(input: {
  subject: Buffer;
  issuer: Buffer;
  spki: Buffer;
  signer: KeyObject;
  notBefore: Date;
  notAfter: Date;
  extensions: readonly Buffer[];
}): string {
  const algorithm = sequence(oid(ECDSA_SHA256));
  const serial = randomBytes(16);
  serial[0] = (serial[0] as number) & 0x7f;
  const tbs = sequence(
    explicit(0, integer(2)),
    integer(serial),
    algorithm,
    input.issuer,
    sequence(utcTime(input.notBefore), utcTime(input.notAfter)),
    input.subject,
    input.spki,
    explicit(3, sequence(...input.extensions)),
  );
  const signature = createSign("SHA256").update(tbs).sign(input.signer);
  return pem("CERTIFICATE", sequence(tbs, algorithm, bitString(signature)));
}

/** PKCS#10 证书请求：空 subject、SAN 扩展请求（ACME 定稿用）。 */
export function certificateRequest(
  names: readonly string[],
  key: KeyObject,
  spki: Buffer,
): string {
  const info = sequence(
    integer(0),
    sequence(),
    spki,
    explicit(
      0,
      sequence(
        oid("1.2.840.113549.1.9.14"),
        set(sequence(extension("2.5.29.17", false, subjectAltName(names)))),
      ),
    ),
  );
  const signature = createSign("SHA256").update(info).sign(key);
  return pem(
    "CERTIFICATE REQUEST",
    sequence(info, sequence(oid(ECDSA_SHA256)), bitString(signature)),
  );
}

/** 证书 TBS 里 subject 那段 DER（第六个元素；没有 version 时第五个），原样取出。 */
export function subjectDer(raw: Buffer): Buffer {
  const tbs = children(children(raw)[0] as Buffer);
  const hasVersion = ((tbs[0] as Buffer)[0] as number) === 0xa0;
  return tbs[hasVersion ? 5 : 4] as Buffer;
}

function header(
  buffer: Buffer,
  offset: number,
): { size: number; length: number } {
  const first = buffer[offset + 1] as number;
  if (first < 0x80) return { size: 2, length: first };
  const count = first & 0x7f;
  let length = 0;
  for (let index = 0; index < count; index += 1)
    length = length * 256 + (buffer[offset + 2 + index] as number);
  return { size: 2 + count, length };
}

function children(element: Buffer): Buffer[] {
  const outer = header(element, 0);
  const out: Buffer[] = [];
  for (let offset = outer.size; offset < outer.size + outer.length; ) {
    const inner = header(element, offset);
    out.push(element.subarray(offset, offset + inner.size + inner.length));
    offset += inner.size + inner.length;
  }
  return out;
}
