import { expect, it } from "vitest";

import { runProbes } from "./health.ts";

it("任一探测失败或超时则整体不就绪", async () => {
  const result = await runProbes(
    {
      good: async () => {},
      bad: async () => {
        throw new Error("postgres://user:secret@host/db refused");
      },
      slow: () => new Promise(() => {}),
    },
    50,
  );
  expect(result.ok).toBe(false);
  expect(result.checks.good?.ok).toBe(true);
  expect(result.checks.bad?.ok).toBe(false);
  expect(result.checks.slow?.ok).toBe(false);
  expect(JSON.stringify(result)).not.toContain("secret");
});

it("全部通过则就绪", async () => {
  expect((await runProbes({ a: async () => {} })).ok).toBe(true);
});
