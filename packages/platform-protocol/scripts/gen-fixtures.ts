/**
 * 生成黄金夹具 `fixtures/`（协议包设计 §9）。输入全部固定（种子、nonce、时间），
 * Ed25519 签名是确定的，所以重复运行字节不变。
 *
 *   node scripts/gen-fixtures.ts           重写 fixtures/
 *   node scripts/gen-fixtures.ts --check   与仓库里的不同就失败
 *
 * 改了帧布局、声明或契约之后重新生成，并在同一个 PR 里提交（minor / major 按 §16.3）。
 */
import {
  createPrivateKey,
  createPublicKey,
  sign,
  type KeyObject,
} from "node:crypto";
import {
  existsSync,
  mkdirSync,
  readdirSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { dirname, join, relative } from "node:path";
import { fileURLToPath } from "node:url";

import { buildSigningInput, joinCompactJws } from "../src/assertion/jws.ts";
import { TOKEN_TYPES } from "../src/assertion/claims.ts";
import { ERRORS } from "../src/errors/registry.ts";
import {
  ASSERTION,
  RELAY_TOKEN,
  SESSION,
  SOURCE_JWS,
  TUNNEL_TOKEN,
} from "../src/identity-vectors/constants.ts";
import { toBase64Url } from "../src/internal/bytes.ts";
import { encodeFrame, type Frame } from "../src/tunnel/frames.ts";
import { challengeSigningInput } from "../src/tunnel/handshake.ts";
import { LIMITS } from "../src/tunnel/limits.ts";
import { PROTOCOL_VERSION } from "../src/version.ts";
import { CLOUD_API_SAMPLES } from "./cloud-api-samples.ts";
import { wireFixtures } from "./wire-fixtures.ts";

const ROOT = fileURLToPath(new URL("../fixtures/", import.meta.url));
const FIXED_NOW_MS = 1_760_000_000_000;
const NOW_S = FIXED_NOW_MS / 1000;
const ISSUER = "https://cloud.armadra.test";
const SOURCE_ID = "0123456789abcdef0123456789abcdef";
const ACCOUNT_ID = "acct_01JAAAAAAAAAAAAAAAAAAAAAAA";

const files = new Map<string, Uint8Array>();
const text = (value: string) => new TextEncoder().encode(value);
const json = (value: unknown) => text(`${JSON.stringify(value, null, 2)}\n`);
const put = (name: string, bytes: Uint8Array) => files.set(name, bytes);

// ------------------------------------------------------------------ keys

/** 固定种子 → Ed25519 私钥（PKCS#8 DER 前缀 + 32 字节种子）。 */
function keyFromSeed(fill: number): KeyObject {
  const prefix = Buffer.from("302e020100300506032b657004220420", "hex");
  const seed = Buffer.alloc(32, fill);
  return createPrivateKey({
    key: Buffer.concat([prefix, seed]),
    format: "der",
    type: "pkcs8",
  });
}

function describeKey(key: KeyObject, kid: string) {
  const jwk = key.export({ format: "jwk" }) as {
    kty: "OKP";
    crv: "Ed25519";
    x: string;
    d: string;
  };
  const publicJwk = createPublicKey(key).export({ format: "jwk" }) as {
    x: string;
  };
  return {
    kid,
    privateJwk: { kty: "OKP", crv: "Ed25519", x: jwk.x, d: jwk.d },
    publicJwk: {
      kty: "OKP",
      crv: "Ed25519",
      x: publicJwk.x,
      kid,
      alg: "EdDSA",
      use: "sig",
    },
  };
}

const issuerKey = keyFromSeed(0x11);
const sourceKey = keyFromSeed(0x22);
const issuer = {
  ...describeKey(issuerKey, "test-issuer-2025-10"),
  issuer: ISSUER,
};
const source = { ...describeKey(sourceKey, SOURCE_ID), sourceId: SOURCE_ID };
put(
  "assertion/keys.json",
  json({ $comment: "只用于测试：私钥公开在仓库里。", issuer, source }),
);

// ------------------------------------------------------------------ tunnel

const openHttp: Frame = {
  type: "open",
  streamId: 1,
  open: {
    kind: "http",
    method: "POST",
    path: "/api/workspaces?limit=10",
    headers: [
      ["host", `${SOURCE_ID}.src.relay.armadra.test`],
      ["content-type", "application/json"],
      ["content-length", "13"],
    ],
    clientOrigin: "https://app.armadra.test",
    remoteIp: "203.0.113.7",
    relayNode: "relay-1",
  },
};
const openWs: Frame = {
  type: "open",
  streamId: 3,
  open: {
    kind: "ws",
    method: "GET",
    path: "/api/events",
    headers: [
      ["host", `${SOURCE_ID}.src.relay.armadra.test`],
      ["upgrade", "websocket"],
      ["sec-websocket-protocol", "armadra-ticket.test-ticket"],
    ],
    clientOrigin: "https://app.armadra.test",
    remoteIp: "203.0.113.7",
    relayNode: "relay-1",
  },
};
const data64k = new Uint8Array(LIMITS.maxDataChunk);
for (let index = 0; index < data64k.length; index += 1)
  data64k[index] = (index * 31 + 7) & 0xff;

const single: Record<string, Frame> = {
  "open-http": openHttp,
  "open-ws": openWs,
  "data-64k": { type: "data", streamId: 1, bytes: data64k },
  end: { type: "end", streamId: 1 },
  "rst-3": { type: "rst", streamId: 1, code: 3 },
  "window-stream": { type: "window", streamId: 1, credit: 131_072 },
  "window-tunnel": { type: "window", streamId: 0, credit: 4_194_304 },
  ping: { type: "ping", ts: FIXED_NOW_MS },
  pong: { type: "pong", ts: FIXED_NOW_MS },
  goaway: { type: "goaway", reason: "shutdown", graceMs: LIMITS.goawayGraceMs },
};
for (const [name, frame] of Object.entries(single))
  put(`tunnel/${name}.bin`, encodeFrame(frame));

const requestHead =
  "POST /api/workspaces?limit=10 HTTP/1.1\r\n" +
  `host: ${SOURCE_ID}.src.relay.armadra.test\r\n` +
  "content-type: application/json\r\ncontent-length: 13\r\n" +
  "x-forwarded-for: 203.0.113.7\r\nx-forwarded-proto: https\r\n\r\n";
const response =
  'HTTP/1.1 200 OK\r\ncontent-type: application/json\r\ncontent-length: 11\r\n\r\n{"ok":true}';
const sequence: Frame[] = [
  openHttp,
  { type: "data", streamId: 1, bytes: text(requestHead) },
  { type: "data", streamId: 1, bytes: text('{"name":"a"}\n') },
  { type: "end", streamId: 1 },
  { type: "data", streamId: 1, bytes: text(response) },
  { type: "end", streamId: 1 },
];
const encoded = sequence.map(encodeFrame);
const joined = new Uint8Array(
  encoded.reduce((sum, frame) => sum + 4 + frame.length, 0),
);
{
  const view = new DataView(joined.buffer);
  let at = 0;
  for (const frame of encoded) {
    view.setUint32(at, frame.length, false);
    joined.set(frame, at + 4);
    at += 4 + frame.length;
  }
}
put("tunnel/sequence-http-roundtrip.bin", joined);

const nonce = toBase64Url(new Uint8Array(16).fill(0xa1));
const nonce2 = toBase64Url(new Uint8Array(16).fill(0xb2));
const relayNode = "relay-1";
const signingInput = challengeSigningInput({
  sourceId: SOURCE_ID,
  nonce,
  nonce2,
  relayNode,
});

// ------------------------------------------------------------------ tokens

function jwt(
  header: object,
  payload: object,
  key: KeyObject = issuerKey,
): string {
  const {
    signingInput: input,
    encodedHeader,
    encodedPayload,
  } = buildSigningInput(header, payload);
  return joinCompactJws(
    encodedHeader,
    encodedPayload,
    new Uint8Array(sign(null, input, key)),
  );
}

const header = (typ: string) => ({ alg: "EdDSA", kid: issuer.kid, typ });
const times = (ttlMs: number, iat = NOW_S) => ({
  iat,
  exp: iat + ttlMs / 1000,
});
const device = {
  deviceId: "dev_01JAAAAAAAAAAAAAAAAAAAAAAA",
  platform: "browser",
  name: "Chrome",
};
const assertion = {
  iss: ISSUER,
  sub: ACCOUNT_ID,
  aud: SOURCE_ID,
  ...times(ASSERTION.ttlMs),
  jti: "jti-assertion-0001",
  ver: 1,
  name: "Dev",
  email: "dev@armadra.test",
  emailVerified: true,
  device,
};

const tunnelToken = jwt(header(TOKEN_TYPES.tunnel), {
  iss: ISSUER,
  sub: SOURCE_ID,
  aud: "relay-tunnel",
  cnf: { jwk: { kty: "OKP", crv: "Ed25519", x: source.publicJwk.x } },
  ...times(TUNNEL_TOKEN.ttlMs),
  jti: "jti-tunnel-0001",
  ver: 1,
});

const tokens: Record<string, string> = {
  valid: jwt(header(TOKEN_TYPES.assertion), assertion),
  expired: jwt(header(TOKEN_TYPES.assertion), {
    ...assertion,
    ...times(ASSERTION.ttlMs, NOW_S - 3600),
    jti: "jti-assertion-expired",
  }),
  "wrong-aud": jwt(header(TOKEN_TYPES.assertion), {
    ...assertion,
    aud: "fedcba9876543210fedcba9876543210",
    jti: "jti-assertion-wrong-aud",
  }),
  /** 头声称 HS256；签名仍是 Ed25519 的 64 字节，拆包时就该按 alg 拒绝。 */
  "wrong-alg": jwt(
    { alg: "HS256", kid: issuer.kid, typ: TOKEN_TYPES.assertion },
    { ...assertion, jti: "jti-assertion-wrong-alg" },
  ),
  guest: jwt(header(TOKEN_TYPES.assertion), {
    ...assertion,
    sub: "guest:lnk_01JAAAAAAAAAAAAAAAAAAAAAAA:r4nd0m",
    name: "Guest",
    email: undefined,
    emailVerified: undefined,
    link: { linkId: "lnk_01JAAAAAAAAAAAAAAAAAAAAAAA", invitationId: "inv_01" },
    jti: "jti-assertion-guest",
  }),
  "with-link": jwt(header(TOKEN_TYPES.assertion), {
    ...assertion,
    org: { orgId: "org_01JAAAAAAAAAAAAAAAAAAAAAAA", role: "member" },
    link: { linkId: "lnk_01JAAAAAAAAAAAAAAAAAAAAAAA", invitationId: "inv_01" },
    jti: "jti-assertion-with-link",
  }),
  /** 协议 1.1：访客经「单个会话、只读」的链接进来。 */
  "scoped-session": jwt(header(TOKEN_TYPES.assertion), {
    ...assertion,
    sub: "guest:lnk_01JAAAAAAAAAAAAAAAAAAAAAAA:r4nd0m",
    name: "Guest",
    email: undefined,
    emailVerified: undefined,
    link: { linkId: "lnk_01JAAAAAAAAAAAAAAAAAAAAAAA", invitationId: "inv_01" },
    scp: ["ws:ws_01", "sess:sess_01", "ro"],
    jti: "jti-assertion-scoped-session",
  }),
  /** 协议 1.1：SaaS 组织成员经团队授予，指定两个工作空间、editor。 */
  "team-grant": jwt(header(TOKEN_TYPES.assertion), {
    ...assertion,
    org: { orgId: "org_01JAAAAAAAAAAAAAAAAAAAAAAA", role: "member" },
    team: { teamId: "team_01JAAAAAAAAAAAAAAAAAAAAAAA", role: "member" },
    role: "editor",
    scp: ["ws:ws_01", "ws:ws_02"],
    jti: "jti-assertion-team-grant",
  }),
  "access-token": jwt(header(TOKEN_TYPES.access), {
    iss: ISSUER,
    sub: ACCOUNT_ID,
    aud: "cloud",
    sid: "ses_01JAAAAAAAAAAAAAAAAAAAAAAA",
    ...times(SESSION.accessTtlMs),
    jti: "jti-access-0001",
    ver: 1,
  }),
  "relay-token": jwt(header(TOKEN_TYPES.relay), {
    iss: ISSUER,
    sub: ACCOUNT_ID,
    aud: "relay",
    src: SOURCE_ID,
    ...times(RELAY_TOKEN.ttlMs),
    jti: "jti-relay-0001",
    ver: 1,
  }),
  /** 协议 1.1：只读、带组织的中继令牌。 */
  "relay-token-ro": jwt(header(TOKEN_TYPES.relay), {
    iss: ISSUER,
    sub: "guest:lnk_01JAAAAAAAAAAAAAAAAAAAAAAA:r4nd0m",
    aud: "relay",
    src: SOURCE_ID,
    ...times(RELAY_TOKEN.ttlMs),
    jti: "jti-relay-ro-0001",
    ver: 1,
    org: "org_01JAAAAAAAAAAAAAAAAAAAAAAA",
    ro: true,
  }),
  "tunnel-token": tunnelToken,
  /** 协议 1.1：属于组织的源的隧道令牌。 */
  "tunnel-token-org": jwt(header(TOKEN_TYPES.tunnel), {
    iss: ISSUER,
    sub: SOURCE_ID,
    aud: "relay-tunnel",
    cnf: { jwk: { kty: "OKP", crv: "Ed25519", x: source.publicJwk.x } },
    ...times(TUNNEL_TOKEN.ttlMs),
    jti: "jti-tunnel-org-0001",
    ver: 1,
    org: "org_01JAAAAAAAAAAAAAAAAAAAAAAA",
  }),
  "source-jws": jwt(
    { alg: "EdDSA", kid: SOURCE_ID, typ: TOKEN_TYPES.source },
    {
      iss: SOURCE_ID,
      aud: ISSUER,
      ...times(SOURCE_JWS.ttlMs),
      jti: "jti-source-0001",
    },
    sourceKey,
  ),
};
for (const [name, token] of Object.entries(tokens))
  put(`assertion/${name}.jwt`, text(`${token}\n`));

put(
  "tunnel/handshake.json",
  json({
    $comment:
      "固定密钥（assertion/keys.json 的 source）与固定 nonce；auth.sig 是对 signingInput 的 Ed25519 签名。",
    signingInput: new TextDecoder().decode(signingInput),
    hello: {
      type: "hello",
      protocol: PROTOCOL_VERSION,
      sourceId: SOURCE_ID,
      coreVersion: "0.2.0",
      capabilities: ["identity.native-session.v1"],
      tunnelToken,
      nonce,
    },
    challenge: { type: "challenge", nonce2, relayNode },
    auth: {
      type: "auth",
      sig: toBase64Url(new Uint8Array(sign(null, signingInput, sourceKey))),
    },
    ready: {
      type: "ready",
      tunnelId: "fedcba9876543210fedcba9876543210",
      limits: {
        maxStreams: LIMITS.maxStreams,
        streamWindow: LIMITS.streamWindow,
        tunnelWindow: LIMITS.tunnelWindow,
        maxFrameBytes: LIMITS.maxFrameBytes,
      },
      heartbeatMs: LIMITS.heartbeatMs,
      protocol: PROTOCOL_VERSION,
    },
    reject: {
      type: "reject",
      code: "tunnel_token_expired",
      message: "tunnel token expired",
    },
  }),
);

// ---------------------------------------------------------- cloud-api / errors

for (const [key, sample] of Object.entries(CLOUD_API_SAMPLES))
  put(`cloud-api/${key}.json`, json(sample));
put("errors.json", json(ERRORS));

// ------------------------------------------------------------------ http1 / ws

for (const [name, bytes] of wireFixtures()) put(name, bytes);

// ------------------------------------------------------------------ write

function walk(directory: string): string[] {
  if (!existsSync(directory)) return [];
  return readdirSync(directory, { withFileTypes: true }).flatMap((entry) => {
    const path = join(directory, entry.name);
    return entry.isDirectory()
      ? walk(path)
      : [relative(ROOT, path).split("\\").join("/")];
  });
}

if (process.argv.includes("--check")) {
  const problems: string[] = [];
  const existing = new Set(walk(ROOT));
  for (const [name, bytes] of files) {
    if (!existing.has(name)) problems.push(`缺少 ${name}`);
    else if (
      !Buffer.from(readFileSync(join(ROOT, name))).equals(Buffer.from(bytes))
    )
      problems.push(`不同 ${name}`);
    existing.delete(name);
  }
  for (const name of existing) problems.push(`多余 ${name}`);
  if (problems.length > 0) {
    console.error(
      `夹具与生成结果不一致（运行 pnpm --filter @armadra/platform-protocol fixtures）：\n${problems.join("\n")}`,
    );
    process.exit(1);
  }
  console.log(`夹具一致（${files.size} 个文件）`);
} else {
  rmSync(ROOT, { recursive: true, force: true });
  for (const [name, bytes] of files) {
    const path = join(ROOT, name);
    mkdirSync(dirname(path), { recursive: true });
    writeFileSync(path, bytes);
  }
  console.log(`已写入 ${files.size} 个夹具到 ${ROOT}`);
}
