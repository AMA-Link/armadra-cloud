package obs

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Direction 是字节流向。
type Direction string

// 两个方向：to_source 是客户端到源，to_client 是源到客户端。
const (
	ToSource Direction = "to_source"
	ToClient Direction = "to_client"
)

var handshakeBuckets = [...]float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// Metrics 是中继指标（Prometheus 文本格式 0.0.4），输出与 TS 的 RelayMetrics.render 逐字节一致。
type Metrics struct {
	gauges      func() (tunnels, streams int)
	toSource    atomic.Int64
	toClient    atomic.Int64
	heartbeat   atomic.Int64
	mu          sync.Mutex
	edge        map[string]int64
	edgeStreams map[string]int64
	handshakes  map[string]int64
	buckets     [len(handshakeBuckets)]int64
	hsCount     int64
	hsSum       float64
}

// NewMetrics 创建指标；gauges 给出当前隧道数与流数。
func NewMetrics(gauges func() (tunnels, streams int)) *Metrics {
	if gauges == nil {
		gauges = func() (int, int) { return 0, 0 }
	}
	return &Metrics{
		gauges:      gauges,
		edge:        map[string]int64{},
		edgeStreams: map[string]int64{},
		handshakes:  map[string]int64{},
	}
}

// AddBytes 累加转发的字节数（热路径，原子操作）。
func (m *Metrics) AddBytes(d Direction, n int) {
	switch d {
	case ToSource:
		m.toSource.Add(int64(n))
	case ToClient:
		m.toClient.Add(int64(n))
	}
}

// EdgeConnection 按结果计一次客户端连接。
func (m *Metrics) EdgeConnection(result string) { m.inc(m.edge, result) }

// EdgeStream 按 OPEN.kind（http / ws / e2e）计一条边缘开出的流。
func (m *Metrics) EdgeStream(kind string) { m.inc(m.edgeStreams, kind) }

// Handshake 按结果计一次握手（不进直方图）。
func (m *Metrics) Handshake(result string) { m.inc(m.handshakes, result) }

// HandshakeDone 记一次成功握手：结果 "ok" 加一，并把耗时记进直方图。
func (m *Metrics) HandshakeDone(d time.Duration) {
	seconds := d.Seconds()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handshakes["ok"]++
	m.hsCount++
	m.hsSum += seconds
	for i, bound := range handshakeBuckets {
		if seconds <= bound {
			m.buckets[i]++
		}
	}
}

// HeartbeatMiss 计一条因心跳超时断开的隧道。
func (m *Metrics) HeartbeatMiss() { m.heartbeat.Add(1) }

func (m *Metrics) inc(target map[string]int64, key string) {
	m.mu.Lock()
	target[key]++
	m.mu.Unlock()
}

// Snapshot 是某一时刻的计数器值（测试用）。
type Snapshot struct {
	Tunnels, Streams int
	Bytes            map[Direction]int64
	Edge             map[string]int64
	EdgeStreams      map[string]int64
	Handshakes       map[string]int64
	HeartbeatMisses  int64
}

func cloneMap(src map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

// Snapshot 返回当前值的拷贝。
func (m *Metrics) Snapshot() Snapshot {
	tunnels, streams := m.gauges()
	m.mu.Lock()
	defer m.mu.Unlock()
	return Snapshot{
		Tunnels: tunnels, Streams: streams,
		Bytes:           map[Direction]int64{ToSource: m.toSource.Load(), ToClient: m.toClient.Load()},
		Edge:            cloneMap(m.edge),
		EdgeStreams:     cloneMap(m.edgeStreams),
		Handshakes:      cloneMap(m.handshakes),
		HeartbeatMisses: m.heartbeat.Load(),
	}
}

// sortedEntries 复刻 JS `[...map].sort()`：按 "键,值" 的字符串序排。
func sortedEntries(src map[string]int64) []string {
	type entry struct {
		key  string
		text string
	}
	entries := make([]entry, 0, len(src))
	for k, v := range src {
		entries = append(entries, entry{k, k + "," + strconv.FormatInt(v, 10)})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].text < entries[j].text })
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.key
	}
	return out
}

