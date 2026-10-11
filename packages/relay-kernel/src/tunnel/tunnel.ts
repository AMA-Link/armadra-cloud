/**
 * 一条隧道（中继设计 §3.2）：`ready` 之后的二进制帧收发、流表、隧道级窗口、心跳与 GOAWAY。
 *
 * ```text
 * open ──GOAWAY 发出──▶ draining ──流全结束或 grace 到──▶ closed
 *   └──── 心跳连续 heartbeatMisses 次未答 / 协议错误 / 对端断开 ────▶ closed
 * ```
 *
 * 传输层抽象成 {@link TunnelTransport}（Node 里是 `ws` 的 WebSocket），流由运行时给的
 * {@link StreamFactory} 造（Node 是 `stream.Duplex`，缺省是 Web Streams 的
 * {@link WebTunnelStream}），所以本类不碰任何运行时 API，单测里用假传输与假时钟驱动。
 * 只有中继发 `OPEN`；对端发来的 `OPEN` 答 `RST protocol`。
 */
import {
  CLOSE,
  decodeFrame,
  encodeFrame,
  FrameError,
  RST,
  type Frame,
  type GoawayReason,
  type OpenPayload,
} from "@armadra/platform-protocol/tunnel";

import type { TrafficCounter } from "../traffic.ts";
import {
  CreditPool,
  type StreamEndpoint,
  type StreamFactory,
  type StreamHost,
} from "./stream.ts";
import { webStreams, type WebTunnelStream } from "./web-stream.ts";

export interface TunnelTransport {
  send(bytes: Uint8Array): void;
  /** 正常关闭（发关闭帧）。 */
  close(code: number, reason: string): void;
  /** 立即断开（心跳超时）。 */
  terminate(): void;
}

export interface TunnelLimitsConfig {
  maxStreams: number;
  streamWindow: number;
  tunnelWindow: number;
  maxDataChunk: number;
  heartbeatMs: number;
  heartbeatMisses: number;
  goawayGraceMs: number;
}

export interface TunnelCloseInfo {
  code: number;
  reason: string;
  /** 被同源新隧道替换（不该再删路由、发下线事件）。 */
  replaced: boolean;
}

export interface TunnelHooks {
  /** 收到 PONG（路由续期）。 */
  onPong?(): void;
  /** 对端发来 GOAWAY：不再给它开新流。 */
  onPeerGoaway?(reason: GoawayReason): void;
  onClose?(info: TunnelCloseInfo): void;
  onHeartbeatMiss?(): void;
  /** 载荷字节计数（指标）：to_source = 中继 → 源，to_client = 源 → 中继。 */
  onBytes?(direction: "to_source" | "to_client", bytes: number): void;
}

export type TunnelState = "open" | "draining" | "closed";

/**
 * 隧道的信用与流表（运行时把对象「休眠」前存下、唤醒后还原，Workers 的 DO 用）。只在
 * `open` 时能存；流本身的状态由运行时各自存（{@link Tunnel.restore} 的 `adopt` 还原）。
 */
export interface TunnelSnapshot {
  tunnelId: string;
  sourceId: string;
  coreVersion: string;
  /** 源在 `hello.capabilities` 里报的项（1.3 起；老快照没有 = 空）。 */
  capabilities?: string[];
  nextStreamId: number;
  sendCredit: number;
  recvRemaining: number;
  recvUnacked: number;
  streams: number[];
}

export class TunnelUnavailableError extends Error {
  readonly reason: "draining" | "closed" | "max_streams";
  constructor(reason: "draining" | "closed" | "max_streams") {
    super(`tunnel unavailable: ${reason}`);
    this.reason = reason;
    this.name = "TunnelUnavailableError";
  }
}

export class Tunnel<S extends StreamEndpoint = WebTunnelStream> {
  state: TunnelState = "open";
  replaced = false;
  private readonly streams = new Map<number, S>();
  /** 流号 → 流量统计句柄（开流时给了才有）。 */
  private readonly counters = new Map<number, TrafficCounter>();
  private nextStreamId = 1;
  private readonly sendPool: CreditPool;
  private recvRemaining: number;
  private recvUnacked = 0;
  private missed = 0;
  private heartbeat: ReturnType<typeof setInterval> | undefined;
  private graceTimer: ReturnType<typeof setTimeout> | undefined;
  private goawayCode: number = CLOSE.tunnelGoaway;
  private readonly closedWaiters: (() => void)[] = [];
  private readonly host: StreamHost<S>;
  private readonly streamFactory: StreamFactory<S>;
  readonly tunnelId: string;
  readonly sourceId: string;
  readonly coreVersion: string;
  /**
   * 源在 `hello.capabilities` 里报的项（握手就绪时由运行时填）。边缘据它决定开不开可选的面
   * （`e2e`：`_relay/e2e`）。
   */
  capabilities: ReadonlySet<string> = new Set();
  private readonly transport: TunnelTransport;
  private readonly limits: TunnelLimitsConfig;
  private readonly hooks: TunnelHooks;

