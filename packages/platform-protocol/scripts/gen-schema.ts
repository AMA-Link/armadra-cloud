/**
 * 生成语言无关的 schema 导出 `schema/`（SaaS 设计 §4.5）：给将来非 TypeScript 的实现
 * （Go 版中继内核）生成类型用，随 npm 包与 tarball 发布。
 *
 * - `assertion/*.json`：五种令牌声明与公钥（zod 4 `z.toJSONSchema`）；
 * - `tunnel/*.json`：握手的 hello / challenge / auth / ready / reject 与帧里的 JSON 载荷；
 * - `errors.json`：错误码 → HTTP 状态码；
 * - `constants.json`：协议版本、路径、隧道限制与身份常量（identity-vectors）。
 *
 * `/v1` 契约的 OpenAPI（`schema/cloud-openapi.json`）由 `tools/contract/generate.mjs`
 * 一起写，这里只校验它在。字节由输入决定，重复运行不变。
 *
 *   node scripts/gen-schema.ts           重写 schema/（OpenAPI 除外）
 *   node scripts/gen-schema.ts --check   与仓库里的不同就失败
 */
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

import { z } from "zod";

import {
  accessClaimsSchema,
  assertionClaimsSchema,
  ed25519PublicJwkSchema,
  jwkSetSchema,
  relayTokenClaimsSchema,
  SCOPE_PREFIX,
  SCOPE_READ_ONLY,
  sourceJwsClaimsSchema,
  TOKEN_TYPES,
  AUDIENCES,
  CLAIMS_VERSION,
  tunnelTokenClaimsSchema,
} from "../src/assertion/claims.ts";
import {
  READ_ONLY_IDENTITY_PREFIXES,
  READ_ONLY_SAFE_METHODS,
} from "../src/assertion/read-only.ts";
import { ERRORS } from "../src/errors/registry.ts";
import * as identity from "../src/identity-vectors/constants.ts";
import { CLOSE, FRAME_TYPE, RST } from "../src/tunnel/codes.ts";
import {
  goawayPayloadSchema,
  openPayloadSchema,
} from "../src/tunnel/frames.ts";
import {
  authSchema,
  challengeSchema,
  helloSchema,
  readySchema,
  rejectSchema,
} from "../src/tunnel/handshake.ts";
import { LIMITS } from "../src/tunnel/limits.ts";
import * as version from "../src/version.ts";

const ROOT = fileURLToPath(new URL("../schema/", import.meta.url));
/** 由 `tools/contract/generate.mjs` 写，这里不动它。 */
const OPENAPI = "cloud-openapi.json";

const files = new Map<string, string>();
const put = (name: string, value: unknown) =>
  files.set(name, `${JSON.stringify(value, null, 2)}\n`);
const schema = (name: string, type: z.ZodType) =>
  put(name, { $id: name, ...z.toJSONSchema(type, { io: "input" }) });

// ------------------------------------------------------------ 断言与令牌

schema("assertion/access-claims.json", accessClaimsSchema);
schema("assertion/assertion-claims.json", assertionClaimsSchema);
schema("assertion/relay-token-claims.json", relayTokenClaimsSchema);
schema("assertion/tunnel-token-claims.json", tunnelTokenClaimsSchema);
schema("assertion/source-jws-claims.json", sourceJwsClaimsSchema);
schema("assertion/ed25519-public-jwk.json", ed25519PublicJwkSchema);
schema("assertion/jwk-set.json", jwkSetSchema);

// ------------------------------------------------------------ 隧道

schema("tunnel/hello.json", helloSchema);
schema("tunnel/challenge.json", challengeSchema);
schema("tunnel/auth.json", authSchema);
schema("tunnel/ready.json", readySchema);
schema("tunnel/reject.json", rejectSchema);
schema("tunnel/open-payload.json", openPayloadSchema);
schema("tunnel/goaway-payload.json", goawayPayloadSchema);

// ------------------------------------------------------------ 错误码与常量

put(
  "errors.json",
  Object.fromEntries(
    Object.entries(ERRORS).map(([code, entry]) => [code, entry.status]),
  ),
);

const constants = Object.fromEntries(
  Object.entries(identity).filter(([, value]) => typeof value !== "function"),
);
put("constants.json", {
  protocol: version.PROTOCOL_VERSION,
  paths: {
    tunnel: version.TUNNEL_PATH,
    internode: version.INTERNODE_PATH,
    platformWellKnown: version.PLATFORM_WELL_KNOWN,
    jwksWellKnown: version.JWKS_WELL_KNOWN,
  },
  headers: {
    protocol: version.PROTOCOL_HEADER,
    relayToken: version.RELAY_TOKEN_HEADER,
  },
  subprotocols: {
    relay: version.RELAY_SUBPROTOCOL_PREFIX,
    ticket: version.TICKET_SUBPROTOCOL_PREFIX,
  },
  tokens: {
    types: TOKEN_TYPES,
    audiences: AUDIENCES,
    claimsVersion: CLAIMS_VERSION,
    scope: { prefix: SCOPE_PREFIX, readOnly: SCOPE_READ_ONLY },
    readOnly: {
      safeMethods: READ_ONLY_SAFE_METHODS,
      identityPrefixes: READ_ONLY_IDENTITY_PREFIXES,
    },
  },
  tunnel: { limits: LIMITS, frameTypes: FRAME_TYPE, rst: RST, close: CLOSE },
  identity: constants,
});

// ------------------------------------------------------------ write

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
  for (const [name, text] of files) {
    if (!existing.has(name)) problems.push(`缺少 ${name}`);
    else if (readFileSync(join(ROOT, name), "utf8") !== text)
      problems.push(`不同 ${name}`);
    existing.delete(name);
  }
  if (!existing.delete(OPENAPI))
    problems.push(`缺少 ${OPENAPI}（运行 pnpm contract:generate）`);
  for (const name of existing) problems.push(`多余 ${name}`);
  if (problems.length > 0) {
    console.error(
      `schema 导出与生成结果不一致（运行 pnpm --filter @armadra/platform-protocol schema）：\n${problems.join("\n")}`,
    );
    process.exit(1);
  }
  console.log(`schema 导出一致（${files.size + 1} 个文件）`);
} else {
  for (const name of walk(ROOT))
    if (name !== OPENAPI) rmSync(join(ROOT, name), { force: true });
  for (const [name, text] of files) {
    const path = join(ROOT, name);
    mkdirSync(dirname(path), { recursive: true });
    writeFileSync(path, text);
  }
  console.log(`已写入 ${files.size} 个 schema 到 ${ROOT}`);
}
