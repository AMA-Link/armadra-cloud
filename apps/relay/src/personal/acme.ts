/**
 * ACME（中继设计 §6.6 `--tls acme`）：`acme-client` 走 RFC 8555，`http-01` 挑战。
 *
 * - 挑战监听：`--acme-http-listen`（缺省 `0.0.0.0:80`）上只答
 *   `/.well-known/acme-challenge/<令牌>`，其余 308 到对外来源。监听一直开着，续期也要用。
 * - 存放：`<data-dir>/acme/`（0700），账户密钥、证书链、私钥都是 0600，先写临时文件再改名。
 * - 启动：已有证书没过期、覆盖 `--host` 就直接用；否则当场签，签不出来就起不来——悄悄退回
 *   自签会让客户端全被拦而运维不知道为什么。
 * - 续期：剩 30 天时续；失败每 12 小时重试，继续用旧证书并计数。
 * - 目录：`--acme-directory`（缺省 Let's Encrypt 生产）；`--acme-ca-bundle` 信任测试 CA（pebble）。
 */
import {
  createPrivateKey,
  createPublicKey,
  generateKeyPairSync,
  X509Certificate,
} from "node:crypto";
import { existsSync, mkdirSync, readFileSync, chmodSync } from "node:fs";
import { createServer, type Server } from "node:http";
import { Agent as HttpsAgent } from "node:https";
import { isIP } from "node:net";
import { join } from "node:path";

import type { ListenAddress } from "@armadra/cloud-shared/config";

import { certificateRequest } from "./der.ts";
import {
  fingerprintOf,
  replaceFile,
  sanNames,
  TlsError,
  type TlsManager,
  type TlsMaterial,
} from "./tls.ts";

export const ACME_DIR = "acme";
export const LETS_ENCRYPT_DIRECTORY =
  "https://acme-v02.api.letsencrypt.org/directory";
export const ACME_RENEW_BEFORE_MS = 30 * 24 * 60 * 60 * 1000;
export const ACME_RETRY_MS = 12 * 60 * 60 * 1000;
const CHALLENGE_PREFIX = "/.well-known/acme-challenge/";

export interface IssueRequest {
  names: readonly string[];
  email: string;
  accountKey: string;
  certificateKey: string;
  /** 令牌 → key authorization；签发途中由挑战监听答出去。 */
  challenges: Map<string, string>;
}

/** 一次签发，答 PEM 证书链（叶证书在前）。测试换成假的。 */
export type AcmeIssuer = (request: IssueRequest) => Promise<string>;

export interface AcmeOptions {
  dataDir: string;
  email: string;
  /** 证书覆盖的名字：`--host`（域名或 IP）。 */
  names: readonly string[];
  directoryUrl: string;
  caBundle?: string;
  httpListen: ListenAddress;
  /** 挑战监听上非挑战请求 308 去的地方（对外来源）。 */
  redirectOrigin: string;
  issuer?: AcmeIssuer;
  now?: () => number;
  retryMs?: number;
  onEvent?: (event: "renewed" | "renew_failed", failures: number) => void;
}

export function acmeClientIssuer(
  directoryUrl: string,
  caBundle?: string,
): AcmeIssuer {
  return async (request) => {
    const acme = await import("acme-client");
    if (caBundle !== undefined)
      acme.axios.defaults.httpsAgent = new HttpsAgent({ ca: caBundle });
    const client = new acme.Client({
      directoryUrl,
      accountKey: request.accountKey,
    });
    await client.createAccount({
      termsOfServiceAgreed: true,
      contact: [`mailto:${request.email}`],
    });
    const order = await client.createOrder({
      identifiers: request.names.map((value) => ({
        type: isIP(value) === 0 ? "dns" : "ip",
        value,
      })),
    });
    for (const authz of await client.getAuthorizations(order)) {
      if (authz.status === "valid") continue;
      const challenge = authz.challenges.find(
        (candidate) => candidate.type === "http-01",
      );
      if (!challenge)
        throw new TlsError(`${authz.identifier.value} 没有 http-01 挑战`);
      request.challenges.set(
        challenge.token,
        await client.getChallengeKeyAuthorization(challenge),
      );
      try {
        await client.completeChallenge(challenge);
        await client.waitForValidStatus(challenge);
      } finally {
        request.challenges.delete(challenge.token);
      }
    }
    const key = createPrivateKey(request.certificateKey);
    const spki = createPublicKey(key).export({
      type: "spki",
      format: "der",
    }) as Buffer;
    const finalized = await client.finalizeOrder(
      order,
      certificateRequest(request.names, key, spki),
    );
    return client.getCertificate(finalized);
  };
}

