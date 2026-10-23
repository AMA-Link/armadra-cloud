package saas

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/control/cluster"
	"github.com/AMA-Link/armadra-cloud/go/internal/edge"
)

type errorList struct {
	mu  sync.Mutex
	ops []string
}

func (l *errorList) add(op string, _ error) { l.mu.Lock(); l.ops = append(l.ops, op); l.mu.Unlock() }
func (l *errorList) has(prefix string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, op := range l.ops {
		if strings.HasPrefix(op, prefix) {
			n++
		}
	}
	return n
}

func testGate(t *testing.T, client goredis.UniversalClient, prefix string, errs *errorList) *gate {
	t.Helper()
	cfg := Config{TeamConcurrent: DefaultTeamConcurrent, TeamConnectPerMinute: DefaultTeamConnectPerMinute}
	mem := control.NewMemory(control.MemoryOptions{Limits: teamLimitRules(cfg)})
	g := newGate(client, prefix, "n", cfg.TeamConcurrent, func() control.Limiter { return mem.Limits() }, errs.add)
	t.Cleanup(g.close)
	return g
}

// 额度键坏了：报出来、按最严格的缺省；事故开关的值坏了：报出来、不改现状。
func TestGateReloadReportsBadValues(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	errs := &errorList{}
	g := testGate(t, r.admin, r.prefix, errs)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(r.admin.Set(ctx, r.prefix+"quota:team:good", cluster.EncodeQuotaValue(control.TeamQuota{Concurrent: 40}), 0).Err())
	must(r.admin.Set(ctx, r.prefix+"quota:team:bad", "not json", 0).Err())
	must(r.admin.Set(ctx, r.prefix+"incident", "{}", 0).Err())
	must(g.reload(ctx))
	if g.limit("good") != 40 || g.limit("bad") != DefaultTeamConcurrent || g.limit("missing") != DefaultTeamConcurrent {
		t.Fatalf("good=%d bad=%d missing=%d", g.limit("good"), g.limit("bad"), g.limit("missing"))
	}
	if errs.has("team quota decode") != 1 || errs.has("incident decode") != 1 || g.Rejecting() {
		t.Fatalf("%v", errs.ops)
	}
}

// 续期只续还在的成员：已释放的不被加回去。
func TestGateRefreshOnlyExistingMembers(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	g := testGate(t, r.admin, r.prefix, &errorList{})
	in := edge.GateInput{SourceID: "s", Subject: "acct", Team: "t1"}
	keep, code, err := g.Admit(ctx, in)
	if err != nil || code != "" {
		t.Fatal(code, err)
	}
	gone, _, _ := g.Admit(ctx, in)
	// 模拟「快照之后、续期之前」被释放：直接从 Redis 删掉，再续期。
	g.mu.Lock()
	var member string
	for m := range g.live["t1"] {
		member = m
		break
	}
	g.mu.Unlock()
	if err := r.admin.ZRem(ctx, r.prefix+"conc:team:t1", member).Err(); err != nil {
		t.Fatal(err)
	}
	g.refresh(ctx)
	if n, _ := r.admin.ZCard(ctx, r.prefix+"conc:team:t1").Result(); n != 1 {
		t.Fatalf("续期不应把删掉的成员加回去：%d", n)
	}
	keep()
	gone()
	g.release.Flush(5 * time.Second)
	if n, _ := r.admin.ZCard(ctx, r.prefix+"conc:team:t1").Result(); n != 0 {
		t.Fatalf("释放后：%d", n)
	}
}

// Redis 出错时团队并发放行：计数指标，日志限频；释放立即返回。
func TestGateFailOpenIsCountedAndThrottled(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	client := goredis.NewClient(&goredis.Options{Addr: addr, MaxRetries: -1})
	defer func() { _ = client.Close() }()
	errs := &errorList{}
	g := testGate(t, client, "relay-test:", errs)
	for i := 0; i < 3; i++ {
		release, code, err := g.Admit(context.Background(), edge.GateInput{Subject: "a", Team: "t"})
		if err != nil || code != "" || release == nil {
			t.Fatalf("应放行：%s %v", code, err)
		}
		release()
	}
	if g.failOpen.Load() != 3 || errs.has("team concurrency") != 1 {
		t.Fatalf("计数 %d、日志 %v", g.failOpen.Load(), errs.ops)
	}
	if !strings.Contains(g.render(), "relay_team_concurrency_fail_open_total 3") {
		t.Fatal(g.render())
	}
	if g.Refuse() != "" {
		t.Fatal("事故开关没开")
	}
}
