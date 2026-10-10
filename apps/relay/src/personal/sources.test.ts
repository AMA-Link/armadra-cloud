import { createAccounts } from "@armadra/control-core/accounts";
import { ControlError } from "@armadra/control-core/errors";
import { createLinks } from "@armadra/control-core/links";
import {
  createAuthenticator,
  type Caller,
} from "@armadra/control-core/sessions";
import { createSources } from "@armadra/control-core/sources";
import {
  assertionClaimsSchema,
  relayTokenClaimsSchema,
  splitCompactJws,
  tunnelTokenClaimsSchema,
} from "@armadra/platform-protocol/assertion";
import { createAuthContext } from "@armadra/relay-kernel/auth/context";
import { verifyRelayToken } from "@armadra/relay-kernel/auth/relay-token";
import { verifyTunnelToken } from "@armadra/relay-kernel/auth/tunnel-token";
import { afterAll, beforeAll, describe, expect, it } from "vitest";

import {
  DEVICE,
  ISSUER,
  PASSWORD,
  personalHarness,
  registerInput,
  SOURCE_ID,
  SOURCE_PUBLIC_JWK,
  sourceJws,
  TEST_STORE_KINDS,
  type Harness,
} from "../../test/personal.ts";

describe.each(TEST_STORE_KINDS)("%s 存储", (store) => {
  let h: Harness;
  let owner: Caller;
  beforeAll(async () => {
    h = await personalHarness({ store });
    const session = await createAccounts(h.context).login(
      { account: "me", password: PASSWORD, device: DEVICE },
      "198.51.100.1",
    );
    owner = await createAuthenticator(h.context).authenticate(
      `Bearer ${session.accessToken}`,
    );
  });
  afterAll(() => h.cleanup());

  const codeOf = async (run: () => unknown) => {
    try {
      await run();
    } catch (error) {
      return error instanceof ControlError ? error.code : String(error);
    }
    return "ok";
  };

  describe("源注册", () => {
    it("注册令牌一次性、10 分钟；过期或用过答 registration_token_invalid；只存哈希", async () => {
      const sources = createSources(h.context);
      const { registrationToken, expiresAtMs, issuer } =
        await sources.registrationToken(owner);
      expect(issuer).toBe(ISSUER);
      expect(expiresAtMs - h.clock.now).toBe(10 * 60_000);
      expect(h.persisted()).not.toContain(registrationToken);
      const result = await sources.register(
        registerInput(registrationToken),
        "ip",
      );
      expect(result).toEqual({
        issuer: ISSUER,
        jwksUrl: `${ISSUER}/.well-known/jwks.json`,
        relayOrigins: [ISSUER],
        trustedOrigins: [ISSUER],
        sourceId: SOURCE_ID,
        ownerAccountId: "acct:me",
      });
      expect(
        await codeOf(() =>
          sources.register(registerInput(registrationToken), "ip"),
        ),
      ).toBe("registration_token_invalid");
      const late = (await sources.registrationToken(owner)).registrationToken;
      h.clock.now += 10 * 60_000 + 1;
      expect(
        await codeOf(() => sources.register(registerInput(late), "ip")),
      ).toBe("registration_token_invalid");
    });

    it("同一 sourceId 换了公钥：source_taken；同钥重登记更新名字；major 不符 protocol_unsupported", async () => {
      const sources = createSources(h.context);
      const other = { ...SOURCE_PUBLIC_JWK, x: "A".repeat(43) };
      expect(
        await codeOf(async () =>
          sources.register(
            {
              ...registerInput(
                (await sources.registrationToken(owner)).registrationToken,
              ),
              publicKey: other,
            },
            "ip",
          ),
        ),
      ).toBe("source_taken");
      expect(
        await codeOf(async () =>
          sources.register(
            {
              ...registerInput(
                (await sources.registrationToken(owner)).registrationToken,
              ),
              protocol: { major: 2, minor: 0 },
            },
            "ip",
          ),
        ),
      ).toBe("protocol_unsupported");
      await sources.register(
        {
          ...registerInput(
            (await sources.registrationToken(owner)).registrationToken,
          ),
          name: "改名",
        },
        "ip",
      );
      expect((await sources.list(owner)).map((row) => row.name)).toEqual([
        "改名",
      ]);
    });

    it("访客不能要注册令牌", async () => {
      const sources = createSources(h.context);
      expect(
        await codeOf(() =>
          sources.registrationToken({
            ...owner,
            role: "guest",
            linkId: "0".repeat(16),
          }),
        ),
      ).toBe("forbidden");
    });
  });

  describe("源 JWS 换隧道令牌", () => {
    it("签名、aud、时间、重放都核；隧道令牌的 cnf 是登记的公钥，中继内核验得过", async () => {
      const sources = createSources(h.context);
      const source = await sources.authenticateSource(
        `Source ${sourceJws(ISSUER, h.clock.now)}`,
      );
      const relay = sources.relayFor(source);
      expect(relay.nodes).toEqual([
        { url: "wss://relay.test:8443/t/v1", weight: 1 },
      ]);
      const claims = tunnelTokenClaimsSchema.parse(
        splitCompactJws(relay.tunnelToken).payload,
      );
      expect(claims.cnf.jwk.x).toBe(SOURCE_PUBLIC_JWK.x);
      expect(claims.jti.startsWith(`t.${SOURCE_ID}.`)).toBe(true);
      const auth = createAuthContext(h.context.control, () => h.clock.now);
      expect(
        (await verifyTunnelToken(auth, relay.tunnelToken, SOURCE_ID)).ok,
      ).toBe(true);

      const replayed = sourceJws(ISSUER, h.clock.now);
      await sources.authenticateSource(`Source ${replayed}`);
      for (const header of [
        `Source ${replayed}`,
        `Source ${sourceJws("https://other.example", h.clock.now)}`,
        `Source ${sourceJws(ISSUER, h.clock.now - 10 * 60_000)}`,
        `Source ${sourceJws(ISSUER, h.clock.now, {}, { sourceId: "f".repeat(32) })}`,
        `Bearer ${sourceJws(ISSUER, h.clock.now)}`,
        undefined,
      ])
        expect(await codeOf(() => sources.authenticateSource(header))).toBe(
          "unauthenticated",
        );
    });
  });

  describe("断言与中继令牌", () => {
    it("owner 拿到断言（aud = sourceId）+ 中继令牌（src、jti 带设备），relayBaseUrl 是 /s/<id>", async () => {
      const sources = createSources(h.context);
      const out = await sources.assertion(owner, SOURCE_ID, undefined);
      expect(out.relayBaseUrl).toBe(`${ISSUER}/s/${SOURCE_ID}`);
      expect(out.online).toBe(false);
      const assertion = assertionClaimsSchema.parse(
        splitCompactJws(out.assertion).payload,
      );
      expect(assertion).toMatchObject({
        iss: ISSUER,
        sub: "acct:me",
        aud: SOURCE_ID,
        name: "me",
        device: { deviceId: owner.deviceId, platform: "desktop" },
      });
      expect(assertion.exp - assertion.iat).toBe(300);
      const relay = relayTokenClaimsSchema.parse(
        splitCompactJws(out.relayToken).payload,
      );
      expect(relay).toMatchObject({
        sub: "acct:me",
        aud: "relay",
        src: SOURCE_ID,
      });
      const auth = createAuthContext(h.context.control, () => h.clock.now);
      expect((await verifyRelayToken(auth, out.relayToken, SOURCE_ID)).ok).toBe(
        true,
      );
      expect(
        await codeOf(() => sources.assertion(owner, "e".repeat(32), undefined)),
      ).toBe("source_access_denied");
    });

    it("轮换签名钥：新令牌用新 kid，旧令牌在旧钥退役（1 小时）前仍验得过", async () => {
      const sources = createSources(h.context);
      const before = await sources.assertion(owner, SOURCE_ID, undefined);
      await h.context.store.rotateKey(h.clock.now);
      const after = await sources.assertion(owner, SOURCE_ID, undefined);
      expect(splitCompactJws(after.relayToken).header.kid).toBe("k2");
      expect(h.context.signer.jwks().keys.map((key) => key.kid)).toEqual([
        "k1",
        "k2",
      ]);
      const auth = createAuthContext(h.context.control, () => h.clock.now);
      expect(
        (await verifyRelayToken(auth, before.relayToken, SOURCE_ID)).ok,
      ).toBe(true);
      h.clock.now += 60 * 60_000 + 1;
      expect(h.context.signer.jwks().keys.map((key) => key.kid)).toEqual([
        "k2",
      ]);
    });
  });

  describe("分享链接与访客", () => {
    it("建链接 → 访客 accept（秘密比对、次数、过期）→ 访客只看到这个源 → 撤销后令牌全部失效", async () => {
      const sources = createSources(h.context);
      const links = createLinks(h.context, sources);
      const auth = createAuthenticator(h.context);
      const created = await links.create(owner, {
        kind: "source_invite",
        sourceId: SOURCE_ID,
        invitationId: "inv_1",
        label: "给同事",
        role: "editor",
        expiresAtMs: h.clock.now + 60 * 60_000,
        maxUses: 2,
      });
      expect(created.url).toBe(`${ISSUER}/j/${created.linkId}`);
      expect(created.url).not.toContain("#");
      expect(h.persisted()).not.toContain(created.secret);
      expect(await links.get(created.linkId)).toMatchObject({
        kind: "source_invite",
        sourceName: "改名",
        exhausted: false,
        requiresAccount: false,
      });
      const accept = (secret?: string) =>
        links.accept(
          { linkId: created.linkId, ...(secret ? { secret } : {}) },
          "203.0.113.9",
        );
      expect(await codeOf(() => accept())).toBe("link_secret_invalid");
      expect(await codeOf(() => accept("wrong"))).toBe("link_secret_invalid");
      const guest = await accept(created.secret);
      expect(guest.guestSession.account.accountId).toMatch(
        new RegExp(`^guest:${created.linkId}:[0-9a-f]+$`),
      );
      const claims = assertionClaimsSchema.parse(
        splitCompactJws(guest.assertion as string).payload,
      );
      expect(claims.link).toEqual({
        linkId: created.linkId,
        invitationId: "inv_1",
      });
      expect(h.events.at(-1)).toMatchObject({
        type: "linkUsed",
        linkId: created.linkId,
      });

      const caller = await auth.authenticate(
        `Bearer ${guest.guestSession.accessToken}`,
      );
      expect(caller.role).toBe("guest");
      expect((await sources.list(caller)).map((row) => row.via)).toEqual([
        "link",
      ]);
      expect(
        (await sources.assertion(caller, SOURCE_ID, undefined)).relayToken,
      ).toBeTruthy();
      expect(await codeOf(() => links.list(caller))).toBe("forbidden");

      await accept(created.secret);
      expect(await codeOf(() => accept(created.secret))).toBe("link_exhausted");

      await links.revoke(owner, created.linkId);
      expect(h.disconnects.at(-1)).toEqual({
        subjectPrefix: `guest:${created.linkId}:`,
      });
      expect(
        await codeOf(() =>
          auth.authenticate(`Bearer ${guest.guestSession.accessToken}`),
        ),
      ).toBe("unauthenticated");
      const relayAuth = createAuthContext(h.context.control, () => h.clock.now);
      expect(
        (
          await verifyRelayToken(
            relayAuth,
            guest.relayToken as string,
            SOURCE_ID,
          )
        ).ok,
      ).toBe(false);
      expect(await codeOf(() => links.get(created.linkId))).toBe(
        "link_invalid",
      );
    });

    it("过期的链接答 link_expired；有效期超过 30 天或已过去答 bad_request", async () => {
      const sources = createSources(h.context);
      const links = createLinks(h.context, sources);
      const short = await links.create(owner, {
        kind: "source_invite",
        sourceId: SOURCE_ID,
        invitationId: "inv_2",
        label: "",
        role: "viewer",
        expiresAtMs: h.clock.now + 1000,
      });
      h.clock.now += 1001;
      expect(
        await codeOf(() =>
          links.accept(
            { linkId: short.linkId, secret: short.secret },
            "203.0.113.10",
          ),
        ),
      ).toBe("link_expired");
      for (const expiresAtMs of [
        h.clock.now - 1,
        h.clock.now + 31 * 24 * 3600_000,
      ])
        expect(
          await codeOf(() =>
            links.create(owner, {
              kind: "source_invite",
              sourceId: SOURCE_ID,
              invitationId: "inv_3",
              label: "",
              role: "viewer",
              expiresAtMs,
            }),
          ),
        ).toBe("bad_request");
    });
  });

  describe("改链接备注", () => {
    it("只改 label（去首尾空白），秘密、次数与期限不动；撤销后仍可改；访客 forbidden，没有答 not_found", async () => {
      const sources = createSources(h.context);
      const links = createLinks(h.context, sources);
      const created = await links.create(owner, {
        kind: "source_invite",
        sourceId: SOURCE_ID,
        invitationId: "inv_note",
        label: "旧备注",
        role: "viewer",
        expiresAtMs: h.clock.now + 60 * 60_000,
        maxUses: 3,
      });
      const before = (await links.list(owner)).find(
        (row) => row.linkId === created.linkId,
      );
      const updated = await links.update(owner, {
        linkId: created.linkId,
        label: "  新备注 ",
      });
      expect(updated).toEqual({ ...before, label: "新备注" });
      expect((await links.get(created.linkId)).label).toBe("新备注");
      const guest = await links.accept(
        { linkId: created.linkId, secret: created.secret },
        "203.0.113.20",
      );
      const caller = await createAuthenticator(h.context).authenticate(
        `Bearer ${guest.guestSession.accessToken}`,
      );
      expect(
        await codeOf(() =>
          links.update(caller, { linkId: created.linkId, label: "x" }),
        ),
      ).toBe("forbidden");
      expect(
        await codeOf(() => links.update(owner, { linkId: "nope", label: "x" })),
      ).toBe("not_found");
      await links.revoke(owner, created.linkId);
      expect(
        await links.update(owner, { linkId: created.linkId, label: "" }),
      ).toMatchObject({ label: "", revokedAtMs: h.clock.now });
    });
  });

  describe("撤销源", () => {
    it("关隧道、断开客户端 WS、通知 me/stream；隧道令牌与中继令牌立即失效；之后 source_revoked", async () => {
      const sources = createSources(h.context);
      const commands: unknown[] = [];
      h.context.control.nodeCommands.subscribe((command) =>
        commands.push(command),
      );
      const tunnel = sources.relayFor(
        await sources.authenticateSource(
          `Source ${sourceJws(ISSUER, h.clock.now)}`,
        ),
      );
      const granted = await sources.assertion(owner, SOURCE_ID, undefined);
      await sources.revoke(owner, SOURCE_ID);
      expect(commands).toEqual([{ type: "closeTunnel", sourceId: SOURCE_ID }]);
      expect(h.disconnects.at(-1)).toEqual({ sourceId: SOURCE_ID });
      expect(h.events.at(-1)).toEqual({
        type: "sourceRevoked",
        sourceId: SOURCE_ID,
      });
      const auth = createAuthContext(h.context.control, () => h.clock.now);
      expect(
        (await verifyTunnelToken(auth, tunnel.tunnelToken, SOURCE_ID)).ok,
      ).toBe(false);
      expect(
        (await verifyRelayToken(auth, granted.relayToken, SOURCE_ID)).ok,
      ).toBe(false);
      expect(
        await codeOf(() => sources.assertion(owner, SOURCE_ID, undefined)),
      ).toBe("source_revoked");
      expect(
        await codeOf(() =>
          sources.authenticateSource(
            `Source ${sourceJws(ISSUER, h.clock.now)}`,
          ),
        ),
      ).toBe("source_revoked");
      expect(await sources.list(owner)).toEqual([]);
    });
  });
});
