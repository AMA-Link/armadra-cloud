/**
 * 日配额（中继设计 §4.2）：Workers Paid 没有硬花费上限，Worker 自己数「今天用了多少」，超了对
 * 源地址的请求与新隧道答 429 `limit_reached`（`/v1`、`/health` 不受影响），避免账单失控；Workers
 * Free 用一个低于 10 万的额度，免得整天失败。
 *
 * - 计数在 `ControlDO` 的同步 KV 里（UTC 自然日一个键）；
 * - `SourceDO` 先在内存里攒（请求、WS 入站消息按 20:1 折算、隧道载荷字节），攒够 100 次或 1 MiB
 *   或到检查周期才报一次，答「是否超了」；`ControlDO` 自己的 `/v1` 请求直接记。
 */

/** WS 入站消息按 20:1 折算成请求（与 DO 计费口径相同）。 */
export const MESSAGES_PER_REQUEST = 20;
const FLUSH_REQUESTS = 100;
const FLUSH_BYTES = 1024 * 1024;

export interface BudgetDelta {
  requests: number;
  bytes: number;
}

/** `SourceDO` 一侧：攒着，够了就报。 */
export class BudgetMeter {
  private requests = 0;
  private messages = 0;
  private pendingBytes = 0;
  private flushing: Promise<void> | null = null;
  exceeded = false;
  private readonly report: (delta: BudgetDelta) => Promise<boolean>;

  constructor(report: (delta: BudgetDelta) => Promise<boolean>) {
    this.report = report;
  }

  request(): void {
    this.requests += 1;
    this.maybeFlush();
  }

  message(): void {
    this.messages += 1;
    if (this.messages >= MESSAGES_PER_REQUEST) {
      this.messages -= MESSAGES_PER_REQUEST;
      this.requests += 1;
    }
    this.maybeFlush();
  }

  bytes(count: number): void {
    this.pendingBytes += count;
    this.maybeFlush();
  }

  /** 报掉攒着的（检查周期调）。 */
  flush(): Promise<void> {
    if (this.flushing) return this.flushing;
    if (this.requests === 0 && this.pendingBytes === 0)
      return Promise.resolve();
    const delta = { requests: this.requests, bytes: this.pendingBytes };
    this.requests = 0;
    this.pendingBytes = 0;
    this.flushing = this.report(delta)
      .then((exceeded) => {
        this.exceeded = exceeded;
      })
      .catch(() => {
        // 报不上：放回去下次再报，不因控制面一时不可达就拒绝服务。
        this.requests += delta.requests;
        this.pendingBytes += delta.bytes;
      })
      .finally(() => {
        this.flushing = null;
      });
    return this.flushing;
  }

  private maybeFlush(): void {
    if (this.requests >= FLUSH_REQUESTS || this.pendingBytes >= FLUSH_BYTES)
      void this.flush();
  }
}

/** UTC 自然日（`YYYY-MM-DD`）。 */
export function dayOf(nowMs: number): string {
  return new Date(nowMs).toISOString().slice(0, 10);
}

export interface BudgetUsage {
  day: string;
  requests: number;
  bytes: number;
}

/** `ControlDO` 自己的 `/v1` 请求先在内存里攒这么多次再写一次存储。 */
const LEDGER_BATCH = 50;

/** `ControlDO` 一侧：同步 KV 里的当日计数。 */
export class BudgetLedger {
  private readonly kv: SyncKvStorage;
  private pending = 0;
  private readonly limits: { requests: number; bytes: number };
  private readonly now: () => number;

  constructor(
    kv: SyncKvStorage,
    limits: { requests: number; bytes: number },
    now: () => number = Date.now,
  ) {
    this.kv = kv;
    this.limits = limits;
    this.now = now;
  }

  private key(day: string): string {
    return `budget:${day}`;
  }

  /** 记一次自己的请求（攒够一批才写存储）。 */
  note(): void {
    this.pending += 1;
    if (this.pending >= LEDGER_BATCH) this.add({ requests: 0, bytes: 0 });
  }

  usage(): BudgetUsage {
    if (this.pending > 0) return this.add({ requests: 0, bytes: 0 }).usage;
    return this.stored();
  }

  private stored(): BudgetUsage {
    const day = dayOf(this.now());
    const stored = this.kv.get<{ requests: number; bytes: number }>(
      this.key(day),
    );
    return { day, requests: stored?.requests ?? 0, bytes: stored?.bytes ?? 0 };
  }

  /** 记一笔，答记完之后是否超了；顺手删掉前几天的键。 */
  add(delta: BudgetDelta): { exceeded: boolean; usage: BudgetUsage } {
    const usage = this.stored();
    const pending = this.pending;
    this.pending = 0;
    const next = {
      requests:
        usage.requests + pending + Math.max(0, Math.floor(delta.requests)),
      bytes: usage.bytes + Math.max(0, Math.floor(delta.bytes)),
    };
    this.kv.put(this.key(usage.day), next);
    if (usage.requests === 0 && usage.bytes === 0)
      for (const [key] of this.kv.list({ prefix: "budget:" }))
        if (key !== this.key(usage.day)) this.kv.delete(key);
    const current = { day: usage.day, ...next };
    return { exceeded: this.over(current), usage: current };
  }

  exceeded(): boolean {
    return this.over(this.usage());
  }

  get limitRequests(): number {
    return this.limits.requests;
  }

  get limitBytes(): number {
    return this.limits.bytes;
  }

  private over(usage: BudgetUsage): boolean {
    return (
      (this.limits.requests > 0 && usage.requests >= this.limits.requests) ||
      (this.limits.bytes > 0 && usage.bytes >= this.limits.bytes)
    );
  }
}
