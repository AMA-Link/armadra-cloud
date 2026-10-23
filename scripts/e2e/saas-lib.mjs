// `pnpm e2e --mode saas` 的零件（saas.mjs 是场景编排）：一次性容器与子进程、控制面客户端（原生 Bearer / 控制台
// Cookie + CSRF）、mailpit 取信、假 core 主机的密钥与 Source 认证头、TOTP。
import { spawn } from "node:child_process";
import {
  createHmac,
  generateKeyPairSync,
  randomBytes,
  sign,
} from "node:crypto";

import { exec, expect, expectEqual, secret, until } from "./lib.mjs";

/* ------------------------------ 容器与进程 ------------------------------ */

export async function startContainer(name, image, ports, env, args = []) {
  const flags = [];
  for (const [host, inner] of ports)
    flags.push("-p", `127.0.0.1:${host}:${inner}`);
  for (const [key, value] of Object.entries(env))
    flags.push("-e", `${key}=${value}`);
  // 不加 --rm：场景 13 要 docker restart Redis；收尾时 rm -f 只删自己起的这几个。
  await exec("docker", ["run", "-d", "--name", name, ...flags, image, ...args]);
  return {
    name,
    stop: () => exec("docker", ["rm", "-f", "-v", name], { allowFail: true }),
  };
}

/** 起一个子进程（控制面或节点），收日志。 */
export function startProcess(name, bin, args, env) {
  const logs = [];
  const child = spawn(bin, args, {
    env: { PATH: process.env.PATH, ...env },
    stdio: ["ignore", "pipe", "pipe"],
  });
  child.stdout.on("data", (chunk) => logs.push(chunk.toString("utf8")));
  child.stderr.on("data", (chunk) => logs.push(chunk.toString("utf8")));
  const exited = new Promise((done) =>
    child.once("exit", (code, signal) => done({ code, signal })),
  );
  return {
    name,
    logs,
    exited,
    signal: (sig) => child.kill(sig),
    get running() {
      return child.exitCode === null && child.signalCode === null;
    },
    stop: async () => {
      if (child.exitCode === null && child.signalCode === null) {
        child.kill("SIGTERM");
        const timer = setTimeout(() => child.kill("SIGKILL"), 20_000);
        await exited;
        clearTimeout(timer);
      }
    },
  };
}

/** 跑一次命令行子命令（migrate、nodes token、operators …），答 stdout / stderr。 */
export async function runCli(bin, args, env) {
  return exec(bin, args, { env: { PATH: process.env.PATH, ...env } });
}

/* ------------------------------ 控制面客户端 ------------------------------ */

const PASSWORD_BASE = "plover-quartz-lantern-41";

/**
 * 一个客户端：原生（bearer，Authorization）或控制台（cookie，Cookie + X-Armadra-CSRF + Origin）。
 * 每个客户端有自己的 X-Forwarded-For（控制面信任回环代理），限流桶互不干扰。
 */
export class Client {
  constructor(base, label, transport) {
    this.base = base;
    this.label = label;
    this.transport = transport;
    this.ip = `10.${randomBytes(1)[0]}.${randomBytes(1)[0]}.${1 + (randomBytes(1)[0] % 250)}`;
    this.jar = new Map();
    this.cookieName = "__Host-armadra-cloud";
    this.email = `${label.toLowerCase()}-${randomBytes(4).toString("hex")}@example.com`;
    this.password = secret(
      `${PASSWORD_BASE}-${randomBytes(6).toString("hex")}`,
      `${label} 口令`,
    );
  }

  headers(extra = {}) {
    const headers = { "x-forwarded-for": this.ip, ...extra };
    if (this.transport === "bearer") {
      if (this.access) headers.authorization = `Bearer ${this.access}`;
    } else {
      headers.origin = this.base;
      const cookie = this.jar.get(this.cookieName);
      if (cookie) headers.cookie = `${this.cookieName}=${cookie}`;
      if (this.csrf) headers["x-armadra-csrf"] = this.csrf;
    }
    return headers;
  }

  async call(method, path, { body, query, headers, anonymous } = {}) {
    const url = new URL(path, this.base);
    for (const [key, value] of Object.entries(query ?? {}))
      url.searchParams.set(key, value);
    // 匿名请求不带凭据；控制台形态仍带 Origin（登录类答复据此决定要不要种 Cookie）。
    const sent = anonymous
      ? {
          "x-forwarded-for": this.ip,
          ...(this.transport === "cookie" ? { origin: this.base } : {}),
          ...headers,
        }
      : this.headers(headers);
    if (body !== undefined) sent["content-type"] = "application/json";
    const response = await fetch(url, {
      method,
      headers: sent,
      body: body === undefined ? undefined : JSON.stringify(body),
      redirect: "manual",
    });
    const text = await response.text();
    let json = {};
    try {
      json = text === "" ? {} : JSON.parse(text);
    } catch {
      json = {};
    }
    for (const line of response.headers.getSetCookie?.() ?? []) {
      const [pair, ...attrs] = line.split(";");
      const [name, value] = pair.split("=");
      const gone = attrs.some((a) => /^\s*max-age=0/i.test(a)) || value === "";
      if (gone) this.jar.delete(name.trim());
      else this.jar.set(name.trim(), secret(value, `${this.label} Cookie`));
    }
    return {
      status: response.status,
      body: json,
      headers: response.headers,
      text,
    };
  }

