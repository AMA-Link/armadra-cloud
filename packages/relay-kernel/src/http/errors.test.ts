import { STATUS_CODES } from "node:http";

import { ERRORS, errorStatus } from "@armadra/platform-protocol/errors";
import { expect, it } from "vitest";

import { errorMessage, rawErrorResponse, statusText } from "./errors.ts";

it("状态行原因短语与 Node 的 STATUS_CODES 逐字相同（注册表里的全部状态码）", () => {
  const statuses = new Set(Object.values(ERRORS).map((entry) => entry.status));
  for (const status of [...statuses, 101, 200, 204])
    expect(statusText(status), String(status)).toBe(STATUS_CODES[status]);
  expect(statusText(599)).toBe("");
});

it("裸错误响应：状态行、定长 JSON 体、no-store、connection: close 与 CORS 头", () => {
  const bytes = rawErrorResponse("source_offline", {
    "access-control-allow-origin": "https://app.example",
  });
  const body = JSON.stringify({
    code: "source_offline",
    message: errorMessage("source_offline"),
  });
  expect(new TextDecoder().decode(bytes)).toBe(
    `HTTP/1.1 ${errorStatus("source_offline")} Service Unavailable\r\n` +
      "content-type: application/json; charset=utf-8\r\n" +
      `content-length: ${body.length}\r\n` +
      "cache-control: no-store\r\nconnection: close\r\n" +
      "access-control-allow-origin: https://app.example\r\n\r\n" +
      body,
  );
});

it("bad_gateway 登记为 502，有固定文案", () => {
  expect(errorStatus("bad_gateway")).toBe(502);
  expect(errorMessage("bad_gateway")).not.toBe("bad_gateway");
});
