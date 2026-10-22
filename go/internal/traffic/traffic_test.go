package traffic_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/traffic"
)

var t0 = time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)

const sourceID = "0123456789abcdef0123456789abcdef"

var key = traffic.Key{SourceID: sourceID, Subject: "acct:alice", DeviceID: "", Kind: traffic.KindHTTP}

var bg = context.Background()

type fixture struct {
	meter *traffic.Meter
	clock *atomic.Int64 // 毫秒
	mu    sync.Mutex
	batch [][]traffic.Row
	drops []int
}

type fixtureOptions struct {
	fail    *atomic.Bool
	delay   time.Duration
	maxRows int
}

func newMeter(o fixtureOptions) *fixture {
	f := &fixture{clock: &atomic.Int64{}}
	f.clock.Store(t0.UnixMilli())
	f.meter = traffic.NewMeter(traffic.MeterOptions{
		Node:           "relay-1",
		Now:            func() time.Time { return time.UnixMilli(f.clock.Load()) },
		MaxPendingRows: o.maxRows,
		Write: func(_ context.Context, rows []traffic.Row) error {
			if o.delay > 0 {
				time.Sleep(o.delay)
			}
			if o.fail != nil && o.fail.Load() {
				return errors.New("db down")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			f.batch = append(f.batch, slices.Clone(rows))
			return nil
		},
		OnError: func(_ error, dropped int) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.drops = append(f.drops, dropped)
		},
	})
	return f
}

func (f *fixture) advance(d time.Duration) { f.clock.Add(d.Milliseconds()) }

func mustFlush(t *testing.T, m *traffic.Meter) {
	t.Helper()
	if err := m.Flush(bg); err != nil {
		t.Fatal(err)
	}
}

func TestAggregation(t *testing.T) {
	t.Run("同一键的流加到同一行，不同键各一行；跨小时分到下一桶", func(t *testing.T) {
		f := newMeter(fixtureOptions{})
		a := f.meter.Counter(key)
		b := f.meter.Counter(key)
		wsKey := key
		wsKey.Kind, wsKey.DeviceID = traffic.KindWS, "dev-1"
		ws := f.meter.Counter(wsKey)
		a.Connection()
		b.Connection()
		a.Bytes(traffic.ToSource, 100)
		b.Bytes(traffic.ToClient, 1000)
		ws.Connection()
		ws.Bytes(traffic.ToClient, 7)
		f.advance(traffic.Bucket + 5*time.Millisecond)
		a.Bytes(traffic.ToClient, 1)
		f.meter.Add(traffic.Sample{Key: key, At: t0, BytesToSource: 3})
		mustFlush(t, f.meter)

		if len(f.batch) != 1 {
			t.Fatalf("批数 = %d", len(f.batch))
		}
		start := t0.UnixMilli()
		want := []traffic.Row{
			{BucketStartMs: start, Node: "relay-1", Key: key, Connections: 2, BytesToSource: 103, BytesToClient: 1000},
			{BucketStartMs: start, Node: "relay-1", Key: wsKey, Connections: 1, BytesToClient: 7},
			{BucketStartMs: start + traffic.BucketMs, Node: "relay-1", Key: key, BytesToClient: 1},
		}
		if !reflect.DeepEqual(f.batch[0], want) {
			t.Fatalf("got  %+v\nwant %+v", f.batch[0], want)
		}
	})

	t.Run("零计数的 Add 也建行；At 为零取现在", func(t *testing.T) {
		f := newMeter(fixtureOptions{})
		f.meter.Add(traffic.Sample{Key: key})
		rows := f.meter.Peek()
		if len(rows) != 1 || rows[0].BucketStartMs != t0.UnixMilli() || rows[0].Connections != 0 {
			t.Fatalf("%+v", rows)
		}
		rows[0].Connections = 99
		if f.meter.Peek()[0].Connections != 0 {
			t.Fatal("Peek 应返回副本")
		}
	})

	t.Run("桶是 UTC 整小时", func(t *testing.T) {
		cases := []struct {
			at   time.Time
			want int64
		}{
			{time.Date(2026, 10, 12, 8, 59, 59, 999e6, time.UTC), t0.UnixMilli()},
			{time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC), t0.UnixMilli() + traffic.BucketMs},
			{time.Date(2026, 10, 12, 16, 30, 0, 0, time.FixedZone("x", 8*3600)), t0.UnixMilli()},
			{time.UnixMilli(-1), -traffic.BucketMs},
		}
		for _, c := range cases {
			if got := traffic.BucketStart(c.at); got != c.want {
				t.Errorf("%v：%d != %d", c.at, got, c.want)
			}
		}
	})
}

