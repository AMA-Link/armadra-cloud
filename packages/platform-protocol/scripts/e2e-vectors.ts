/**
 * 端到端通道 `armadra-e2e/1` 的黄金向量（`fixtures/e2e/`）。
 *
 * 这里是**独立的参照实现**：只用 `node:crypto`（X25519、HKDF、AES-256-GCM、Ed25519、HMAC），
 * 不调用 `src/e2e/` 的任何代码；`src/e2e/` 用 WebCrypto 实现，测试拿它对着这些字节逐个比。
 * 两套实现对上，才说明构造（转录、标签、布局）写对了，而不只是自洽。
 *
 * 输入全部固定（种子、nonce、秘密），X25519 / Ed25519 / AES-GCM 都是确定的，重复运行字节不变。
 */
import {
  createCipheriv,
  createHash,
  createHmac,
  createPrivateKey,
  createPublicKey,
  diffieHellman,
  hkdfSync,
  sign,
  type KeyObject,
} from "node:crypto";

const PROTOCOL = "armadra-e2e/1";
const LABEL = {
  hsC2s: `${PROTOCOL} hs c2s`,
  hsS2c: `${PROTOCOL} hs s2c`,
  c2s: `${PROTOCOL} c2s`,
  s2c: `${PROTOCOL} s2c`,
  sas: `${PROTOCOL} sas`,
  proof: `${PROTOCOL} proof`,
  core: `${PROTOCOL} core`,
  device: `${PROTOCOL} device`,
};

const hex = (bytes: Uint8Array) => Buffer.from(bytes).toString("hex");
const text = (value: string) => Buffer.from(value, "utf8");
const sha256 = (...parts: Uint8Array[]) =>
  createHash("sha256").update(Buffer.concat(parts)).digest();
const hkdf = (
  ikm: Uint8Array,
  salt: Uint8Array,
  info: string,
  length: number,
) => Buffer.from(hkdfSync("sha256", ikm, salt, text(info), length));
const u32 = (value: number) => {
  const out = Buffer.alloc(4);
  out.writeUInt32BE(value);
  return out;
};

function seal(
  key: Uint8Array,
  nonce: Uint8Array,
  aad: Uint8Array,
  plaintext: Uint8Array,
): Buffer {
  const cipher = createCipheriv("aes-256-gcm", key, nonce);
  cipher.setAAD(aad);
  return Buffer.concat([
    cipher.update(plaintext),
    cipher.final(),
    cipher.getAuthTag(),
  ]);
}

const PKCS8 = {
  x25519: "302e020100300506032b656e04220420",
  ed25519: "302e020100300506032b657004220420",
};

function keyFromSeed(kind: keyof typeof PKCS8, seed: Buffer): KeyObject {
  return createPrivateKey({
    key: Buffer.concat([Buffer.from(PKCS8[kind], "hex"), seed]),
    format: "der",
    type: "pkcs8",
  });
}

function rawPublic(key: KeyObject): Buffer {
  const jwk = createPublicKey(key).export({ format: "jwk" }) as { x: string };
  return Buffer.from(jwk.x, "base64url");
}

function x25519Public(raw: Buffer): KeyObject {
  return createPublicKey({
    key: { kty: "OKP", crv: "X25519", x: raw.toString("base64url") },
    format: "jwk",
  });
}

const BASE32 = "abcdefghijklmnopqrstuvwxyz234567";
function base32(bytes: Uint8Array): string {
  let bits = "";
  for (const byte of bytes) bits += byte.toString(2).padStart(8, "0");
  while (bits.length % 5 !== 0) bits += "0";
  let out = "";
  for (let at = 0; at < bits.length; at += 5)
    out += BASE32[Number.parseInt(bits.slice(at, at + 5), 2)];
  return out;
}

/** 内层隧道帧（与 `tunnel/frames.ts` 的布局相同，这里手写，不经实现）。 */
function frameOpen(streamId: number, open: object): Buffer {
  return Buffer.concat([
    Buffer.of(1),
    u32(streamId),
    text(JSON.stringify(open)),
  ]);
}
function frameData(streamId: number, bytes: Uint8Array): Buffer {
  return Buffer.concat([Buffer.of(2), u32(streamId), bytes]);
}
function frameEnd(streamId: number): Buffer {
  return Buffer.concat([Buffer.of(3), u32(streamId)]);
}

