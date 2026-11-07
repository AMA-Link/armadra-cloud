// Package budget 是个人中转的每日配额（#217）：同一个账号每个 UTC 日经中继的端到端通道数与字节数
// （双向合计）上限。缺省都是 0（不限）。
//
//   - 计数：通道数在边缘准入时（令牌验过之后，匿名请求烧不掉配额）加一；字节由流量累加器的
//     OnBytes 钩子累加。两个数都在内存里，热路径只是一次加法。
//   - 超额：新通道被拒（Gate 答 rate_limited，WS 关闭原因 daily_budget_exceeded）；已建立的流不切断，
//     字节照常累加。到下一个 UTC 零点自动清零。
//   - 对齐：Reconcile 周期性把内存里的数与流量账本（含别的节点已落库的、本节点还没落库的）对齐，
//     取较大者；进程重启后用同一个函数从账本恢复当天用量。对齐是近似的：先读账本再读内存里未落库的部分，
//     两次读取之间正好落库时会多算一小段（不超过一个落库周期的增量），只会偏严，不会偏松。
//   - 个人中转只有一个账号，所以配额按整个中转合计（含访客链接的流量）。
package budget

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/traffic"
)

// Limits 是每日上限；0 = 不限。
type Limits struct {
	// Requests 是每个 UTC 日的端到端通道数。
	Requests int64
	// Bytes 是每个 UTC 日经中继的字节数，两个方向合计。
	Bytes int64
}

// Enabled 是否限了什么。
func (l Limits) Enabled() bool { return l.Requests > 0 || l.Bytes > 0 }

// Budget 是当日用量的内存账。可并发使用。
type Budget struct {
	limits Limits
	now    func() time.Time

	mu       sync.Mutex
	day      int64 // 当前 UTC 日的起点（毫秒）
	requests int64
	bytes    int64
	rejected int64
	// warnedDay 是已经报过「用尽」的 UTC 日。
	warnedDay int64
}

// New 建当日账；now 缺省 time.Now。
func New(limits Limits, now func() time.Time) *Budget {
	if now == nil {
		now = time.Now
	}
	b := &Budget{limits: limits, now: now}
	b.day = dayStart(now().UnixMilli())
	return b
}

// Limits 是配置的上限。
func (b *Budget) Limits() Limits { return b.limits }

func dayStart(ms int64) int64 {
	start := ms - ms%store.DayMs
	if ms%store.DayMs < 0 {
		start -= store.DayMs
	}
	return start
}

// rollLocked 换日清零，答现在的毫秒。
func (b *Budget) rollLocked() int64 {
	nowMs := b.now().UnixMilli()
	if day := dayStart(nowMs); day != b.day {
		b.day, b.requests, b.bytes = day, 0, 0
	}
	return nowMs
}

// Verdict 是一次准入的结果。
type Verdict struct {
	OK bool
	// RetryAfter 是到下一个 UTC 零点的等待（被拒时）。
	RetryAfter time.Duration
	// First 为真表示这是当天第一次被拒（调用方记一条日志）。
	First bool
}

// Admit 为一条新通道占一个名额：没超额答 OK 并把通道数加一；通道数或字节数已到上限就拒绝（计入拒绝次数）。
func (b *Budget) Admit() Verdict {
	b.mu.Lock()
	defer b.mu.Unlock()
	nowMs := b.rollLocked()
	if (b.limits.Requests > 0 && b.requests >= b.limits.Requests) || (b.limits.Bytes > 0 && b.bytes >= b.limits.Bytes) {
		b.rejected++
		first := b.warnedDay != b.day
		b.warnedDay = b.day
		return Verdict{RetryAfter: time.Duration(b.day+store.DayMs-nowMs) * time.Millisecond, First: first}
	}
	b.requests++
	return Verdict{OK: true}
}

// AddBytes 累加经中继的字节数。超额不影响已建立的流，字节照样记。
func (b *Budget) AddBytes(n int64) {
	if n <= 0 {
		return
	}
	b.mu.Lock()
	b.rollLocked()
	b.bytes += n
	b.mu.Unlock()
}

// Usage 是当日用量与拒绝次数（拒绝次数跨日累计，进程内）。
type Usage struct {
	Requests, Bytes, Rejected int64
	// Day 是 UTC 日的起点（毫秒）。
	Day int64
}

// Usage 读当日用量。
func (b *Budget) Usage() Usage {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked()
	return Usage{Requests: b.requests, Bytes: b.bytes, Rejected: b.rejected, Day: b.day}
}

// Observe 把别处看到的当日用量并进来，取较大者；day 不是当前 UTC 日（对齐读账本期间跨了零点）就丢弃。
func (b *Budget) Observe(day, requests, bytes int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked()
	if day != b.day {
		return
	}
	b.requests = max(b.requests, requests)
	b.bytes = max(b.bytes, bytes)
}

// ledgerQueryLimit 是对齐时读账本的行数上限（按日合并之后一天的行数远小于它）。
const ledgerQueryLimit = 1_000_000

// Reconcile 读流量账本里当天的合计，加上累加器里还没落库的部分，并进内存账（取较大者）。
// meter 可以为 nil（账本已是全部）。账本读不出来返回错误，内存账不变。
func (b *Budget) Reconcile(ctx context.Context, ts store.TrafficStore, tenantID string, meter *traffic.Meter) error {
	day := dayStart(b.now().UnixMilli())
	rows, err := ts.Query(ctx, tenantID, store.TrafficQuery{
		FromMs: day, ToMs: day + store.DayMs, Granularity: store.GranularityDay, Limit: ledgerQueryLimit,
	})
	if err != nil {
		return err
	}
	var requests, bytes int64
	for _, row := range rows {
		requests += row.Connections
		bytes += row.BytesToSource + row.BytesToClient
	}
	if meter != nil {
		for _, row := range meter.Peek() {
			if row.BucketStartMs >= day && row.BucketStartMs < day+store.DayMs {
				requests += row.Connections
				bytes += row.BytesToSource + row.BytesToClient
			}
		}
	}
	b.Observe(day, requests, bytes)
	return nil
}

// Render 是 Prometheus 文本：当日用量、上限与拒绝次数；只列配了上限的项。
func (b *Budget) Render() string {
	u := b.Usage()
	var out strings.Builder
	gauge := func(name, help string, value int64) {
		fmt.Fprintf(&out, "# HELP %s %s\n# TYPE %s gauge\n%s %d\n", name, help, name, name, value)
	}
	if b.limits.Requests > 0 {
		gauge("relay_daily_requests_used", "Relay channels admitted today (UTC)", u.Requests)
		gauge("relay_daily_requests_limit", "Daily relay channel budget", b.limits.Requests)
	}
	if b.limits.Bytes > 0 {
		gauge("relay_daily_bytes_used", "Relayed bytes today (UTC), both directions", u.Bytes)
		gauge("relay_daily_bytes_limit", "Daily relayed byte budget", b.limits.Bytes)
	}
	fmt.Fprintf(&out, "# HELP relay_daily_budget_rejected_total Channels refused because the daily budget was used up\n"+
		"# TYPE relay_daily_budget_rejected_total counter\nrelay_daily_budget_rejected_total %d\n", u.Rejected)
	return out.String()
}
