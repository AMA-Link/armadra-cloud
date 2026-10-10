/**
 * 隧道流在 Workers 里的一端：信用与队列全在内核的 {@link StreamCore}；这里只把它接成「推给
 * 处理者」（HTTP 响应解析、WS 解帧）与「排队写」两件事，并在每次动过信用之后通知宿主
 * （`onActivity`：SourceDO 据此把快照写进 WS 附件，休眠前总是最新的）。
 */
import { RST } from "@armadra/platform-protocol/tunnel";
import {
  StreamCore,
  StreamResetError,
  type StreamCoreState,
  type StreamEndpoint,
  type StreamFactory,
  type StreamHost,
  type StreamOptions,
} from "@armadra/relay-kernel/tunnel/stream";

/** 处理者：交来的字节、对端 END、对端 RST（或隧道关闭）。 */
export interface EndpointHandler {
  /** 答 false = 暂时不要了（等 {@link TunnelEndpoint.read}）。 */
  data(chunk: Uint8Array): boolean;
  end(): void;
  reset(error: StreamResetError): void;
}

export class TunnelEndpoint implements StreamEndpoint {
  readonly streamId: number;
  readonly core: StreamCore<TunnelEndpoint>;
  private handler: EndpointHandler | null = null;
  private queue: Promise<void> = Promise.resolve();
  private failure: Error | null = null;
  private readonly rejects = new Set<(error: Error) => void>();
  private readonly activity: (endpoint: TunnelEndpoint) => void;

  constructor(
    streamId: number,
    host: StreamHost<TunnelEndpoint>,
    options: StreamOptions,
    onActivity: (endpoint: TunnelEndpoint) => void,
  ) {
    this.streamId = streamId;
    this.activity = onActivity;
    this.core = new StreamCore(streamId, host, options, this, {
      push: (chunk) => this.handler?.data(chunk) ?? false,
      eof: () => this.handler?.end(),
      reset: (error) => {
        this.fail(error);
        this.core.destroy(error.rstCode);
        this.handler?.reset(error);
      },
    });
  }

  /** 接上处理者并开始收（开流之后、下一个事件之前调用）。 */
  attach(handler: EndpointHandler): void {
    this.handler = handler;
    this.read();
  }

  /** 还原快照里的信用（休眠后唤醒）。 */
  restore(state: StreamCoreState): void {
    this.core.restore(state);
  }

  get idle(): boolean {
    return this.core.idle;
  }

  get destroyed(): boolean {
    return this.core.destroyed;
  }

  read(): void {
    this.core.read();
    this.onActivity();
  }

  /** 排队写一块；全部发出（拿到信用）后完成。流已重置 / 销毁则拒绝。 */
  write(bytes: Uint8Array): Promise<void> {
    const next = this.queue.then(
      () =>
        new Promise<void>((resolve, reject) => {
          if (this.failure) {
            reject(this.failure);
            return;
          }
          if (bytes.length === 0) {
            resolve();
            return;
          }
          this.rejects.add(reject);
          this.core.write(bytes, () => {
            this.rejects.delete(reject);
            this.onActivity();
            resolve();
          });
          this.onActivity();
        }),
    );
    this.queue = next.catch(() => {});
    return next;
  }

  /** 排在已有的写之后发 END。 */
  finish(): Promise<void> {
    const next = this.queue.then(
      () =>
        new Promise<void>((resolve, reject) => {
          if (this.failure) {
            reject(this.failure);
            return;
          }
          this.core.finish(() => resolve());
          this.onActivity();
        }),
    );
    this.queue = next.catch(() => {});
    return next;
  }

  /** 本地销毁（缺省 `RST clientGone`）。 */
  destroy(code: number = RST.clientGone): void {
    if (this.core.destroyed) return;
    this.fail(new StreamResetError(code));
    this.core.destroy(code);
    this.onActivity();
  }

  receiveData(bytes: Uint8Array): boolean {
    const ok = this.core.receiveData(bytes);
    this.onActivity();
    return ok;
  }
  receiveEnd(): void {
    this.core.receiveEnd();
    this.onActivity();
  }
  receiveRst(code: number): void {
    this.core.receiveRst(code);
    this.onActivity();
  }
  receiveWindow(credit: number): void {
    this.core.receiveWindow(credit);
    this.onActivity();
  }

  private onActivity(): void {
    this.activity(this);
  }

  private fail(error: Error): void {
    if (this.failure) return;
    this.failure = error;
    for (const reject of this.rejects) reject(error);
    this.rejects.clear();
  }
}

/** 隧道的流工厂：每条 OPEN（或还原的流）一个 {@link TunnelEndpoint}。 */
export function endpoints(
  onActivity: (endpoint: TunnelEndpoint) => void,
): StreamFactory<TunnelEndpoint> {
  return (streamId, host, options) =>
    new TunnelEndpoint(streamId, host, options, onActivity);
}
