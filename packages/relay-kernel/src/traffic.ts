/**
 * 中继流量统计（cloud-api §19）的内核部分：与运行时、存储都无关的内存累加器。
 *
 * - 维度：UTC 整小时桶 × 节点 × 源 × 中继令牌 `sub` × `dev` × 类别（`e2e` / `http` / `ws` / `media`）；
 *   计数：开流次数、两个方向的载荷字节（与 `relay_bytes_total` 同源：隧道 `DATA` 帧的载荷）。
 * - 热路径只做加法：每条流开流时拿一个 {@link TrafficCounter}，之后每帧只是对缓存的那一行加一个数，
 *   不拼键、不查表、不碰库；跨小时或刚 flush 过才重新定位一次。
 * - 落库由运行时定时调 {@link TrafficMeter.flush}（Node 每 60 秒；Workers 在一阵流量之后），一次把攒下
 *   的行交给 `write`（存储端口的批量 upsert，同键相加）。写失败的行并回内存，下次再写；积压超过上限
 *   才丢弃并报告。flush 串行，不会两批同时写。
 *
 * 只记元数据：没有路径、头、IP 与正文。
 */

export const TRAFFIC_KINDS = ["e2e", "http", "ws", "media"] as const;
export type TrafficKind = (typeof TRAFFIC_KINDS)[number];

export const TRAFFIC_BUCKET_MS = 3_600_000;
/** Node 中继的落库周期。 */
export const TRAFFIC_FLUSH_MS = 60_000;
/** 写不进去时最多在内存里攒这么多行，再多就丢弃这一批并报告。 */
export const TRAFFIC_MAX_PENDING_ROWS = 50_000;

/** 一条流归到哪一行（不含时间与节点）。 */
export interface TrafficKey {
  sourceId: string;
  /** 中继令牌的 `sub`。 */
  subject: string;
  /** 中继令牌的 `dev`；没带时为空串。 */
  deviceId: string;
  kind: TrafficKind;
}

export interface TrafficRow extends TrafficKey {
  bucketStartMs: number;
  node: string;
  connections: number;
  /** 客户端 → 源。 */
  bytesToSource: number;
  /** 源 → 客户端。 */
  bytesToClient: number;
}

/** 一次累加：缺省的计数是 0，`atMs` 缺省是现在。 */
export interface TrafficSample extends TrafficKey {
  atMs?: number;
  connections?: number;
  bytesToSource?: number;
  bytesToClient?: number;
}

/** 统计的去处：累加（同步、只碰内存）与落库（异步、按批）。 */
export interface TrafficSink {
  add(sample: TrafficSample): void;
  flush(): Promise<void>;
}

/** 把一批行写进存储（同键相加）。 */
export type TrafficWriter = (rows: TrafficRow[]) => Promise<void>;

export interface TrafficMeterOptions {
  node: string;
  write: TrafficWriter;
  now?: () => number;
  /** 写失败时报告（`dropped` = 因积压超限丢掉的行数，0 表示已并回内存）。 */
  onError?: (error: unknown, dropped: number) => void;
  maxPendingRows?: number;
}

export function trafficBucketStart(atMs: number): number {
  return Math.floor(atMs / TRAFFIC_BUCKET_MS) * TRAFFIC_BUCKET_MS;
}

function keyOf(bucketStartMs: number, key: TrafficKey): string {
  return `${bucketStartMs}\u0000${key.sourceId}\u0000${key.subject}\u0000${key.deviceId}\u0000${key.kind}`;
}

/** 一条流的计数句柄：开流时拿，之后每帧调 {@link bytes}。 */
export class TrafficCounter {
  readonly key: TrafficKey;
  private readonly meter: TrafficMeter;
  private row: TrafficRow | null = null;
  private generation = -1;
  private bucketEndMs = 0;

  constructor(meter: TrafficMeter, key: TrafficKey) {
    this.meter = meter;
    this.key = key;
  }

  /** 开了一条流（HTTP 请求、WS、媒体票兑取、端到端通道）。 */
  connection(): void {
    this.slot().connections += 1;
  }