  /** 请求并断言状态码（与可选的错误码）。 */
  async expectCall(what, status, code, method, path, options) {
    const res = await this.call(method, path, options);
    expectEqual(
      res.status,
      status,
      `${what} 的状态码（${res.text.slice(0, 200)}）`,
    );
    if (code) expectEqual(res.body.code, code, `${what} 的错误码`);
    return res;
  }

  /** 接住一个会话答复（register / login / device poll / mfaVerify）。 */
  adopt(res) {
    expectEqual(
      res.status,
      200,
      `${this.label} 登录类答复（${res.text.slice(0, 200)}）`,
    );
    const session = res.body.session;
    this.access = secret(session.accessToken, `${this.label} 访问令牌`);
    if (session.refreshToken)
      this.refresh = secret(session.refreshToken, `${this.label} 刷新令牌`);
    this.accountId = session.account.accountId;
    this.deviceId = session.device.deviceId;
    if (res.body.csrfToken)
      this.csrf = secret(res.body.csrfToken, `${this.label} CSRF`);
    // 控制台形态的会话在 Cookie 里，访问令牌不用于请求。
    if (this.transport === "cookie") this.access = undefined;
    return session;
  }

  async register() {
    const res = await this.call("POST", "/v1/auth/register", {
      anonymous: true,
      body: {
        email: this.email,
        password: this.password,
        displayName: `用户 ${this.label}`,
        device: {
          platform: this.transport === "bearer" ? "desktop" : "browser",
          name: `${this.label} 设备`,
        },
      },
    });
    this.adopt(res);
    // 注册的答复不带 CSRF 值：控制台形态由 me.get 取回（契约 §1）。
    if (this.transport === "cookie" && !this.csrf) {
      const me = await this.call("GET", "/v1/me");
      expectEqual(
        me.status,
        200,
        `${this.label} me.get（${me.text.slice(0, 160)}）`,
      );
      this.csrf = secret(me.body.csrfToken, `${this.label} CSRF`);
    }
    return res;
  }
}

export const decodePart = (token, index = 1) =>
  JSON.parse(
    Buffer.from(token.split(".")[index], "base64url").toString("utf8"),
  );

/* ------------------------------ mailpit ------------------------------ */

export function mailpit(api) {
  const get = async (path) => {
    const response = await fetch(`${api}${path}`);
    expect(response.ok, `mailpit ${path} 答 ${response.status}`);
    return response.json();
  };
  return {
    /** 等发给 address 的信里出现匹配 pattern 的链接，答 { match, id, count }。 */
    async link(address, pattern, { count = 1, timeoutMs = 20_000 } = {}) {
      return until(
        `发给 ${address} 的信里有 ${pattern}`,
        async () => {
          const list = await get(
            `/api/v1/search?query=${encodeURIComponent(`to:${address}`)}`,
          );
          const found = [];
          for (const item of list.messages ?? []) {
            const full = await get(`/api/v1/message/${item.ID}`);
            const match = pattern.exec(full.Text ?? "");
            if (match) found.push({ match, id: item.ID, created: full.Date });
          }
          if (found.length < count) return false;
          found.sort((a, b) => new Date(a.created) - new Date(b.created));
          const last = found.at(-1);
          return { match: last.match, id: last.id, count: found.length };
        },
        timeoutMs,
        150,
      );
    },
  };
}

/* ------------------------------ 主机（假 core） ------------------------------ */

export function newHost(label) {
  const { privateKey, publicKey } = generateKeyPairSync("ed25519");
  return {
    label,
    id: randomBytes(16).toString("hex"),
    privateKey,
    publicKey,
    x: publicKey.export({ format: "jwk" }).x,
  };
}

export const b64u = (data) => Buffer.from(data).toString("base64url");

/** 主机对控制面的认证头：`Authorization: Source <jws>`（EdDSA，iss = sourceId，aud = 控制面来源）。 */
export function sourceAuth(host, audience) {
  const now = Math.floor(Date.now() / 1000);
  const input = `${b64u(JSON.stringify({ alg: "EdDSA", kid: host.id, typ: "armadra-source" }))}.${b64u(
    JSON.stringify({
      iss: host.id,
      aud: audience,
      iat: now,
      exp: now + 60,
      jti: randomBytes(8).toString("hex"),
    }),
  )}`;
  const signature = sign(null, Buffer.from(input), host.privateKey);
  return { authorization: `Source ${input}.${b64u(signature)}` };
}

// RFC 6238：HMAC-SHA1、30 秒、6 位。
export function base32Decode(text) {
  const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567";
  let bits = 0;
  let value = 0;
  const out = [];
  for (const ch of text.replace(/=+$/, "").toUpperCase()) {
    value = (value << 5) | alphabet.indexOf(ch);
    bits += 5;
    if (bits >= 8) {
      out.push((value >>> (bits - 8)) & 0xff);
      bits -= 8;
    }
  }
  return Buffer.from(out);
}

export function totp(secretBase32, atMs = Date.now()) {
  const counter = Buffer.alloc(8);
  counter.writeBigUInt64BE(BigInt(Math.floor(atMs / 1000 / 30)));
  const digest = createHmac("sha1", base32Decode(secretBase32))
    .update(counter)
    .digest();
  const offset = digest[digest.length - 1] & 0xf;
  const code = (digest.readUInt32BE(offset) & 0x7fffffff) % 1_000_000;
  return String(code).padStart(6, "0");
}
