/**
 * `TunnelStream`：一条隧道流的 `stream.Duplex` 适配（中继设计 §3.3、协议包 §3.3）。信用、
 * 队列与关闭规则全在内核的 {@link StreamCore}；这里只把它接到 Duplex 的读写回调上：
 *
 * - `_read` → `core.read()`，交出的块 `push` 给消费者（`push` 答 false 就停）；
 * - `_write` → `core.write()`，信用不够时回调挂起，背压经 Duplex 的写水位传给上游 `pipe`；
 * - `_final` → 发 `END`；`_destroy` → 发 `RST`（无错误时 `clientGone`）；
 * - 收 `RST` → `destroy(StreamResetError)`。
 *
 * 本类对角色无感：中继节点与假 core 都能用（只有中继发 `OPEN`）；TypeScript 版中继删除后只剩假 core 在用。
 */
import { Duplex } from "node:stream";

import {
  CreditPool,
  resetCodeFor,
  StreamCore,
  StreamResetError,
  type StreamFactory,
  type StreamHost as KernelStreamHost,
  type StreamOptions,
} from "@armadra/relay-kernel/tunnel/stream";

export { CreditPool, StreamResetError };

/** 隧道交给 {@link TunnelStream} 的宿主（流表里的对象就是它自己）。 */
export type StreamHost = KernelStreamHost<TunnelStream>;

export class TunnelStream extends Duplex {
  readonly streamId: number;
  private readonly core: StreamCore<TunnelStream>;

  constructor(streamId: number, host: StreamHost, options: StreamOptions) {
    super({ allowHalfOpen: true });
    this.streamId = streamId;
    this.core = new StreamCore(streamId, host, options, this, {
      push: (chunk) =>
        this.push(Buffer.from(chunk.buffer, chunk.byteOffset, chunk.length)),
      eof: () => {
        this.push(null);
      },
      reset: (error) => {
        this.destroy(error);
      },
    });
  }

  /** 对端 → 本端、本端 → 对端的载荷字节数（只计数，不留内容）。 */
  get bytesIn(): number {
    return this.core.bytesIn;
  }
  get bytesOut(): number {
    return this.core.bytesOut;
  }

  // ------------------------------------------------------------ 来自对端的帧

  /** 收到 DATA；超出授予的信用答 false（流控违规）。 */
  receiveData(bytes: Uint8Array): boolean {
    return this.core.receiveData(bytes);
  }

  receiveEnd(): void {
    this.core.receiveEnd();
  }

  receiveRst(code: number): void {
    this.core.receiveRst(code);
  }

  receiveWindow(credit: number): void {
    this.core.receiveWindow(credit);
  }

  // ------------------------------------------------------------ Duplex

  override _read(): void {
    this.core.read();
  }

  override _write(
    chunk: Buffer,
    _encoding: BufferEncoding,
    callback: (error?: Error | null) => void,
  ): void {
    this.core.write(chunk, () => callback());
  }

  override _final(callback: (error?: Error | null) => void): void {
    this.core.finish(() => callback());
  }

  override _destroy(
    error: Error | null,
    callback: (error?: Error | null) => void,
  ): void {
    this.core.destroy(resetCodeFor(error));
    callback(error);
  }

  // ------------------------------------------------------------ net.Socket 兼容

  // `http.Server.emit("connection")`、`http.request({ createConnection })` 与 `ws`
  // 会调用这些方法；隧道流没有 TCP 选项可设，全部是空操作。
  setNoDelay(): this {
    return this;
  }
  setKeepAlive(): this {
    return this;
  }
  setTimeout(_ms: number, callback?: () => void): this {
    if (callback) this.once("timeout", callback);
    return this;
  }
  ref(): this {
    return this;
  }
  unref(): this {
    return this;
  }
}

/** 隧道用的流工厂：每条 OPEN 一个 {@link TunnelStream}。 */
export const nodeStreams: StreamFactory<TunnelStream> = (
  streamId,
  host,
  options,
) => new TunnelStream(streamId, host, options);