func TestBatchWrite(t *testing.T) {
	t.Run("一次 flush 一批；没东西不写；flush 之后的字节进下一批", func(t *testing.T) {
		f := newMeter(fixtureOptions{})
		mustFlush(t, f.meter)
		if len(f.batch) != 0 {
			t.Fatal("没东西不应写")
		}
		c := f.meter.Counter(key)
		for range 1000 {
			c.Bytes(traffic.ToSource, 10)
		}
		if len(f.batch) != 0 || f.meter.Pending() != 1 {
			t.Fatalf("batches=%d pending=%d", len(f.batch), f.meter.Pending())
		}
		mustFlush(t, f.meter)
		c.Bytes(traffic.ToSource, 5)
		mustFlush(t, f.meter)
		mustFlush(t, f.meter)
		var got []int64
		for _, b := range f.batch {
			got = append(got, b[0].BytesToSource)
		}
		if !slices.Equal(got, []int64{10_000, 5}) || f.meter.Pending() != 0 {
			t.Fatalf("got %v pending %d", got, f.meter.Pending())
		}
	})

	t.Run("新的一行才通知（运行时据此安排延后的 flush），同一行再加不通知", func(t *testing.T) {
		var rows atomic.Int32
		m := traffic.NewMeter(traffic.MeterOptions{
			Node:  "n",
			Now:   func() time.Time { return t0 },
			Write: func(context.Context, []traffic.Row) error { return nil },
			OnRow: func() { rows.Add(1) },
		})
		c := m.Counter(key)
		c.Connection()
		c.Bytes(traffic.ToSource, 1)
		wsKey := key
		wsKey.Kind = traffic.KindWS
		m.Counter(wsKey).Connection()
		if rows.Load() != 2 {
			t.Fatalf("rows = %d", rows.Load())
		}
		mustFlush(t, m)
		c.Bytes(traffic.ToClient, 1)
		if rows.Load() != 3 {
			t.Fatalf("rows = %d", rows.Load())
		}
	})

	t.Run("写失败：并回内存，下次连同新的一起写；积压超限丢掉这一批并报告", func(t *testing.T) {
		var failing atomic.Bool
		failing.Store(true)
		f := newMeter(fixtureOptions{fail: &failing, maxRows: 2})
		c := f.meter.Counter(key)
		c.Bytes(traffic.ToSource, 1)
		if err := f.meter.Flush(bg); err == nil {
			t.Fatal("写失败应返回错误")
		}
		if !slices.Equal(f.drops, []int{0}) {
			t.Fatalf("drops = %v", f.drops)
		}
		c.Bytes(traffic.ToSource, 2)
		failing.Store(false)
		mustFlush(t, f.meter)
		if len(f.batch) != 1 || len(f.batch[0]) != 1 || f.batch[0][0].BytesToSource != 3 {
			t.Fatalf("%+v", f.batch)
		}
		failing.Store(true)
		for _, subject := range []string{"a", "b", "c"} {
			k := key
			k.Subject = subject
			f.meter.Counter(k).Connection()
		}
		_ = f.meter.Flush(bg)
		if f.drops[len(f.drops)-1] != 3 || f.meter.Pending() != 0 {
			t.Fatalf("drops=%v pending=%d", f.drops, f.meter.Pending())
		}
	})
}

// 全部批次按键加总（模拟存储的 upsert）。
func totals(batches [][]traffic.Row) map[string]traffic.Row {
	out := map[string]traffic.Row{}
	for _, b := range batches {
		for _, r := range b {
			id := fmt.Sprint(r.BucketStartMs, r.Node, r.SourceID, r.Subject, r.DeviceID, r.Kind)
			f := out[id]
			if f.Node == "" {
				f = r
			} else {
				f.Connections += r.Connections
				f.BytesToSource += r.BytesToSource
				f.BytesToClient += r.BytesToClient
			}
			out[id] = f
		}
	}
	return out
}

