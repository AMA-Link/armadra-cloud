/**
 * 一条隧道流的信用与队列（中继设计 §3.3、协议包 §3.3），与运行时无关：只用 `Uint8Array`，
 * 不碰 `node:stream` 与 Web Streams。运行时各自在外面包一层（Web Streams 版在 `web-stream.ts`；端到端的假 core 用
 * `packages/e2e-fake-core` 的 `stream.Duplex` 版）。
 *
 * 写（本端 → 对端）：按 `maxDataChunk` 切块，每块同时扣「流信用」与「隧道信用」，
 * 任一不足就挂起这次写的回调，直到 `WINDOW` 补回——背压经适配层传给上游。可用信用比
 * 一块小时发能发的那部分（DATA 永远不超过当前信用）。
 *
 * 读（对端 → 本端）：`DATA` 先进本地队列，适配层要（{@link StreamCore.read}）才交给
 * 消费者；交出去的字节累计 ≥ 窗口一半时发 `WINDOW(streamId, 已消费)`，隧道级的消费由
 * 宿主同样补回（`WINDOW(0, …)`）。对端发得比授予的信用多 → `receiveData` 答 false，宿主
 * 发 `RST 5` 并以 4400 关隧道。
 *
 * 关闭：收 `END` → 交出 EOF；本地结束 → 发 `END`；收 `RST` → 适配层销毁自己；本地销毁 →
 * 发 `RST`（无错误时 `clientGone`）。两端都 `END` 后从流表摘掉。
 *
 * 本类对角色无感：中继与测试里的假 core 都能用（只有中继发 `OPEN`）。
 */
import { RST, type Frame } from "@armadra/platform-protocol/tunnel";

export class StreamResetError extends Error {
  readonly rstCode: number;
  constructor(rstCode: number) {
    super(`stream reset ${rstCode}`);
    this.rstCode = rstCode;
    this.name = "StreamResetError";
  }
}

/** 本地销毁时发给对端的 RST 码：对端重置原样、其他错误 `internal`、无错误 `clientGone`。 */
export function resetCodeFor(error: unknown): number {
  if (error instanceof StreamResetError) return error.rstCode;
  return error ? RST.internal : RST.clientGone;
}

/** 隧道级发送信用：所有流共享；不够时登记等待者，`add` 时挨个叫醒。 */
export class CreditPool {
  private readonly waiters = new Set<() => void>();
  available: number;

  constructor(available: number) {
    this.available = available;
  }

  take(max: number): number {
    const granted = Math.max(0, Math.min(max, this.available));
    this.available -= granted;
    return granted;
  }

  add(credit: number): void {
    this.available += credit;
    const waiting = [...this.waiters];
    this.waiters.clear();
    for (const wake of waiting) {
      if (this.available <= 0) {
        this.waiters.add(wake);
        continue;
      }
      wake();
    }
  }

  wait(wake: () => void): void {
    this.waiters.add(wake);
  }

  cancel(wake: () => void): void {
    this.waiters.delete(wake);
  }
}

/** 隧道交给每条流的宿主。`S` 是流在流表里的对象（运行时的适配层）。 */
export interface StreamHost<S = unknown> {
  sendFrame(frame: Frame): void;
  /** 隧道级发送信用。 */
  readonly sendCredit: CreditPool;
  /** 本端交给消费者的字节数（隧道级补信用用）。 */
  consumed(bytes: number): void;
  /** 流不再需要留在流表里（两端都 END，或 RST）。可能被调多次。 */
  closed(stream: S): void;
}

export interface StreamOptions {
  streamWindow: number;
  maxDataChunk: number;
}

/** 隧道对一条流的要求：流号与四个收帧入口。 */
export interface StreamEndpoint {
  readonly streamId: number;
  /** 收到 DATA；超出授予的信用答 false（流控违规）。 */
  receiveData(bytes: Uint8Array): boolean;
  receiveEnd(): void;
  receiveRst(code: number): void;
  receiveWindow(credit: number): void;
}

