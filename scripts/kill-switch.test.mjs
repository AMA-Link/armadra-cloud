// node --test scripts/kill-switch.test.mjs
//
// 拉闸脚本：缺省演练（不读令牌、不连网络，子进程里把 fetch 换成抛错也照样成功）、参数校验、
// --apply 时对假 API 发的请求与幂等（已经是目标就不再切）。
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import test from "node:test";
import assert from "node:assert/strict";

import {
  DEFAULT_ACCOUNT,
  MAINTENANCE_SERVICE,
  parseArgs,
  planKillSwitch,
  runKillSwitch,
  UsageError,
} from "./kill-switch.mjs";

const SCRIPT = fileURLToPath(new URL("./kill-switch.mjs", import.meta.url));

function run(args, env = {}) {
  return spawnSync(
    process.execPath,
    // 子进程里 fetch 一调就抛：演练要是发了请求就会失败。
    [
      "--import",
      "data:text/javascript,globalThis.fetch=()=>{throw new Error('network used')}",
      SCRIPT,
      ...args,
    ],
    {
      encoding: "utf8",
      env: {
        PATH: process.env.PATH,
        CLOUDFLARE_API_TOKEN: "tok-secret-value",
        ...env,
      },
    },
  );
}

test("缺省演练：打印要发的请求，不读令牌、不连网络，退出码 0", () => {
  for (const action of ["status", "maintenance", "disable", "restore"]) {
    const result = run([action]);
    assert.equal(result.status, 0, result.stderr);
    assert.match(result.stdout, /演练（没加 --apply/);
    assert.match(result.stdout, /relay\.armadra\.com/);
    assert.doesNotMatch(result.stdout + result.stderr, /tok-secret-value/);
  }
  const maintenance = run(["maintenance", "--env", "staging"]).stdout;
  assert.match(
    maintenance,
    /PUT https:\/\/api\.cloudflare\.com\/client\/v4\/accounts\/a27d9dab9190150768845b62cfd57cfe\/workers\/domains/,
  );
  assert.match(
    maintenance,
    /"hostname":"relay-staging\.armadra\.com","service":"armadra-relay-maintenance"/,
  );
});

test("用法错误以 64 退出；--help 以 0 退出", () => {
  assert.equal(run([]).status, 64);
  assert.equal(run(["explode"]).status, 64);
  assert.equal(run(["restore", "--env", "dev"]).status, 64);
  assert.equal(run(["restore", "--token", "x"]).status, 64);
  const help = run(["--help"]);
  assert.equal(help.status, 0);
  assert.match(help.stdout, /用法: node scripts\/kill-switch\.mjs/);
});

test("--apply 没有令牌：64，什么也不发", () => {
  const result = run(["maintenance", "--apply"], { CLOUDFLARE_API_TOKEN: "" });
  assert.equal(result.status, 64);
  assert.match(result.stdout, /缺少 CLOUDFLARE_API_TOKEN/);
});

test("参数与计划", () => {
  assert.throws(() => parseArgs(["status", "--account", "nope"]), UsageError);
  assert.equal(parseArgs(["status"]).account, DEFAULT_ACCOUNT);
  assert.equal(
    parseArgs(["status"], { CLOUDFLARE_ACCOUNT_ID: "0".repeat(32) }).account,
    "0".repeat(32),
  );
  const plan = planKillSwitch(parseArgs(["restore"]));
  assert.deepEqual(
    plan.steps.map((step) => step.method),
    ["GET", "PUT"],
  );
  assert.deepEqual(plan.steps[1].body, {
    hostname: "relay.armadra.com",
    service: "armadra-relay",
    zone_name: "armadra.com",
    enabled: true,
  });
  assert.equal(
    planKillSwitch(parseArgs(["restore", "--env", "staging"])).steps[1].body
      .service,
    "armadra-relay-staging",
  );
});

function fakeApi(domains) {
  const calls = [];
  const fetcher = async (url, init) => {
    calls.push({
      url,
      method: init.method,
      authorization: init.headers.authorization,
      body: init.body ? JSON.parse(init.body) : undefined,
    });
    const ok = (result) =>
      new Response(JSON.stringify({ success: true, errors: [], result }), {
        status: 200,
      });
    if (init.method === "GET") return ok(domains);
    if (init.method === "PUT") return ok({ ...init.body, id: "d1" });
    if (init.method === "DELETE") return ok(null);
    return new Response("{}", { status: 405 });
  };
  return { calls, fetcher };
}

test("--apply：maintenance 先查再切，带 Bearer；已经是目标就不再切", async () => {
  const lines = [];
  const api = fakeApi([
    { id: "d1", hostname: "relay.armadra.com", service: "armadra-relay" },
  ]);
  const code = await runKillSwitch(parseArgs(["maintenance", "--apply"]), {
    fetch: api.fetcher,
    token: "tok",
    print: (line) => lines.push(line),
  });
  assert.equal(code, 0);
  assert.deepEqual(
    api.calls.map((call) => call.method),
    ["GET", "PUT"],
  );
  assert.equal(api.calls[0].authorization, "Bearer tok");
  assert.match(
    api.calls[0].url,
    /workers\/domains\?hostname=relay\.armadra\.com$/,
  );
  assert.equal(api.calls[1].body.service, MAINTENANCE_SERVICE);
  assert.ok(
    lines.some((line) =>
      line.includes("现在：relay.armadra.com → armadra-relay"),
    ),
  );
  assert.ok(!lines.join("\n").includes("tok"));

  const again = fakeApi([
    { id: "d1", hostname: "relay.armadra.com", service: MAINTENANCE_SERVICE },
  ]);
  await runKillSwitch(parseArgs(["maintenance", "--apply"]), {
    fetch: again.fetcher,
    token: "tok",
    print: () => {},
  });
  assert.deepEqual(
    again.calls.map((call) => call.method),
    ["GET"],
  );
});

test("--apply：disable 按查到的 id 删；没有就不删；API 报错时抛出带错误码的信息", async () => {
  const api = fakeApi([
    { id: "dom-9", hostname: "relay.armadra.com", service: "armadra-relay" },
  ]);
  await runKillSwitch(parseArgs(["disable", "--apply"]), {
    fetch: api.fetcher,
    token: "tok",
    print: () => {},
  });
  assert.equal(api.calls[1].method, "DELETE");
  assert.match(api.calls[1].url, /workers\/domains\/dom-9$/);

  const none = fakeApi([]);
  await runKillSwitch(parseArgs(["disable", "--apply"]), {
    fetch: none.fetcher,
    token: "tok",
    print: () => {},
  });
  assert.deepEqual(
    none.calls.map((call) => call.method),
    ["GET"],
  );

  await assert.rejects(
    runKillSwitch(parseArgs(["restore", "--apply"]), {
      fetch: async () =>
        new Response(
          JSON.stringify({
            success: false,
            errors: [{ code: 10007, message: "service not found" }],
          }),
          { status: 404 },
        ),
      token: "tok",
      print: () => {},
    }),
    /404：10007 service not found/,
  );
});
