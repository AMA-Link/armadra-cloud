import { z } from "zod";

/**
 * 契约里 `z.email()` 用的邮箱正则的源码（zod 4 默认的 `regexes.email`）。
 * 写进 `schema/constants.json` 的 `validation.emailPattern`，Go 版的入参解释器按它判 `format: email`，
 * 两边不各写一份；`test/validation.test.ts` 守住它与 `z.email()` 的行为一致。
 */
export const EMAIL_PATTERN = z.regexes.email.source;
