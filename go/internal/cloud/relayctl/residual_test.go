package relayctl_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/relayctl"
	"github.com/AMA-Link/armadra-cloud/go/internal/control/cluster"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// scriptedStore 让 TeamEntitlements 按调用次数返回脚本里的并发额度（超出脚本取最后一个）：模拟额度变更与重放交错。
type scriptedStore struct {
	saas.Store
	script []int
	calls  atomic.Int64
}

func (s *scriptedStore) Tx(c context.Context, fn func(saas.Tx) error) error {
	return s.Store.Tx(c, func(tx saas.Tx) error { return fn(scriptedTx{Tx: tx, s: s}) })
}

type scriptedTx struct {
	saas.Tx
	s *scriptedStore
}

func (t scriptedTx) TeamEntitlements(context.Context, string) ([]saas.TeamEntitlement, error) {
	n := int(t.s.calls.Add(1)) - 1
	v := t.s.script[min(n, len(t.s.script)-1)]
	return []saas.TeamEntitlement{{TeamID: "t1", Plan: "free", Overrides: json.RawMessage(fmt.Sprintf(`{"concurrent": %d}`, v))}}, nil
}

func (s *scriptedStore) reads() int { return int(s.calls.Load()) }

func (p *partialEnv) quotaValue(team string) int {
	p.t.Helper()
	raw, err := p.admin.Get(ctx, p.prefix+"quota:team:"+team).Bytes()
	if err != nil {
		p.t.Fatal(err)
	}
	q, err := cluster.DecodeQuotaValue(team, raw)
	if err != nil {
		p.t.Fatal(err)
	}
	return q.Concurrent
}

// 额度重写的最后一轮写完后再读一次只比对：写完那一刻库里又变了，不能静悄悄地留着旧值。
func TestSyncQuotaFinalWriteIsVerified(t *testing.T) {
	p := newPartialEnv(t, func(string, string, string) bool { return false })
	// 前三次读各不相同（每轮都被交错的变更改掉），第四次（只比对）是库里的最新值：不一致，报错。
	unstable := &scriptedStore{Store: p.st, script: []int{11, 12, 13, 14}}
	s := &relayctl.Sync{Store: unstable, Tenant: "cloud", Redis: p.rc}
	if err := s.Run(ctx); !errors.Is(err, relayctl.ErrQuotaUnstable) {
		t.Fatalf("最后一轮写后库里又变了，应报 ErrQuotaUnstable：%v", err)
	}
	// 第三轮写完之后核对一致：成功，Redis 是最新值。
	stable := &scriptedStore{Store: p.st, script: []int{21, 22, 23, 23}}
	s = &relayctl.Sync{Store: stable, Tenant: "cloud", Redis: p.rc}
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := p.quotaValue("t1"); got != 23 || stable.reads() != 4 {
		t.Fatalf("Redis 额度 %d（应为 23），读 %d 次（应为 4）", got, stable.reads())
	}
	// 没有交错：写一轮、读一次核对就收敛。
	calm := &scriptedStore{Store: p.st, script: []int{7}}
	if err := (&relayctl.Sync{Store: calm, Tenant: "cloud", Redis: p.rc}).Run(ctx); err != nil || calm.reads() != 2 || p.quotaValue("t1") != 7 {
		t.Fatalf("无交错：%v 读 %d 次 额度 %d", err, calm.reads(), p.quotaValue("t1"))
	}
}