function describe(cert: string, key: string): TlsMaterial {
  const parsed = new X509Certificate(cert);
  return {
    cert,
    key,
    source: "acme",
    fingerprint: fingerprintOf(parsed),
    notAfter: parsed.validTo,
    names: sanNames(parsed),
  };
}

function ecKeyPem(): string {
  return generateKeyPairSync("ec", { namedCurve: "prime256v1" })
    .privateKey.export({ type: "pkcs8", format: "pem" })
    .toString();
}

export interface AcmeManager extends TlsManager {
  close(): Promise<void>;
}

export async function startAcme(options: AcmeOptions): Promise<AcmeManager> {
  const now = options.now ?? Date.now;
  const directory = join(options.dataDir, ACME_DIR);
  mkdirSync(directory, { recursive: true, mode: 0o700 });
  chmodSync(directory, 0o700);
  const certFile = join(directory, "cert.pem");
  const keyFile = join(directory, "key.pem");
  const accountFile = join(directory, "account.key");
  if (!existsSync(accountFile)) replaceFile(accountFile, ecKeyPem());
  const accountKey = readFileSync(accountFile, "utf8");
  const issuer =
    options.issuer ?? acmeClientIssuer(options.directoryUrl, options.caBundle);
  const challenges = new Map<string, string>();

  const server: Server = createServer((req, res) => {
    const path = (req.url ?? "/").split("?")[0] ?? "/";
    if (path.startsWith(CHALLENGE_PREFIX)) {
      const answer = challenges.get(path.slice(CHALLENGE_PREFIX.length));
      res.writeHead(answer ? 200 : 404, {
        "content-type": "text/plain; charset=utf-8",
        "cache-control": "no-store",
      });
      res.end(answer ?? "");
      return;
    }
    res.writeHead(308, {
      location: `${options.redirectOrigin}${req.url ?? "/"}`,
    });
    res.end();
  });
  await new Promise<void>((resolve, reject) => {
    server.once("error", reject);
    server.listen(options.httpListen.port, options.httpListen.host, () => {
      server.off("error", reject);
      resolve();
    });
  });

  const existing = (): TlsMaterial | undefined => {
    if (!existsSync(certFile) || !existsSync(keyFile)) return undefined;
    try {
      const material = describe(
        readFileSync(certFile, "utf8"),
        readFileSync(keyFile, "utf8"),
      );
      if (new Date(material.notAfter).getTime() <= now()) return undefined;
      const have = material.names.map((name) => name.toLowerCase());
      if (!options.names.every((name) => have.includes(name.toLowerCase())))
        return undefined;
      return material;
    } catch {
      return undefined;
    }
  };

  const issue = async (): Promise<TlsMaterial> => {
    const certificateKey = ecKeyPem();
    const chain = await issuer({
      names: options.names,
      email: options.email,
      accountKey,
      certificateKey,
      challenges,
    });
    const material = describe(chain, certificateKey);
    replaceFile(keyFile, certificateKey);
    replaceFile(certFile, chain);
    return material;
  };

  let material = existing();
  if (!material) {
    try {
      material = await issue();
    } catch (error) {
      await new Promise<void>((resolve) => server.close(() => resolve()));
      throw error instanceof TlsError
        ? error
        : new TlsError(
            `ACME 签发失败，拒绝启动：${String((error as Error)?.message ?? error).slice(0, 200)}`,
          );
    }
  }

  let failures = 0;
  let timer: NodeJS.Timeout | undefined;
  let stopped = false;
  return {
    current: () => material as TlsMaterial,
    start(onChange, onError) {
      const tick = async () => {
        if (stopped) return;
        const remaining =
          new Date((material as TlsMaterial).notAfter).getTime() - now();
        if (remaining <= ACME_RENEW_BEFORE_MS) {
          try {
            material = await issue();
            failures = 0;
            options.onEvent?.("renewed", 0);
            onChange(material);
          } catch (error) {
            failures += 1;
            options.onEvent?.("renew_failed", failures);
            onError(error);
          }
        }
        if (!stopped) {
          timer = setTimeout(
            () => void tick(),
            options.retryMs ?? ACME_RETRY_MS,
          );
          timer.unref();
        }
      };
      void tick();
      return () => {
        stopped = true;
        clearTimeout(timer);
      };
    },
    async close() {
      stopped = true;
      clearTimeout(timer);
      server.closeAllConnections();
      await new Promise<void>((resolve) => server.close(() => resolve()));
    },
  };
}
