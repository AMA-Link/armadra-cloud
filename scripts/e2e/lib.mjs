// e2e 的公共件：脱敏日志、对中继的 HTTPS / WebSocket（解析 relay → 127.0.0.1，信任中继 CA）、
// docker compose 调用、等待与步骤记录。不依赖 Armadra 源码。
import { spawn } from "node:child_process";
import { request } from "node:https";

import { WebSocket } from "ws";

/* ------------------------------ 脱敏 ------------------------------ */

const secrets = new Map();

/** 登记一个秘密：之后任何输出里出现它都会被抹掉，收尾时整批日志要查不到它。 */
export function secret(value, label = "秘密") {
  if (typeof value === "string" && value.length >= 8) secrets.set(value, label);
  return value;
}

export function redact(text) {
  let out = String(text).replace(/#pair=[^\s"']+/g, "#pair=…");
  for (const value of secrets.keys()) out = out.split(value).join("…");
  return out;
}

export function secretEntries() {
  return [...secrets];
}

export function log(message) {
  console.log(redact(message));
}

/* ------------------------------ 步骤 ------------------------------ */

export class Report {
  constructor() {
    this.steps = [];
    this.startedAt = Date.now();
  }

  async step(name, run) {
    const started = Date.now();
    const entry = { name, ok: false, ms: 0 };
    this.steps.push(entry);
    try {
      const detail = await run();
      entry.ok = true;
      if (detail !== undefined) entry.detail = redact(String(detail));
      return detail;
    } catch (error) {
      entry.error = redact(error instanceof Error ? error.message : error);
      throw error;
    } finally {
      entry.ms = Date.now() - started;
      log(
        `  ${entry.ok ? "通过" : "失败"}  ${name}（${entry.ms} ms）${
          entry.detail ? `：${entry.detail}` : ""
        }${entry.error ? `：${entry.error}` : ""}`,
      );
    }
  }
}

export function expect(condition, message) {
  if (!condition) throw new Error(`断言失败：${message}`);
}

export function expectEqual(actual, wanted, what) {
  if (actual !== wanted)
    throw new Error(
      `断言失败：${what}，期望 ${JSON.stringify(wanted)}，实际 ${JSON.stringify(actual)}`,
    );
}

export const sleep = (ms) => new Promise((done) => setTimeout(done, ms));

export async function until(what, check, timeoutMs = 15_000, everyMs = 200) {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const value = await check();
    if (value) return value;
    if (Date.now() > deadline) throw new Error(`等待超时：${what}`);
    await sleep(everyMs);
  }
}

/* ------------------------------ docker ------------------------------ */

/** 跑一条命令，收齐输出。`input` 写进标准输入。失败（非零）抛错，信息已脱敏。 */
export function exec(command, args, { input, env, cwd, allowFail } = {}) {
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, {
      cwd,
      env: { ...process.env, ...env },
      stdio: ["pipe", "pipe", "pipe"],
    });
    const out = [];
    const err = [];
    child.stdout.on("data", (chunk) => out.push(chunk));
    child.stderr.on("data", (chunk) => err.push(chunk));
    child.on("error", reject);
    child.on("close", (code) => {
      const stdout = Buffer.concat(out).toString("utf8");
      const stderr = Buffer.concat(err).toString("utf8");
      if (code !== 0 && !allowFail)
        reject(
          new Error(
            redact(
              `${command} ${args.slice(0, 4).join(" ")} 退出 ${code}：${stderr.slice(-600)}`,
            ),
          ),
        );
      else resolve({ code, stdout, stderr });
    });
    child.stdin.end(input);
  });
}

/* ------------------------------ 对中继 ------------------------------ */

/**
 * 解析 `relay` 到 127.0.0.1（中继只发布在宿主机回环上），证书按中继 CA 校验——
 * 主机名 `relay` 在证书 SAN 里，所以 TLS 校验是真的，不关校验。
 */
export function relayEndpoint({ issuer, getCa }) {
  const lookup = (_host, options, callback) =>
    options?.all
      ? callback(null, [{ address: "127.0.0.1", family: 4 }])
      : callback(null, "127.0.0.1", 4);

  function http(method, url, { headers, body, insecure } = {}) {
    const target = new URL(url, issuer);
    const payload = body === undefined ? undefined : JSON.stringify(body);
    const ca = getCa();
    return new Promise((resolve, reject) => {
      const outgoing = request(
        target,
        {
          method,
          lookup,
          agent: false,
          ...(insecure === true || ca === ""
            ? { rejectUnauthorized: false }
            : { ca }),
          headers: {
            ...(payload === undefined
              ? {}
              : {
                  "content-type": "application/json",
                  "content-length": String(Buffer.byteLength(payload)),
                }),
            ...headers,
          },
        },
        (response) => {
          const chunks = [];
          response.on("data", (chunk) => chunks.push(chunk));
          response.on("end", () => {
            const text = Buffer.concat(chunks).toString("utf8");
            let parsed = {};
            try {
              parsed = text === "" ? {} : JSON.parse(text);
            } catch {
              parsed = {};
            }
            resolve({
              status: response.statusCode ?? 0,
              headers: response.headers,
              text,
              body: parsed,
            });
          });
        },
      );
      outgoing.on("error", reject);
      if (payload !== undefined) outgoing.write(payload);
      outgoing.end();
    });
  }

  /** 升级 WebSocket：等到 101 后观察一小会儿，看会不会马上被中继以 44xx 关掉。 */
  function ws(url, protocols, origin) {
    const socket = new WebSocket(url, protocols, {
      origin,
      ca: getCa(),
      lookup,
      perMessageDeflate: false,
    });
    socket.binaryType = "nodebuffer";
    const messages = [];
    socket.on("message", (data, isBinary) =>
      messages.push(isBinary ? data : Buffer.from(data)),
    );
    let closeCode;
    socket.on("close", (code) => {
      closeCode = code;
    });
    return new Promise((resolve) => {
      let opened = false;
      const finish = (status) =>
        resolve({
          socket,
          status,
          messages,
          get closeCode() {
            return closeCode;
          },
          closed: () =>
            new Promise((done) =>
              closeCode === undefined
                ? socket.once("close", done)
                : done(closeCode),
            ),
        });
      socket.once("open", () => {
        opened = true;
        const timer = setTimeout(() => finish(101), 300);
        socket.once("close", () => {
          clearTimeout(timer);
          finish(101);
        });
      });
      socket.once("unexpected-response", (_request, response) =>
        finish(response.statusCode ?? 0),
      );
      socket.once("error", () => {
        if (!opened) finish(0);
      });
    });
  }

  return { http, ws };
}

export function jsonFrames(messages) {
  return messages.map((data) => {
    try {
      return JSON.parse(data.toString("utf8"));
    } catch {
      return {};
    }
  });
}
