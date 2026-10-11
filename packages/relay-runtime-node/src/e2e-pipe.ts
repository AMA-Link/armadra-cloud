/**
 * 端到端通道的 Node 外壳（cloud-api §20）：中继自己终止客户端 WebSocket，二进制消息 ↔ 隧道流
 * `OPEN kind: "e2e"` 的 `DATA`，字节原样，不解析（里面是 `armadra-e2e/1` 的密文单元）。
 *
 * - 上行：二进制消息写进流，流的写水位满了就暂停读客户端（`ws.pause()`），`drain` 后恢复；
 *   文本消息以 4400 关闭并重置流。客户端关闭 → `END`；掉线 → `RST clientGone`。
 * - 下行：流的每块作为一条二进制消息发出（边界不保证，通道按长度自己切）；客户端缓冲超过
 *   高水位就暂停读流，低于低水位再恢复。源 `END` → 1000，源 `RST` / 隧道没了 → 4404。
 * - 活性：每 `pingMs` 发一次 ping，`idleMs` 内客户端没有任何消息或 pong 就断开。
 *
 * 结束时记一条只含元数据的日志（源、流、字节数、时长、关闭码）。
 */
import { CLOSE } from "@armadra/platform-protocol/tunnel";
import type { RelayLog } from "@armadra/relay-kernel/log";
import type { WebSocket } from "ws";

import type { TunnelStream } from "./stream.ts";

/** 客户端缓冲到这么多就暂停读流。 */
const HIGH_WATER = 1024 * 1024;
const LOW_WATER = 256 * 1024;
/** 客户端关了之后等源回 END 的时长。 */
const END_GRACE_MS = 5_000;

export interface E2ePipeOptions {
  sourceId: string;
  pingMs: number;
  idleMs: number;
  log: RelayLog;
  /** 管道结束（两边都收拾完）：还并发名额、摘连接表。 */
  done(): void;
}

export function pipeE2e(
  ws: WebSocket,
  stream: TunnelStream,
  options: E2ePipeOptions,
): void {
  const startedAt = Date.now();
  let lastClientMs = startedAt;
  let bytesUp = 0;
  let bytesDown = 0;
  let finished = false;
  let closeCode: number | undefined;

  const closeClient = (code: number, reason: string) => {
    closeCode ??= code;
    if (ws.readyState === ws.OPEN || ws.readyState === ws.CONNECTING)
      ws.close(code, reason);
  };

  const finish = () => {
    if (finished) return;
    finished = true;
    clearInterval(timer);
    options.log.info("端到端通道结束", {
      sourceId: options.sourceId,
      streamId: stream.streamId,
      kind: "e2e",
      bytesIn: bytesUp,
      bytesOut: bytesDown,
      durationMs: Date.now() - startedAt,
      code: closeCode,
    });
    options.done();
  };

  // ---------------------------------------------------------------- 上行
  ws.on("message", (data, isBinary) => {
    lastClientMs = Date.now();
    if (!isBinary) {
      closeClient(CLOSE.badFrame, "binary_only");
      stream.destroy();
      return;
    }
    const chunk = Array.isArray(data)
      ? Buffer.concat(data)
      : Buffer.isBuffer(data)
        ? data
        : Buffer.from(data);
    if (chunk.length === 0 || stream.destroyed || stream.writableEnded) return;
    bytesUp += chunk.length;
    if (!stream.write(chunk)) {
      ws.pause();
      stream.once("drain", () => ws.resume());
    }
  });
  ws.on("pong", () => {
    lastClientMs = Date.now();
  });
  ws.on("close", (code) => {
    closeCode ??= code;
    // 正常关闭：发 END（源读到 EOF）；掉线：重置流。
    if (code === 1006 || code === 1015) stream.destroy();
    else if (!stream.destroyed && !stream.writableEnded) stream.end();
    finish();
    // 等源回 END 收尾；源迟迟不回就重置，别让流一直占着名额。
    if (!stream.destroyed)
      setTimeout(() => stream.destroy(), END_GRACE_MS).unref?.();
  });
  ws.on("error", () => {
    stream.destroy();
  });

  // ---------------------------------------------------------------- 下行
  stream.on("data", (chunk: Buffer) => {
    bytesDown += chunk.length;
    ws.send(chunk, { binary: true }, () => {
      if (stream.isPaused() && ws.bufferedAmount < LOW_WATER) stream.resume();
    });
    if (ws.bufferedAmount > HIGH_WATER) stream.pause();
  });
  stream.on("end", () => closeClient(CLOSE.normal, ""));
  stream.on("error", () => closeClient(CLOSE.sourceOffline, "source_offline"));
  stream.on("close", () => {
    if (!stream.readableEnded)
      closeClient(CLOSE.sourceOffline, "source_offline");
  });

  // ---------------------------------------------------------------- 活性
  const timer = setInterval(() => {
    if (Date.now() - lastClientMs >= options.idleMs) {
      closeCode ??= 1006;
      ws.terminate();
      return;
    }
    if (ws.readyState === ws.OPEN) ws.ping();
  }, options.pingMs);
  timer.unref?.();
}
