package core_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/core/coretest"
)

// slowPolicy 让 Watch 停在中间，模拟解析可见集合很慢（SaaS 取全部页）。
type slowPolicy struct {
	core.AccessPolicy
	entered chan struct{}
	release chan struct{}
	watcher core.Watcher
}

func (p *slowPolicy) Watch(context.Context, core.Caller) (core.Watcher, error) {
	close(p.entered)
	<-p.release
	return p.watcher, nil
}

type resyncWatcher struct {
	calls   atomic.Int64
	block   chan struct{}
	started chan struct{}
	once    sync.Once
}

func (w *resyncWatcher) Sees(string) bool   { return true }
func (w *resyncWatcher) Refresh(core.Event) {}
func (w *resyncWatcher) Close()             {}
func (w *resyncWatcher) Resync(context.Context) error {
	w.calls.Add(1)
	if w.started != nil {
		w.once.Do(func() { close(w.started) })
	}
	if w.block != nil {
		<-w.block
	}
	return nil
}

// 先登记、先订阅，再算可见集合：Watch 进行中发生的事件不会漏，解析完按序补投。
func TestAttachDoesNotLoseEventsEmittedWhileWatching(t *testing.T) {
	e := newEnv(t, coretest.Options{})
	policy := &slowPolicy{
		AccessPolicy: core.NewPersonalPolicy(e.h.Context),
		entered:      make(chan struct{}), release: make(chan struct{}), watcher: &resyncWatcher{},
	}
	e.h.Context.Policy = policy
	stream := core.NewMeStream(e.h.Context)
	var mu sync.Mutex
	var got []string
	attached := make(chan struct{})
	go func() {
		defer close(attached)
		_, err := stream.Attach(e.owner(), func(ev core.Event) {
			mu.Lock()
			got = append(got, ev.EventSource())
			mu.Unlock()
		}, nil)
		if err != nil {
			t.Error(err)
		}
	}()
	<-policy.entered
	e.h.Context.Emit(core.NewSourceRevoked("s1"), nil)
	e.h.Context.Emit(core.NewSourceRevoked("s2"), nil)
	close(policy.release)
	<-attached
	e.h.Context.Emit(core.NewSourceRevoked("s3"), nil)
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 || got[0] != "s1" || got[1] != "s2" || got[2] != "s3" {
		t.Fatalf("Watch 期间的事件要按序补投：%v", got)
	}
}

// 同一账号的重算合并：一轮在跑时来的多次请求只多算一轮；连接自己的重算函数也可用。
func TestResyncCoalescesPerAccount(t *testing.T) {
	e := newEnv(t, coretest.Options{})
	w := &resyncWatcher{block: make(chan struct{}), started: make(chan struct{})}
	e.h.Context.Policy = &slowPolicy{AccessPolicy: core.NewPersonalPolicy(e.h.Context), entered: make(chan struct{}), release: make(chan struct{}), watcher: w}
	close(e.h.Context.Policy.(*slowPolicy).release)
	stream := core.NewMeStream(e.h.Context)
	owner := e.owner()
	_, resync, err := stream.AttachResyncable(owner, func(core.Event) {}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); stream.Resync(context.Background(), owner.AccountID) }()
	<-w.started
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); stream.Resync(context.Background(), owner.AccountID) }()
	}
	time.Sleep(100 * time.Millisecond)
	close(w.block)
	wg.Wait()
	if n := w.calls.Load(); n != 2 {
		t.Fatalf("5 次请求应合并成 2 轮（在跑的一轮 + 一轮补算）：%d", n)
	}
	if err := resync(context.Background()); err != nil || w.calls.Load() != 3 {
		t.Fatalf("连接自己的重算：%v %d", err, w.calls.Load())
	}
}

// Watch 读库到登记之间的变动：登记先于读库，窗口里到的重算请求（成员变动）被记下，解析完补算一次，
// 不只靠 30 秒心跳。
func TestAttachResyncsOnceWhenChangeLandedDuringWatch(t *testing.T) {
	e := newEnv(t, coretest.Options{})
	w := &resyncWatcher{}
	policy := &slowPolicy{
		AccessPolicy: core.NewPersonalPolicy(e.h.Context),
		entered:      make(chan struct{}), release: make(chan struct{}), watcher: w,
	}
	e.h.Context.Policy = policy
	stream := core.NewMeStream(e.h.Context)
	owner := e.owner()
	attached := make(chan struct{})
	go func() {
		defer close(attached)
		if _, _, err := stream.AttachResyncable(owner, func(core.Event) {}, nil); err != nil {
			t.Error(err)
		}
	}()
	<-policy.entered
	// 此刻连接已登记：Prepare 能找到它。
	if accounts := stream.Accounts(); len(accounts) != 1 {
		t.Fatalf("Watch 进行中连接就该已登记：%v", accounts)
	}
	stream.Resync(context.Background(), owner.AccountID) // watcher 还没好：被跳过并记下
	if w.calls.Load() != 0 {
		t.Fatal("watcher 还没解析完，不该重算")
	}
	close(policy.release)
	<-attached
	if n := w.calls.Load(); n != 1 {
		t.Fatalf("解析完应补算恰好一次：%d", n)
	}
}

// 窗口里没有任何变动：不多算。
func TestAttachDoesNotResyncWithoutChange(t *testing.T) {
	e := newEnv(t, coretest.Options{})
	w := &resyncWatcher{}
	policy := &slowPolicy{AccessPolicy: core.NewPersonalPolicy(e.h.Context), entered: make(chan struct{}), release: make(chan struct{}), watcher: w}
	close(policy.release)
	e.h.Context.Policy = policy
	stream := core.NewMeStream(e.h.Context)
	if _, _, err := stream.AttachResyncable(e.owner(), func(core.Event) {}, nil); err != nil {
		t.Fatal(err)
	}
	if n := w.calls.Load(); n != 0 {
		t.Fatalf("没有变动不该补算：%d", n)
	}
}
