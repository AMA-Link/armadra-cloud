/** 隧道表（运行时无关）：假 WebSocket 走完黄金握手 → ready、路由与在线事件、帧转交、关闭。 */
import { createLogger } from "@armadra/cloud-shared/log";
import {
  FIXED_NOW_MS,
  readJsonFixture,
} from "@armadra/platform-protocol/fixtures";
import {
  CLOSE,
  decodeFrame,
  encodeFrame,
  type Auth,
  type Challenge,
  type Hello,
} from "@armadra/platform-protocol/tunnel";
import { describe, expect, it } from "vitest";

import { SOURCE_ID, testControlPlane } from "../../test/tokens.ts";
import { createAuthContext } from "../auth/context.ts";
import type { PresenceEvent } from "../control/types.ts";
import { relayLog } from "../log.ts";
import { DEFAULT_TUNNEL_LIMITS } from "../main.ts";
import { RelayMetrics } from "../metrics.ts";
import { TunnelHub, type TunnelSocket } from "./hub.ts";
import { webStreams } from "./web-stream.ts";

const fixture = readJsonFixture<{
  hello: Hello;
  challenge: Challenge;
  auth: Auth;
}>("tunnel/handshake.json");

class FakeSocket implements TunnelSocket {
  readonly texts: unknown[] = [];
  readonly binaries: Uint8Array[] = [];
  closedWith: { code: number; reason: string } | null = null;
  terminated = false;
  get isOpen(): boolean {
    return this.closedWith === null && !this.terminated;
  }
  sendText(text: string): void {
    this.texts.push(JSON.parse(text));
  }
  sendBinary(bytes: Uint8Array): void {
    this.binaries.push(bytes);
  }
  close(code: number, reason: string): void {
    this.closedWith = { code, reason };
  }
  terminate(): void {
    this.terminated = true;
  }
}

const settle = () => new Promise((resolve) => setTimeout(resolve, 0));
const utf8 = new TextEncoder();

function hub() {
  const control = testControlPlane({ now: () => FIXED_NOW_MS });
  const events: PresenceEvent[] = [];
  control.presence.subscribe((event) => events.push(event));
  const tunnels: TunnelHub<ReturnType<typeof webStreams>> = new TunnelHub({
    control,
    auth: createAuthContext(control, () => FIXED_NOW_MS),
    nodeId: fixture.challenge.relayNode,
    limits: DEFAULT_TUNNEL_LIMITS,
    handshakeTimeoutMs: 10_000,
    log: relayLog(createLogger({ name: "test", write: () => {} })),
    metrics: new RelayMetrics(() => tunnels.counts()),
    streams: webStreams,
    nonce: () => fixture.challenge.nonce2,
  });
  return { tunnels, control, events };
}

describe("TunnelHub", () => {
  it("握手 → ready：写路由、发 sourceOnline；二进制帧交给隧道；断开删路由、发 sourceOffline", async () => {
    const { tunnels, control, events } = hub();
    const socket = new FakeSocket();
    const connection = tunnels.accept(socket);
    connection.message(utf8.encode(JSON.stringify(fixture.hello)), false);
    await settle();
    expect(socket.texts).toEqual([fixture.challenge]);
    connection.message(utf8.encode(JSON.stringify(fixture.auth)), false);
    await settle();
    expect(socket.texts[1]).toMatchObject({
      type: "ready",
      heartbeatMs: DEFAULT_TUNNEL_LIMITS.heartbeatMs,
    });
    expect(tunnels.counts()).toEqual({ tunnels: 1, streams: 0 });
    const route = await control.routes.get(SOURCE_ID);
    expect(route?.node).toBe(fixture.challenge.relayNode);
    const tunnel = await tunnels.lookup(SOURCE_ID);
    expect(tunnel?.tunnelId).toBe(route?.tunnelId);
    expect(events.map((event) => event.type)).toEqual(["sourceOnline"]);

    connection.message(encodeFrame({ type: "ping", ts: 7 }), true);
    expect(socket.binaries.map((bytes) => decodeFrame(bytes))).toEqual([
      { type: "pong", ts: 7 },
    ]);

    connection.closed(CLOSE.goingAway);
    await settle();
    expect(tunnels.counts().tunnels).toBe(0);
    expect(await control.routes.get(SOURCE_ID)).toBeNull();
    expect(events.map((event) => event.type)).toEqual([
      "sourceOnline",
      "sourceOffline",
    ]);
    tunnel?.close(CLOSE.normal, "done");
  });

  it("ready 之前发二进制：关 4400，不发 reject", async () => {
    const { tunnels } = hub();
    const socket = new FakeSocket();
    tunnels.accept(socket).message(new Uint8Array([1, 2, 3]), true);
    await settle();
    expect(socket.closedWith).toEqual({
      code: CLOSE.badFrame,
      reason: "binary_before_ready",
    });
    expect(socket.texts).toEqual([]);
  });

  it("关闭后不再收新隧道", async () => {
    const { tunnels } = hub();
    expect(tunnels.accepting).toBe(true);
    await tunnels.shutdown(0);
    expect(tunnels.accepting).toBe(false);
  });
});