  constructor(
    tunnelId: string,
    sourceId: string,
    coreVersion: string,
    transport: TunnelTransport,
    limits: TunnelLimitsConfig,
    hooks: TunnelHooks = {},
    streamFactory: StreamFactory<S> = webStreams as unknown as StreamFactory<S>,
  ) {
    this.tunnelId = tunnelId;
    this.sourceId = sourceId;
    this.coreVersion = coreVersion;
    this.transport = transport;
    this.limits = limits;
    this.hooks = hooks;
    this.streamFactory = streamFactory;
    this.sendPool = new CreditPool(limits.tunnelWindow);
    this.recvRemaining = limits.tunnelWindow;
    this.host = {
      sendFrame: (frame) => this.send(frame),
      sendCredit: this.sendPool,
      consumed: (bytes) => this.consumed(bytes),
      closed: (stream) => this.release(stream),
    };
  }

  get streamCount(): number {
    return this.streams.size;
  }

  /** 流表里的流（快照与断开用）。 */
  streamList(): S[] {
    return [...this.streams.values()];
  }

  /** 存下信用与流号（只在 `open` 时，否则答 null）。 */
  snapshot(): TunnelSnapshot | null {
    if (this.state !== "open") return null;
    return {
      tunnelId: this.tunnelId,
      sourceId: this.sourceId,
      coreVersion: this.coreVersion,
      capabilities: [...this.capabilities],
      nextStreamId: this.nextStreamId,
      sendCredit: this.sendPool.available,
      recvRemaining: this.recvRemaining,
      recvUnacked: this.recvUnacked,
      streams: [...this.streams.keys()],
    };
  }

  /**
   * 从 {@link snapshot} 还原一条隧道（不发任何帧、不起心跳）。`adopt` 按流号还原运行时的流
   * （拿到宿主与流选项，自己恢复信用）；答 null 的流对端还以为开着，这里发 `RST clientGone`
   * 让它释放。
   */
  static restore<S extends StreamEndpoint>(
    snapshot: TunnelSnapshot,
    transport: TunnelTransport,
    limits: TunnelLimitsConfig,
    hooks: TunnelHooks,
    streamFactory: StreamFactory<S>,
    adopt: (
      streamId: number,
      host: StreamHost<S>,
      options: { streamWindow: number; maxDataChunk: number },
    ) => S | null,
  ): Tunnel<S> {
    const tunnel = new Tunnel<S>(
      snapshot.tunnelId,
      snapshot.sourceId,
      snapshot.coreVersion,
      transport,
      limits,
      hooks,
      streamFactory,
    );
    tunnel.capabilities = new Set(snapshot.capabilities ?? []);
    tunnel.nextStreamId = snapshot.nextStreamId;
    tunnel.sendPool.available = snapshot.sendCredit;
    tunnel.recvRemaining = snapshot.recvRemaining;
    tunnel.recvUnacked = snapshot.recvUnacked;
    const options = {
      streamWindow: limits.streamWindow,
      maxDataChunk: limits.maxDataChunk,
    };
    for (const streamId of snapshot.streams) {
      const stream = adopt(streamId, tunnel.host, options);
      if (stream) tunnel.streams.set(streamId, stream);
      else tunnel.send({ type: "rst", streamId, code: RST.clientGone });
    }
    return tunnel;
  }

  /** 能否再开流（`open` 且没到 `maxStreams`）。 */
  get accepting(): boolean {
    return this.state === "open";
  }

  start(): void {
    this.heartbeat = setInterval(() => this.tick(), this.limits.heartbeatMs);
    this.heartbeat.unref?.();
  }

  /**
   * 为一条边缘连接开流：发 OPEN。草稿态、满流时抛 {@link TunnelUnavailableError}。给了 `counter`
   * 就把这条流两个方向的 `DATA` 载荷记到它上面（流量统计）。
   */
  openStream(open: OpenPayload, counter?: TrafficCounter): S {
    if (this.state !== "open") throw new TunnelUnavailableError(this.state);
    if (this.streams.size >= this.limits.maxStreams)
      throw new TunnelUnavailableError("max_streams");
    const streamId = this.nextStreamId;
    this.nextStreamId += 1;
    this.send({ type: "open", streamId, open });
    const stream = this.streamFactory(streamId, this.host, {
      streamWindow: this.limits.streamWindow,
      maxDataChunk: this.limits.maxDataChunk,
    });
    this.streams.set(streamId, stream);
    if (counter) this.counters.set(streamId, counter);
    return stream;
  }

  /** 给已有的流挂上流量统计句柄（休眠唤醒后还原的流用）。 */
  meter(streamId: number, counter: TrafficCounter): void {
    if (this.streams.has(streamId)) this.counters.set(streamId, counter);
  }

