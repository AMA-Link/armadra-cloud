/**
 * Worker 入口与对象里的闸、运维端点、托管页面与平台信息。
 */
import { describe, expect, it } from "vitest";

import {
  ACCOUNT,
  call,
  DEVICE,
  heartbeatHint,
  json,
  login,
  nextIp,
  postJson,
  registerSource,
} from "./api.ts";
import { FakeCore } from "./fake-core.ts";

describe("闸（Rate Limiting 绑定）", () => {
  it("登录按 IP：超过绑定的额度答 429 rate_limited 并带 Retry-After", async () => {
    const ip = nextIp();
    const statuses: number[] = [];
    for (let attempt = 0; attempt < 7; attempt += 1) {
      const response = await postJson(
        "/v1/auth/login",
        {
          account: `nobody-${attempt}`,
          password: "wrong-password-x",
          device: DEVICE,
        },
        { ip },
      );
      statuses.push(response.status);
      if (response.status === 429) {
        expect((await json(response)).code).toBe("rate_limited");
        expect(response.headers.get("retry-after")).toBe("60");
      } else await response.arrayBuffer();
    }
    expect(statuses.slice(0, 5)).toEqual([401, 401, 401, 401, 401]);
    expect(statuses.at(-1)).toBe(429);
    // 别的 IP 不受影响。
    expect((await login(nextIp())).accessToken).toBeTruthy();
  });
});

describe("对象里的上限", () => {
  it("隧道帧超过 maxFrameBytes：以 4413 关闭", async () => {
    const session = await login();
    const source = await registerSource(session);
    const hint = await heartbeatHint(source);
    const core = new FakeCore(source);
    expect(
      (await core.connect(hint.nodes[0]?.url ?? "", hint.tunnelToken)).ready,
    ).toBeTruthy();
    core.ws?.send(new Uint8Array(1024 * 1024 + 64));
    expect((await core.closed()).code).toBe(4413);
  });

  it("握手阶段的文本消息超过 64 KiB：以 4400 关闭", async () => {
    const session = await login();
    const source = await registerSource(session);
    const response = await call(`/t/v1?src=${source.sourceId}`, {
      headers: { upgrade: "websocket" },
    });
    const ws = response.webSocket;
    if (!ws) throw new Error("没有 WS");
    ws.accept();
    const done = new Promise<number>((resolve) =>
      ws.addEventListener("close", (event) => resolve(event.code)),
    );
    ws.send("x".repeat(65 * 1024));
    expect(await done).toBe(4400);
  });
});

describe("运维端点", () => {
  it("/health 自己答；/ready 看控制面；/metrics 要令牌", async () => {
    const health = await call("/health");
    expect(await json(health)).toMatchObject({
      ok: true,
      mode: "personal",
      node: "cf",
      version: "0.9.1",
    });
    expect(health.headers.get("armadra-protocol")).toBe("1.3");
    const ready = await call("/ready");
    expect(ready.status).toBe(200);
    await ready.arrayBuffer();
    const denied = await call("/metrics");
    expect(denied.status).toBe(401);
    await denied.arrayBuffer();
    const metrics = await call("/metrics", {
      headers: { authorization: "Bearer test-metrics-token" },
    });
    expect(metrics.status).toBe(200);
    const text = await metrics.text();
    expect(text).toContain("relay_tunnels ");
    expect(text).toContain('relay_budget_limit{kind="requests"} 2000000');
  });

  it("platform.info：personal、名称、托管页面、协议 1.2、没有挑战", async () => {
    const info = await json(await call("/.well-known/armadra-platform"));
    expect(info).toMatchObject({
      mode: "personal",
      name: "Test relay",
      issuer: "https://relay.armadra.test",
      protocol: { major: 1, minor: 3 },
      relay: { addressing: "path" },
      webApp: "https://relay.armadra.test/app/",
    });
    expect(info.challenge).toBeUndefined();
    const jwks = await json<{ keys: { kid: string }[] }>(
      await call("/.well-known/jwks.json"),
    );
    expect(jwks.keys.map((key) => key.kid)).toEqual(["k1"]);
  });

  it("/v1 允许任意来源；没实现的 procedure 404；口令错 401 不泄露账号是否存在", async () => {
    const preflight = await call("/v1/auth/login", { method: "OPTIONS" });
    expect(preflight.status).toBe(204);
    expect(preflight.headers.get("access-control-allow-origin")).toBe("*");
    const orgs = await call("/v1/orgs", { method: "POST", body: "{}" });
    expect(orgs.status).toBe(404);
    expect((await json(orgs)).code).toBe("not_found");
    const wrong = await postJson("/v1/auth/login", {
      account: ACCOUNT,
      password: "definitely-wrong-1",
      device: DEVICE,
    });
    expect(wrong.status).toBe(401);
    expect((await json(wrong)).code).toBe("credentials_invalid");
  });
});

describe("托管页面", () => {
  it("/ 与 /app 跳 /app/；/app/* 答 index.html（不缓存、带 CSP）；根下的产物文件直接答", async () => {
    const root = await call("/", { redirect: "manual" });
    expect(root.status).toBe(308);
    expect(root.headers.get("location")).toBe("/app/");
    const app = await call("/app/");
    expect(app.status).toBe(200);
    expect(app.headers.get("cache-control")).toBe("no-store");
    expect(app.headers.get("content-security-policy")).toContain(
      "default-src 'self'",
    );
    expect(await app.text()).toContain("<title>Armadra</title>");
    const deep = await call("/app/canvas/123");
    expect(await deep.text()).toContain("<title>Armadra</title>");
    const join = await call("/j/0123456789abcdef");
    expect(await join.text()).toContain("<title>Armadra</title>");
    const asset = await call("/assets/index-Ab12Cd34.js");
    expect(asset.status).toBe(200);
    expect(asset.headers.get("cache-control")).toBe(
      "public, max-age=31536000, immutable",
    );
    await asset.arrayBuffer();
    const missing = await call("/nope.txt");
    expect(missing.status).toBe(404);
    expect((await json(missing)).code).toBe("not_found");
  });
});
