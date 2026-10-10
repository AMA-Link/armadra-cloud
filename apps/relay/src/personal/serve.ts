/**
 * `personal serve` 装配（中继设计 §1、§6）：存储（`--store file|sqlite`，见 `store.ts`）→ 签名钥 →
 * 内存控制面（JWKS = 自己的公钥，撤销按存储判）→ TLS → 中继内核（隧道终端 + `/s/<id>/` 边缘）
 * + `/v1` 子集 + `/v1/me/stream` + 托管页面。
 *
 * **不碰 PostgreSQL / Redis**（AGENTS.md；`boundaries.test.ts` 扫 import 守住）。日志只有启动 /
 * 停止 / 证书 / 存储这类进程事件，不记请求、令牌、口令。
 */
import { constants, readFileSync } from "node:fs";
import { access, mkdir } from "node:fs/promises";

import {
  createChallengeGate,
  type ChallengeVerifier,
} from "@armadra/control-core/challenge";
import { createMeStreamCore } from "@armadra/control-core/me-stream";
import { LAST_SEEN_FLUSH_MS } from "@armadra/control-core/sessions";
import { trustedOriginsOf } from "@armadra/control-core/sources";
import { guestPrefix } from "@armadra/control-core/subjects";
import { createLogger, type Logger } from "@armadra/cloud-shared/log";
import { createMemoryControlPlane } from "@armadra/relay-kernel/control/memory";
import type { LimitKind, LimitRule } from "@armadra/relay-kernel/control/types";
import { pathAddressing } from "@armadra/relay-kernel/edge/addressing";
import {
  createClientIp,
  type ClientIp,
} from "@armadra/relay-runtime-node/client-ip";
import { errorName } from "@armadra/relay-kernel/log";
import {
  startRelay,
  type RunningRelay,
} from "@armadra/relay-runtime-node/start-relay";

import { RELAY_VERSION } from "../version.ts";
import {
  startAcme,
  LETS_ENCRYPT_DIRECTORY,
  type AcmeIssuer,
  type AcmeManager,
} from "./acme.ts";
import { createPersonalApi } from "./api.ts";
import { createTurnstileVerifier, readTurnstileSecret } from "./challenge.ts";
import {
  createPersonalContext,
  stateRevocation,
  stateSigner,
} from "./context.ts";
import { createState, readPasswordFile } from "./init.ts";
import { holdRunLock } from "./runlock.ts";
import { openPersonalStore, resolveStoreKind, storeExists } from "./store.ts";
import type { PersonalStore } from "./store-types.ts";
import { createMeStream } from "./stream.ts";
import {
  fileManager,
  selfSignedManager,
  tlsNames,
  type TlsManager,
} from "./tls.ts";
import {
  defaultIssuer,
  type PersonalServeOptions,
  type RateLimitOptions,
} from "./args.ts";
import { createWebPages, usableWebRoot } from "./web.ts";

export interface RunningPersonalRelay {
  url: string;
  issuer: string;
  metricsUrl?: string;
  /** 信任锚指纹（自签 / 文件模式）；明文与 ACME 没有要钉的指纹时为 null。 */
  fingerprint: string | null;
  relay: RunningRelay;
  store: PersonalStore;
  close(): Promise<void>;
}

export interface PersonalServeDeps {
  now?: () => number;
  log?: Logger;
  acmeIssuer?: AcmeIssuer;
  /** 测试用：缩短 me/stream 的心跳。 */
  streamPingMs?: number;
  /** 测试用：换掉挑战的提供方（缺省按 `--challenge` 调 Turnstile siteverify）。 */
  challengeVerifier?: ChallengeVerifier;
}

export function issuerFor(options: PersonalServeOptions): string {
  return options.publicOrigin ?? defaultIssuer(options.host, options.port);
}

/** 打开存储；没有时若给了账号与口令就自动 init（compose 与 `pnpm relay:personal` 走这条）。 */
async function openOrInit(
  options: PersonalServeOptions,
  issuer: string,
  log: Logger,
): Promise<PersonalStore> {
  const { kind, legacy } = resolveStoreKind(options.dataDir, options.store);
  if (
    !storeExists(options.dataDir) &&
    options.account &&
    options.passwordFile
  ) {
    const store = await createState({
      dataDir: options.dataDir,
      account: options.account,
      password: readPasswordFile(options.passwordFile),
      issuer,
      store: kind,
    });
    log.info("已初始化个人中转", { account: options.account, store: kind });
    return store;
  }
  const store = openPersonalStore(options.dataDir, kind);
  if (legacy)
    log.info(
      "正在用状态文件：可以用 personal migrate --to sqlite 迁到 SQLite",
      {
        store: kind,
      },
    );
  return store;
}

