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

/**
 * 带 scrypt 的请求（登录、改口令）是这组用例里最重、也最受机器负载影响的一段。账号口令用 `SCRYPT`
 * （N=2^15 r=8 p=1，参数由协议包与身份向量钉死，变了会在那边先失败）派生，一次是约 55 ms 的纯 CPU
 * 活。本地 workerd 实测一次登录的整个往返：安静时 55–65 ms；10 核机上另压 50 / 100 / 200 个空转进程
 * （环境里还有别的测试在跑）时，中位 0.65 / 0.94 / 1.5 s，最慢 0.9 / 1.1 / 2.0 s；整套并行、load 100
 * 时单次最慢 2.6 s。一次派生给 5 s，比最慢实测高一倍：机器再忙也不会误报，派生真卡住（或慢出一个
 * 数量级）则在那一段就报出是哪个请求，而不是整条用例笼统地超时。
 */
export const DERIVE_BUDGET_MS = 5_000;

/**
 * 控制面的对象第一次被请求时才起：建库、跑迁移、生成签名钥，并为种子账号做 1 次 scrypt。安静时
 * 约 80 ms，另压 100 / 200 个进程时 1.1–2.6 s。单独限 10 s，不混进后面的登录里。
 */
export const COLD_START_BUDGET_MS = 10_000;

/**
 * 不含 scrypt 的一串往返（登记源、接隧道、换中继令牌、开通道、回显几个字节等）的用例上限。整套并行、
 * load 100 时这类用例实测 0.3–4.5 s，另压 200 个空转进程（load 190–240）时最慢 10.1 s（1 MiB 回显），
 * 缺省的 5 s 不够；给 20 s，约两倍余量。
 */
export const REQUESTS_TIMEOUT_MS = 20_000;

/**
 * 整条用例的上限：`REQUESTS_TIMEOUT_MS` 加 `derives` 次 scrypt 各按 `DERIVE_BUDGET_MS`。登录算 1 次，
 * 改口令（校验旧口令 + 派生新口令）算 2 次，口令错的登录也做 1 次等价派生。
 */
export function scryptTimeout(derives: number): number {
  return REQUESTS_TIMEOUT_MS + derives * DERIVE_BUDGET_MS;
}

/**
 * 一段限时：做完后核对这一段的耗时，超了抛出带标签的错误，说明是哪一段慢、慢了多少。不用定时器去
 * 抢：scrypt 在这个 isolate 里占着线程，定时器要等它做完才轮得到，抢不过；事后核对在 CPU 忙时也量得出。
 * 这一段真卡死不返回的话，由整条用例的上限兜住。
 */
export async function within<T>(
  label: string,
  budgetMs: number,
  work: Promise<T>,
): Promise<T> {
  const started = performance.now();
  const result = await work;
  const elapsed = Math.round(performance.now() - started);
  if (elapsed > budgetMs)
    throw new Error(`${label}用了 ${elapsed} ms，超过 ${budgetMs} ms`);
  return result;
}

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
  options: {
    token?: string;
    ip?: string;
    authorization?: string;
    /** 这个请求要做几次 scrypt 派生；给了就按 `DERIVE_BUDGET_MS` 单独限时。 */
    derives?: number;
  } = {},
): Promise<Response> {
  const headers: Record<string, string> = {
    "content-type": "application/json",
  };
  if (options.token) headers.authorization = `Bearer ${options.token}`;
  if (options.authorization) headers.authorization = options.authorization;
  const sent = call(path, {
    method: "POST",
    body: JSON.stringify(body),
    headers,
    ip: options.ip,
  });
  if (!options.derives) return sent;
  return within(
    `POST ${path}（${options.derives} 次 scrypt）`,
    options.derives * DERIVE_BUDGET_MS,
    sent,
  );
}

export interface Session {
  accessToken: string;
  refreshToken?: string;
}

let warmed: Promise<void> | undefined;

/** 唤醒控制面的对象并等它起好，冷启动单独限时；每个测试文件只做一次。 */
export function warmUp(): Promise<void> {
  warmed ??= within(
    "控制面冷启动",
    COLD_START_BUDGET_MS,
    call("/.well-known/armadra-platform").then((response) =>
      response.arrayBuffer().then(() => undefined),
    ),
  );
  return warmed;
}

export async function login(ip = nextIp()): Promise<Session> {
  await warmUp();
  const response = await postJson(
    "/v1/auth/login",
    { account: ACCOUNT, password: PASSWORD, device: DEVICE },
    { ip, derives: 1 },
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

/**
 * 大块字节的比对摘要：长度与第一个不同的下标（相同答 -1）。断言写成
 * `expect(byteDiff(got, sent)).toEqual({ length: sent.length, firstMismatch: -1 })`。
 * 几百 KiB 的 `Uint8Array` 直接 `toEqual` 会逐元素走通用的深比较，安静时就要近 1 秒、机器忙时
 * 五六秒，比被测的转发本身慢两个数量级；这里一遍循环，失败时也只报一个下标。
 */
export function byteDiff(
  actual: Uint8Array,
  expected: Uint8Array,
): { length: number; firstMismatch: number } {
  const shared = Math.min(actual.length, expected.length);
  let firstMismatch = -1;
  for (let index = 0; index < shared; index += 1)
    if (actual[index] !== expected[index]) {
      firstMismatch = index;
      break;
    }
  if (firstMismatch === -1 && actual.length !== expected.length)
    firstMismatch = shared;
  return { length: actual.length, firstMismatch };
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
