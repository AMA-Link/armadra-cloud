/**
 * personal 模式装配：数据目录 → 内存控制面 → 中继内核（隧道终端 + 边缘，`/s/<id>/` 寻址）。
 * **不碰 PostgreSQL / Redis**（AGENTS.md；`boundaries.test.ts` 扫 import 守住）。
 *
 * 账号、签名钥、源目录、分享与 TLS 在 R2 落地。在那之前 JWKS 是空的：隧道令牌与中继令牌
 * 都验不过，隧道与边缘对外是「全部拒绝」——起得来、`/health` 正常，但不放任何流量。
 */
import { createLogger } from "@armadra/cloud-shared/log";
import { constants } from "node:fs";
import { access, mkdir } from "node:fs/promises";

import { createMemoryControlPlane } from "../control/memory.ts";
import { pathAddressing } from "../edge/addressing.ts";
import { startRelay } from "../main.ts";
import { RELAY_VERSION } from "../version.ts";
import {
  isLoopback,
  SUPPORTED_TLS_MODES,
  UsageError,
  type PersonalServeOptions,
} from "./args.ts";

export interface RunningPersonalRelay {
  url: string;
  metricsUrl?: string;
  close(): Promise<void>;
}

/** personal 的 issuer：`https://<--host>:<--port>`（中继设计 §7 `RELAY_PUBLIC_ORIGIN` 的缺省）。 */
export function personalIssuer(options: PersonalServeOptions): string {
  const host = options.host.includes(":") ? `[${options.host}]` : options.host;
  return options.port === 443
    ? `https://${host}`
    : `https://${host}:${options.port}`;
}

export async function startPersonalRelay(
  options: PersonalServeOptions,
): Promise<RunningPersonalRelay> {
  if (!SUPPORTED_TLS_MODES.includes(options.tls)) {
    throw new UsageError(
      `--tls ${options.tls} 尚未实现，当前只支持 --tls plain`,
    );
  }
  const log = createLogger({ name: "relay", level: options.logLevel });
  await mkdir(options.dataDir, { recursive: true, mode: 0o700 });
  if (options.tls === "plain" && !isLoopback(options.listen.host)) {
    log.warn(
      "明文监听非回环地址，只应放在终止 TLS 的反向代理或容器端口映射之后",
      {
        listen: `${options.listen.host}:${options.listen.port}`,
      },
    );
  }
  const control = createMemoryControlPlane({
    mode: "personal",
    issuer: personalIssuer(options),
    // R2：换成签名钥的公钥集。
    jwks: () => ({ keys: [] }),
  });
  const relay = await startRelay({
    mode: "personal",
    nodeId: options.host,
    version: RELAY_VERSION,
    listen: options.listen,
    control,
    addressing: pathAddressing(),
    log,
    probes: {
      dataDir: () => access(options.dataDir, constants.R_OK | constants.W_OK),
    },
    metricsListen: options.metricsListen,
    metricsToken: options.metricsToken,
  });
  log.info("个人中转已启动", {
    url: relay.url,
    host: options.host,
    tls: options.tls,
    ...(relay.metricsUrl ? { metrics: relay.metricsUrl } : {}),
  });
  return {
    url: relay.url,
    metricsUrl: relay.metricsUrl,
    async close() {
      await relay.close(5_000);
      log.info("个人中转已停止");
    },
  };
}
