package dataplane_test

import (
	"context"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/budget"
	"github.com/AMA-Link/armadra-cloud/go/internal/dataplane"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil/relayenv"
	"github.com/AMA-Link/armadra-cloud/go/internal/traffic"
)

// 每日配额接在边缘准入上（#217）：令牌验过的通道占名额，超额的新通道以 4429 rate_limited 被拒，
// 关闭原因 daily_budget_exceeded；已建立的流不切断；匿名请求（无令牌）不烧配额。
func TestDailyBudgetGate(t *testing.T) {
	run := func(t *testing.T, limits budget.Limits) (*harness, *budget.Budget) {
		b := budget.New(limits, nil)
		h := newHarness(t, harnessOptions{plane: func(o *dataplane.Options) {
			o.Gate = &budget.Gate{Budget: b}
			o.Traffic = traffic.NewMeter(traffic.MeterOptions{Node: "relay-test", Write: func(context.Context, []traffic.Row) error { return nil }, OnBytes: b.AddBytes})
		}})
		h.core(relayenv.CoreOptions{Capabilities: []string{"e2e"}})
		return h, b
	}

	t.Run("通道数到线：已建立的流照常，新通道 4429", func(t *testing.T) {
		h, b := run(t, budget.Limits{Requests: 2})
		token := h.relayToken(nil)
		first, second := h.channel(token), h.channel(token)
		if _, _, open := first.outcome(); !open {
			t.Fatal("第 1 条应开成")
		}
		if _, _, open := second.outcome(); !open {
			t.Fatal("第 2 条应开成")
		}
		third := h.channel(token)
		info := third.wait(t)
		if info.code != 4429 || info.reason != "daily_budget_exceeded" {
			t.Fatalf("第 3 条应被 4429 daily_budget_exceeded 拒绝：%+v", info)
		}
		// 已建立的流不被切断，照常回显。
		if got := first.echo(t, []byte("still-open")); string(got) != "still-open" {
			t.Fatalf("%q", got)
		}
		if u := b.Usage(); u.Requests != 2 || u.Rejected != 1 {
			t.Fatalf("%+v", u)
		}
	})

	t.Run("匿名请求不烧配额", func(t *testing.T) {
		h, b := run(t, budget.Limits{Requests: 1})
		for i := 0; i < 3; i++ {
			info := h.channel("").wait(t)
			if info.code != 4401 {
				t.Fatalf("无令牌应 4401：%+v", info)
			}
		}
		if u := b.Usage(); u.Requests != 0 || u.Rejected != 0 {
			t.Fatalf("匿名请求不该计数：%+v", u)
		}
		if _, _, open := h.channel(h.relayToken(nil)).outcome(); !open {
			t.Fatal("有令牌的第 1 条应开成")
		}
	})

	t.Run("字节到线：经通道的字节双向计入，超额后新通道被拒、旧的不断", func(t *testing.T) {
		h, b := run(t, budget.Limits{Bytes: 3000})
		token := h.relayToken(nil)
		c := h.channel(token)
		for i := 0; i < 2; i++ {
			c.echo(t, make([]byte, 1000)) // 每次上下各 1000
		}
		eventually(t, "字节计入", func() bool { return b.Usage().Bytes >= 4000 })
		info := h.channel(token).wait(t)
		if info.code != 4429 || info.reason != "daily_budget_exceeded" {
			t.Fatalf("字节超额应拒绝新通道：%+v", info)
		}
		if got := c.echo(t, make([]byte, 500)); len(got) != 500 {
			t.Fatal("已建立的流不该被切断")
		}
	})
}
