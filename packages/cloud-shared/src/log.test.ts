import { expect, it } from "vitest";

import { createLogger } from "./log.ts";

it("按级别过滤，字段不能覆盖保留键", () => {
  const lines: string[] = [];
  const log = createLogger({
    name: "t",
    level: "info",
    write: (line) => lines.push(line),
    now: () => 0,
  });
  log.debug("hidden");
  log.info("hello", { port: 8100, msg: "override", skipped: undefined });
  expect(lines).toHaveLength(1);
  expect(JSON.parse(lines[0] ?? "")).toEqual({
    time: "1970-01-01T00:00:00.000Z",
    level: "info",
    name: "t",
    msg: "hello",
    port: 8100,
  });
});
