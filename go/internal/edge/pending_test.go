package edge

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/netpoll"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
)

// 登记到绑定连接之间的窗口里被要求断开：绑定时要立即关掉，不能既脱离管理又不被断开。
func TestDisconnectBeforeBindClosesAtBind(t *testing.T) {
	e := &Edge{o: Options{Log: obs.Nop()}, wheel: newWheel(),
		policy: NewPolicy(PolicyOptions{Control: control.NewMemory(control.MemoryOptions{})})}
	ec := &edgeConn{e: e, sourceID: "s", subject: "acct", started: time.Now()}
	e.policy.Track(ec, "s", "acct")
	taken := e.policy.Take(ConnectionFilter{SourceID: "s"})
	if len(taken) != 1 {
		t.Fatalf("应摘到 1 条：%d", len(taken))
	}
	taken[0].Disconnect() // c 还空着
	fc := &fakeConn{h: ec}
	ec.bind(fc)
	fc.mu.Lock()
	closed := fc.closed
	fc.mu.Unlock()
	if !closed {
		t.Fatal("绑定时应立即关闭")
	}
	if !ec.isFinished() {
		t.Fatal("OnClose 应已跑过")
	}
}

func TestDisconnectAfterBindClosesNow(t *testing.T) {
	ec := &edgeConn{started: time.Now()}
	fc := &fakeConn{h: ec}
	ec.e = &Edge{o: Options{Log: obs.Nop()}, wheel: newWheel(),
		policy: NewPolicy(PolicyOptions{Control: control.NewMemory(control.MemoryOptions{})})}
	ec.bind(fc)
	ec.Disconnect()
	if !fc.closed {
		t.Fatal("应立即关闭")
	}
}

// slowLimits 的 Release 卡住，用来证明 OnClose 不会等它。
type slowPlane struct {
	*control.Memory
	release chan struct{}
	called  atomic.Int64
}

type slowLimiter struct{ p *slowPlane }

func (p *slowPlane) Limits() control.Limiter { return slowLimiter{p} }
func (l slowLimiter) Hit(ctx context.Context, k control.LimitKind, key string) (control.LimitResult, error) {
	return l.p.Memory.Limits().Hit(ctx, k, key)
}
func (l slowLimiter) Release(ctx context.Context, k control.LimitKind, key string) error {
	l.p.called.Add(1)
	<-l.p.release
	return l.p.Memory.Limits().Release(ctx, k, key)
}

func TestOnCloseDoesNotWaitForControlPlane(t *testing.T) {
	sp := &slowPlane{Memory: control.NewMemory(control.MemoryOptions{}), release: make(chan struct{})}
	e := &Edge{o: Options{Log: obs.Nop()}, wheel: newWheel(),
		policy: NewPolicy(PolicyOptions{Control: sp})}
	defer e.policy.Close()
	ec := &edgeConn{e: e, sourceID: "s", subject: "acct", started: time.Now()}
	fc := &fakeConn{h: ec}
	var c netpoll.Conn = fc
	e.wheel.add(ec)
	e.policy.Track(ec, "s", "acct")
	done := make(chan struct{})
	go func() { ec.OnClose(c, nil); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("OnClose 被控制面调用拖住了")
	}
	if tracked, wheel := e.policy.Tracked(), e.wheel.size(); tracked != 0 || wheel != 0 {
		t.Fatalf("登记没摘干净：%d %d", tracked, wheel)
	}
	close(sp.release)
	if !e.policy.Flush(3 * time.Second) {
		t.Fatal("没排空")
	}
	if sp.called.Load() != 1 {
		t.Fatalf("Release 应调用一次：%d", sp.called.Load())
	}
}