/** 边缘限流：命令行 / 环境变量 > 状态文件 > 缺省；登录等防暴力破解的规则不在其中。 */
export function edgeLimitRules(
  ...sources: (RateLimitOptions | undefined)[]
): Partial<Record<LimitKind, LimitRule>> {
  const pick = (key: keyof RateLimitOptions) =>
    sources.find((source) => source?.[key] !== undefined)?.[key];
  const rules: Partial<Record<LimitKind, LimitRule>> = {};
  for (const [key, kind] of [
    ["anonymous", "edge.connect.ip"],
    ["preflight", "edge.preflight.ip"],
    ["token", "edge.connect.token"],
    ["account", "edge.connect.acct"],
  ] as const) {
    const max = pick(key);
    if (max !== undefined)
      rules[kind] = { type: "rate", max, windowMs: 60_000 };
  }
  return rules;
}

export async function startPersonalRelay(
  options: PersonalServeOptions,
  deps: PersonalServeDeps = {},
): Promise<RunningPersonalRelay> {
  const log =
    deps.log ?? createLogger({ name: "relay", level: options.logLevel });
  const now = deps.now ?? Date.now;
  await mkdir(options.dataDir, { recursive: true, mode: 0o700 });
  const issuer = issuerFor(options);
  const store = await openOrInit(options, issuer, log);
  try {
    if (store.issuer() !== issuer) {
      store.setIssuer(issuer);
      log.warn("对外地址变了，已登记的源需要重新登记", { issuer });
    }
    return await startServing(options, deps, { log, now, issuer, store });
  } catch (error) {
    store.close();
    throw error;
  }
}

