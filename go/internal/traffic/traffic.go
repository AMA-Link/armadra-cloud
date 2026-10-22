// Package traffic 是中继流量统计（cloud-api §19）的内核部分：与运行时、存储都无关的内存累加器。
//
//   - 维度：UTC 整小时桶 × 节点 × 源 × 中继令牌 sub × dev × 类别（e2e / http / ws / media）；
//     计数：开流次数、两个方向的载荷字节（与 relay_bytes_total 同源：隧道 DATA 帧的载荷）。
//   - 热路径只做加法：每条流开流时拿一个 Counter，之后每帧只是对缓存的那一行加一个数，
//     不拼键、不查表、不碰库；跨小时或刚 Flush 过才重新定位一次。
//   - 落库由运行时定时调 Flush（Run 每 60 秒），一次把攒下的行交给 Writer（存储端口的批量 upsert，
//     同键相加）。写失败的行并回内存，下次再写；积压超过上限才丢弃并报告。Flush 串行，不会两批同时写。
//
// 只记元数据：没有路径、头、IP 与正文。
package traffic

import (
	"context"
	"sync"
	"time"
)

// Kind 是流的类别。
type Kind string

// 流的类别。
const (
	KindE2E   Kind = "e2e"
	KindHTTP  Kind = "http"
	KindWS    Kind = "ws"
	KindMedia Kind = "media"
)

const (
	// Bucket 是统计桶的长度（UTC 整小时）。
	Bucket = time.Hour
	// BucketMs 同 Bucket，单位毫秒。
	BucketMs = int64(Bucket / time.Millisecond)
	// FlushInterval 是 Run 的落库周期。
	FlushInterval = 60 * time.Second
	// MaxPendingRows：写不进去时最多在内存里攒这么多行，再多就丢弃这一批并报告。
	MaxPendingRows = 50_000
	finalFlushWait = 5 * time.Second
)

// Key 是一条流归到哪一行（不含时间与节点）。
type Key struct {
	SourceID string
	// Subject 是中继令牌的 sub。
	Subject string
	// DeviceID 是中继令牌的 dev；没带时为空串。
	DeviceID string
	Kind     Kind
}

// Row 是一行统计。
type Row struct {
	BucketStartMs int64
	Node          string
	Key
	Connections int64
	// BytesToSource 是客户端到源的字节。
	BytesToSource int64
	// BytesToClient 是源到客户端的字节。
	BytesToClient int64
}

// Sample 是一次累加；At 为零值时取现在。
type Sample struct {
	Key
	At            time.Time
	Connections   int64
	BytesToSource int64
	BytesToClient int64
}

// Direction 是字节的方向。
type Direction int

// 字节的方向。
const (
	ToSource Direction = iota
	ToClient
)

// Writer 把一批行写进存储（同键相加）。
type Writer func(ctx context.Context, rows []Row) error

// MeterOptions 配置 Meter。
type MeterOptions struct {
	Node  string
	Write Writer
	// Now 供测试换时钟，缺省 time.Now。
	Now func() time.Time
	// OnError 写失败时报告（dropped = 因积压超限丢掉的行数，0 表示已并回内存）。
	OnError func(err error, dropped int)
	// OnRow 攒下了新的一行（上次 Flush 之后第一次见到这个桶与键）时调用，调用时不持锁。
	OnRow func()
	// MaxPendingRows 缺省 MaxPendingRows。
	MaxPendingRows int
}

// BucketStart 返回 t 所在 UTC 整小时桶的起点（毫秒）。
func BucketStart(t time.Time) int64 { return bucketStartMs(t.UnixMilli()) }

func bucketStartMs(ms int64) int64 {
	start := ms - ms%BucketMs
	if ms%BucketMs < 0 {
		start -= BucketMs
	}
	return start
}

type rowID struct {
	bucketStartMs int64
	key           Key
}

// Meter 是内存累加器，可并发使用。
type Meter struct {
	node    string
	write   Writer
	now     func() time.Time
	onError func(err error, dropped int)
	onRow   func()
	maxRows int

	flushMu sync.Mutex // 串行化 Flush

	mu sync.Mutex
	// generation 每次 Flush 换一代：句柄发现代变了就重新定位，不会加到已经交出去的行上。
	generation uint64
	rows       map[rowID]*Row
	order      []*Row // 按建行顺序，Flush 与 Peek 保持这个顺序
}

// NewMeter 建一个累加器。
func NewMeter(o MeterOptions) *Meter {
	m := &Meter{
		node: o.Node, write: o.Write, now: o.Now, onError: o.OnError, onRow: o.OnRow,
		maxRows: o.MaxPendingRows, rows: map[rowID]*Row{},
	}
	if m.now == nil {
		m.now = time.Now
	}
	if m.maxRows <= 0 {
		m.maxRows = MaxPendingRows
	}
	return m
}

