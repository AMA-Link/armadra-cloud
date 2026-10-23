// .env.dev 的读取，以及把它映射成 Go 控制面（armadra-cloud）认的环境变量。
import { readFileSync } from "node:fs";

/** .env.dev（KEY=VALUE 行，不带引号）→ 对象；Go 二进制不认 node 的 --env-file，自己读。 */
export function parseEnv(text) {
  const env = {};
  for (const line of text.split("\n")) {
    const match = /^([A-Za-z_][A-Za-z0-9_]*)=(.*)$/.exec(line.trim());
    if (match) env[match[1]] = match[2];
  }
  return env;
}

export function readEnvFile(file) {
  return parseEnv(readFileSync(file, "utf8"));
}

/**
 * 控制面的环境：.env.dev 里只有 ARMADRA_CLOUD_SMTP_URL（早先 TS 控制面的写法）时，
 * 拆成 Go 版认的 SMTP_HOST / PORT / TLS / FROM（本地 mailpit 明文，只许回环地址）。
 */
export function cloudEnv(env) {
  const out = { ...env };
  const url = out.ARMADRA_CLOUD_SMTP_URL;
  if (url && !out.ARMADRA_CLOUD_SMTP_HOST) {
    const parsed = new URL(url);
    out.ARMADRA_CLOUD_SMTP_HOST = parsed.hostname;
    if (parsed.port) out.ARMADRA_CLOUD_SMTP_PORT = parsed.port;
    out.ARMADRA_CLOUD_SMTP_TLS ??= "none";
    out.ARMADRA_CLOUD_SMTP_FROM ??= "armadra@localhost";
  }
  delete out.ARMADRA_CLOUD_SMTP_URL;
  return out;
}