/** 运行时给隧道的「造一条流」：Node 造 `Duplex`，Web 造 {@link WebTunnelStream}。 */
export type StreamFactory<S extends StreamEndpoint> = (
  streamId: number,
  host: StreamHost<S>,
  options: StreamOptions,
) => S;

/** 适配层：接住交出来的字节、EOF 与对端重置。 */
export interface StreamSink {
  /** 交出一块；答 false 表示消费者暂时不要了（等下一次 {@link StreamCore.read}）。 */
  push(chunk: Uint8Array): boolean;
  /** 对端 END 且队列已交完。 */
  eof(): void;
  /** 对端 RST（或隧道关闭）：适配层销毁自己，并最终调 {@link StreamCore.destroy}。 */
  reset(error: StreamResetError): void;
}

/**
 * 一条流的信用与结束标记（运行时把对象「休眠」前存下、唤醒后还原，Workers 的 DO 用）。
 * 队列与在途的写不在其中：只有 {@link StreamCore.idle} 时才能存。
 */
export interface StreamCoreState {
  sendCredit: number;
  recvRemaining: number;
  unacked: number;
  remoteEnded: boolean;
  eofPushed: boolean;
  localEnded: boolean;
  peerReset: boolean;
  bytesIn: number;
  bytesOut: number;
}

interface PendingWrite {
  chunk: Uint8Array;
  offset: number;
  callback: () => void;
}

export class StreamCore<S = unknown> {
  /** 本端还能发的流信用。 */
  private sendCredit: number;
  /** 授予对端、对端还没用掉的信用。 */
  private recvRemaining: number;
  /** 已交给消费者、还没用 WINDOW 补回去的字节。 */
  private unacked = 0;
  private readonly queue: Uint8Array[] = [];
  private wantsRead = false;
  private pending: PendingWrite | null = null;
  private remoteEnded = false;
  private eofPushed = false;
  private localEnded = false;
  private peerReset = false;
  private released = false;
  /** 本地已销毁（或因对端重置而销毁）。 */
  destroyed = false;
  /** 对端 → 本端、本端 → 对端的载荷字节数（只计数，不留内容）。 */
  bytesIn = 0;
  bytesOut = 0;
  readonly streamId: number;
  private readonly host: StreamHost<S>;
  private readonly options: StreamOptions;
  private readonly owner: S;
  private readonly sink: StreamSink;

  constructor(
    streamId: number,
    host: StreamHost<S>,
    options: StreamOptions,
    owner: S,
    sink: StreamSink,
  ) {
    this.streamId = streamId;
    this.host = host;
    this.options = options;
    this.owner = owner;
    this.sink = sink;
    this.sendCredit = options.streamWindow;
    this.recvRemaining = options.streamWindow;
  }

  /** 没有排队待交的字节、也没有在途的写（此时 {@link snapshot} 才完整）。 */
  get idle(): boolean {
    return this.queue.length === 0 && this.pending === null;
  }

  /** 存下信用与结束标记（{@link idle} 为假时抛错：队列与在途的写不进快照）。 */
  snapshot(): StreamCoreState {
    if (!this.idle) throw new Error("流还有排队或在途的字节，不能存快照");
    return {
      sendCredit: this.sendCredit,
      recvRemaining: this.recvRemaining,
      unacked: this.unacked,
      remoteEnded: this.remoteEnded,
      eofPushed: this.eofPushed,
      localEnded: this.localEnded,
      peerReset: this.peerReset,
      bytesIn: this.bytesIn,
      bytesOut: this.bytesOut,
    };
  }

  /** 还原 {@link snapshot}（刚构造、还没收发过帧时调用）。 */
  restore(state: StreamCoreState): void {
    this.sendCredit = state.sendCredit;
    this.recvRemaining = state.recvRemaining;
    this.unacked = state.unacked;
    this.remoteEnded = state.remoteEnded;
    this.eofPushed = state.eofPushed;
    this.localEnded = state.localEnded;
    this.peerReset = state.peerReset;
    this.bytesIn = state.bytesIn;
    this.bytesOut = state.bytesOut;
  }

  // ------------------------------------------------------------ 来自对端的帧

