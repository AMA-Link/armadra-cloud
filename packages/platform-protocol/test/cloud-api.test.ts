import { describe, expect, it } from "vitest";
import type { z } from "zod";

import {
  cloudContract,
  listProcedures,
  PLATFORM_CAPABILITIES,
  sectionOf,
  SCOPES,
  TRAFFIC_KINDS,
} from "../src/cloud-api/index.ts";
import { TEAM_ROLES } from "../src/assertion/index.ts";
import {
  meStreamClientMessageSchema,
  meStreamEventSchema,
} from "../src/cloud-api/events.ts";
import { errorStatus, isErrorCode } from "../src/errors/registry.ts";
import { PROTOCOL_VERSION } from "../src/version.ts";
import { listFixtures, readJsonFixture } from "../src/fixtures/index.ts";

const procedures = listProcedures(cloudContract);

describe("契约结构", () => {
  it("覆盖设计 §5 的域、节点回报（§17）与 1.4 的 SaaS 各组（§21–§28）", () => {
    expect(Object.keys(cloudContract)).toEqual([
      "platform",
      "auth",
      "me",
      "sources",
      "links",
      "teams",
      "invitations",
      "groups",
      "grants",
      "accessRequests",
      "trust",
      "audit",
      "billing",
      "apiTokens",
      "admin",
      "status",
      "relay",
    ]);
    expect(procedures.length).toBeGreaterThanOrEqual(100);
  });

  it("「方法 + 路径」唯一", () => {
    const seen = new Map<string, string>();
    for (const entry of procedures) {
      const route = `${entry.method} ${entry.path}`;
      expect(
        seen.get(route),
        `${entry.key} 与 ${seen.get(route)} 重复 ${route}`,
      ).toBeUndefined();
      seen.set(route, entry.key);
    }
  });

  it("每条都有方法、路径、scope、since 与 cloud-api.md 的节号", () => {
    for (const entry of procedures) {
      expect(entry.path, entry.key).toMatch(/^\/(v1\/|\.well-known\/|health$)/);
      expect(["GET", "POST", "PATCH", "PUT", "DELETE"], entry.key).toContain(
        entry.method,
      );
      expect(
        entry.meta.scope.length,
        `${entry.key} 没有 scope`,
      ).toBeGreaterThan(0);
      for (const scope of entry.meta.scope)
        expect(SCOPES, entry.key).toContain(scope);
      expect(entry.meta.since, entry.key).toMatch(/^\d+\.\d+\.\d+$/);
      expect(sectionOf(entry.key), entry.key).toMatch(/^§\d+$/);
    }
  });

  it("路径参数都在 input schema 里", () => {
    for (const entry of procedures) {
      const params = [...entry.path.matchAll(/\{(\w+)\}/g)].map(
        (match) => match[1],
      );
      const input = entry.procedure["~orpc"].inputSchema as
        | z.ZodObject
        | undefined;
      for (const param of params)
        expect(
          Object.keys(input?.shape ?? {}),
          `${entry.key} 缺 ${param}`,
        ).toContain(param);
    }
  });

  it("所有 errors 都在注册表里，状态码一致", () => {
    for (const entry of procedures) {
      const errorMap = entry.procedure["~orpc"].errorMap as Record<
        string,
        { status?: number }
      >;
      for (const code of entry.errorCodes) {
        expect(isErrorCode(code), `${entry.key}: ${code}`).toBe(true);
        expect(errorMap[code]?.status).toBeGreaterThanOrEqual(400);
      }
    }
  });
});

describe("cloud-api 夹具", () => {
  const names = listFixtures("cloud-api");

  it("每条 procedure 正好一份夹具", () => {
    expect(
      names.map((name) => name.replace(/^cloud-api\/|\.json$/g, "")).sort(),
    ).toEqual(procedures.map((entry) => entry.key).sort());
  });

  it.each(procedures.map((entry) => [entry.key, entry] as const))(
    "%s 的 input / output 通过各自 schema",
    (key, entry) => {
      const sample = readJsonFixture<{
        input: unknown;
        output?: unknown;
        outputs?: unknown[];
      }>(`cloud-api/${key}.json`);
      const def = entry.procedure["~orpc"];
      const input = def.inputSchema as z.ZodType;
      const output = def.outputSchema as z.ZodType;
      expect(input.safeParse(sample.input).error).toBeUndefined();
      const outputs = sample.outputs ?? [sample.output];
      expect(outputs.length).toBeGreaterThan(0);
      for (const value of outputs)
        expect(output.safeParse(value).error).toBeUndefined();
    },
  );
});

