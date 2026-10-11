/**
 * 中继指标（中继设计 §4）：文本格式（Prometheus exposition 0.0.4）。只在
 * `RELAY_METRICS_LISTEN` 单独的监听上暴露，可再要求 `Authorization: Bearer <METRICS_TOKEN>`。
 *
 * - `relay_tunnels`、`relay_streams`：当前值。
 * - `relay_bytes_total{direction}`：`to_source`（客户端 → 源）/ `to_client`（源 → 客户端）。
 * - `relay_edge_connections_total{result}`、`relay_handshake_total{result}`：按结果计数。
 * - `relay_edge_streams_total{kind}`：边缘开出的流，按 `OPEN.kind`（`http` / `ws` / `e2e`）。
 * - `relay_heartbeat_miss_total`：心跳超时断开的隧道数。
 * - `relay_handshake_seconds`：握手耗时直方图。
 */
export type ByteDirection = "to_source" | "to_client";
export type EdgeStreamKind = "http" | "ws" | "e2e";

const HANDSHAKE_BUCKETS = [0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10];

export class RelayMetrics {
  private readonly bytes: Record<ByteDirection, number> = {
    to_source: 0,
    to_client: 0,
  };
  private readonly edge = new Map<string, number>();
  private readonly edgeStreams = new Map<EdgeStreamKind, number>();
  private readonly handshakes = new Map<string, number>();
  private heartbeatMisses = 0;
  private readonly handshakeBuckets = HANDSHAKE_BUCKETS.map(() => 0);
  private handshakeCount = 0;
  private handshakeSum = 0;
  private readonly gauges: () => { tunnels: number; streams: number };

  constructor(gauges: () => { tunnels: number; streams: number }) {
    this.gauges = gauges;
  }

  addBytes(direction: ByteDirection, count: number): void {
    this.bytes[direction] += count;
  }

  edgeConnection(result: string): void {
    this.edge.set(result, (this.edge.get(result) ?? 0) + 1);
  }

  edgeStream(kind: EdgeStreamKind): void {
    this.edgeStreams.set(kind, (this.edgeStreams.get(kind) ?? 0) + 1);
  }

  handshake(result: string, seconds?: number): void {
    this.handshakes.set(result, (this.handshakes.get(result) ?? 0) + 1);
    if (seconds === undefined) return;
    this.handshakeCount += 1;
    this.handshakeSum += seconds;
    HANDSHAKE_BUCKETS.forEach((bound, index) => {
      if (seconds <= bound)
        this.handshakeBuckets[index] = (this.handshakeBuckets[index] ?? 0) + 1;
    });
  }

  heartbeatMiss(): void {
    this.heartbeatMisses += 1;
  }

  /** 测试用：某个计数器的当前值。 */
  snapshot() {
    return {
      ...this.gauges(),
      bytes: { ...this.bytes },
      edge: Object.fromEntries(this.edge),
      edgeStreams: Object.fromEntries(this.edgeStreams),
      handshakes: Object.fromEntries(this.handshakes),
      heartbeatMisses: this.heartbeatMisses,
    };
  }

  render(): string {
    const { tunnels, streams } = this.gauges();
    const lines: string[] = [];
    const family = (name: string, type: string, help: string) =>
      lines.push(`# HELP ${name} ${help}`, `# TYPE ${name} ${type}`);
    family("relay_tunnels", "gauge", "Open tunnels on this node.");
    lines.push(`relay_tunnels ${tunnels}`);
    family("relay_streams", "gauge", "Open streams on this node.");
    lines.push(`relay_streams ${streams}`);
    family("relay_bytes_total", "counter", "Payload bytes relayed.");
    for (const [direction, value] of Object.entries(this.bytes))
      lines.push(`relay_bytes_total{direction="${direction}"} ${value}`);
    family(
      "relay_edge_connections_total",
      "counter",
      "Client connections by result.",
    );
    for (const [result, value] of [...this.edge].sort())
      lines.push(`relay_edge_connections_total{result="${result}"} ${value}`);
    family(
      "relay_edge_streams_total",
      "counter",
      "Streams opened at the edge by kind.",
    );
    for (const [kind, value] of [...this.edgeStreams].sort())
      lines.push(`relay_edge_streams_total{kind="${kind}"} ${value}`);
    family("relay_handshake_total", "counter", "Tunnel handshakes by result.");
    for (const [result, value] of [...this.handshakes].sort())
      lines.push(`relay_handshake_total{result="${result}"} ${value}`);
    family(
      "relay_heartbeat_miss_total",
      "counter",
      "Tunnels closed after missed heartbeats.",
    );
    lines.push(`relay_heartbeat_miss_total ${this.heartbeatMisses}`);
    family(
      "relay_handshake_seconds",
      "histogram",
      "Tunnel handshake duration.",
    );
    HANDSHAKE_BUCKETS.forEach((bound, index) =>
      lines.push(
        `relay_handshake_seconds_bucket{le="${bound}"} ${this.handshakeBuckets[index]}`,
      ),
    );
    lines.push(
      `relay_handshake_seconds_bucket{le="+Inf"} ${this.handshakeCount}`,
      `relay_handshake_seconds_sum ${this.handshakeSum}`,
      `relay_handshake_seconds_count ${this.handshakeCount}`,
    );
    return `${lines.join("\n")}\n`;
  }
}