// Render 输出 Prometheus 文本格式。
func (m *Metrics) Render() string {
	tunnels, streams := m.gauges()
	m.mu.Lock()
	edge, edgeStreams, handshakes := cloneMap(m.edge), cloneMap(m.edgeStreams), cloneMap(m.handshakes)
	buckets, hsCount, hsSum := m.buckets, m.hsCount, m.hsSum
	m.mu.Unlock()

	var b strings.Builder
	line := func(s string) { b.WriteString(s); b.WriteByte('\n') }
	family := func(name, typ, help string) {
		line("# HELP " + name + " " + help)
		line("# TYPE " + name + " " + typ)
	}
	family("relay_tunnels", "gauge", "Open tunnels on this node.")
	line("relay_tunnels " + strconv.Itoa(tunnels))
	family("relay_streams", "gauge", "Open streams on this node.")
	line("relay_streams " + strconv.Itoa(streams))
	family("relay_bytes_total", "counter", "Payload bytes relayed.")
	line(`relay_bytes_total{direction="to_source"} ` + strconv.FormatInt(m.toSource.Load(), 10))
	line(`relay_bytes_total{direction="to_client"} ` + strconv.FormatInt(m.toClient.Load(), 10))
	family("relay_edge_connections_total", "counter", "Client connections by result.")
	for _, k := range sortedEntries(edge) {
		line(`relay_edge_connections_total{result="` + k + `"} ` + strconv.FormatInt(edge[k], 10))
	}
	family("relay_edge_streams_total", "counter", "Streams opened at the edge by kind.")
	for _, k := range sortedEntries(edgeStreams) {
		line(`relay_edge_streams_total{kind="` + k + `"} ` + strconv.FormatInt(edgeStreams[k], 10))
	}
	family("relay_handshake_total", "counter", "Tunnel handshakes by result.")
	for _, k := range sortedEntries(handshakes) {
		line(`relay_handshake_total{result="` + k + `"} ` + strconv.FormatInt(handshakes[k], 10))
	}
	family("relay_heartbeat_miss_total", "counter", "Tunnels closed after missed heartbeats.")
	line("relay_heartbeat_miss_total " + strconv.FormatInt(m.heartbeat.Load(), 10))
	family("relay_handshake_seconds", "histogram", "Tunnel handshake duration.")
	for i, bound := range handshakeBuckets {
		line(`relay_handshake_seconds_bucket{le="` + jsNumber(bound) + `"} ` + strconv.FormatInt(buckets[i], 10))
	}
	line(`relay_handshake_seconds_bucket{le="+Inf"} ` + strconv.FormatInt(hsCount, 10))
	line("relay_handshake_seconds_sum " + jsNumber(hsSum))
	line("relay_handshake_seconds_count " + strconv.FormatInt(hsCount, 10))
	return b.String()
}

// jsNumber 复刻 JS `String(number)`：最短往返表示，小于 1e-6 或不小于 1e21 用指数。
func jsNumber(f float64) string {
	if math.IsNaN(f) {
		return "NaN"
	}
	if math.IsInf(f, 1) {
		return "Infinity"
	}
	if math.IsInf(f, -1) {
		return "-Infinity"
	}
	if f == 0 {
		return "0"
	}
	abs := math.Abs(f)
	if abs >= 1e21 || abs < 1e-6 {
		s := strconv.FormatFloat(f, 'e', -1, 64) // 如 1e-07、1.5e+21
		mant, exp, _ := strings.Cut(s, "e")
		sign := exp[0]
		digits := strings.TrimLeft(exp[1:], "0")
		return mant + "e" + string(sign) + digits
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}
