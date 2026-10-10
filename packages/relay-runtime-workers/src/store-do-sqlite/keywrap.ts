/**
 * 签名私钥的包裹（`private_key_wrap = 'aes-256-gcm'`）：Worker secret `RELAY_MASTER_KEY`（32 字节，
 * 64 位十六进制）做 AES-256-GCM。DO 存储本身静态加密，多这一层让导出、迁移或误配时私钥不以
 * 明文出现。密文 = base64(12 字节 IV ‖ 密文 ‖ 16 字节标签)，附加数据是固定的用途串。
 * 丢了主密钥 = 签名钥作废（换钥、所有客户端重新登录，见运维手册）。
 */
import { createCipheriv, createDecipheriv, randomBytes } from "node:crypto";

import type { KeyWrap } from "@armadra/control-store-sqlite/db";

export const KEY_WRAP_AES_GCM = "aes-256-gcm";
const AAD = Buffer.from("armadra-relay signing key v1", "utf8");

export class MasterKeyError extends Error {
  constructor() {
    super("RELAY_MASTER_KEY 要是 64 位十六进制（32 字节）");
    this.name = "MasterKeyError";
  }
}

export function masterKeyWrap(hex: string | undefined): KeyWrap {
  const trimmed = (hex ?? "").trim();
  if (!/^[0-9a-fA-F]{64}$/.test(trimmed)) throw new MasterKeyError();
  const key = Buffer.from(trimmed, "hex");
  return {
    name: KEY_WRAP_AES_GCM,
    seal(plain) {
      const iv = randomBytes(12);
      const cipher = createCipheriv("aes-256-gcm", key, iv);
      cipher.setAAD(AAD);
      const body = Buffer.concat([
        cipher.update(Buffer.from(plain, "utf8")),
        cipher.final(),
      ]);
      return Buffer.concat([iv, body, cipher.getAuthTag()]).toString("base64");
    },
    open(sealed) {
      const bytes = Buffer.from(sealed, "base64");
      if (bytes.length < 12 + 16 + 1) throw new Error("ciphertext too short");
      const iv = bytes.subarray(0, 12);
      const tag = bytes.subarray(bytes.length - 16);
      const decipher = createDecipheriv("aes-256-gcm", key, iv);
      decipher.setAAD(AAD);
      decipher.setAuthTag(tag);
      return Buffer.concat([
        decipher.update(bytes.subarray(12, bytes.length - 16)),
        decipher.final(),
      ]).toString("utf8");
    },
  };
}
