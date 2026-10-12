/**
 * 测试里的客户端：经 Worker（`SELF`）调 `/v1`，登记源、换隧道令牌与中继令牌。只用测试值。
 */
import {
  generateKeyPairSync,
  randomBytes,
  sign,
  type KeyObject,
} from "node:crypto";

import {
  buildSigningInput,
  joinCompactJws,
  TOKEN_TYPES,
} from "@armadra/platform-protocol/assertion";
import { SELF } from "cloudflare:test";

export const ISSUER = "https://relay.armadra.test";
export const ACCOUNT = "owner";
export const PASSWORD = "plover-quartz-lantern-41";
export const DEVICE = { platform: "desktop", name: "test" } as const;

let ipCounter = 10;
/** 每个用例一个客户端 IP（按 IP 的闸互不干扰）。 */
export function nextIp(): string {
  ipCounter += 1;
  return `198.51.100.${ipCounter}`;
}

export function call(
  path: string,
  init: RequestInit & { ip?: string } = {},
): Promise<Response> {
  const headers = new Headers(init.headers);
  headers.set("cf-connecting-ip", init.ip ?? "203.0.113.1");
  return SELF.fetch(`${ISSUER}${path}`, { ...init, headers });
}

export async function json<T = Record<string, unknown>>(
  response: Response,
): Promise<T> {
  return (await response.json()) as T;
}

export async function postJson(
  path: string,
  body: unknown,
  options: { token?: string; ip?: string; authorization?: string } = {},
): Promise<Response> {
  const headers: Record<string, string> = {
    "content-type": "application/json",
  };
  if (options.token) headers.authorization = `Bearer ${options.token}`;
  if (options.authorization) headers.authorization = options.authorization;
  return call(path, {
    method: "POST",
    body: JSON.stringify(body),
    headers,
    ip: options.ip,
  });
}

export interface Session {
  accessToken: string;
  refreshToken?: string;
}

export async function login(ip = nextIp()): Promise<Session> {
  const response = await postJson(
    "/v1/auth/login",
    { account: ACCOUNT, password: PASSWORD, device: DEVICE },
    { ip },
  );
  if (response.status !== 200)
    throw new Error(`login ${response.status} ${await response.text()}`);
  return (await json<{ session: Session }>(response)).session;
}

export interface TestSource {
  sourceId: string;
  privateKey: KeyObject;
  x: string;
}

export async function registerSource(session: Session): Promise<TestSource> {
  const tokenResponse = await postJson(
    "/v1/sources/registration-tokens",
    {},
    { token: session.accessToken },
  );
  const { registrationToken } = await json<{ registrationToken: string }>(
    tokenResponse,
  );
  const { privateKey, publicKey } = generateKeyPairSync("ed25519");
  const x = (publicKey.export({ format: "jwk" }) as { x: string }).x;
  const sourceId = randomBytes(16).toString("hex");
  const response = await postJson("/v1/sources/register", {
    registrationToken,
    sourceId,
    publicKey: { kty: "OKP", crv: "Ed25519", x },
    name: "test-source",
    kind: "server",
    coreVersion: "0.2.0-test",
    capabilities: [],
    protocol: { major: 1, minor: 2 },
  });
  if (response.status !== 200)
    throw new Error(`register ${response.status} ${await response.text()}`);
  return { sourceId, privateKey, x };
}

/** 源 JWS（`Authorization: Source <jws>`）。 */
export function sourceJws(source: TestSource): string {
  const iat = Math.floor(Date.now() / 1000);
  const { signingInput, encodedHeader, encodedPayload } = buildSigningInput(
    { alg: "EdDSA", typ: TOKEN_TYPES.source, kid: source.sourceId },
    {
      iss: source.sourceId,
      aud: ISSUER,
      iat,
      exp: iat + 60,
      jti: randomBytes(12).toString("hex"),
    },
  );
  return joinCompactJws(
    encodedHeader,
    encodedPayload,
    new Uint8Array(sign(null, signingInput, source.privateKey)),
  );
}

export interface RelayHint {
  tunnelToken: string;
  nodes: { url: string; weight: number }[];
}

export async function heartbeatHint(source: TestSource): Promise<RelayHint> {
  const response = await call("/v1/sources/me/relay", {
    headers: { authorization: `Source ${sourceJws(source)}` },
  });
  if (response.status !== 200)
    throw new Error(`relay ${response.status} ${await response.text()}`);
  return json<RelayHint>(response);
}

