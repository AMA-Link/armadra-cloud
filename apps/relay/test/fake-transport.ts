/** 单测用的隧道传输：记下发出的帧，不碰网络。 */
import {
  decodeFrame,
  encodeFrame,
  LIMITS,
  type Frame,
} from "@armadra/platform-protocol/tunnel";

import {
  Tunnel,
  type TunnelHooks,
  type TunnelLimitsConfig,
} from "../src/tunnel/tunnel.ts";

export class FakeTransport {
  readonly sent: Uint8Array[] = [];
  closedWith: { code: number; reason: string } | null = null;
  terminated = false;

  send(bytes: Uint8Array): void {
    this.sent.push(bytes);
  }
  close(code: number, reason: string): void {
    this.closedWith = { code, reason };
  }
  terminate(): void {
    this.terminated = true;
  }

  frames(): Frame[] {
    return this.sent.map((bytes) => decodeFrame(bytes));
  }

  take(): Frame[] {
    const frames = this.frames();
    this.sent.length = 0;
    return frames;
  }
}

export const TEST_LIMITS: TunnelLimitsConfig = {
  maxStreams: LIMITS.maxStreams,
  streamWindow: LIMITS.streamWindow,
  tunnelWindow: LIMITS.tunnelWindow,
  maxDataChunk: LIMITS.maxDataChunk,
  heartbeatMs: LIMITS.heartbeatMs,
  heartbeatMisses: LIMITS.heartbeatMisses,
  goawayGraceMs: LIMITS.goawayGraceMs,
};

export function fakeTunnel(
  limits: Partial<TunnelLimitsConfig> = {},
  hooks: TunnelHooks = {},
) {
  const transport = new FakeTransport();
  const tunnel = new Tunnel(
    "fedcba9876543210fedcba9876543210",
    "0123456789abcdef0123456789abcdef",
    "0.2.0",
    transport,
    { ...TEST_LIMITS, ...limits },
    hooks,
  );
  const feed = (frame: Frame) => tunnel.receive(encodeFrame(frame));
  return { tunnel, transport, feed };
}

export const OPEN = {
  kind: "http" as const,
  method: "GET",
  path: "/",
  headers: [["host", "x"]] as [string, string][],
  clientOrigin: null,
  remoteIp: "127.0.0.1",
  relayNode: "relay-1",
};
