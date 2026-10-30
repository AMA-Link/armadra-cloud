package obs

import (
	"math"
	"runtime/metrics"
	"strings"
	"testing"
)

// 运行时的细桶按上界累计进输出的桶：上界不超过 le 才计入，+Inf 与 _count 是总数。
func TestRenderSchedLatencyAccumulatesFineBuckets(t *testing.T) {
	h := &metrics.Float64Histogram{
		Buckets: []float64{math.Inf(-1), 0, 0.00005, 0.0002, 0.003, 0.03, 2, math.Inf(1)},
		Counts:  []uint64{0, 10, 5, 4, 3, 2, 1},
	}
	text := renderSchedLatency(h)
	for _, line := range []string{
		"# TYPE relay_sched_latency_seconds histogram",
		`relay_sched_latency_seconds_bucket{le="0.0001"} 10`,
		`relay_sched_latency_seconds_bucket{le="0.0005"} 15`,
		`relay_sched_latency_seconds_bucket{le="0.001"} 15`,
		`relay_sched_latency_seconds_bucket{le="0.005"} 19`,
		`relay_sched_latency_seconds_bucket{le="0.025"} 19`,
		`relay_sched_latency_seconds_bucket{le="0.05"} 22`,
		`relay_sched_latency_seconds_bucket{le="1"} 22`,
		`relay_sched_latency_seconds_bucket{le="+Inf"} 25`,
		"relay_sched_latency_seconds_count 25",
	} {
		if !strings.Contains(text, line+"\n") {
			t.Fatalf("缺少 %q：\n%s", line, text)
		}
	}
	if strings.Contains(text, "_sum") {
		t.Fatal("运行时不给总和，不输出 _sum")
	}
}

// 真实的运行时直方图读得出来，桶单调不减。
func TestRenderSchedLatencyReadsRuntime(t *testing.T) {
	text := RenderSchedLatency()
	if !strings.Contains(text, "relay_sched_latency_seconds_count ") {
		t.Fatalf("没有调度延迟：%q", text)
	}
	prev := -1
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "relay_sched_latency_seconds_bucket") {
			continue
		}
		var n int
		fields := strings.Fields(line)
		for _, ch := range fields[len(fields)-1] {
			n = n*10 + int(ch-'0')
		}
		if n < prev {
			t.Fatalf("桶要单调不减：%s", text)
		}
		prev = n
	}
}
