/**
 * personal 的 TLS（中继设计 §6.6，契约 §11）。
 *
 * - `self-signed`（缺省）：`<data-dir>/tls/` 下一张 CA（ECDSA P-256，10 年）与它签的叶证书
 *   （90 天，SAN = `--host` + `localhost` + `127.0.0.1`），私钥 0600。叶证书剩 7 天以内或不再
 *   覆盖 `--host` 时自动重签；CA 不变，客户端钉一次指纹就够。CA 坏了或过期**不**悄悄换——那会让
 *   所有钉过指纹的客户端同时失效——而是拒绝启动。`GET /ca.crt` 发 CA。
 * - `file`：`--tls-cert` / `--tls-key`，文件变化时热加载（轮询 stat；坏了继续用旧的）。
 * - `acme`：见 `acme.ts`。
 *
 * 指纹 = 信任锚 DER 的 SHA-256，小写 hex、无分隔符（与 core Gateway `localCa` 同一定义）：
 * 自签模式是 CA 的；文件模式是链里最后一张（只有一张时是它自己）；ACME 是叶证书的。
 */
import {
  createHash,
  createPrivateKey,
  createPublicKey,
  generateKeyPairSync,
  randomBytes,
  X509Certificate,
  type KeyObject,
} from "node:crypto";
import {
  chmodSync,
  existsSync,
  mkdirSync,
  readFileSync,
  renameSync,
  unwatchFile,
  watchFile,
  writeFileSync,
} from "node:fs";
import { join } from "node:path";

import type { TlsPair } from "@armadra/cloud-shared/http";

import {
  bitString,
  boolean,
  commonName,
  extension,
  implicit,
  integer,
  octetString,
  oid,
  sequence,
  signCertificate,
  subjectAltName,
  subjectDer,
  subjectKeyId,
} from "./der.ts";

export const TLS_DIR = "tls";
export const CA_CERT = "ca.crt";
export const CA_KEY = "ca.key";
export const LEAF_CERT = "leaf.crt";
export const LEAF_KEY = "leaf.key";

export const CA_DAYS = 3650;
export const LEAF_DAYS = 90;
export const LEAF_RENEW_BEFORE_DAYS = 7;
/** 多久检查一次叶证书该不该重签。 */
export const RENEW_CHECK_MS = 12 * 60 * 60 * 1000;

const DAY_MS = 24 * 60 * 60 * 1000;

export type TlsSource = "self-signed" | "file" | "acme";

export interface TlsMaterial extends TlsPair {
  source: TlsSource;
  /** 信任锚指纹（见文件头）。 */
  fingerprint: string;
  /** `GET /ca.crt` 发的 PEM：自签是 CA；文件模式是链里最后一张（多于一张时）。 */
  anchor?: string;
  notAfter: string;
  names: string[];
}

export class TlsError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "TlsError";
  }
}

/** 证书要覆盖的名字：`--host`、`localhost`、`127.0.0.1`（去重，保序）。 */
export function tlsNames(host: string): string[] {
  const literal =
    host.startsWith("[") && host.endsWith("]") ? host.slice(1, -1) : host;
  return [...new Set([literal, "localhost", "127.0.0.1"])];
}

export function fingerprintOf(certificate: X509Certificate | string): string {
  const parsed =
    typeof certificate === "string"
      ? new X509Certificate(certificate)
      : certificate;
  return createHash("sha256").update(parsed.raw).digest("hex");
}

export function pemBlocks(text: string): string[] {
  return (
    text.match(
      /-----BEGIN CERTIFICATE-----[\s\S]+?-----END CERTIFICATE-----/g,
    ) ?? []
  ).map((block) => `${block}\n`);
}

export function sanNames(parsed: X509Certificate): string[] {
  return (parsed.subjectAltName ?? "")
    .split(",")
    .map((entry) => entry.trim().replace(/^(DNS|IP Address|IP):/, ""))
    .filter((entry) => entry !== "");
}

function covers(parsed: X509Certificate, names: readonly string[]): boolean {
  const have = sanNames(parsed).map((name) => name.toLowerCase());
  return names.every((name) => have.includes(name.toLowerCase()));
}

function privateDirectory(directory: string): void {
  mkdirSync(directory, { recursive: true, mode: 0o700 });
  chmodSync(directory, 0o700);
}

/** 先写临时文件再改名；私钥 0600（先建成 0600 再写，不受 umask 影响）。 */
export function replaceFile(file: string, content: string, mode = 0o600): void {
  const temporary = `${file}.${process.pid}.${randomBytes(4).toString("hex")}.tmp`;
  writeFileSync(temporary, content, { mode });
  chmodSync(temporary, mode);
  renameSync(temporary, file);
}

