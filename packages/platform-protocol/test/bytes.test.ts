import { randomBytes } from "node:crypto";
import { expect, it } from "vitest";

import {
  bytesEqual,
  fromBase64Url,
  fromUtf8,
  toBase64Url,
  utf8,
} from "../src/internal/bytes.ts";

it("base64url 与 Node 的实现逐字相同，往返不变", () => {
  for (let length = 0; length < 70; length += 1) {
    const bytes = new Uint8Array(randomBytes(length));
    const encoded = toBase64Url(bytes);
    expect(encoded).toBe(Buffer.from(bytes).toString("base64url"));
    expect(bytesEqual(fromBase64Url(encoded), bytes)).toBe(true);
  }
});

it("严格解码：非法字符、长度、多余的位都拒绝", () => {
  expect(() => fromBase64Url("ab+c")).toThrow();
  expect(() => fromBase64Url("abcde")).toThrow();
  expect(() => fromBase64Url("AB")).toThrow();
  expect(fromBase64Url("AA")).toEqual(new Uint8Array([0]));
});

it("UTF-8 严格解码", () => {
  expect(fromUtf8(utf8("口令 🔐"))).toBe("口令 🔐");
  expect(() => fromUtf8(new Uint8Array([0xff]))).toThrow();
});
