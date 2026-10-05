import { strict as assert } from "node:assert";
import { test } from "node:test";
import { fill, findBlocks } from "./generate.mjs";

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
