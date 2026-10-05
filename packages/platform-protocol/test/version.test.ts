import { expect, it } from "vitest";

import { PROTOCOL_VERSION } from "../src/index.ts";

it("协议 1.0", () => {
  expect(PROTOCOL_VERSION).toEqual({ major: 1, minor: 0 });
});