func TestConcurrent(t *testing.T) {
	t.Run("上千条流交错计数、慢存储上多次并发 flush：写进去的总数与加上的一字不差", func(t *testing.T) {
		f := newMeter(fixtureOptions{delay: time.Millisecond})
		keys := make([]traffic.Key, 20)
		for i := range keys {
			kind := traffic.KindHTTP
			if i%2 != 0 {
				kind = traffic.KindWS
			}
			keys[i] = traffic.Key{SourceID: sourceID, Subject: fmt.Sprintf("acct:%d", i%5), DeviceID: fmt.Sprintf("dev-%d", i%4), Kind: kind}
		}
		counters := make([]*traffic.Counter, 2000)
		for i := range counters {
			counters[i] = f.meter.Counter(keys[i%len(keys)])
		}
		const workers = 8
		var up, down, conns atomic.Int64
		var flushes sync.WaitGroup
		for round := range 50 {
			var wg sync.WaitGroup
			for w := range workers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					seed := uint64(42 + round*workers + w)
					next := func() uint64 {
						seed = (seed*6364136223846793005 + 1442695040888963407)
						return seed >> 33
					}
					for i := w; i < len(counters); i += workers {
						u, d := int(next()%1000), int(next()%5000)
						counters[i].Bytes(traffic.ToSource, u)
						counters[i].Bytes(traffic.ToClient, d)
						up.Add(int64(u))
						down.Add(int64(d))
						if next()%10 == 0 {
							counters[i].Connection()
							conns.Add(1)
						}
					}
				}()
			}
			if round%7 == 0 {
				flushes.Add(1)
				go func() {
					defer flushes.Done()
					_ = f.meter.Flush(bg)
				}()
			}
			wg.Wait()
			if round == 25 {
				f.advance(traffic.Bucket)
			}
		}
		flushes.Wait()
		mustFlush(t, f.meter)
		var gotUp, gotDown, gotConns int64
		rows := totals(f.batch)
		for _, r := range rows {
			gotUp += r.BytesToSource
			gotDown += r.BytesToClient
			gotConns += r.Connections
		}
		if gotUp != up.Load() || gotDown != down.Load() || gotConns != conns.Load() {
			t.Fatalf("写入 %d/%d/%d，加上 %d/%d/%d", gotUp, gotDown, gotConns, up.Load(), down.Load(), conns.Load())
		}
		// 20 个键 × 两个小时桶。
		if len(rows) != 40 {
			t.Fatalf("行数 = %d", len(rows))
		}
	})
}

func TestNilCounter(t *testing.T) {
	t.Run("nil 句柄什么也不做（未开统计时隧道传 nil）", func(t *testing.T) {
		var c *traffic.Counter
		c.Connection()
		c.Bytes(traffic.ToSource, 10)
		c.Bytes(traffic.ToClient, 10)
	})
}

func TestRun(t *testing.T) {
	t.Run("ctx 结束后最后 flush 一次", func(t *testing.T) {
		f := newMeter(fixtureOptions{})
		f.meter.Counter(key).Connection()
		ctx, cancel := context.WithCancel(bg)
		done := make(chan struct{})
		go func() { f.meter.Run(ctx); close(done) }()
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Run 没有退出")
		}
		if len(f.batch) != 1 {
			t.Fatalf("批数 = %d", len(f.batch))
		}
	})
}

func TestOverhead(t *testing.T) {
	t.Run("热路径每次计数是常数级的加法（百万次在百毫秒量级内）", func(t *testing.T) {
		f := newMeter(fixtureOptions{})
		c := f.meter.Counter(key)
		const rounds = 1_000_000
		for range 10_000 { // 预热
			c.Bytes(traffic.ToSource, 1)
		}
		start := time.Now()
		for i := range rounds {
			d := traffic.ToSource
			if i%2 != 0 {
				d = traffic.ToClient
			}
			c.Bytes(d, 16_384)
		}
		perCall := time.Since(start) / rounds
		if f.meter.Pending() != 1 {
			t.Fatalf("pending = %d", f.meter.Pending())
		}
		// 本机实测每次几十纳秒（竞态检测下几百纳秒）；这里只防退化成按帧拼键、查表或写库（那会是几十微秒级）。
		// 门槛放得宽，免得机器忙（CI、竞态检测）时误报。
		if perCall > 20*time.Microsecond {
			t.Fatalf("每次 %v", perCall)
		}
	})
}