function entries(list: Buffer[], padding: boolean): Buffer {
  const body = Buffer.concat(
    list.flatMap((entry) => [u32(entry.length), entry]),
  );
  if (!padding || body.length % 64 === 0) return body;
  const padded = Math.ceil((body.length + 4) / 64) * 64;
  return Buffer.concat([body, Buffer.alloc(padded - body.length)]);
}

function record(
  key: Buffer,
  salt: Buffer,
  counter: number,
  plaintext: Buffer,
): Buffer {
  const header = u32(plaintext.length + 16);
  const nonce = Buffer.concat([salt, Buffer.alloc(4), u32(counter)]);
  return Buffer.concat([header, seal(key, nonce, header, plaintext)]);
}

export interface E2eVectorInput {
  name: string;
  sourceId: string;
  flags: number;
  pinned: boolean;
  clientEphemeralSeed: number;
  coreEphemeralSeed: number;
  coreIdentitySeed: number;
  deviceSeed: number;
  nonceFill: number;
  platform: string;
  deviceName: string;
  /** 配对秘密（hex），没有就不带 proof。 */
  proofSecret: string | null;
}

export function e2eVector(input: E2eVectorInput) {
  const seed = (fill: number) => Buffer.alloc(32, fill);
  const clientEphemeral = keyFromSeed(
    "x25519",
    seed(input.clientEphemeralSeed),
  );
  const coreEphemeral = keyFromSeed("x25519", seed(input.coreEphemeralSeed));
  const coreIdentity = keyFromSeed("ed25519", seed(input.coreIdentitySeed));
  const device = keyFromSeed("ed25519", seed(input.deviceSeed));
  const eC = rawPublic(clientEphemeral);
  const eS = rawPublic(coreEphemeral);
  const coreIdentityPub = rawPublic(coreIdentity);
  const devicePub = rawPublic(device);
  const nonceC = Buffer.alloc(16, input.nonceFill);
  const pin = input.pinned
    ? sha256(coreIdentityPub).subarray(0, 8)
    : Buffer.alloc(8);

  const h0 = sha256(text(PROTOCOL), text(input.sourceId));
  const m1 = Buffer.concat([Buffer.of(1, input.flags), eC, nonceC, pin]);
  const h1 = sha256(h0, m1);

  const dh = diffieHellman({
    privateKey: coreEphemeral,
    publicKey: x25519Public(eC),
  });
  const dhClient = diffieHellman({
    privateKey: clientEphemeral,
    publicKey: x25519Public(eS),
  });
  if (!dh.equals(dhClient)) throw new Error("X25519 两侧不一致");
  const h2 = sha256(h1, eS);
  const kHsS2c = hkdf(dh, h2, LABEL.hsS2c, 32);
  const kHsC2s = hkdf(dh, h2, LABEL.hsC2s, 32);
  const sigCore = sign(
    null,
    Buffer.concat([text(LABEL.core), h2, coreIdentityPub]),
    coreIdentity,
  );
  const zero = Buffer.alloc(12);
  const c2 = seal(kHsS2c, zero, h2, Buffer.concat([coreIdentityPub, sigCore]));
  const m2 = Buffer.concat([eS, c2]);
  const h3 = sha256(h2, c2);

  const sigDevice = sign(
    null,
    Buffer.concat([text(LABEL.device), h3, devicePub]),
    device,
  );
  const proofKey = input.proofSecret
    ? hkdf(
        Buffer.from(input.proofSecret, "hex"),
        Buffer.alloc(0),
        LABEL.proof,
        32,
      )
    : null;
  const proof = proofKey
    ? createHmac("sha256", proofKey).update(h3).digest()
    : null;
  const platform = text(input.platform);
  const deviceName = text(input.deviceName);
  const devicePlain = Buffer.concat([
    devicePub,
    sigDevice,
    Buffer.of(platform.length),
    platform,
    Buffer.of(deviceName.length),
    deviceName,
    Buffer.of(proof ? 32 : 0),
    proof ?? Buffer.alloc(0),
  ]);
  const m3 = seal(kHsC2s, zero, h3, devicePlain);
  const h4 = sha256(h3, m3);

  const c2sBytes = hkdf(dh, h4, LABEL.c2s, 36);
  const s2cBytes = hkdf(dh, h4, LABEL.s2c, 36);
  const c2s = { key: c2sBytes.subarray(0, 32), salt: c2sBytes.subarray(32) };
  const s2c = { key: s2cBytes.subarray(0, 32), salt: s2cBytes.subarray(32) };
  const sas = String(
    hkdf(dh, h4, LABEL.sas, 4).readUInt32BE(0) % 1_000_000,
  ).padStart(6, "0");
  const padding = (input.flags & 1) !== 0;

  const accept = {
    type: "accept",
    deviceStatus: input.proofSecret ? "trusted" : "pending",
    ...(input.proofSecret ? {} : { pendingId: "pnd_01" }),
    limits: {
      maxStreams: 256,
      streamWindow: 262_144,
      tunnelWindow: 8_388_608,
      maxFrameBytes: 1_048_576,
    },
  };
  const acceptRecord = record(
    s2c.key,
    s2c.salt,
    0,
    entries([text(JSON.stringify(accept))], padding),
  );
  const request =
    "GET /api/health HTTP/1.1\r\nhost: core\r\nauthorization: Bearer t\r\nconnection: close\r\n\r\n";
  const open = {
    kind: "http",
    method: "GET",
    path: "/api/health",
    headers: [],
    clientOrigin: null,
    remoteIp: "",
    relayNode: "",
  };
  const clientFrames = [
    frameOpen(1, open),
    frameData(1, text(request)),
    frameEnd(1),
  ];
  const clientRecords = [0, 1].map((counter) =>
    record(c2s.key, c2s.salt, counter, entries(clientFrames, padding)),
  );

  return {
    name: input.name,
    inputs: {
      sourceId: input.sourceId,
      flags: input.flags,
      pinned: input.pinned,
      clientEphemeralSeed: hex(seed(input.clientEphemeralSeed)),
      coreEphemeralSeed: hex(seed(input.coreEphemeralSeed)),
      coreIdentitySeed: hex(seed(input.coreIdentitySeed)),
      deviceSeed: hex(seed(input.deviceSeed)),
      nonceC: hex(nonceC),
      platform: input.platform,
      deviceName: input.deviceName,
      proofSecret: input.proofSecret,
    },
    keys: {
      clientEphemeralPub: hex(eC),
      coreEphemeralPub: hex(eS),
      coreIdentityPub: hex(coreIdentityPub),
      devicePub: hex(devicePub),
      coreFingerprint: base32(sha256(coreIdentityPub)),
      pinHint: hex(sha256(coreIdentityPub).subarray(0, 8)),
      deviceId: hex(sha256(devicePub).subarray(0, 16)),
    },
    transcript: {
      h0: hex(h0),
      h1: hex(h1),
      h2: hex(h2),
      h3: hex(h3),
      h4: hex(h4),
    },
    secrets: {
      dh: hex(dh),
      handshakeCoreToClient: hex(kHsS2c),
      handshakeClientToCore: hex(kHsC2s),
      clientToCore: { key: hex(c2s.key), salt: hex(c2s.salt) },
      coreToClient: { key: hex(s2c.key), salt: hex(s2c.salt) },
      proof: proof ? hex(proof) : null,
    },
    sas,
    messages: { m1: hex(m1), m2: hex(m2), m3: hex(m3) },
    records: {
      /** r0：core → 客户端第一条记录（计数器 0），一个 JSON 条目。 */
      accept: { value: accept, unit: hex(acceptRecord) },
      /** 客户端 → core 计数器 0、1 的两条记录，内容都是这三个内层帧。 */
      clientFrames: clientFrames.map(hex),
      client: clientRecords.map(hex),
    },
  };
}

export const E2E_VECTOR_INPUTS: E2eVectorInput[] = [
  {
    name: "pairing",
    sourceId: "0123456789abcdef0123456789abcdef",
    flags: 0,
    pinned: false,
    clientEphemeralSeed: 0x31,
    coreEphemeralSeed: 0x32,
    /** 与 assertion/keys.json 的 source 同一把源钥。 */
    coreIdentitySeed: 0x22,
    deviceSeed: 0x33,
    nonceFill: 0xc1,
    platform: "browser",
    deviceName: "Chrome",
    proofSecret: "5e".repeat(16),
  },
  {
    name: "pinned-padded",
    sourceId: "0123456789abcdef0123456789abcdef",
    flags: 1,
    pinned: true,
    clientEphemeralSeed: 0x41,
    coreEphemeralSeed: 0x42,
    coreIdentitySeed: 0x22,
    deviceSeed: 0x43,
    nonceFill: 0xd2,
    platform: "ios",
    deviceName: "我的 iPhone",
    proofSecret: null,
  },
];
