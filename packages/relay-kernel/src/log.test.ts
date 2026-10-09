import { createLogger } from "@armadra/cloud-shared/log";
import { expect, it } from "vitest";

import { errorName, relayLog } from "./log.ts";
import { RelayMetrics } from "./metrics.ts";

it("日志只写白名单字段，非标量与未知字段被丢掉", () => {
  const lines: string[] = [];
  const log = relayLog(
    createLogger({ name: "t", write: (l) => lines.push(l) }),
  );
  log.info("流结束", {
    sourceId: "s",
    bytesIn: 3,
    ...({
      path: "/api/x?ticket=secret",
      authorization: "Bearer secret",
      headers: { a: 1 },
    } as object),
  });
  const record = JSON.parse(lines[0] ?? "{}") as Record<string, unknown>;
  expect(record).toMatchObject({ msg: "流结束", sourceId: "s", bytesIn: 3 });
  expect(lines[0]).not.toContain("secret");
  expect(record).not.toHaveProperty("path");
  expect(record).not.toHaveProperty("headers");
});

it("errorName 只给类名", () => {
  expect(errorName(new TypeError("令牌 abc"))).toBe("TypeError");
  expect(errorName("x")).toBe("string");
});

it("指标是文本格式，计数按标签累加", () => {
  const metrics = new RelayMetrics(() => ({ tunnels: 1, streams: 2 }));
  metrics.addBytes("to_client", 10);
  metrics.addBytes("to_client", 5);
  metrics.edgeConnection("ok");
  metrics.edgeConnection("ok");
  metrics.edgeConnection("source_offline");
  metrics.handshake("ok", 0.02);
  metrics.heartbeatMiss();
  const text = metrics.render();
  expect(text).toContain("relay_tunnels 1\n");
  expect(text).toContain("relay_streams 2\n");
  expect(text).toContain('relay_bytes_total{direction="to_client"} 15');
  expect(text).toContain('relay_bytes_total{direction="to_source"} 0');
  expect(text).toContain('relay_edge_connections_total{result="ok"} 2');
  expect(text).toContain(
    'relay_edge_connections_total{result="source_offline"} 1',
  );
  expect(text).toContain('relay_handshake_total{result="ok"} 1');
  expect(text).toContain('relay_handshake_seconds_bucket{le="0.05"} 1');
  expect(text).toContain('relay_handshake_seconds_bucket{le="0.01"} 0');
  expect(text).toContain("relay_heartbeat_miss_total 1");
});
