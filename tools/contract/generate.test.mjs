import { strict as assert } from "node:assert";
import { test } from "node:test";
import { fill, findBlocks, renderSection } from "./generate.mjs";

test("重写生成块，块外文字不动", () => {
  const input =
    "# t\n\n<!-- rpc:begin contract=§2 -->\nold\n<!-- rpc:end -->\n\ntail\n";
  const output = fill(input, (section) => `new ${section}\n`);
  assert.equal(
    output,
    "# t\n\n<!-- rpc:begin contract=§2 -->\n\nnew §2\n\n<!-- rpc:end -->\n\ntail\n",
  );
  assert.deepEqual(findBlocks(output), [
    { section: "§2", body: "\nnew §2\n\n" },
  ]);
});

test("begin / end 不配对时报错", () => {
  assert.throws(() => findBlocks("<!-- rpc:begin contract=§2 -->\n"), /不配对/);
});

test("形状表只列本节的 procedure，没有时写一行说明", () => {
  const procedures = [
    {
      key: "auth.login",
      method: "POST",
      path: "/v1/auth/login",
      meta: { scope: ["anonymous"] },
      errorCodes: ["rate_limited"],
    },
    {
      key: "orgs.get",
      method: "GET",
      path: "/v1/orgs/{orgId}",
      meta: { scope: ["session"] },
      errorCodes: [],
    },
  ];
  const sectionOf = (key) => (key.startsWith("auth.") ? "§2" : "§3");
  const table = renderSection("§2", procedures, sectionOf);
  assert.match(
    table,
    /\| `auth.login` \| `POST \/v1\/auth\/login` \| `anonymous` \| `rate_limited` \|/,
  );
  assert.doesNotMatch(table, /orgs\.get/);
  assert.match(renderSection("§9", procedures, sectionOf), /没有/);
});