  receiveData(bytes: Uint8Array): boolean {
    if (bytes.length > this.recvRemaining) return false;
    this.recvRemaining -= bytes.length;
    this.bytesIn += bytes.length;
    if (this.remoteEnded || this.destroyed) {
      // 已经结束或本地已销毁：丢弃，但隧道级信用照样还回去。
      this.host.consumed(bytes.length);
      return true;
    }
    this.queue.push(bytes);
    this.drain();
    return true;
  }

  receiveEnd(): void {
    if (this.remoteEnded) return;
    this.remoteEnded = true;
    this.drain();
    this.maybeRelease();
  }

  receiveRst(code: number): void {
    this.peerReset = true;
    this.sink.reset(new StreamResetError(code));
  }

  receiveWindow(credit: number): void {
    this.sendCredit += credit;
    this.pump();
  }

  // ------------------------------------------------------------ 读

  /** 消费者要数据了：把队列里的交出去，直到它说够了。 */
  read(): void {
    this.wantsRead = true;
    this.drain();
  }

  private drain(): void {
    while (this.wantsRead && this.queue.length > 0) {
      const chunk = this.queue.shift() as Uint8Array;
      this.acknowledge(chunk.length);
      if (!this.sink.push(chunk)) this.wantsRead = false;
    }
    if (this.queue.length === 0 && this.remoteEnded && !this.eofPushed) {
      this.eofPushed = true;
      this.sink.eof();
    }
  }

  private acknowledge(bytes: number): void {
    this.host.consumed(bytes);
    if (this.remoteEnded) return;
    this.unacked += bytes;
    if (this.unacked >= this.options.streamWindow / 2) {
      const credit = this.unacked;
      this.unacked = 0;
      this.recvRemaining += credit;
      this.host.sendFrame({ type: "window", streamId: this.streamId, credit });
    }
  }

  // ------------------------------------------------------------ 写

  /** 写一块（同一时刻只有一块在途）；全部发出后调 `callback`。销毁后不再回调。 */
  write(chunk: Uint8Array, callback: () => void): void {
    this.pending = { chunk, offset: 0, callback };
    this.pump();
  }

  private readonly pump = (): void => {
    const pending = this.pending;
    if (!pending || this.destroyed) return;
    while (pending.offset < pending.chunk.length) {
      const want = Math.min(
        pending.chunk.length - pending.offset,
        this.options.maxDataChunk,
        this.sendCredit,
      );
      if (want <= 0) return; // 等这条流的 WINDOW
      const granted = this.host.sendCredit.take(want);
      if (granted === 0) {
        this.host.sendCredit.wait(this.pump);
        return;
      }
      this.sendCredit -= granted;
      this.bytesOut += granted;
      this.host.sendFrame({
        type: "data",
        streamId: this.streamId,
        bytes: pending.chunk.subarray(pending.offset, pending.offset + granted),
      });
      pending.offset += granted;
    }
    this.pending = null;
    pending.callback();
  };

  /** 本端写完：发 END（对端已重置则不发），`callback` 之后若两端都结束就摘掉。 */
  finish(callback: () => void): void {
    if (!this.peerReset) {
      this.localEnded = true;
      this.host.sendFrame({ type: "end", streamId: this.streamId });
    }
    callback();
    this.maybeRelease();
  }

  /**
   * 本地销毁：没被对端重置、也没两端都结束时发 `RST code`；队列里还没交出去的字节把
   * 隧道级信用还回去。只生效一次。
   */
  destroy(code: number): void {
    if (this.destroyed) return;
    this.destroyed = true;
    this.host.sendCredit.cancel(this.pump);
    this.pending = null;
    if (!this.peerReset && !(this.localEnded && this.remoteEnded))
      this.host.sendFrame({ type: "rst", streamId: this.streamId, code });
    const dropped = this.queue.reduce((sum, chunk) => sum + chunk.length, 0);
    this.queue.length = 0;
    if (dropped > 0) this.host.consumed(dropped);
    this.release();
  }

  private maybeRelease(): void {
    if (this.localEnded && this.remoteEnded) this.release();
  }

  private release(): void {
    if (this.released) return;
    this.released = true;
    this.host.closed(this.owner);
  }
}
