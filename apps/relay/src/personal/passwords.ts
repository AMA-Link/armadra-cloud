/**
 * personal 的口令策略：控制核的策略 + 常见口令表（`@armadra/control-core/common-passwords.txt`，
 * Workers 中继打进同一份；构建时拷到 out/ 旁边）+ CLI 的中文提示。派生与比对在
 * `@armadra/control-core/passwords`。
 */
import { existsSync, readFileSync } from "node:fs";
import { createRequire } from "node:module";

import {
  parseCommonPasswords,
  passwordViolation as violation,
} from "@armadra/control-core/passwords";
import type { PasswordRule } from "@armadra/control-core/ports";
import { PASSWORD_POLICY } from "@armadra/platform-protocol/identity-vectors";

export { hashPassword, verifyPassword } from "@armadra/control-core/passwords";

let common: ReadonlySet<string> | undefined;

/** 常见口令表的位置：单文件旁的 `common-passwords.txt`，否则控制核包里的。 */
export function commonPasswordsFile(): string | URL {
  const bundled = new URL("./common-passwords.txt", import.meta.url);
  if (existsSync(bundled)) return bundled;
  return createRequire(import.meta.url).resolve(
    "@armadra/control-core/common-passwords.txt",
  );
}

/** 常见口令表，第一次用到时读一次。 */
function commonPasswords(): ReadonlySet<string> {
  common ??= parseCommonPasswords(readFileSync(commonPasswordsFile(), "utf8"));
  return common;
}

/** 第一条不过的规则；全过时 `undefined`。 */
export function passwordViolation(
  password: string,
  names: readonly string[] = [],
): PasswordRule | undefined {
  return violation(password, names, commonPasswords());
}

/** CLI 的提示（简体中文短句）。线上只发码。 */
export const PASSWORD_RULE_HINTS: Record<PasswordRule, string> = {
  password_too_short: `口令太短：至少 ${PASSWORD_POLICY.minLengthDefault} 个字符`,
  password_too_long: `口令太长：不超过 ${PASSWORD_POLICY.maxBytes} 字节`,
  password_contains_name: "口令不能包含账号名",
  password_too_common: "口令太常见",
};
