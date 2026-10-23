package control_test

import (
	"fmt"
	"math"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/control"
)

func TestRendezvousScoreIsDeterministicAndFinite(t *testing.T) {
	a := control.RendezvousScore("0123456789abcdef0123456789abcdef", "a", 1)
	if a != control.RendezvousScore("0123456789abcdef0123456789abcdef", "a", 1) {
		t.Fatal("同输入应同分")
	}
	if math.IsInf(a, 0) || math.IsNaN(a) || a < 0 {
		t.Fatalf("分数应是有限的非负数：%v", a)
	}
	if got := control.RendezvousScore("s", "a", 2); math.Abs(got-control.RendezvousScore("s", "a", 1)/2) > 1e-12 {
		t.Fatalf("权重翻倍分数减半：%v", got)
	}
	if control.RendezvousScore("s", "a", 0) != control.RendezvousScore("s", "a", 1) {
		t.Fatal("权重 0 按 1 算")
	}
}

func TestRankNodesDropsDrainingAndAssignsWeights(t *testing.T) {
	nodes := []control.NodeInfo{
		{ID: "a", URL: "wss://a/t/v1", Weight: 1}, {ID: "b", URL: "wss://b/t/v1", Weight: 1},
		{ID: "c", URL: "wss://c/t/v1", Weight: 1, Draining: true}, {ID: "d", URL: "wss://d/t/v1", Weight: 1},
		{ID: "e", URL: "wss://e/t/v1", Weight: 1},
	}
	got := control.RankNodes("src", nodes)
	if len(got) != 3 {
		t.Fatalf("答前三个：%+v", got)
	}
	for i, n := range got {
		if n.ID == "c" {
			t.Fatal("排空的节点不进 hint")
		}
		if n.Weight != control.HintWeights[i] {
			t.Fatalf("第 %d 个权重 %d", i, n.Weight)
		}
	}
	if len(control.RankNodes("src", nodes[:1])) != 1 || len(control.RankNodes("src", nil)) != 0 {
		t.Fatal("节点不足三个时有几个答几个")
	}
}

// 加一个节点只把一部分源的首选挪到新节点上，其余源的首选不变（rendezvous 的性质）。
func TestRankNodesIsStableWhenANodeJoins(t *testing.T) {
	before := []control.NodeInfo{{ID: "a", Weight: 1}, {ID: "b", Weight: 1}, {ID: "c", Weight: 1}}
	after := append(append([]control.NodeInfo(nil), before...), control.NodeInfo{ID: "d", Weight: 1})
	moved, total := 0, 2000
	counts := map[string]int{}
	for i := 0; i < total; i++ {
		src := fmt.Sprintf("%032x", i)
		first, next := control.RankNodes(src, before)[0].ID, control.RankNodes(src, after)[0].ID
		counts[next]++
		if first != next {
			if next != "d" {
				t.Fatalf("%s 的首选从 %s 挪到了 %s（只应挪到新节点）", src, first, next)
			}
			moved++
		}
	}
	if moved < total/8 || moved > total*3/8 {
		t.Fatalf("约四分之一的源应挪到新节点：%d/%d", moved, total)
	}
	for _, id := range []string{"a", "b", "c", "d"} {
		if counts[id] < total/8 {
			t.Fatalf("分布太偏：%v", counts)
		}
	}
}

// 权重大的节点更常排第一。
func TestRankNodesHonoursWeight(t *testing.T) {
	nodes := []control.NodeInfo{{ID: "heavy", Weight: 3}, {ID: "light", Weight: 1}}
	heavy := 0
	for i := 0; i < 2000; i++ {
		if control.RankNodes(fmt.Sprintf("%032x", i), nodes)[0].ID == "heavy" {
			heavy++
		}
	}
	// 加权 rendezvous：首选概率 = 3 / (3 + 1)。
	if heavy < 1350 || heavy > 1650 {
		t.Fatalf("权重 3:1 时约 75%% 首选重的：%d/2000", heavy)
	}
}
