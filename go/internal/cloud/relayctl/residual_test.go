package relayctl_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

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

// relay.revoke / relay.kick 作业放宽 max_attempts；定时重放从 PostgreSQL 补回撤销，已有的键不覆盖。
func TestRevokeJobsAreNotCappedAndReplayHealsRevocations(t *testing.T) {
	p := newPartialEnv(t, writesToRevoked)
	node, _ := p.newNode("node-1")
	exp := inHour()
	if err := p.d.IndexIssued(ctx, relayctl.Issued{Subject: "a1", SourceID: "s1", DeviceID: "d1", JTI: "r.lost", ExpMs: exp}); err != nil {
		t.Fatal(err)
	}
	// Redis 只拒写、订阅与读正常：撤销落了 PostgreSQL，Redis 没写成。
	p.fail.Store(true)
	if _, err := p.d.RevokeWhere(ctx, relayctl.Filter{JTI: "r.lost"}); err == nil {
		t.Fatal("Redis 写失败要报错")
	}
	var maxAttempts int
	if err := p.db.Pool().QueryRow(ctx, "SELECT max_attempts FROM jobs WHERE kind = $1 AND dedupe_key = 'r.lost'", relayctl.JobRevoke).Scan(&maxAttempts); err != nil || maxAttempts != relayctl.MaxAttempts {
		t.Fatalf("relay.revoke 的 max_attempts：%d %v", maxAttempts, err)
	}
	if relayctl.MaxAttempts < 100 {
		t.Fatalf("上限应远大于默认的 8：%d", relayctl.MaxAttempts)
	}
	// 踢人作业同样放宽。
	p2 := newPartialEnv(t, func(_, name, _ string) bool { return name == "publish" })
	if err := p2.d.IndexIssued(ctx, relayctl.Issued{Subject: "a1", SourceID: "s1", DeviceID: "d1", JTI: "r.k", ExpMs: exp}); err != nil {
		t.Fatal(err)
	}
	p2.fail.Store(true)
	p2.d.Revocations().DeviceRevoked(ctx, "a1", "d1")
	if err := p2.db.Pool().QueryRow(ctx, "SELECT max_attempts FROM jobs WHERE kind = $1", relayctl.JobKick).Scan(&maxAttempts); err != nil || maxAttempts != relayctl.MaxAttempts {
		t.Fatalf("relay.kick 的 max_attempts：%d %v", maxAttempts, err)
	}

	// 作业运行器没来得及（或已用尽）：故障恢复后，定时重放把撤销补回 Redis。
	p.fail.Store(false)
	if ok, _ := node.Tokens().IsRevoked(ctx, "r.lost"); ok {
		t.Fatal("重放前节点不该知道")
	}
	// 已有的键不覆盖：先给另一枚令牌手工写一个特殊到期，重放后不变。
	keep := p.prefix + "revoked:r.keep"
	if err := p.d.IndexIssued(ctx, relayctl.Issued{Subject: "a1", SourceID: "s1", DeviceID: "d1", JTI: "r.keep", ExpMs: exp}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.d.RevokeWhere(ctx, relayctl.Filter{JTI: "r.keep"}); err != nil {
		t.Fatal(err)
	}
	before, _ := p.admin.PTTL(ctx, keep).Result()
	if err := p.admin.PExpire(ctx, keep, 30*time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	s := &relayctl.Sync{Store: p.st, Tenant: "cloud", Redis: p.rc, ReplayEvery: 50 * time.Millisecond}
	stop := s.StartRevocationReplay(ctx)
	defer stop()
	waitFor(t, "定时重放把撤销送到节点", func() bool { ok, _ := node.Tokens().IsRevoked(ctx, "r.lost"); return ok })
	if after, _ := p.admin.PTTL(ctx, keep).Result(); after > 30*time.Minute || after <= 0 || before <= 0 {
		t.Fatalf("已有的键不该被 NX 覆盖：before %v after %v", before, after)
	}
}