  /** 收到一条二进制消息。 */
  receive(bytes: Uint8Array): void {
    if (this.state === "closed") return;
    let frame: Frame;
    try {
      frame = decodeFrame(bytes);
    } catch (error) {
      const tooLarge =
        error instanceof FrameError && error.code === "frame_too_large";
      this.close(tooLarge ? CLOSE.frameTooLarge : CLOSE.badFrame, "bad_frame");
      return;
    }
    switch (frame.type) {
      case "open":
        this.send({
          type: "rst",
          streamId: frame.streamId,
          code: RST.protocol,
        });
        return;
      case "data": {
        const length = frame.bytes.length;
        if (length > this.recvRemaining) {
          this.flowViolation(frame.streamId);
          return;
        }
        this.recvRemaining -= length;
        this.hooks.onBytes?.("to_client", length);
        this.counters.get(frame.streamId)?.bytes("to_client", length);
        const stream = this.streams.get(frame.streamId);
        if (!stream) {
          this.consumed(length);
          return;
        }
        if (!stream.receiveData(frame.bytes))
          this.flowViolation(frame.streamId);
        return;
      }
      case "end":
        this.streams.get(frame.streamId)?.receiveEnd();
        return;
      case "rst":
        this.streams.get(frame.streamId)?.receiveRst(frame.code);
        return;
      case "window":
        if (frame.streamId === 0) this.sendPool.add(frame.credit);
        else this.streams.get(frame.streamId)?.receiveWindow(frame.credit);
        return;
      case "ping":
        this.send({ type: "pong", ts: frame.ts });
        return;
      case "pong":
        this.missed = 0;
        this.hooks.onPong?.();
        return;
      case "goaway":
        if (this.state === "open") {
          this.state = "draining";
          this.hooks.onPeerGoaway?.(frame.reason);
          this.armGrace(frame.graceMs);
          this.closeIfDrained();
        }
        return;
    }
  }

  /**
   * 发 GOAWAY，不再开新流；旧流自然结束后（或 `graceMs` 到了）关闭：
   * `replaced` 用 4491，其余 4492。
   */
  goaway(reason: GoawayReason, graceMs = this.limits.goawayGraceMs): void {
    if (this.state === "closed") return;
    if (reason === "replaced") {
      this.replaced = true;
      this.goawayCode = CLOSE.tunnelReplaced;
    }
    if (this.state === "open") {
      this.send({ type: "goaway", reason, graceMs });
      this.state = "draining";
    }
    this.armGrace(graceMs);
    this.closeIfDrained();
  }

  /** 关隧道：所有流以 `RST sourceGone` 结束，传输层发关闭码。 */
  close(code: number, reason: string): void {
    if (this.state === "closed") return;
    this.finish(code, reason);
    this.transport.close(code, reason);
  }

  /** 传输层已经断了（对端关闭或网络错误）。 */
  transportClosed(code: number, reason = "transport_closed"): void {
    if (this.state === "closed") return;
    this.finish(code, reason);
  }

  /** 等隧道关闭（GOAWAY 排空用）。 */
  closed(): Promise<void> {
    if (this.state === "closed") return Promise.resolve();
    return new Promise((resolve) => this.closedWaiters.push(resolve));
  }

  // ------------------------------------------------------------ 内部

  private finish(code: number, reason: string): void {
    this.state = "closed";
    clearInterval(this.heartbeat);
    clearTimeout(this.graceTimer);
    for (const stream of [...this.streams.values()])
      stream.receiveRst(RST.sourceGone);
    this.streams.clear();
    this.counters.clear();
    this.hooks.onClose?.({ code, reason, replaced: this.replaced });
    for (const resolve of this.closedWaiters.splice(0)) resolve();
  }

  private tick(): void {
    if (this.state === "closed") return;
    if (this.missed >= this.limits.heartbeatMisses) {
      this.hooks.onHeartbeatMiss?.();
      this.finish(CLOSE.goingAway, "heartbeat_timeout");
      this.transport.terminate();
      return;
    }
    this.missed += 1;
    this.send({ type: "ping", ts: Date.now() });
  }

  private send(frame: Frame): void {
    if (this.state === "closed") return;
    if (frame.type === "data") {
      this.hooks.onBytes?.("to_source", frame.bytes.length);
      this.counters.get(frame.streamId)?.bytes("to_source", frame.bytes.length);
    }
    this.transport.send(encodeFrame(frame));
  }

  private consumed(bytes: number): void {
    this.recvUnacked += bytes;
    if (this.recvUnacked >= this.limits.tunnelWindow / 2) {
      const credit = this.recvUnacked;
      this.recvUnacked = 0;
      this.recvRemaining += credit;
      this.send({ type: "window", streamId: 0, credit });
    }
  }

  private flowViolation(streamId: number): void {
    this.send({ type: "rst", streamId, code: RST.flowControl });
    this.close(CLOSE.badFrame, "flow_control");
  }

  private release(stream: S): void {
    if (this.streams.get(stream.streamId) === stream) {
      this.streams.delete(stream.streamId);
      this.counters.delete(stream.streamId);
    }
    this.closeIfDrained();
  }

  private armGrace(graceMs: number): void {
    if (this.graceTimer !== undefined) return;
    this.graceTimer = setTimeout(
      () => this.close(this.goawayCode, "goaway_grace"),
      graceMs,
    );
    this.graceTimer.unref?.();
  }

  private closeIfDrained(): void {
    if (this.state === "draining" && this.streams.size === 0)
      this.close(this.goawayCode, "goaway");
  }
}
