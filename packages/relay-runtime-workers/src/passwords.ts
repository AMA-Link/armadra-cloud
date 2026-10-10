/**
 * Workers 中继的口令策略：控制核的策略 + 与 Node 中继同一份常见口令表（打进 Worker）。
 */
import commonPasswordsText from "@armadra/control-core/common-passwords.txt";
import {
  parseCommonPasswords,
  passwordViolation as violation,
} from "@armadra/control-core/passwords";
import type { PasswordRule } from "@armadra/control-core/ports";

let common: ReadonlySet<string> | undefined;

export function passwordViolation(
  password: string,
  names: readonly string[] = [],
): PasswordRule | undefined {
  common ??= parseCommonPasswords(commonPasswordsText);
  return violation(password, names, common);
}