export interface Assertion {
  relayToken: string;
  relayBaseUrl: string;
  online: boolean;
}

export async function assertion(
  session: Session,
  sourceId: string,
): Promise<Assertion> {
  const response = await postJson(
    `/v1/sources/${sourceId}/assertion`,
    { device: DEVICE },
    { token: session.accessToken },
  );
  if (response.status !== 200)
    throw new Error(`assertion ${response.status} ${await response.text()}`);
  return json<Assertion>(response);
}

/** 经中继访问源：`/s/<id>/<path>`，带中继令牌头。 */
export function viaRelay(
  sourceId: string,
  path: string,
  init: RequestInit & { token?: string; ip?: string } = {},
): Promise<Response> {
  const headers = new Headers(init.headers);
  if (init.token) headers.set("armadra-relay-token", init.token);
  return call(`/s/${sourceId}${path}`, { ...init, headers });
}

/** 经中继开一条 WS：答已 accept 的 WebSocket 与响应。 */
export async function openClientWs(
  sourceId: string,
  path: string,
  token: string,
  protocols: string[] = ["armadra-ticket.test"],
): Promise<{ response: Response; ws: WebSocket | null }> {
  const response = await call(`/s/${sourceId}${path}`, {
    headers: {
      upgrade: "websocket",
      "sec-websocket-key": randomBytes(16).toString("base64"),
      "sec-websocket-version": "13",
      "sec-websocket-protocol": [`armadra-relay.${token}`, ...protocols].join(
        ", ",
      ),
    },
  });
  const ws = response.webSocket;
  ws?.accept();
  if (ws) ws.binaryType = "arraybuffer";
  return { response, ws };
}

/** 经中继开一条端到端通道（`_relay/e2e`，子协议 `armadra-e2e.1`）。 */
export function openChannel(
  sourceId: string,
  token: string,
): Promise<{ response: Response; ws: WebSocket | null }> {
  return openClientWs(sourceId, "/_relay/e2e", token, ["armadra-e2e.1"]);
}

/**
 * 开一条通道看结果：开成答 `null`（随后关掉），被拒答关闭码（中继先完成升级再关），HTTP 拒绝
 * 答状态码。
 */
export async function channelOutcome(
  sourceId: string,
  token: string,
): Promise<number | null> {
  const { response, ws } = await openChannel(sourceId, token);
  if (!ws) return response.status;
  const outcome = await Promise.race([
    closed(ws).then((event) => event.code),
    new Promise<null>((resolve) => setTimeout(() => resolve(null), 200)),
  ]);
  if (outcome === null) ws.close(1000, "done");
  return outcome;
}

/** 经通道回显一段字节（假 core 把 e2e 流接回自己）。 */
export async function echoChannel(
  ws: WebSocket,
  payload: Uint8Array,
): Promise<Uint8Array> {
  const chunks: Uint8Array[] = [];
  let size = 0;
  const done = new Promise<void>((resolve) => {
    const onMessage = (event: MessageEvent) => {
      const chunk = new Uint8Array(event.data as ArrayBuffer);
      chunks.push(chunk);
      size += chunk.length;
      if (size >= payload.length) {
        ws.removeEventListener("message", onMessage);
        resolve();
      }
    };
    ws.addEventListener("message", onMessage);
  });
  for (let offset = 0; offset < payload.length; offset += 60_000)
    ws.send(payload.subarray(offset, offset + 60_000));
  await done;
  const all = new Uint8Array(size);
  let offset = 0;
  for (const chunk of chunks) {
    all.set(chunk, offset);
    offset += chunk.length;
  }
  return all;
}

/** 等一个 WS 事件。 */
export function nextMessage(
  ws: WebSocket,
  timeoutMs = 5_000,
): Promise<string | ArrayBuffer> {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error("等消息超时")), timeoutMs);
    ws.addEventListener(
      "message",
      (event) => {
        clearTimeout(timer);
        resolve(event.data as string | ArrayBuffer);
      },
      { once: true },
    );
  });
}

export function closed(
  ws: WebSocket,
  timeoutMs = 5_000,
): Promise<{ code: number; reason: string }> {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error("等关闭超时")), timeoutMs);
    ws.addEventListener(
      "close",
      (event) => {
        clearTimeout(timer);
        resolve({ code: event.code, reason: event.reason });
      },
      { once: true },
    );
  });
}