// Counter 返回一条流的计数句柄。
func (m *Meter) Counter(k Key) *Counter { return &Counter{meter: m, key: k} }

// Add 做一次累加。
func (m *Meter) Add(s Sample) {
	m.mu.Lock()
	at := s.At
	if at.IsZero() {
		at = m.now()
	}
	row, created := m.rowForLocked(BucketStart(at), s.Key)
	row.Connections += s.Connections
	row.BytesToSource += s.BytesToSource
	row.BytesToClient += s.BytesToClient
	m.mu.Unlock()
	if created && m.onRow != nil {
		m.onRow()
	}
}

// Pending 返回攒着还没写的行数。
func (m *Meter) Pending() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rows)
}

// Peek 返回攒着的行的副本（不清空；测试与 status 看内存里的数用）。
func (m *Meter) Peek() []Row {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Row, len(m.order))
	for i, r := range m.order {
		out[i] = *r
	}
	return out
}

// rowForLocked 取这一桶这一键的行，没有就建。调用方持锁。
func (m *Meter) rowForLocked(bucketStartMs int64, k Key) (row *Row, created bool) {
	id := rowID{bucketStartMs, k}
	if r, ok := m.rows[id]; ok {
		return r, false
	}
	r := &Row{BucketStartMs: bucketStartMs, Node: m.node, Key: k}
	m.rows[id] = r
	m.order = append(m.order, r)
	return r, true
}

func (m *Meter) notify(created int) {
	for range created {
		if m.onRow != nil {
			m.onRow()
		}
	}
}

// Flush 把攒下的行交给 Writer；串行，返回这一批写入的错误（已按规则并回内存或丢弃并报告）。
// 没有攒着的行时不写。
func (m *Meter) Flush(ctx context.Context) error {
	m.flushMu.Lock()
	defer m.flushMu.Unlock()

	m.mu.Lock()
	if len(m.rows) == 0 {
		m.mu.Unlock()
		return nil
	}
	batch := make([]Row, len(m.order))
	for i, r := range m.order {
		batch[i] = *r
	}
	m.rows = map[rowID]*Row{}
	m.order = nil
	m.generation++
	m.mu.Unlock()

	// 交给 Writer 的是副本，之后谁也不会再动它。
	err := m.write(ctx, batch)
	if err == nil {
		return nil
	}
	m.mu.Lock()
	if len(m.rows)+len(batch) > m.maxRows {
		m.mu.Unlock()
		if m.onError != nil {
			m.onError(err, len(batch))
		}
		return err
	}
	created := 0
	for _, r := range batch {
		target, c := m.rowForLocked(r.BucketStartMs, r.Key)
		if c {
			created++
		}
		target.Connections += r.Connections
		target.BytesToSource += r.BytesToSource
		target.BytesToClient += r.BytesToClient
	}
	m.mu.Unlock()
	m.notify(created)
	if m.onError != nil {
		m.onError(err, 0)
	}
	return err
}

// Run 每 FlushInterval 调一次 Flush，直到 ctx 结束，然后用新的 5 秒 context 最后 Flush 一次。
func (m *Meter) Run(ctx context.Context) {
	tick := time.NewTicker(FlushInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			final, cancel := context.WithTimeout(context.Background(), finalFlushWait)
			defer cancel()
			_ = m.Flush(final)
			return
		case <-tick.C:
			_ = m.Flush(ctx)
		}
	}
}

// Counter 是一条流的计数句柄：开流时拿，之后每帧调 Bytes。nil 的 Counter 什么也不做。
type Counter struct {
	meter *Meter
	key   Key

	// 以下由 meter.mu 保护。
	row         *Row
	generation  uint64
	hasRow      bool
	bucketEndMs int64
}

// Connection 记开了一条流（HTTP 请求、WS、媒体票兑取、端到端通道）。
func (c *Counter) Connection() {
	if c == nil {
		return
	}
	c.add(func(r *Row) { r.Connections++ })
}

// Bytes 记 n 字节载荷。
func (c *Counter) Bytes(d Direction, n int) {
	if c == nil {
		return
	}
	if d == ToSource {
		c.add(func(r *Row) { r.BytesToSource += int64(n) })
	} else {
		c.add(func(r *Row) { r.BytesToClient += int64(n) })
	}
}

func (c *Counter) add(apply func(*Row)) {
	m := c.meter
	m.mu.Lock()
	nowMs := m.now().UnixMilli()
	created := false
	if !c.hasRow || c.generation != m.generation || nowMs >= c.bucketEndMs {
		start := bucketStartMs(nowMs)
		c.row, created = m.rowForLocked(start, c.key)
		c.generation = m.generation
		c.bucketEndMs = start + BucketMs
		c.hasRow = true
	}
	apply(c.row)
	m.mu.Unlock()
	if created && m.onRow != nil {
		m.onRow()
	}
}
