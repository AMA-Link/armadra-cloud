package tunnel_test

import (
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/tunnel"
)

type item struct {
	name  string
	flow  tunnel.Flow
	left  int // 还剩几帧
	frame int // 每帧字节
}

func drain(q *tunnel.Sched[*item], budget int) []string {
	var out []string
	q.Drain(budget, func(it *item) (int, bool) {
		if it.left == 0 {
			return -1, false
		}
		it.left--
		out = append(out, it.name)
		return it.frame, it.left > 0
	})
	return out
}

func TestSchedRoundRobinFreshFirstAndBudget(t *testing.T) {
	var q tunnel.Sched[*item]
	bulk := []*item{{name: "a", left: 100, frame: 100}, {name: "b", left: 100, frame: 100}, {name: "c", left: 100, frame: 100}}
	for _, it := range bulk {
		q.Add(it, &it.flow, it.left*it.frame)
	}
	// 老流按轮转，每批到预算（250 字节 → 3 帧：100+100 未到、再一帧到 300）就停。
	if got := drain(&q, 250); len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("%v", got)
	}
	// 下一批仍从 a 开始，没有谁被跳过或重复。
	for _, it := range bulk {
		q.Add(it, &it.flow, it.left*it.frame) // 已在队列里：忽略
	}
	for round := 0; round < 4; round++ {
		drain(&q, 250)
	}
	counts := map[string]int{}
	for _, it := range bulk {
		counts[it.name] = 100 - it.left
	}
	if d := counts["a"] - counts["c"]; d > 1 || d < -1 {
		t.Fatalf("轮转份额不均：%v", counts)
	}
	// 稀疏流：停了几批之后再就绪，排在老流前面出帧。
	probe := &item{name: "p", left: 1, frame: 10}
	q.Add(probe, &probe.flow, 10)
	got := drain(&q, 250)
	if len(got) == 0 || got[0] != "p" {
		t.Fatalf("稀疏流应先出：%v", got)
	}
	// 老流每批至少一帧，哪怕预算不足、哪怕稀疏流很多。
	var many []*item
	for i := 0; i < 50; i++ {
		it := &item{name: "s", left: 1, frame: 100}
		many = append(many, it)
		q.Add(it, &it.flow, 100)
	}
	got = drain(&q, 100)
	hasOld := false
	for _, n := range got {
		if n == "a" || n == "b" || n == "c" {
			hasOld = true
		}
	}
	if !hasOld {
		t.Fatalf("老流被稀疏流饿死：%v", got)
	}
	_ = many
}

func TestSchedSkipsFinishedAndResets(t *testing.T) {
	var q tunnel.Sched[*item]
	dead := &item{name: "dead"}
	q.Add(dead, &dead.flow, 0)
	live := &item{name: "live", left: 2, frame: 1}
	q.Add(live, &live.flow, 2)
	got := drain(&q, 100)
	if len(got) != 1 || got[0] != "live" {
		t.Fatalf("%v", got)
	}
	// 摘掉的流可以再次加入。
	dead.left = 1
	q.Add(dead, &dead.flow, 1)
	if got := drain(&q, 100); len(got) != 2 {
		t.Fatalf("%v", got)
	}
	q.Reset()
	if got := drain(&q, 100); len(got) != 0 {
		t.Fatalf("重置之后应为空：%v", got)
	}
}

// 稀疏流 = 上一帧已经写出之后再就绪：批还没写完时，已写出帧的流再就绪仍是稀疏流（按批判定会让它进老流），
// 帧还没写出的流进老流（抢在帧与帧之间反复补数据换不到优先）。
func TestSchedSparseMeansPreviousFrameWritten(t *testing.T) {
	var q tunnel.Sched[*item]
	a := &item{name: "a", left: 100, frame: 100}
	b := &item{name: "b", left: 100, frame: 100}
	k := &item{name: "k", left: 1, frame: 10}
	s := &item{name: "s", left: 1, frame: 10}
	q.Add(k, &k.flow, 10)
	q.Add(s, &s.flow, 10)
	q.Add(a, &a.flow, 10000)
	q.Add(b, &b.flow, 10000)
	base := q.Drain(1000, func(it *item) (int, bool) {
		it.left--
		return it.frame, it.left > 0
	}) // 帧序：k=1 s=2 a=3 b=4
	// 写端写到第 1 帧：k 的帧已写出，s、a、b 的还没；这一批还没写完，k 与 s 又各来一个按键。
	q.NoteSent(base + 1)
	k.left, s.left = 1, 1
	q.Add(k, &k.flow, 10)
	q.Add(s, &s.flow, 10)
	var order []string
	q.Drain(1000, func(it *item) (int, bool) {
		if it.left == 0 {
			return -1, false
		}
		it.left--
		order = append(order, it.name)
		return it.frame, it.left > 0
	})
	pos := map[string]int{}
	for i, n := range order {
		pos[n] = i
	}
	if pos["k"] != 0 {
		t.Fatalf("上一帧已写出的流再就绪应先出：%v", order)
	}
	if pos["s"] < pos["a"] {
		t.Fatalf("上一帧还没写出的流不应抢在老流前面：%v", order)
	}
}
