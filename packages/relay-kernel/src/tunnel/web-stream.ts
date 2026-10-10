/**
 * 隧道流的 Web Streams 适配：`readable`（对端 → 本端）与 `writable`（本端 → 对端），
 * 信用、队列与关闭规则全在 {@link StreamCore}。Web Streams 在 Node 与 Workers 上都有，
 * 所以这是隧道的缺省流；Node 运行时为了接 `http` 与 `ws` 另用 `stream.Duplex` 适配。
 *
 * - 读：`pull` → `core.read()`；内部队列满了（`desiredSize <= 0`）就停，等下一次 `pull`。
 *   `cancel` = 本地销毁（`RST clientGone`）。
 * - 写：每块等 `core.write()` 回调（信用背压）；`close` 发 `END`；`abort` 发 `RST`。
 * - 对端 `RST` / 隧道关闭：两边都以 {@link StreamResetError} 出错，在途的写被拒绝。
 */
import {
  resetCodeFor,
  StreamCore,
  StreamResetError,
  type StreamEndpoint,
  type StreamFactory,
  type StreamHost,
  type StreamOptions,
} from "./stream.ts";

export class WebTunnelStream implements StreamEndpoint {
  readonly streamId: number;
  readonly readable: ReadableStream<Uint8Array>;
  readonly writable: WritableStream<Uint8Array>;
  private readonly core: StreamCore<WebTunnelStream>;
  private controller: ReadableStreamDefaultController<Uint8Array> | undefined;
  private readableDone = false;
  private failure: Error | null = null;
  private rejectWrite: ((error: Error) => void) | null = null;

  constructor(
    streamId: number,
    host: StreamHost<WebTunnelStream>,
    options: StreamOptions,
  ) {
    this.streamId = streamId;
    this.core = new StreamCore(streamId, host, options, this, {
      push: (chunk) => {
        if (this.readableDone || !this.controller) return false;
        this.controller.enqueue(chunk);
        return (this.controller.desiredSize ?? 0) > 0;
      },
      eof: () => {
        if (this.readableDone) return;
        this.readableDone = true;
        this.controller?.close();
      },
      reset: (error) => this.fail(error),
    });
    this.readable = new ReadableStream<Uint8Array>({
      start: (controller) => {
        this.controller = controller;
      },
      pull: () => this.core.read(),
      cancel: () => {
        this.readableDone = true;
        this.destroy();
      },
    });
    this.writable = new WritableStream<Uint8Array>({
      write: (chunk) =>
        new Promise<void>((resolve, reject) => {
          if (this.failure) {
            reject(this.failure);
            return;
          }
          this.rejectWrite = reject;
          this.core.write(chunk, () => {
            this.rejectWrite = null;
            resolve();
          });
        }),
      close: () =>
        new Promise<void>((resolve, reject) => {
          if (this.failure) reject(this.failure);
          else this.core.finish(resolve);
        }),
      abort: (reason: unknown) => this.destroy(reason),
    });
  }

  get bytesIn(): number {
    return this.core.bytesIn;
  }
  get bytesOut(): number {
    return this.core.bytesOut;
  }
  get destroyed(): boolean {
    return this.core.destroyed;
  }

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

  /** 本地销毁：发 `RST`（`reason` 是 {@link StreamResetError} 时用它的码，没有时 `clientGone`）。 */
  destroy(reason?: unknown): void {
    if (this.core.destroyed) return;
    const error =
      reason instanceof Error
        ? reason
        : new StreamResetError(resetCodeFor(reason));
    this.settle(error);
    this.core.destroy(resetCodeFor(reason));
  }

  private fail(error: StreamResetError): void {
    if (this.core.destroyed) return;
    this.settle(error);
    this.core.destroy(error.rstCode);
  }

  private settle(error: Error): void {
    this.failure = error;
    if (!this.readableDone) {
      this.readableDone = true;
      this.controller?.error(error);
    }
    this.rejectWrite?.(error);
    this.rejectWrite = null;
  }
}

/** 缺省的流工厂：每条 OPEN 一个 {@link WebTunnelStream}。 */
export const webStreams: StreamFactory<WebTunnelStream> = (
  streamId,
  host,
  options,
) => new WebTunnelStream(streamId, host, options);
