/**
 * 日配额（单独一个文件：会把当天的计数推满）。超了：源地址的请求与新隧道答 429 limit_reached，
 * `/v1` 与 `/health` 照常；计数清掉后恢复。
 */
import {
  env,
  runDurableObjectAlarm,
  runInDurableObject,
} from "cloudflare:test";
import { expect, it } from "vitest";

import { dayOf } from "../src/budget.ts";
import type { ControlDO } from "../src/control-do.ts";
import { CONTROL_OBJECT_NAME, sourceObjectName } from "../src/env.ts";
import {
  assertion,
  call,
  heartbeatHint,
  json,
  login,
  registerSource,
  viaRelay,
} from "./api.ts";
import { FakeCore } from "./fake-core.ts";

it("超了日配额：源地址 429 limit_reached；/v1 照常；清掉计数后恢复", async () => {
  const session = await login();
  const source = await registerSource(session);
  const hint = await heartbeatHint(source);
  const core = new FakeCore(source);
  expect(
    (await core.connect(hint.nodes[0]?.url ?? "", hint.tunnelToken)).ready,
  ).toBeTruthy();
  const { relayToken } = await assertion(session, source.sourceId);
  const ok = await viaRelay(source.sourceId, "/echo-request", {
    token: relayToken,
  });
  expect(ok.status).toBe(200);
  await ok.arrayBuffer();

  const control = env.CONTROL.get(env.CONTROL.idFromName(CONTROL_OBJECT_NAME));
  const sourceStub = env.SOURCE.get(
    env.SOURCE.idFromName(sourceObjectName(source.sourceId)),
  );
  await runInDurableObject(control, (instance: ControlDO) =>
    instance.budgetAdd({ requests: 2_000_000, bytes: 0 }),
  );
  // 源的对象攒着的计数在检查周期里报上去，顺便拿回「超了」。
  expect(await runDurableObjectAlarm(sourceStub)).toBe(true);
  const refused = await viaRelay(source.sourceId, "/echo-request", {
    token: relayToken,
  });
  expect(refused.status).toBe(429);
  expect((await json(refused)).code).toBe("limit_reached");
  const tunnel = await call(`/t/v1?src=${source.sourceId}`, {
    headers: { upgrade: "websocket" },
  });
  expect(tunnel.status).toBe(429);
  await tunnel.arrayBuffer();
  const api = await call("/.well-known/armadra-platform");
  expect(api.status).toBe(200);
  await api.arrayBuffer();
  const metrics = await call("/metrics", {
    headers: { authorization: "Bearer test-metrics-token" },
  });
  expect(await metrics.text()).toMatch(
    /relay_budget_used\{kind="requests"\} 2000\d{3}/,
  );

  await runInDurableObject(control, (_instance, state) => {
    state.storage.kv.delete(`budget:${dayOf(Date.now())}`);
  });
  await runDurableObjectAlarm(sourceStub);
  const again = await viaRelay(source.sourceId, "/echo-request", {
    token: relayToken,
  });
  expect(again.status).toBe(200);
  await again.arrayBuffer();
});