function spkiOf(key: KeyObject): Buffer {
  return createPublicKey(key).export({ type: "spki", format: "der" }) as Buffer;
}

export interface Ca {
  cert: string;
  key: KeyObject;
  parsed: X509Certificate;
}

function loadOrCreateCa(directory: string, now: Date): Ca {
  const certFile = join(directory, CA_CERT);
  const keyFile = join(directory, CA_KEY);
  if (existsSync(certFile) || existsSync(keyFile)) {
    let ca: Ca;
    try {
      const cert = readFileSync(certFile, "utf8");
      const key = createPrivateKey(readFileSync(keyFile, "utf8"));
      ca = { cert, key, parsed: new X509Certificate(cert) };
    } catch {
      throw new TlsError(
        "自签 CA 读不出来，拒绝启动：检查 tls/ca.crt 与 tls/ca.key",
      );
    }
    if (!ca.parsed.checkPrivateKey(ca.key))
      throw new TlsError("自签 CA 的证书与私钥对不上，拒绝启动");
    if (new Date(ca.parsed.validTo).getTime() <= now.getTime())
      throw new TlsError(
        "自签 CA 已过期，拒绝启动：删除 tls/ 目录后客户端需要重新确认指纹",
      );
    return ca;
  }
  const ca = createCa(now);
  replaceFile(
    keyFile,
    ca.key.export({ type: "pkcs8", format: "pem" }).toString(),
  );
  replaceFile(certFile, ca.cert);
  return ca;
}

/** 一张新的 CA（ECDSA P-256，10 年，pathLen 0）；只在内存里，落盘由调用方。 */
export function createCa(now: Date): Ca {
  const { privateKey } = generateKeyPairSync("ec", {
    namedCurve: "prime256v1",
  });
  const spki = spkiOf(privateKey);
  const name = commonName(`Armadra Relay CA ${randomBytes(4).toString("hex")}`);
  const cert = signCertificate({
    subject: name,
    issuer: name,
    spki,
    signer: privateKey,
    notBefore: new Date(now.getTime() - 60 * 60 * 1000),
    notAfter: new Date(now.getTime() + CA_DAYS * DAY_MS),
    extensions: [
      // CA，pathLen 0：只签叶证书。
      extension("2.5.29.19", true, sequence(boolean(true), integer(0))),
      // keyCertSign | cRLSign
      extension("2.5.29.15", true, bitString(Buffer.from([0x06]), 1)),
      extension("2.5.29.14", false, octetString(subjectKeyId(spki))),
    ],
  });
  return { cert, key: privateKey, parsed: new X509Certificate(cert) };
}

export function issueLeaf(
  ca: { key: KeyObject; parsed: X509Certificate },
  names: readonly string[],
  now: Date,
  days = LEAF_DAYS,
  key?: KeyObject,
): TlsPair {
  const privateKey =
    key ?? generateKeyPairSync("ec", { namedCurve: "prime256v1" }).privateKey;
  const spki = spkiOf(privateKey);
  const cert = signCertificate({
    subject: commonName(names[0] ?? "localhost"),
    issuer: subjectDer(ca.parsed.raw),
    spki,
    signer: ca.key,
    notBefore: new Date(now.getTime() - 60 * 60 * 1000),
    notAfter: new Date(now.getTime() + days * DAY_MS),
    extensions: [
      extension("2.5.29.19", true, sequence()),
      // digitalSignature
      extension("2.5.29.15", true, bitString(Buffer.from([0x80]), 7)),
      // extKeyUsage: serverAuth
      extension("2.5.29.37", false, sequence(oid("1.3.6.1.5.5.7.3.1"))),
      extension("2.5.29.17", false, subjectAltName(names)),
      extension("2.5.29.14", false, octetString(subjectKeyId(spki))),
      extension(
        "2.5.29.35",
        false,
        sequence(implicit(0, subjectKeyId(spkiOf(ca.key)))),
      ),
    ],
  });
  return {
    cert,
    key: privateKey.export({ type: "pkcs8", format: "pem" }).toString(),
  };
}

function usableLeaf(
  directory: string,
  ca: Ca,
  names: readonly string[],
  now: Date,
): TlsPair | undefined {
  const certFile = join(directory, LEAF_CERT);
  const keyFile = join(directory, LEAF_KEY);
  if (!existsSync(certFile) || !existsSync(keyFile)) return undefined;
  try {
    const cert = readFileSync(certFile, "utf8");
    const key = readFileSync(keyFile, "utf8");
    const parsed = new X509Certificate(cert);
    const remaining = new Date(parsed.validTo).getTime() - now.getTime();
    if (remaining <= LEAF_RENEW_BEFORE_DAYS * DAY_MS) return undefined;
    if (!parsed.verify(ca.parsed.publicKey)) return undefined;
    if (!parsed.checkPrivateKey(createPrivateKey(key))) return undefined;
    if (!covers(parsed, names)) return undefined;
    return { cert, key };
  } catch {
    return undefined;
  }
}

