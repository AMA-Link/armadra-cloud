import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import { z } from "zod";

import { EMAIL_PATTERN } from "../src/validation.ts";

const vectors = JSON.parse(
  readFileSync(new URL("./format-vectors.json", import.meta.url), "utf8"),
) as {
  email: { valid: string[]; invalid: string[] };
  uri: { valid: string[]; invalid: string[] };
};
const constants = JSON.parse(
  readFileSync(new URL("../schema/constants.json", import.meta.url), "utf8"),
) as { validation: { emailPattern: string } };

// Go 版的入参解释器读同一份向量（go/internal/protocol/schema），两边行为不一致即失败。
describe("入参格式向量", () => {
  it("导出的邮箱正则就是 z.email() 用的那个", () => {
    expect(EMAIL_PATTERN).toBe(z.regexes.email.source);
    expect(constants.validation.emailPattern).toBe(EMAIL_PATTERN);
  });

  it("邮箱向量与 z.email() 一致", () => {
    for (const value of vectors.email.valid)
      expect(z.email().safeParse(value).success, value).toBe(true);
    for (const value of vectors.email.invalid)
      expect(z.email().safeParse(value).success, value).toBe(false);
  });

  it("uri 向量与 z.url() 一致", () => {
    for (const value of vectors.uri.valid)
      expect(z.url().safeParse(value).success, value).toBe(true);
    for (const value of vectors.uri.invalid)
      expect(z.url().safeParse(value).success, value).toBe(false);
  });
});
