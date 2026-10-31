/**
 * 改口令（cloud-api §18）在 workerd 上：`ControlDO` 的 DO SQLite 端口 + 撤销通知。其它设备的会话与
 * 刷新令牌即时失效，当前设备留着并换新刷新令牌；用例结束把口令改回去（其它用例用同一个账号）。
 */
import { describe, expect, it } from "vitest";

import {
  ACCOUNT,
  call,
  DEVICE,
  json,
  login,
  nextIp,
  PASSWORD,
  postJson,
  scryptTimeout,
  warmUp,
} from "./api.ts";

const NEXT = "saffron-glacier-meadow-77";

const me = (accessToken: string) =>
  call("/v1/me", { headers: { authorization: `Bearer ${accessToken}` } });

interface Answer {
  session: {
    accessToken: string;
    refreshToken: string;
    device: { deviceId: string };
  };
}

// 每条用例的上限按其中 scrypt 次数给（`scryptTimeout`，成本构成见 api.ts）：登录 1 次，改口令 2 次
// （校验旧口令 + 派生新口令），旧口令不对的改口令 1 次，策略不过的在派生之前就拒了。
describe("改口令", () => {
  // 第一个请求要唤醒控制面的对象（冷启动，含种子账号的 1 次 scrypt），所以单独预热并限时。
  it(
    "platform.info 报 auth.password-change",
    { timeout: scryptTimeout(1) },
    async () => {
      await warmUp();
      const info = await json<{ capabilities: string[] }>(
        await call("/.well-known/armadra-platform"),
      );
      expect(info.capabilities).toContain("auth.password-change");
    },
  );

  // 共 3 次 scrypt：登录、旧口令不对的改口令、再登录。
  it(
    "旧口令不对 401 credentials_invalid；策略不过 400；都不改口令",
    { timeout: scryptTimeout(3) },
    async () => {
      const session = await login();
      const ip = nextIp();
      const wrong = await postJson(
        "/v1/auth/password",
        { password: "wrong-old-password", newPassword: NEXT },
        { token: session.accessToken, ip, derives: 1 },
      );
      expect(wrong.status).toBe(401);
      expect((await json(wrong)).code).toBe("credentials_invalid");
      const weak = await postJson(
        "/v1/auth/password",
        { password: PASSWORD, newPassword: "short" },
        { token: session.accessToken, ip },
      );
      expect(weak.status).toBe(400);
      expect((await json(weak)).code).toBe("password_too_short");
      expect((await login()).accessToken).toBeTruthy();
    },
  );

  // 共 9 次 scrypt：登录 2、改口令 2、旧口令登录 1、新口令登录 1、改回去 2、再登录 1。
  it(
    "成功：其它设备失效、当前设备保留并换新刷新令牌；旧口令登不上，再改回去",
    { timeout: scryptTimeout(9) },
    async () => {
      const current = await login();
      const other = await login();
      const changed = await postJson(
        "/v1/auth/password",
        { password: PASSWORD, newPassword: NEXT },
        { token: current.accessToken, ip: nextIp(), derives: 2 },
      );
      expect(changed.status).toBe(200);
      const { session } = await json<Answer>(changed);
      expect(session.refreshToken).not.toBe(current.refreshToken);

      expect((await me(current.accessToken)).status).toBe(200);
      expect((await me(other.accessToken)).status).toBe(401);
      const otherRefresh = await postJson("/v1/auth/refresh", {
        refreshToken: other.refreshToken,
      });
      expect((await json(otherRefresh)).code).toBe("session_revoked");
      const staleRefresh = await postJson("/v1/auth/refresh", {
        refreshToken: current.refreshToken,
      });
      expect((await json(staleRefresh)).code).toBe("session_revoked");
      const renewed = await postJson("/v1/auth/refresh", {
        refreshToken: session.refreshToken,
      });
      expect(renewed.status).toBe(200);

      const oldLogin = await postJson(
        "/v1/auth/login",
        { account: ACCOUNT, password: PASSWORD, device: DEVICE },
        { ip: nextIp(), derives: 1 },
      );
      expect((await json(oldLogin)).code).toBe("credentials_invalid");
      const newLogin = await postJson(
        "/v1/auth/login",
        { account: ACCOUNT, password: NEXT, device: DEVICE },
        { ip: nextIp(), derives: 1 },
      );
      expect(newLogin.status).toBe(200);
      const { session: fresh } = await json<Answer>(newLogin);

      const back = await postJson(
        "/v1/auth/password",
        { password: NEXT, newPassword: PASSWORD },
        { token: fresh.accessToken, ip: nextIp(), derives: 2 },
      );
      expect(back.status).toBe(200);
      expect((await login()).accessToken).toBeTruthy();
    },
  );

  it("没带会话：401", async () => {
    const response = await postJson("/v1/auth/password", {
      password: PASSWORD,
      newPassword: NEXT,
    });
    expect(response.status).toBe(401);
  });
});