async function startServing(
  options: PersonalServeOptions,
  deps: PersonalServeDeps,
  base: {
    log: Logger;
    now: () => number;
    issuer: string;
    store: PersonalStore;
  },
): Promise<RunningPersonalRelay> {
  const { log, now, issuer, store } = base;

  let tls: TlsManager | undefined;
  let acme: AcmeManager | undefined;
  if (options.tls === "self-signed")
    tls = selfSignedManager(options.dataDir, tlsNames(options.host));
  else if (options.tls === "file" && options.tlsCert && options.tlsKey)
    tls = fileManager(options.tlsCert, options.tlsKey);
  else if (options.tls === "acme" && options.acme) {
    acme = await startAcme({
      dataDir: options.dataDir,
      email: options.acme.email,
      names: [options.host],
      directoryUrl: options.acme.directory ?? LETS_ENCRYPT_DIRECTORY,
      ...(options.acme.caBundle
        ? { caBundle: readFileSync(options.acme.caBundle, "utf8") }
        : {}),
      httpListen: options.acme.httpListen,
      redirectOrigin: issuer,
      ...(deps.acmeIssuer ? { issuer: deps.acmeIssuer } : {}),
      onEvent: (event, failures) =>
        event === "renewed"
          ? log.info("ACME 证书已续期")
          : log.warn("ACME 续期失败，继续用旧证书", { failures }),
    });
    tls = acme;
  }

  const signer = stateSigner(store, now);
  // 内核起来之前没有可断开的连接；之后指向它。
  const relayRef: { current?: RunningRelay } = {};
  const control = createMemoryControlPlane({
    mode: "personal",
    issuer,
    jwks: () => signer.jwks(),
    revoked: stateRevocation(store, now),
    limits: edgeLimitRules(options.rateLimits, store.settings().rateLimits),
    now,
  });
  const context = createPersonalContext({
    store,
    control,
    signer,
    now,
    disconnect: (filter) => relayRef.current?.disconnect(filter),
  });
  if (options.challenge) {
    const { siteKey, scope, secretFile } = options.challenge;
    context.challenge = createChallengeGate({
      siteKey,
      scope,
      verifier:
        deps.challengeVerifier ??
        createTurnstileVerifier({ secret: readTurnstileSecret(secretFile) }),
      now,
      onRejected: (procedure, reasons) =>
        log.info("人机挑战没通过", { procedure, reasons: reasons.join(",") }),
    });
    log.info("已开启人机挑战", {
      provider: "turnstile",
      scope: scope.join(","),
    });
  }
  const streamCore = createMeStreamCore(context);
  const stream = createMeStream(streamCore, {
    now,
    ...(deps.streamPingMs ? { pingMs: deps.streamPingMs } : {}),
  });
  const webRoot = usableWebRoot(options.webRoot);
  if (options.webRoot && !webRoot)
    log.warn("页面产物目录没有 index.html，/app 答错误页", {
      webRoot: options.webRoot,
    });
  const trustedProxy = options.trustedProxy ?? store.settings().trustedProxy;
  let clientIp: ClientIp;
  try {
    clientIp = createClientIp(trustedProxy);
  } catch (error) {
    throw new Error(
      `trustedProxy 不合法：${error instanceof Error ? error.message : String(error)}`,
    );
  }
  const api = createPersonalApi(context, streamCore, {
    clientIp,
    webApp: () => (webRoot ? `${signer.issuer}/app/` : null),
    // 每次现读：状态文件里的 displayName 被别的进程改了也即时生效。
    name: () => options.name ?? store.settings().displayName ?? options.host,
    onError: (error) => log.error("请求处理失败", { error: errorName(error) }),
  });
  const web = createWebPages({
    webRoot,
    caPem: () => tls?.current().anchor,
  });

  const relay = await startRelay({
    mode: "personal",
    nodeId: options.host.slice(0, 64),
    version: RELAY_VERSION,
    listen: options.listen,
    control,
    addressing: pathAddressing(),
    // 与登记时发给源的可信来源相同；所有源一样，答案不随某个源变。
    corsOrigins: () =>
      trustedOriginsOf({
        issuer: store.issuer(),
        trustedOrigins: store.settings().trustedOrigins,
      }),
    clientIp,
    log,
    probes: {
      dataDir: () => access(options.dataDir, constants.R_OK | constants.W_OK),
    },
    metricsListen: options.metricsListen,
    metricsToken: options.metricsToken,
    extra: (req, res) => api.handle(req, res) || web.handle(req, res),
    extraUpgrade: (req, socket, head) =>
      stream.handleUpgrade(req, socket, head),
    ...(tls ? { tls: tls.current() } : {}),
    now,
  }).catch(async (error: unknown) => {
    await acme?.close();
    throw error;
  });
  relayRef.current = relay;

  // 源上下线：记最近在线与版本（只在内存累计，和设备的 lastSeen 一起每 5 分钟落盘）。
  const seenSources = new Map<string, { atMs: number; coreVersion?: string }>();
  const offPresence = control.presence.subscribe((event) => {
    seenSources.set(event.sourceId, {
      atMs: event.atMs,
      ...(event.type === "sourceOnline"
        ? { coreVersion: event.coreVersion }
        : {}),
    });
  });
  const flush = async () => {
    try {
      await api.authenticator.flush();
      if (seenSources.size > 0) {
        const batch = new Map(seenSources);
        seenSources.clear();
        store.recordSourcesSeen(batch);
      }
    } catch (error) {
      log.warn("最近在线时间写入失败", { error: errorName(error) });
    }
  };
  const flushTimer = setInterval(() => void flush(), LAST_SEEN_FLUSH_MS);
  flushTimer.unref();

  // 别的进程（passwd / rotate-key / links revoke）改了存储：新撤销的源与链接立即断开。
  let previous = store.revoked();
  const stopWatch = store.watch(
    () => {
      const current = store.revoked();
      for (const sourceId of current.sources)
        if (!previous.sources.has(sourceId)) {
          control.command({ type: "closeTunnel", sourceId });
          relay.disconnect({ sourceId });
        }
      for (const linkId of current.links)
        if (!previous.links.has(linkId))
          relay.disconnect({ subjectPrefix: guestPrefix(linkId) });
      previous = current;
      log.info("存储有外部改动，已重新读取", { store: store.kind });
    },
    (error) => log.error("存储重新读取失败", { error: errorName(error) }),
  );
  const releaseRunLock = holdRunLock(options.dataDir);

  const stopTls = tls?.start(
    (material) => {
      relay.setTls(material);
      log.info("证书已更换", { notAfter: material.notAfter });
    },
    (error) =>
      log.warn("证书更新失败，继续用旧证书", { error: errorName(error) }),
  );

  const material = tls?.current();
  const fingerprint =
    material && material.source !== "acme" ? material.fingerprint : null;
  log.info("个人中转已启动", {
    url: relay.url,
    issuer,
    host: options.host,
    tls: options.tls,
    store: store.kind,
    ...(fingerprint ? { fingerprint } : {}),
    web: webRoot ? "ready" : "missing",
    ...(relay.metricsUrl ? { metrics: relay.metricsUrl } : {}),
  });

  let closing: Promise<void> | undefined;
  return {
    url: relay.url,
    issuer,
    metricsUrl: relay.metricsUrl,
    fingerprint,
    relay,
    store,
    close() {
      closing ??= (async () => {
        clearInterval(flushTimer);
        stopWatch();
        stopTls?.();
        offPresence();
        await flush();
        stream.close();
        await relay.close(5_000);
        await acme?.close();
        releaseRunLock();
        store.close();
        log.info("个人中转已停止");
      })();
      return closing;
    },
  };
}
