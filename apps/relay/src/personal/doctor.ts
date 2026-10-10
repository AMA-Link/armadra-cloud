/**
 * `personal doctor`（Node 中继存储设计 §5.4「缺省即安全」）：只看不改，逐项报「通过 / 警告 / 失败」。
 *
 * 检查：数据目录与存储文件、私钥文件的权限；存储能否打开、SQLite 完整性；监听地址与 TLS；
 * 受信代理配置；口令哈希参数、锁定状态与残留的口令文件；自签 CA 的有效期。
 * 有任一项失败退出码为 1。输出只有路径、模式位、个数与时间，不打印口令、令牌或私钥。
 */
import { X509Certificate } from "node:crypto";
import { existsSync, readdirSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";

import { openDatabase } from "@armadra/control-store-sqlite/open";
import {
  createClientIp,
  parseProxyList,
} from "@armadra/relay-kernel/http/client-ip";
import { SCRYPT } from "@armadra/platform-protocol/identity-vectors";

import { ACME_DIR } from "./acme.ts";
import { isLoopback, type PersonalDoctorOptions } from "./args.ts";
import type { Print } from "./commands.ts";
import { readPasswordFile } from "./init.ts";
import { passwordViolation, verifyPassword } from "./passwords.ts";
import { openPersonalStore, resolveStoreKind } from "./store.ts";
import type { PersonalStore, StoreSettings } from "./store-types.ts";
import { CA_CERT, TLS_DIR } from "./tls.ts";

type Level = "pass" | "warn" | "fail";

export interface Finding {
  level: Level;
  item: string;
  detail: string;
}

const LABELS: Record<Level, string> = {
  pass: "通过",
  warn: "警告",
  fail: "失败",
};

const DAY_MS = 24 * 60 * 60 * 1000;
const octal = (mode: number) => (mode & 0o777).toString(8).padStart(4, "0");

/** 组与其他用户有任何权限位就算松。 */
function permission(
  path: string,
  item: string,
  expected: number,
): Finding | null {
  if (!existsSync(path) || process.platform === "win32") return null;
  const mode = statSync(path).mode & 0o777;
  return {
    level: (mode & 0o077) === 0 ? "pass" : "warn",
    item,
    detail:
      (mode & 0o077) === 0
        ? `${path}（${octal(mode)}）`
        : `${path}（${octal(mode)}，应为 ${octal(expected)}）`,
  };
}

function keyFiles(dataDir: string): string[] {
  return [TLS_DIR, ACME_DIR].flatMap((dir) => {
    const base = join(dataDir, dir);
    if (!existsSync(base)) return [];
    return readdirSync(base)
      .filter((name) => /key/.test(name))
      .map((name) => join(base, name));
  });
}

function bind(options: PersonalDoctorOptions): Finding {
  const address = `${options.listen.host}:${options.listen.port}`;
  const loopback = isLoopback(options.listen.host);
  if (options.tls === "plain")
    return loopback
      ? {
          level: "pass",
          item: "监听地址",
          detail: `明文只绑回环（在反向代理之后）：${address}`,
        }
      : {
          level: "fail",
          item: "监听地址",
          detail: `明文监听了非回环地址，serve 会拒绝启动：${address}`,
        };
  return {
    level: "pass",
    item: "监听地址",
    detail: `${loopback ? "只绑回环" : "对外监听"}，TLS ${options.tls}：${address}`,
  };
}

/** 前缀太短（IPv4 < /8、IPv6 < /16）等于信任任何人伪造客户端 IP。 */
function tooBroad(entry: string): boolean {
  const [address = "", prefix] = entry.split("/");
  if (prefix === undefined) return false;
  return Number(prefix) < (address.includes(":") ? 16 : 8);
}

function trustedProxy(
  options: PersonalDoctorOptions,
  stored: StoreSettings | undefined,
): Finding {
  const proxies =
    options.trustedProxy !== undefined
      ? parseProxyList(options.trustedProxy)
      : (stored?.trustedProxy?.proxies ?? []);
  if (proxies.length === 0)
    return options.tls === "plain"
      ? {
          level: "warn",
          item: "受信代理",
          detail:
            "明文模式在反向代理之后却没配受信代理：按 IP 的限流与锁定会合成一个桶",
        }
      : { level: "pass", item: "受信代理", detail: "未配置（直连）" };
  try {
    createClientIp({ proxies });
  } catch (error) {
    return {
      level: "fail",
      item: "受信代理",
      detail: error instanceof Error ? error.message : String(error),
    };
  }
  const broad = proxies.filter(tooBroad);
  if (broad.length > 0)
    return {
      level: "warn",
      item: "受信代理",
      detail: `网段太宽，任何人都能伪造客户端 IP：${broad.join(", ")}`,
    };
  return { level: "pass", item: "受信代理", detail: `${proxies.length} 条` };
}

async function password(
  options: PersonalDoctorOptions,
  store: PersonalStore,
  nowMs: number,
): Promise<Finding[]> {
  const { account } = store.snapshot();
  const findings: Finding[] = [];
  const weak =
    account.password.cost < SCRYPT.cost ||
    account.password.block < SCRYPT.block ||
    account.password.length < SCRYPT.length;
  findings.push(
    weak
      ? {
          level: "warn",
          item: "口令哈希",
          detail: "参数低于当前缺省：用 personal passwd 重设一次",
        }
      : {
          level: "pass",
          item: "口令哈希",
          detail: `scrypt N=${account.password.cost}`,
        },
  );
  if (account.lockedUntilMs > nowMs)
    findings.push({
      level: "warn",
      item: "账号锁定",
      detail: `锁定到：${new Date(account.lockedUntilMs).toISOString()}`,
    });
  const file = options.passwordFile;
  if (file && existsSync(file)) {
    const mode = permission(file, "口令文件权限", 0o600);
    if (mode) findings.push(mode);
    const text = readPasswordFile(file);
    const rule = passwordViolation(text, [account.name]);
    if (rule)
      findings.push({
        level: "warn",
        item: "口令强度",
        detail: `口令文件里的口令不过策略：${rule}`,
      });
    if (await verifyPassword(text, account.password))
      findings.push({
        level: rule ? "warn" : "pass",
        item: "口令文件",
        detail: `与当前口令一致，初始化之后不再读取，可以删掉：${file}`,
      });
  }
  return findings;
}

function certificate(dataDir: string, nowMs: number): Finding | null {
  const file = join(dataDir, TLS_DIR, CA_CERT);
  if (!existsSync(file)) return null;
  try {
    const notAfter = Date.parse(
      new X509Certificate(readFileSync(file)).validTo,
    );
    const days = Math.floor((notAfter - nowMs) / DAY_MS);
    return {
      level: days < 0 ? "fail" : days < 365 ? "warn" : "pass",
      item: "自签 CA",
      detail: days < 0 ? "已过期" : `剩余 ${days} 天`,
    };
  } catch {
    return { level: "fail", item: "自签 CA", detail: `读不出证书：${file}` };
  }
}

export async function diagnose(
  options: PersonalDoctorOptions,
  nowMs = Date.now(),
): Promise<Finding[]> {
  const { dataDir } = options;
  const findings: Finding[] = [];
  const add = (finding: Finding | null) => {
    if (finding) findings.push(finding);
  };
  if (!existsSync(dataDir))
    return [{ level: "fail", item: "数据目录", detail: `不存在：${dataDir}` }];
  add(permission(dataDir, "数据目录权限", 0o700));

  let store: PersonalStore | undefined;
  try {
    resolveStoreKind(dataDir);
    store = openPersonalStore(dataDir);
  } catch (error) {
    add({
      level: "fail",
      item: "存储",
      detail: error instanceof Error ? error.message : String(error),
    });
  }
  if (store) {
    try {
      add({
        level: "pass",
        item: "存储",
        detail: `${store.kind === "sqlite" ? "SQLite" : "状态文件"}，${store.path}`,
      });
      for (const suffix of store.kind === "sqlite"
        ? ["", "-wal", "-shm"]
        : [""])
        add(permission(`${store.path}${suffix}`, "存储文件权限", 0o600));
      if (store.kind === "sqlite") {
        const db = openDatabase(store.path);
        try {
          const result = db.integrityCheck();
          add(
            result.length === 1 && result[0] === "ok"
              ? { level: "pass", item: "数据库完整性", detail: "ok" }
              : {
                  level: "fail",
                  item: "数据库完整性",
                  detail: `${result.length} 处问题`,
                },
          );
        } finally {
          db.close();
        }
      }
      for (const finding of await password(options, store, nowMs)) add(finding);
      add(trustedProxy(options, store.settings()));
    } finally {
      store.close();
    }
  } else {
    add(trustedProxy(options, undefined));
  }
  for (const file of keyFiles(dataDir))
    add(permission(file, "私钥文件权限", 0o600));
  add(bind(options));
  add(certificate(dataDir, nowMs));
  return findings;
}

/** 打印并答退出码（有失败为 1）。 */
export async function runDoctor(
  options: PersonalDoctorOptions,
  print: Print,
  nowMs = Date.now(),
): Promise<number> {
  const findings = await diagnose(options, nowMs);
  for (const finding of findings)
    print(`${LABELS[finding.level]}  ${finding.item}：${finding.detail}`);
  const count = (level: Level) =>
    findings.filter((finding) => finding.level === level).length;
  print(
    `检查完成：通过 ${count("pass")}，警告 ${count("warn")}，失败 ${count("fail")}`,
  );
  return count("fail") > 0 ? 1 : 0;
}
