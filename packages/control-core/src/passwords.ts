/**
 * 口令：策略与派生（平台设计 §17.3）。参数全部来自协议包 identity-vectors，与 core 一致：
 *
 * - 策略按顺序报第一条不过的：码点数 ≥ 12 → UTF-8 ≤ 1024 字节 → 不含账号名（三个字符以下
 *   的名字不算）→ 不在常见口令表里（小写精确比对）。码与 core 同名。常见口令表由装配方给
 *   （personal：随包的 `common-passwords.txt`）。
 * - 派生：scrypt N=2^15 r=8 p=1 len=32、16 字节随机盐；参数存进记录，校验按记录里的参数。
 * - 比较定时安全。
 */
import { randomBytes, scrypt, timingSafeEqual } from "node:crypto";

import {
  PASSWORD_POLICY,
  SCRYPT,
} from "@armadra/platform-protocol/identity-vectors";

import type { PasswordRecord, PasswordRule } from "./ports.ts";

/** 常见口令表的文本（`#` 注释、空行忽略）→ 小写集合。 */
export function parseCommonPasswords(text: string): ReadonlySet<string> {
  const entries = new Set<string>();
  for (const line of text.split(/\r?\n/)) {
    const value = line.trim();
    if (value === "" || value.startsWith("#")) continue;
    entries.add(value.toLowerCase());
  }
  return entries;
}

/** 第一条不过的规则；全过时 `undefined`。 */
export function passwordViolation(
  password: string,
  names: readonly string[],
  common: ReadonlySet<string>,
): PasswordRule | undefined {
  if ([...password].length < PASSWORD_POLICY.minLengthDefault)
    return "password_too_short";
  if (Buffer.byteLength(password, "utf8") > PASSWORD_POLICY.maxBytes)
    return "password_too_long";
  const lowered = password.toLowerCase();
  for (const name of names) {
    const value = name.trim().toLowerCase();
    if ([...value].length < PASSWORD_POLICY.minNameLength) continue;
    if (lowered.includes(value)) return "password_contains_name";
  }
  if (common.has(lowered)) return "password_too_common";
  return undefined;
}

function derive(
  password: string,
  salt: Buffer,
  record: Pick<PasswordRecord, "cost" | "block" | "parallel" | "length">,
): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    scrypt(
      password,
      salt,
      record.length,
      {
        N: record.cost,
        r: record.block,
        p: record.parallel,
        maxmem: 64 * 1024 * 1024,
      },
      (error, key) => (error ? reject(error) : resolve(key)),
    );
  });
}

export async function hashPassword(
  password: string,
  salt: Buffer = randomBytes(SCRYPT.saltBytes),
): Promise<PasswordRecord> {
  const params = {
    cost: SCRYPT.cost,
    block: SCRYPT.block,
    parallel: SCRYPT.parallel,
    length: SCRYPT.length,
  };
  const hash = await derive(password, salt, params);
  return {
    kdf: "scrypt",
    ...params,
    salt: salt.toString("base64"),
    hash: hash.toString("base64"),
  };
}

export async function verifyPassword(
  password: string,
  record: PasswordRecord,
): Promise<boolean> {
  if (Buffer.byteLength(password, "utf8") > PASSWORD_POLICY.maxBytes)
    return false;
  const expected = Buffer.from(record.hash, "base64");
  const actual = await derive(
    password,
    Buffer.from(record.salt, "base64"),
    record,
  );
  return expected.length === actual.length && timingSafeEqual(expected, actual);
}

/** 账号名不对时也做一次同样成本的派生，别让响应时间泄露账号名。 */
export async function burnPasswordCheck(password: string): Promise<void> {
  await derive(password.slice(0, 1024), Buffer.alloc(SCRYPT.saltBytes), {
    cost: SCRYPT.cost,
    block: SCRYPT.block,
    parallel: SCRYPT.parallel,
    length: SCRYPT.length,
  });
}
