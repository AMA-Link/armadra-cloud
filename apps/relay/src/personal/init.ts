/**
 * `personal init`（中继设计 §6.2）：建账号（口令过 identity-vectors 策略、scrypt 哈希）、生成
 * 第一把签名钥、写存储（新装缺省 `relay.db`，`--store file` 写 `state.json`，`--store postgres` 写库）；自签模式顺带生成
 * CA 与叶证书并答指纹。已初始化就拒绝，不覆盖。
 *
 * 口令来源：`--password-file`（`-` 是标准输入）或 `RELAY_PERSONAL_PASSWORD_FILE`；都没有时在终端
 * 上不回显地读两遍。口令只在内存里经过，不打印、不落盘。
 */
import { readFileSync } from "node:fs";
import { createInterface } from "node:readline";

import { checkPassword } from "@armadra/control-core/accounts";
import { ControlError } from "@armadra/control-core/errors";
import type { PasswordRule } from "@armadra/control-core/ports";
import { generateSigningKey } from "@armadra/control-core/signer";

import {
  hashPassword,
  PASSWORD_RULE_HINTS,
  passwordViolation,
} from "./passwords.ts";
import { STATE_VERSION, type State } from "./state.ts";
import { createPersonalStore } from "./store.ts";
import type {
  DatabaseOptions,
  PersonalStore,
  StoreKind,
} from "./store-types.ts";
import { UsageError } from "./args.ts";

/** 口令文件末尾的一个换行不算口令（`echo` 与编辑器都会加）。 */
export function readPasswordFile(file: string): string {
  const text =
    file === "-" ? readFileSync(0, "utf8") : readFileSync(file, "utf8");
  return text.replace(/\r?\n$/, "");
}

async function prompt(question: string): Promise<string> {
  const input = process.stdin;
  const output = process.stderr;
  const rl = createInterface({ input, output, terminal: true });
  // 不回显：吞掉 readline 的逐字回写。
  const write = (rl as unknown as { _writeToOutput?: (text: string) => void })
    ._writeToOutput;
  (rl as unknown as { _writeToOutput: (text: string) => void })._writeToOutput =
    (text: string) => {
      if (text.includes(question)) write?.call(rl, text);
    };
  try {
    return await new Promise<string>((resolve) =>
      rl.question(question, resolve),
    );
  } finally {
    rl.close();
    output.write("\n");
  }
}

/** 取口令：文件 / 标准输入 / 终端交互（`confirm` 时读两遍）。 */
export async function obtainPassword(
  passwordFile: string | undefined,
  confirm: boolean,
): Promise<string> {
  if (passwordFile) return readPasswordFile(passwordFile);
  if (!process.stdin.isTTY)
    throw new UsageError("缺少口令：给 --password-file，或在终端上运行");
  const first = await prompt("口令：");
  if (confirm) {
    const second = await prompt("再输一次：");
    if (first !== second) throw new UsageError("两次输入的口令不一致");
  }
  return first;
}

/** 口令策略不过 → 带中文提示的 UsageError（CLI 用）。 */
export function passwordUsageError(error: unknown): unknown {
  if (error instanceof ControlError && error.code in PASSWORD_RULE_HINTS)
    return new UsageError(PASSWORD_RULE_HINTS[error.code as PasswordRule]);
  return error;
}

export async function createState(input: {
  dataDir: string;
  account: string;
  password: string;
  issuer: string;
  /** 缺省 sqlite（新装）。 */
  store?: StoreKind;
  /** `store` 是 postgres 时的连接。 */
  database?: DatabaseOptions;
  nowMs?: number;
}): Promise<PersonalStore> {
  try {
    checkPassword(
      { passwordPolicy: passwordViolation },
      input.password,
      input.account,
    );
  } catch (error) {
    throw passwordUsageError(error);
  }
  const nowMs = input.nowMs ?? Date.now();
  const state: State = {
    version: STATE_VERSION,
    revision: 0,
    issuer: input.issuer,
    account: {
      name: input.account,
      password: await hashPassword(input.password),
      createdAtMs: nowMs,
      updatedAtMs: nowMs,
      failures: 0,
      lockedUntilMs: 0,
      lastFailureAtMs: 0,
    },
    signingKeys: [generateSigningKey("k1", nowMs)],
    sources: [],
    registrationTokens: [],
    devices: [],
    links: [],
    settings: { allowGuests: true, trustedOrigins: [] },
  };
  return createPersonalStore(
    input.dataDir,
    input.store ?? "sqlite",
    state,
    input.database,
  );
}
