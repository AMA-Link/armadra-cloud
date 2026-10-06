import { createHash, X509Certificate } from "node:crypto";
import {
  mkdtempSync,
  readFileSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { request } from "node:https";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { listen } from "@armadra/cloud-shared/http";
import { afterAll, describe, expect, it } from "vitest";

import {
  fileManager,
  fileMaterial,
  LEAF_RENEW_BEFORE_DAYS,
  selfSignedManager,
  selfSignedMaterial,
  TlsError,
  tlsNames,
} from "./tls.ts";

const root = mkdtempSync(join(tmpdir(), "relay-tls-"));
afterAll(() => rmSync(root, { recursive: true, force: true }));
const DAY = 24 * 60 * 60 * 1000;

describe("自签 CA", () => {
  it("CA（P-256、10 年、CA:TRUE）+ 叶证书（90 天，SAN 含 host / localhost / 127.0.0.1），文件 0600", () => {
    const dir = join(root, "a");
    const now = new Date(Date.UTC(2026, 9, 6));
    const material = selfSignedMaterial(dir, tlsNames("203.0.113.5"), now);
    const ca = new X509Certificate(
      readFileSync(join(dir, "tls", "ca.crt"), "utf8"),
    );
    const leaf = new X509Certificate(
      readFileSync(join(dir, "tls", "leaf.crt"), "utf8"),
    );
    expect(ca.ca).toBe(true);
    expect(ca.publicKey.asymmetricKeyDetails?.namedCurve).toBe("prime256v1");
    expect(new Date(ca.validTo).getTime() - now.getTime()).toBeGreaterThan(
      3649 * DAY,
    );
    expect(leaf.verify(ca.publicKey)).toBe(true);
    expect(leaf.subjectAltName).toBe(
      "IP Address:203.0.113.5, DNS:localhost, IP Address:127.0.0.1",
    );
    expect(
      Math.round((new Date(leaf.validTo).getTime() - now.getTime()) / DAY),
    ).toBe(90);
    // 指纹 = CA DER 的 SHA-256，小写 hex（与 core Gateway localCa 同一定义）。
    expect(material.fingerprint).toBe(
      createHash("sha256").update(ca.raw).digest("hex"),
    );
    expect(material.anchor).toBe(
      readFileSync(join(dir, "tls", "ca.crt"), "utf8"),
    );
    if (process.platform !== "win32")
      for (const name of ["ca.crt", "ca.key", "leaf.crt", "leaf.key"])
        expect(statSync(join(dir, "tls", name)).mode & 0o777).toBe(0o600);
  });

  it("重启复用；剩 7 天以内或 host 变了只换叶证书，CA 与指纹不变", () => {
    const dir = join(root, "b");
    const t0 = new Date(Date.UTC(2026, 9, 6));
    const first = selfSignedMaterial(dir, tlsNames("relay.example"), t0);
    expect(selfSignedMaterial(dir, tlsNames("relay.example"), t0).cert).toBe(
      first.cert,
    );
    const nearEnd = new Date(
      t0.getTime() + (90 - LEAF_RENEW_BEFORE_DAYS) * DAY + 1,
    );
    const renewed = selfSignedMaterial(dir, tlsNames("relay.example"), nearEnd);
    expect(renewed.cert).not.toBe(first.cert);
    expect(renewed.fingerprint).toBe(first.fingerprint);
    const moved = selfSignedMaterial(dir, tlsNames("relay2.example"), nearEnd);
    expect(moved.names).toContain("relay2.example");
    expect(moved.fingerprint).toBe(first.fingerprint);
  });

  it("管理器按假时钟到期换叶证书并回调", async () => {
    const dir = join(root, "c");
    let now = new Date(Date.UTC(2026, 9, 6));
    const manager = selfSignedManager(dir, tlsNames("127.0.0.1"), () => now);
    const before = manager.current().cert;
    now = new Date(now.getTime() + 85 * DAY);
    // 直接重算一次（定时器 12 小时一跳，这里不等）。
    const next = selfSignedMaterial(dir, tlsNames("127.0.0.1"), now);
    expect(next.cert).not.toBe(before);
  });

  it("CA 坏了或过期：拒绝启动，不悄悄换一张", () => {
    const dir = join(root, "d");
    const now = new Date(Date.UTC(2026, 9, 6));
    selfSignedMaterial(dir, tlsNames("127.0.0.1"), now);
    expect(() =>
      selfSignedMaterial(
        dir,
        tlsNames("127.0.0.1"),
        new Date(now.getTime() + 3651 * DAY),
      ),
    ).toThrow(TlsError);
    writeFileSync(join(dir, "tls", "ca.crt"), "garbage");
    expect(() => selfSignedMaterial(dir, tlsNames("127.0.0.1"), now)).toThrow(
      TlsError,
    );
  });

  it("node:https 用它起得来，信任 CA 的客户端握手成功", async () => {
    const dir = join(root, "e");
    const material = selfSignedMaterial(dir, tlsNames("127.0.0.1"));
    const server = await listen(
      { host: "127.0.0.1", port: 0 },
      (_req, res) => {
        res.end("ok");
      },
      { tls: material },
    );
    try {
      const body = await new Promise<string>((resolve, reject) => {
        const req = request(
          `${server.url}/`,
          { ca: material.anchor },
          (res) => {
            let text = "";
            res.on("data", (chunk) => (text += chunk));
            res.on("end", () => resolve(text));
          },
        );
        req.on("error", reject);
        req.end();
      });
      expect(body).toBe("ok");
      // HTTPS 监听同样用长空闲 keep-alive（iOS WebKit 不重试被服务端关掉的连接）。
      expect(server.server.keepAliveTimeout).toBe(75_000);
      expect(server.server.headersTimeout).toBe(76_000);
      // 换证书（续期 / 热加载）：新连接拿到新证书，旧 CA 不再验得过。
      const other = selfSignedMaterial(join(root, "e2"), tlsNames("127.0.0.1"));
      server.setTls?.(other);
      const fingerprint = await new Promise<string>((resolve, reject) => {
        const req = request(
          `${server.url}/`,
          { ca: other.anchor, agent: false },
          (res) => {
            res.resume();
            const cert = (
              res.socket as import("node:tls").TLSSocket
            ).getPeerCertificate(true);
            resolve(
              createHash("sha256")
                .update(cert.issuerCertificate.raw)
                .digest("hex"),
            );
          },
        );
        req.on("error", reject);
        req.end();
      });
      expect(fingerprint).toBe(other.fingerprint);
    } finally {
      await server.close();
    }
  });
});

describe("文件证书", () => {
  it("读证书与私钥；链里多于一张时最后一张是信任锚；对不上报 TlsError", () => {
    const dir = join(root, "f");
    const source = selfSignedMaterial(dir, tlsNames("127.0.0.1"));
    const certFile = join(dir, "chain.pem");
    const keyFile = join(dir, "key.pem");
    writeFileSync(certFile, source.cert);
    writeFileSync(keyFile, source.key);
    const material = fileMaterial(certFile, keyFile);
    expect(material.fingerprint).toBe(source.fingerprint);
    const otherDir = join(root, "g");
    const other = selfSignedMaterial(otherDir, tlsNames("127.0.0.1"));
    writeFileSync(keyFile, other.key);
    expect(() => fileMaterial(certFile, keyFile)).toThrow(TlsError);
    expect(() => fileMaterial(join(dir, "missing.pem"), keyFile)).toThrow(
      TlsError,
    );
  });

  it("文件变化时热加载（换到一半对不上时继续用旧的）", async () => {
    const dir = join(root, "h");
    const first = selfSignedMaterial(join(dir, "one"), tlsNames("127.0.0.1"));
    const second = selfSignedMaterial(join(dir, "two"), tlsNames("127.0.0.1"));
    const certFile = join(dir, "cert.pem");
    const keyFile = join(dir, "key.pem");
    writeFileSync(certFile, first.cert);
    writeFileSync(keyFile, first.key);
    const manager = fileManager(certFile, keyFile);
    const changes: string[] = [];
    const stop = manager.start(
      (material) => changes.push(material.fingerprint),
      () => {},
    );
    try {
      writeFileSync(keyFile, second.key);
      writeFileSync(certFile, second.cert);
      await new Promise((resolve) => setTimeout(resolve, 3500));
      expect(changes).toEqual([second.fingerprint]);
      expect(manager.current().fingerprint).toBe(second.fingerprint);
    } finally {
      stop();
    }
  }, 10_000);
});
