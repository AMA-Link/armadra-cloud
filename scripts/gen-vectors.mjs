// 生成协议包的身份测试向量 packages/platform-protocol/src/identity-vectors/vectors.json。
//
//   node scripts/gen-vectors.mjs           重写
//   node scripts/gen-vectors.mjs --check   与仓库里的那份不同就失败
//
// 用 node:crypto 按 identity-vectors 的常量算：scrypt（固定盐）与令牌哈希。
// 两仓的测试对着这份文件跑，所以它是提交进仓库的，输入固定、输出确定。
import { createHash, scryptSync } from "node:crypto";
import { readFileSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

import {
  SCRYPT,
  TOKEN_HASH_KINDS,
  TOKEN_HASH_PREFIX,
} from "../packages/platform-protocol/src/identity-vectors/constants.ts";

const target = fileURLToPath(
  new URL(
    "../packages/platform-protocol/src/identity-vectors/vectors.json",
    import.meta.url,
  ),
);

const SCRYPT_INPUTS = [
  {
    password: "correct horse battery staple",
    salt: "000102030405060708090a0b0c0d0e0f",
  },
  {
    password: "armadra-dev-password",
    salt: "f0e1d2c3b4a5968778695a4b3c2d1e0f",
  },
  {
    password: "口令里有中文与 emoji 🔐",
    salt: "5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a",
  },
];

const TOKEN_VALUES = ["aGVsbG8td29ybGQ", "0123456789abcdef0123456789abcdef"];

function generate() {
  const scrypt = SCRYPT_INPUTS.map(({ password, salt }) => ({
    password,
    salt,
    hex: scryptSync(password, Buffer.from(salt, "hex"), SCRYPT.length, {
      N: SCRYPT.cost,
      r: SCRYPT.block,
      p: SCRYPT.parallel,
      maxmem: 64 * 1024 * 1024,
    }).toString("hex"),
  }));
  const tokenHash = TOKEN_HASH_KINDS.flatMap((kind) =>
    TOKEN_VALUES.map((value) => ({
      kind,
      value,
      hex: createHash("sha256")
        .update(`${TOKEN_HASH_PREFIX}${kind}\u0000${value}`, "utf8")
        .digest("hex"),
    })),
  );
  return `${JSON.stringify({ scrypt, tokenHash }, null, 2)}\n`;
}

const output = generate();
if (process.argv.includes("--check")) {
  if (readFileSync(target, "utf8") !== output) {
    console.error(
      "vectors.json 与常量不一致；运行 node scripts/gen-vectors.mjs",
    );
    process.exit(1);
  }
  console.log("vectors.json 与常量一致");
} else {
  writeFileSync(target, output);
  console.log(`已写入 ${target}`);
}