/** 自签：CA 有就读、没有就建；叶证书能用就用，否则重签。 */
export function selfSignedMaterial(
  dataDir: string,
  names: readonly string[],
  now: Date = new Date(),
): TlsMaterial {
  const directory = join(dataDir, TLS_DIR);
  privateDirectory(directory);
  const ca = loadOrCreateCa(directory, now);
  let leaf = usableLeaf(directory, ca, names, now);
  if (!leaf) {
    leaf = issueLeaf(ca, names, now);
    replaceFile(join(directory, LEAF_KEY), leaf.key);
    replaceFile(join(directory, LEAF_CERT), leaf.cert, 0o600);
  }
  const parsed = new X509Certificate(leaf.cert);
  return {
    // 链：叶证书 + CA，客户端拿到完整链。
    cert: `${leaf.cert}${ca.cert}`,
    key: leaf.key,
    source: "self-signed",
    fingerprint: fingerprintOf(ca.parsed),
    anchor: ca.cert,
    notAfter: parsed.validTo,
    names: sanNames(parsed),
  };
}

/** 状态命令用：已有自签 CA 的指纹（没有答 null，不生成）。 */
export function existingCaFingerprint(dataDir: string): string | null {
  const file = join(dataDir, TLS_DIR, CA_CERT);
  if (!existsSync(file)) return null;
  try {
    return fingerprintOf(readFileSync(file, "utf8"));
  } catch {
    return null;
  }
}

export function fileMaterial(certFile: string, keyFile: string): TlsMaterial {
  let cert: string;
  let key: string;
  let parsed: X509Certificate;
  try {
    cert = readFileSync(certFile, "utf8");
    key = readFileSync(keyFile, "utf8");
    parsed = new X509Certificate(cert);
    if (!parsed.checkPrivateKey(createPrivateKey(key)))
      throw new TlsError("证书与私钥对不上");
  } catch (error) {
    if (error instanceof TlsError) throw error;
    throw new TlsError("证书或私钥读不出来：检查 --tls-cert 与 --tls-key");
  }
  const chain = pemBlocks(cert);
  const anchor = chain.length > 1 ? chain[chain.length - 1] : undefined;
  return {
    cert,
    key,
    source: "file",
    fingerprint: fingerprintOf(anchor ?? parsed),
    ...(anchor ? { anchor } : {}),
    notAfter: parsed.validTo,
    names: sanNames(parsed),
  };
}

export interface TlsManager {
  current(): TlsMaterial;
  /** 证书换了（续期、热加载）时回调；答停止函数。 */
  start(
    onChange: (material: TlsMaterial) => void,
    onError: (error: unknown) => void,
  ): () => void;
}

export function selfSignedManager(
  dataDir: string,
  names: readonly string[],
  now: () => Date = () => new Date(),
): TlsManager {
  let material = selfSignedMaterial(dataDir, names, now());
  return {
    current: () => material,
    start(onChange, onError) {
      const timer = setInterval(() => {
        try {
          const next = selfSignedMaterial(dataDir, names, now());
          if (next.cert !== material.cert) {
            material = next;
            onChange(next);
          }
        } catch (error) {
          onError(error);
        }
      }, RENEW_CHECK_MS);
      timer.unref();
      return () => clearInterval(timer);
    },
  };
}

export function fileManager(
  certFile: string,
  keyFile: string,
  options: { pollMs?: number } = {},
): TlsManager {
  const pollMs = options.pollMs ?? 2000;
  let material = fileMaterial(certFile, keyFile);
  return {
    current: () => material,
    start(onChange, onError) {
      let pending: NodeJS.Timeout | undefined;
      const reload = () => {
        clearTimeout(pending);
        pending = setTimeout(() => {
          try {
            const next = fileMaterial(certFile, keyFile);
            if (next.cert !== material.cert || next.key !== material.key) {
              material = next;
              onChange(next);
            }
          } catch (error) {
            // 换到一半（证书新、私钥旧）时对不上：继续用旧的，等下一次变化。
            onError(error);
          }
        }, 500);
        pending.unref();
      };
      // 轮询 stat 而不是 fs.watch：证书常被「写临时文件再改名」替换，inode 会变。
      for (const file of [certFile, keyFile])
        watchFile(file, { interval: pollMs, persistent: false }, reload);
      // watchFile 的基线 stat 是异步取的：构造之后、基线之前的替换不会触发，启动时补查一次。
      reload();
      return () => {
        clearTimeout(pending);
        for (const file of [certFile, keyFile]) unwatchFile(file, reload);
      };
    },
  };
}
