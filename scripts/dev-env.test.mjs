// node --test scripts/dev-env.test.mjs
import assert from "node:assert/strict";
import test from "node:test";

import { cloudEnv, parseEnv } from "./dev-env.mjs";

test("parseEnv 跳过注释与空行", () => {
  assert.deepEqual(parseEnv("# x\n\nA=1\nB=a=b\n"), { A: "1", B: "a=b" });
});

test("cloudEnv 把 SMTP_URL 拆成 Go 控制面认的变量", () => {
  const out = cloudEnv({ ARMADRA_CLOUD_SMTP_URL: "smtp://127.0.0.1:1026" });
  assert.equal(out.ARMADRA_CLOUD_SMTP_HOST, "127.0.0.1");
  assert.equal(out.ARMADRA_CLOUD_SMTP_PORT, "1026");
  assert.equal(out.ARMADRA_CLOUD_SMTP_TLS, "none");
  assert.ok(out.ARMADRA_CLOUD_SMTP_FROM);
  assert.equal("ARMADRA_CLOUD_SMTP_URL" in out, false);
});

test("cloudEnv 已有 SMTP_HOST 时不覆盖", () => {
  const out = cloudEnv({
    ARMADRA_CLOUD_SMTP_URL: "smtp://127.0.0.1:1026",
    ARMADRA_CLOUD_SMTP_HOST: "mail.local",
  });
  assert.equal(out.ARMADRA_CLOUD_SMTP_HOST, "mail.local");
});

test("本地开发脚本起的是 Go 控制面，不再碰预留 501 的 apps/cloud（#118）", async () => {
  const { readFileSync } = await import("node:fs");
  for (const file of ["dev-run.mjs", "dev.sh", "dev-cloud.mjs"]) {
    const text = readFileSync(new URL(`./${file}`, import.meta.url), "utf8");
    assert.equal(text.includes("apps/cloud"), false, file);
  }
  assert.match(
    readFileSync(new URL("./dev-run.mjs", import.meta.url), "utf8"),
    /resolveCloudBin/,
  );
});