  bytes(direction: "to_source" | "to_client", count: number): void {
    const row = this.slot();
    if (direction === "to_source") row.bytesToSource += count;
    else row.bytesToClient += count;
  }

  /** 当前该加到的那一行：跨了小时或 flush 过（换了一代）才重新定位。 */
  private slot(): TrafficRow {
    const nowMs = this.meter.now();
    if (
      this.row !== null &&
      this.generation === this.meter.generation &&
      nowMs < this.bucketEndMs
    )
      return this.row;
    const bucketStartMs = trafficBucketStart(nowMs);
    this.row = this.meter.rowFor(bucketStartMs, this.key);
    this.generation = this.meter.generation;
    this.bucketEndMs = bucketStartMs + TRAFFIC_BUCKET_MS;
    return this.row;
  }
}

export class TrafficMeter implements TrafficSink {
  readonly node: string;
  readonly now: () => number;
  /** 每次 flush 换一代：句柄发现代变了就重新定位，不会加到已经交出去的行上。 */
  generation = 0;
  private rows = new Map<string, TrafficRow>();
  private chain: Promise<void> = Promise.resolve();
  private readonly write: TrafficWriter;
  private readonly onError: TrafficMeterOptions["onError"];
  private readonly maxPendingRows: number;

  constructor(options: TrafficMeterOptions) {
    this.node = options.node;
    this.write = options.write;
    this.now = options.now ?? Date.now;
    this.onError = options.onError;
    this.maxPendingRows = options.maxPendingRows ?? TRAFFIC_MAX_PENDING_ROWS;
  }

  /** 一条流的计数句柄。 */
  counter(key: TrafficKey): TrafficCounter {
    return new TrafficCounter(this, {
      sourceId: key.sourceId,
      subject: key.subject,
      deviceId: key.deviceId,
      kind: key.kind,
    });
  }

  add(sample: TrafficSample): void {
    const row = this.rowFor(
      trafficBucketStart(sample.atMs ?? this.now()),
      sample,
    );
    row.connections += sample.connections ?? 0;
    row.bytesToSource += sample.bytesToSource ?? 0;
    row.bytesToClient += sample.bytesToClient ?? 0;
  }

  /** 攒着还没写的行数。 */
  get pending(): number {
    return this.rows.size;
  }

  /** 攒着的行（不清空；测试与 `status` 看内存里的数用）。 */
  peek(): TrafficRow[] {
    return [...this.rows.values()].map((row) => ({ ...row }));
  }

  /** @internal 句柄用：这一桶这一键的行（没有就建）。 */
  rowFor(bucketStartMs: number, key: TrafficKey): TrafficRow {
    const id = keyOf(bucketStartMs, key);
    let row = this.rows.get(id);
    if (!row) {
      row = {
        bucketStartMs,
        node: this.node,
        sourceId: key.sourceId,
        subject: key.subject,
        deviceId: key.deviceId,
        kind: key.kind,
        connections: 0,
        bytesToSource: 0,
        bytesToClient: 0,
      };
      this.rows.set(id, row);
    }
    return row;
  }

  /** 把攒下的行交给 `write`；串行，答这一次（以及排在前面的）写完。 */
  flush(): Promise<void> {
    this.chain = this.chain.then(() => this.flushOnce());
    return this.chain;
  }

  private async flushOnce(): Promise<void> {
    if (this.rows.size === 0) return;
    const batch = [...this.rows.values()];
    this.rows = new Map();
    this.generation += 1;
    try {
      await this.write(batch);
    } catch (error) {
      if (this.rows.size + batch.length > this.maxPendingRows) {
        this.onError?.(error, batch.length);
        return;
      }
      for (const row of batch) {
        const target = this.rowFor(row.bucketStartMs, row);
        target.connections += row.connections;
        target.bytesToSource += row.bytesToSource;
        target.bytesToClient += row.bytesToClient;
      }
      this.onError?.(error, 0);
    }
  }
}
