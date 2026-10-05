/**
 * 黄金夹具（协议包设计 §9），**Node 专用、只在测试里用**：按名字读包里 `fixtures/`
 * 下的文件。`name` 是相对 `fixtures/` 的路径，例如 `tunnel/ping.bin`、
 * `assertion/valid.jwt`、`cloud-api/auth.login.json`。
 *
 * 夹具由 `node scripts/gen-fixtures.ts`（在包目录下）生成并提交；测试对着字节比。
 */
import { readdirSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

/** 夹具里所有时间的「现在」：2025-10-09T09:06:40Z。JWT 的 `iat` = FIXED_NOW_MS / 1000。 */
export const FIXED_NOW_MS = 1_760_000_000_000;

const ROOT = new URL("../../fixtures/", import.meta.url);

/** 夹具目录的绝对路径。 */
export const FIXTURES_DIR = fileURLToPath(ROOT);

function safeUrl(name: string): URL {
  if (name.startsWith("/") || name.split("/").includes("..")) {
    throw new Error(`夹具名不合法：${name}`);
  }
  return new URL(name, ROOT);
}

export function readFixture(name: string): Uint8Array {
  return new Uint8Array(readFileSync(safeUrl(name)));
}

export function readTextFixture(name: string): string {
  return readFileSync(safeUrl(name), "utf8").trimEnd();
}

export function readJsonFixture<T = unknown>(name: string): T {
  return JSON.parse(readFileSync(safeUrl(name), "utf8")) as T;
}

/** 一个子目录下的夹具名（相对 `fixtures/`），按名字排序。 */
export function listFixtures(directory: string): string[] {
  return readdirSync(safeUrl(`${directory}/`))
    .sort()
    .map((name) => `${directory}/${name}`);
}

/**
 * 帧序列文件（`tunnel/sequence-*.bin`）的布局：每帧前置 u32 BE 长度。
 * 隧道上每帧是一条独立的 WS 二进制消息，文件里需要分隔。
 */
export function splitSequence(bytes: Uint8Array): Uint8Array[] {
  const frames: Uint8Array[] = [];
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  let at = 0;
  while (at < bytes.length) {
    if (at + 4 > bytes.length) throw new Error("序列在长度前缀处截断");
    const length = view.getUint32(at, false);
    at += 4;
    if (at + length > bytes.length) throw new Error("序列在帧内截断");
    frames.push(bytes.slice(at, at + length));
    at += length;
  }
  return frames;
}

export function joinSequence(frames: readonly Uint8Array[]): Uint8Array {
  const total = frames.reduce((sum, frame) => sum + 4 + frame.length, 0);
  const out = new Uint8Array(total);
  const view = new DataView(out.buffer);
  let at = 0;
  for (const frame of frames) {
    view.setUint32(at, frame.length, false);
    out.set(frame, at + 4);
    at += 4 + frame.length;
  }
  return out;
}

export interface TestKey {
  kid: string;
  privateJwk: { kty: "OKP"; crv: "Ed25519"; x: string; d: string };
  publicJwk: {
    kty: "OKP";
    crv: "Ed25519";
    x: string;
    kid: string;
    alg: "EdDSA";
    use: "sig";
  };
}

export interface TestKeys {
  /** 控制面 / personal 中继的签名钥。 */
  issuer: TestKey & { issuer: string };
  /** 源（core）的钥；`kid` = sourceId。 */
  source: TestKey & { sourceId: string };
}

/** 测试用 Ed25519 钥对（**只用于测试**，私钥公开在仓库里）。 */
export const TEST_KEYS: TestKeys = readJsonFixture<TestKeys>(
  "assertion/keys.json",
);