describe("协议 1.4：团队与 SaaS 的预留 procedure", () => {
  const byKey = new Map(procedures.map((entry) => [entry.key, entry]));
  const input = (key: string) =>
    byKey.get(key)?.procedure["~orpc"].inputSchema as z.ZodType;

  it("没有 orgs.* 和 org 范围；计费挂在团队下", () => {
    expect(procedures.some((entry) => entry.key.startsWith("orgs."))).toBe(
      false,
    );
    expect(SCOPES).not.toContain("org:admin");
    expect(SCOPES).not.toContain("org:owner");
    for (const entry of procedures)
      expect(entry.path, entry.key).not.toContain("/v1/orgs");
    expect(byKey.get("billing.plan")).toMatchObject({
      method: "GET",
      path: "/v1/billing/plan",
      meta: { scope: ["session", "team:admin", "team:billing"] },
    });
    expect(byKey.get("billing.usage")?.meta.scope).toEqual([
      "session",
      "team:admin",
      "team:billing",
    ]);
    expect(byKey.get("billing.checkout")?.meta.scope).toEqual([
      "session",
      "team:billing",
    ]);
    expect(byKey.get("billing.webhook")?.meta.scope).toEqual([
      "billing-webhook",
    ]);
  });

  it("运营后台只用 operator scope", () => {
    const admin = procedures.filter((entry) => entry.key.startsWith("admin."));
    expect(admin.length).toBeGreaterThanOrEqual(8);
    for (const entry of admin) {
      expect(entry.meta.scope, entry.key).toEqual(["operator"]);
      expect(entry.path, entry.key).toMatch(/^\/v1\/admin\//);
    }
    for (const entry of procedures)
      if (!entry.key.startsWith("admin."))
        expect(entry.meta.scope, entry.key).not.toContain("operator");
  });

  it("新 procedure 的节号、since 与错误码", () => {
    expect(sectionOf("teams.create")).toBe("§21");
    expect(sectionOf("invitations.accept")).toBe("§22");
    expect(sectionOf("groups.setLead")).toBe("§23");
    expect(sectionOf("grants.effective")).toBe("§24");
    expect(sectionOf("accessRequests.approve")).toBe("§24");
    expect(sectionOf("audit.export")).toBe("§25");
    expect(sectionOf("billing.checkout")).toBe("§26");
    expect(sectionOf("apiTokens.create")).toBe("§27");
    expect(sectionOf("admin.incident")).toBe("§28");
    expect(sectionOf("status.get")).toBe("§28");
    expect(sectionOf("trust.list")).toBe("§31");
    expect(sectionOf("auth.deviceGet")).toBe("§29");
    expect(sectionOf("me.notificationPrefs")).toBe("§29");
    expect(sectionOf("auth.emailVerify")).toBe("§29");
    expect(sectionOf("me.spaces")).toBe("§29");
    expect(sectionOf("sources.transferStart")).toBe("§4");
    expect(byKey.get("teams.memberRemove")?.errorCodes).toContain(
      "team_last_owner",
    );
    expect(byKey.get("me.delete")?.errorCodes).toContain("team_owner_required");
    expect(errorStatus("team_role_insufficient")).toBe(403);
    expect(errorStatus("team_owner_required")).toBe(409);
    expect(errorStatus("invitation_expired")).toBe(410);
    expect(errorStatus("transfer_pending")).toBe(409);
    expect(errorStatus("email_token_invalid")).toBe(400);
    expect(errorStatus("webhook_signature_invalid")).toBe(401);
    for (const entry of procedures)
      if (
        /^(teams|invitations|groups|grants|accessRequests|trust|audit|apiTokens|admin|status)\./.test(
          entry.key,
        )
      )
        // 1.4 随 0.10.0 发布；之后只加不改的补充（如 invitations.preview）从 0.11.0 起。
        expect(["0.10.0", "0.11.0"], entry.key).toContain(entry.meta.since);
  });

  it("注册令牌带空间，缺省个人空间", () => {
    const schema = input("sources.registrationToken");
    expect(schema.safeParse({}).success).toBe(true);
    expect(schema.safeParse({ space: { kind: "personal" } }).success).toBe(
      true,
    );
    expect(
      schema.safeParse({ space: { kind: "team", teamId: "team_1" } }).success,
    ).toBe(true);
    expect(schema.safeParse({ space: { kind: "team" } }).success).toBe(false);
    expect(
      schema.safeParse({ space: { kind: "org", orgId: "o" } }).success,
    ).toBe(false);
  });

  it("团队角色五档；邀请和角色变更不能给 owner", () => {
    expect(TEAM_ROLES).toEqual([
      "owner",
      "admin",
      "member",
      "billing",
      "guest",
    ]);
    const role = input("teams.memberRole");
    const base = { teamId: "t", accountId: "a" };
    expect(role.safeParse({ ...base, role: "billing" }).success).toBe(true);
    expect(role.safeParse({ ...base, role: "owner" }).success).toBe(false);
    expect(role.safeParse({ ...base, role: "lead" }).success).toBe(false);
    const invite = input("invitations.create");
    expect(
      invite.safeParse({
        kind: "team",
        teamId: "t",
        email: "a@b.test",
        role: "owner",
      }).success,
    ).toBe(false);
    expect(
      invite.safeParse({
        kind: "host",
        sourceId: "0123456789abcdef0123456789abcdef",
        email: "a@b.test",
        role: "viewer",
      }).success,
    ).toBe(true);
  });

  it("space 维度：sources.list、links.list、billing.*、trust.list 收 personal / team，不收别的", () => {
    for (const key of [
      "sources.list",
      "links.list",
      "billing.plan",
      "billing.invoices",
      "trust.list",
    ]) {
      const schema = input(key);
      const shape = (extra: object) => ({
        ...extra,
        space: { kind: "team", teamId: "t" },
      });
      expect(schema.safeParse(shape({})).success, key).toBe(true);
      expect(
        schema.safeParse({ space: { kind: "personal" } }).success,
        key,
      ).toBe(true);
      expect(
        schema.safeParse({ space: { kind: "org", orgId: "o" } }).success,
        key,
      ).toBe(false);
    }
    expect(input("billing.plan").safeParse({}).success).toBe(false);
    expect(byKey.get("trust.list")?.errorCodes).toContain(
      "team_role_insufficient",
    );
  });

  it("设备授权页：deviceGet / deviceDeny；通知偏好里安全类不可改", () => {
    expect(byKey.get("auth.deviceGet")).toMatchObject({
      method: "GET",
      path: "/v1/auth/device",
    });
    expect(
      input("auth.deviceDeny").safeParse({ userCode: "BCDF-GHJK" }).success,
    ).toBe(true);
    const update = input("me.notificationPrefsUpdate");
    expect(update.safeParse({ hostPresence: false }).success).toBe(true);
    expect(
      byKey
        .get("me.notificationPrefs")
        ?.procedure["~orpc"].outputSchema.safeParse({
          invitations: true,
          accessRequests: true,
          hostPresence: true,
          security: false,
        }).success,
    ).toBe(false);
  });

  it("控制台要的错误码都在注册表里", () => {
    for (const code of [
      "email_token_invalid",
      "email_token_expired",
      "team_slug_taken",
      "team_role_insufficient",
      "team_last_owner",
      "team_owner_required",
      "team_suspended",
      "team_deleted",
      "invitation_expired",
      "invitation_email_mismatch",
      "member_exists",
      "host_registration_disabled",
      "guests_disabled",
      "anonymous_links_disabled",
      "plan_feature_unavailable",
      "transfer_pending",
      "device_untrusted",
      "link_frozen",
    ])
      expect(isErrorCode(code), code).toBe(true);
    expect(errorStatus("team_last_owner")).toBe(409);
    expect(errorStatus("team_deleted")).toBe(410);
    expect(errorStatus("link_frozen")).toBe(403);
  });

  it("授予主体四种；GET 列表的分页与时间按 query 字符串收", () => {
    const create = input("grants.create");
    const source = "0123456789abcdef0123456789abcdef";
    for (const subject of [
      { kind: "team", teamId: "t" },
      { kind: "member", accountId: "a" },
      { kind: "group", groupId: "g" },
      { kind: "guest", accountId: "a" },
    ])
      expect(
        create.safeParse({ sourceId: source, subject, role: "viewer" }).success,
      ).toBe(true);
    expect(
      create.safeParse({
        sourceId: source,
        subject: { kind: "team", teamId: "t" },
        role: "owner",
      }).success,
    ).toBe(false);
    expect(
      input("audit.list").parse({ teamId: "t", fromMs: "1", toMs: "2" }),
    ).toEqual({ teamId: "t", fromMs: 1, toMs: 2, limit: 100 });
    expect(
      input("audit.list").safeParse({ teamId: "t", limit: "501" }).success,
    ).toBe(false);
  });
});

describe("me.stream 事件", () => {
  it("协议 1.4 的团队事件", () => {
    const sourceId = "0123456789abcdef0123456789abcdef";
    const events = [
      { type: "memberChanged", teamId: "t", change: "roleChanged", atMs: 1 },
      {
        type: "invitationChanged",
        invitationId: "i",
        state: "accepted",
        atMs: 1,
      },
      { type: "grantChanged", sourceId, atMs: 1 },
      { type: "accessRequested", requestId: "r", sourceId, atMs: 1 },
      { type: "deviceTrustRequested", requestId: "r", sourceId, atMs: 1 },
      {
        type: "deviceTrustResolved",
        requestId: "r",
        outcome: "trusted",
        atMs: 1,
      },
      {
        type: "planChanged",
        space: { kind: "team", teamId: "t" },
        plan: "team",
        status: "active",
        atMs: 1,
      },
    ];
    for (const event of events)
      expect(meStreamEventSchema.parse(event)).toEqual(event);
    expect(
      meStreamEventSchema.safeParse({ type: "memberChanged", teamId: "t" })
        .success,
    ).toBe(false);
  });

  it("七种事件按 type 区分", () => {
    const sourceId = "0123456789abcdef0123456789abcdef";
    const events = [
      { type: "sourceOnline", sourceId, sinceMs: 1, relayOrigin: "https://r" },
      { type: "sourceOffline", sourceId, atMs: 2 },
      { type: "sourceRevoked", sourceId },
      { type: "accessRevoked", sourceId },
      { type: "linkUsed", linkId: "l", sourceId, by: { guest: true }, atMs: 3 },
      { type: "ping", ts: 4 },
      { type: "pong", ts: 4 },
    ];
    for (const event of events)
      expect(meStreamEventSchema.parse(event)).toEqual(event);
    expect(meStreamEventSchema.safeParse({ type: "unknown" }).success).toBe(
      false,
    );
    expect(meStreamClientMessageSchema.parse({ type: "ping" })).toEqual({
      type: "ping",
    });
  });
});

describe("挑战（§16）与节点回报（§17）", () => {
  const input = (key: string) =>
    procedures.find((entry) => entry.key === key)?.procedure["~orpc"]
      .inputSchema as z.ZodType;
  const output = (key: string) =>
    procedures.find((entry) => entry.key === key)?.procedure["~orpc"]
      .outputSchema as z.ZodType;
  const device = { platform: "desktop", name: "Laptop" };

  it("协议 1.2：挑战与节点回报的错误码登记在注册表里", () => {
    expect(PROTOCOL_VERSION).toEqual({ major: 1, minor: 4 });
    expect(errorStatus("challenge_required")).toBe(400);
    expect(errorStatus("challenge_invalid")).toBe(400);
    expect(errorStatus("storage_unavailable")).toBe(503);
    expect(errorStatus("not_initialized")).toBe(503);
  });

  it("auth.login 与 links.accept 的 challenge 可选，只认 turnstile", () => {
    const login = input("auth.login");
    const base = { account: "owner", password: "pw", device };
    expect(login.safeParse(base).success).toBe(true);
    expect(
      login.safeParse({
        ...base,
        challenge: { provider: "turnstile", token: "t" },
      }).success,
    ).toBe(true);
    expect(
      login.safeParse({ ...base, challenge: { provider: "other", token: "t" } })
        .success,
    ).toBe(false);
    expect(
      login.safeParse({
        ...base,
        challenge: { provider: "turnstile", token: "" },
      }).success,
    ).toBe(false);
    const accept = input("links.accept");
    expect(
      accept.safeParse({
        linkId: "0123456789abcdef",
        challenge: { provider: "turnstile", token: "x".repeat(2048) },
      }).success,
    ).toBe(true);
    expect(
      accept.safeParse({
        linkId: "0123456789abcdef",
        challenge: { provider: "turnstile", token: "x".repeat(2049) },
      }).success,
    ).toBe(false);
  });

  it("auth.changePassword（0.3.10）：会话、旧口令 + 新口令、可选挑战，答当前设备的会话", () => {
    const entry = listProcedures().find(
      (one) => one.key === "auth.changePassword",
    );
    expect(entry).toMatchObject({
      method: "POST",
      path: "/v1/auth/password",
      meta: { scope: ["session"], since: "0.3.10" },
    });
    expect(entry?.errorCodes).toEqual(
      expect.arrayContaining([
        "credentials_invalid",
        "account_locked",
        "password_too_short",
        "challenge_required",
      ]),
    );
    expect(sectionOf("auth.changePassword")).toBe("§18");
    const change = input("auth.changePassword");
    expect(change.safeParse({ password: "a", newPassword: "b" }).success).toBe(
      true,
    );
    expect(change.safeParse({ newPassword: "b" }).success).toBe(false);
    expect(
      change.safeParse({
        password: "a",
        newPassword: "b",
        challenge: { provider: "turnstile", token: "t" },
      }).success,
    ).toBe(true);
    expect(PLATFORM_CAPABILITIES).toContain("auth.password-change");
  });

  it("me.traffic（0.5.0）：会话、query 里的毫秒按数字收、粒度缺省按小时", () => {
    const entry = listProcedures().find((one) => one.key === "me.traffic");
    expect(entry).toMatchObject({
      method: "GET",
      path: "/v1/me/traffic",
      meta: { scope: ["session"], since: "0.5.0" },
    });
    expect(entry?.errorCodes).toEqual(["bad_request"]);
    expect(sectionOf("me.traffic")).toBe("§19");
    const query = input("me.traffic");
    expect(query.parse({ fromMs: "0", toMs: "3600000" })).toEqual({
      fromMs: 0,
      toMs: 3_600_000,
      granularity: "hour",
    });
    expect(
      query.safeParse({ fromMs: "1", toMs: "2", granularity: "week" }).success,
    ).toBe(false);
    expect(query.safeParse({ fromMs: "-1", toMs: "2" }).success).toBe(false);
    expect(
      query.safeParse({ fromMs: "1", toMs: "2", sourceId: "nope" }).success,
    ).toBe(false);
    const bucket = output("me.traffic");
    expect(
      bucket.safeParse({
        buckets: [
          {
            bucketStartMs: 0,
            node: "n",
            sourceId: "s",
            subject: "acct:a",
            deviceId: "",
            kind: "telnet",
            connections: 0,
            bytesToSource: 0,
            bytesToClient: 0,
          },
        ],
      }).success,
    ).toBe(false);
    expect(TRAFFIC_KINDS).toEqual(["e2e", "http", "ws", "media"]);
    expect(PLATFORM_CAPABILITIES).toContain("me.traffic");
  });

  it("platform.info.challenge 可选，scope 至少一条", () => {
    const info = output("platform.info");
    const base = {
      mode: "personal",
      issuer: "https://relay.armadra.test",
      protocol: { major: 1, minor: 2 },
      capabilities: ["challenge.turnstile"],
      relay: { addressing: "path" },
      webApp: null,
      registrationsOpen: true,
    };
    expect(info.safeParse(base).success).toBe(true);
    expect(
      info.safeParse({
        ...base,
        challenge: {
          provider: "turnstile",
          siteKey: "k",
          scope: ["auth.login"],
        },
      }).success,
    ).toBe(true);
    expect(
      info.safeParse({
        ...base,
        challenge: { provider: "turnstile", siteKey: "k", scope: [] },
      }).success,
    ).toBe(false);
  });

  it("节点地址可带 query；节点回报的事件 1–256 条、ttlMs 10 秒到 10 分钟", () => {
    const hint = output("sources.heartbeatHint");
    const limits = {
      maxStreams: 256,
      streamWindow: 262_144,
      tunnelWindow: 8_388_608,
      maxFrameBytes: 1_048_576,
    };
    expect(
      hint.safeParse({
        tunnelToken: "t",
        expiresAtMs: 1,
        nodes: [
          {
            url: "wss://relay.armadra.test/t/v1?src=0123456789abcdef0123456789abcdef",
            weight: 1,
          },
        ],
        limits,
      }).success,
    ).toBe(true);
    const presence = input("relay.nodePresence");
    const event = {
      type: "sourceOffline",
      sourceId: "0123456789abcdef0123456789abcdef",
      tunnelId: "fedcba9876543210fedcba9876543210",
      atMs: 1,
    };
    expect(
      presence.safeParse({ node: "cf", ttlMs: 120_000, events: [event] })
        .success,
    ).toBe(true);
    expect(
      presence.safeParse({ node: "cf", ttlMs: 120_000, events: [] }).success,
    ).toBe(false);
    expect(
      presence.safeParse({ node: "cf", ttlMs: 5_000, events: [event] }).success,
    ).toBe(false);
    expect(
      presence.safeParse({ node: "CF", ttlMs: 120_000, events: [event] })
        .success,
    ).toBe(false);
  });
});

describe("协议 1.4 补充：控制台缺口（G1–G9）", () => {
  const byKey = new Map(procedures.map((entry) => [entry.key, entry]));
  const input = (key: string) =>
    byKey.get(key)?.procedure["~orpc"].inputSchema as z.ZodType;
  const output = (key: string) =>
    byKey.get(key)?.procedure["~orpc"].outputSchema as z.ZodType;

  it("G1 auth.stepUp 至少给一项凭据", () => {
    expect(byKey.get("auth.stepUp")).toMatchObject({
      method: "POST",
      path: "/v1/auth/step-up",
    });
    expect(input("auth.stepUp").safeParse({}).success).toBe(false);
    expect(input("auth.stepUp").safeParse({ totpCode: "123456" }).success).toBe(
      true,
    );
    expect(
      output("auth.stepUp").safeParse({ elevatedUntilMs: 1 }).success,
    ).toBe(true);
  });

  it("G2 auth.login 的 scope 只能是 operator，错误码已登记", () => {
    const login = input("auth.login");
    const base = {
      account: "ops@armadra.test",
      password: "x",
      device: { platform: "desktop", name: "Browser" },
    };
    expect(login.safeParse({ ...base, scope: "operator" }).success).toBe(true);
    expect(login.safeParse({ ...base, scope: "root" }).success).toBe(false);
    expect(errorStatus("operator_mfa_required")).toBe(403);
  });

  it("G5 sources.list 的 limit 在 1 到 200", () => {
    const list = input("sources.list");
    expect(list.safeParse({ limit: 200, status: "revoked" }).success).toBe(
      true,
    );
    expect(list.safeParse({ limit: 201 }).success).toBe(false);
    expect(list.safeParse({ limit: 0 }).success).toBe(false);
    expect(list.safeParse({ status: "gone" }).success).toBe(false);
  });

  it("G6 billing.usage 的 granularity", () => {
    const usage = input("billing.usage");
    const space = { space: { kind: "personal" } };
    expect(usage.safeParse({ ...space, granularity: "day" }).success).toBe(
      true,
    );
    expect(usage.safeParse({ ...space, granularity: "hour" }).success).toBe(
      false,
    );
  });

  it("G8 trustRequest 不再带 sas", () => {
    const sample = readJsonFixture("cloud-api/trust.list.json") as {
      output: { requests: Record<string, unknown>[] };
    };
    for (const request of sample.output.requests)
      expect(request).not.toHaveProperty("sas");
  });

  it("trustRequest 带可选的 pendingId（0.11.0 起，只加不改）", () => {
    const sample = readJsonFixture("cloud-api/trust.list.json") as {
      output: { requests: Record<string, unknown>[] };
    };
    const schema = cloudContract.trust.list["~orpc"].outputSchema as z.ZodType;
    const request = sample.output.requests[0]!;
    expect(request.pendingId).toBe("pend_01");
    expect(schema.safeParse({ requests: [request] }).success).toBe(true);
    const { pendingId: _omitted, ...legacy } = request;
    expect(schema.safeParse({ requests: [legacy] }).success).toBe(true);
    expect(
      schema.safeParse({ requests: [{ ...request, pendingId: 1 }] }).success,
    ).toBe(false);
  });

  it("G9 registrations_paused 是 503，platform.info.registrationsOpen 可缺", () => {
    expect(errorStatus("registrations_paused")).toBe(503);
    expect(byKey.get("auth.register")).toBeDefined();
    const info = output("platform.info");
    expect(
      info.safeParse({
        mode: "saas",
        issuer: "https://cloud.armadra.test",
        protocol: { major: 1, minor: 4 },
        capabilities: [],
        relay: { addressing: "subdomain" },
        webApp: null,
      }).success,
    ).toBe(true);
  });
});

describe("platform.info 的挑战范围（cloud-api §16）", () => {
  it("客户端解析忽略不认识的 scope 项，认识的保留", async () => {
    const { platformChallengeSchema } = await import(
      "../src/cloud-api/index.ts"
    );
    const parsed = platformChallengeSchema.parse({
      provider: "turnstile",
      siteKey: "k",
      scope: ["auth.login", "auth.future", "auth.register"],
    });
    expect(parsed.scope).toEqual(["auth.login", "auth.register"]);
  });

  it("scope 过滤后为空时整个 challenge 视为没有，platform.info 照常解析", async () => {
    const { platformInfoSchema } = await import("../src/cloud-api/index.ts");
    const base = {
      mode: "saas",
      issuer: "https://app.armadra.test",
      protocol: { major: 1, minor: 4 },
      capabilities: ["auth.password"],
      relay: { addressing: "subdomain" },
      webApp: "https://app.armadra.test",
    };
    const parsed = platformInfoSchema.parse({
      ...base,
      challenge: { provider: "turnstile", siteKey: "k", scope: ["x.future"] },
    });
    expect(parsed.challenge).toBeUndefined();
    const kept = platformInfoSchema.parse({
      ...base,
      challenge: {
        provider: "turnstile",
        siteKey: "k",
        scope: ["x.future", "auth.login"],
      },
    });
    expect(kept.challenge?.scope).toEqual(["auth.login"]);
  });

  it("0.11.0 只加不改：drain 的 reason、deviceGet 的 ipPrefix、links.get 的 ownerName、team_hosts_online", () => {
    const drain = input("admin.relayNodeDrain");
    expect(drain.safeParse({ node: "relay-1", drain: true }).success).toBe(true);
    expect(
      drain.safeParse({ node: "relay-1", drain: true, reason: "维护" }).success,
    ).toBe(true);
    expect(
      drain.safeParse({ node: "relay-1", drain: true, reason: "" }).success,
    ).toBe(false);
    expect(
      drain.safeParse({ node: "relay-1", drain: true, reason: "x".repeat(501) })
        .success,
    ).toBe(false);
    const out = (key: string) =>
      byKey.get(key)?.procedure["~orpc"].outputSchema;
    const device = { platform: "server", name: "n", requestedAtMs: 1, expiresAtMs: 2 };
    expect(out("auth.deviceGet").safeParse(device).success).toBe(true);
    expect(
      out("auth.deviceGet").safeParse({ ...device, ipPrefix: "2001:db8::/64" })
        .success,
    ).toBe(true);
    const link = {
      kind: "source_invite",
      label: "l",
      expiresAtMs: 1,
      exhausted: false,
      requiresAccount: false,
    };
    expect(out("links.get").safeParse(link).success).toBe(true);
    expect(out("links.get").safeParse({ ...link, ownerName: "Alice" }).success).toBe(
      true,
    );
    expect(byKey.get("teams.delete")?.errorCodes).toContain("team_hosts_online");
  });
});
