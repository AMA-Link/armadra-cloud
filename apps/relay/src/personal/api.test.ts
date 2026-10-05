import {
  cloudContract,
  listProcedures,
} from "@armadra/platform-protocol/cloud-api";
import { expect, it } from "vitest";

import { PERSONAL_CAPABILITIES, PERSONAL_PROCEDURES } from "./api.ts";
import { PLATFORM_CAPABILITIES } from "@armadra/platform-protocol/cloud-api";

it("personal 实现的 procedure 都在契约里（拼错一个就会悄悄变成 404）", () => {
  const keys = listProcedures(cloudContract).map((entry) => entry.key);
  for (const key of PERSONAL_PROCEDURES) expect(keys, key).toContain(key);
  expect(new Set(PERSONAL_PROCEDURES).size).toBe(PERSONAL_PROCEDURES.length);
});

it("报的能力是契约能力表的子集", () => {
  for (const capability of PERSONAL_CAPABILITIES)
    expect(PLATFORM_CAPABILITIES).toContain(capability);
});
