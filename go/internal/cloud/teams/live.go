package teams

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/relayctl"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
)

// liveLogEvery 是「读不到同时连接数」这条日志的最短间隔（另有失败计数）。
const liveLogEvery = 10 * time.Second

// Live 读团队此刻的同时连接数（中继的并发计数）。nil 或没有 Relay 时答 0；读失败答 0、限频记日志并累计失败数，
// 不让用量页整页失败。
type Live struct {
	relay    relayctl.Control
	log      *obs.Logger
	failures atomic.Int64
	lastLog  atomic.Int64
	now      func() time.Time
}

// NewLive 建读取器；relay 可为 nil，log 缺省不记。
func NewLive(relay relayctl.Control, log *obs.Logger) *Live {
	if log == nil {
		log = obs.Nop()
	}
	return &Live{relay: relay, log: log, now: time.Now}
}

// Failures 是读失败的累计次数（指标）。
func (l *Live) Failures() int64 {
	if l == nil {
		return 0
	}
	return l.failures.Load()
}

// Concurrent 答团队的同时连接数。
func (l *Live) Concurrent(ctx context.Context, teamID string) int {
	if l == nil || l.relay == nil {
		return 0
	}
	n, err := l.relay.TeamConcurrent(ctx, teamID)
	if err != nil {
		l.failures.Add(1)
		now := l.now().UnixMilli()
		if last := l.lastLog.Load(); now-last >= liveLogEvery.Milliseconds() && l.lastLog.CompareAndSwap(last, now) {
			l.log.Warn("读团队同时连接数失败，按 0 计", obs.Operation("teams.usage"), obs.Error(err))
		}
		return 0
	}
	return n
}
