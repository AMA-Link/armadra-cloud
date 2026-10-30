package obs

import (
	"runtime/metrics"
	"strconv"
	"strings"
)

// schedLatencyName 是 Go 运行时的调度延迟：goroutine 就绪之后等多久才真正开始运行（自进程启动累计）。
const schedLatencyName = "/sched/latencies:seconds"

// schedBounds 是输出的桶上界（秒）：从 0.1 ms 到 1 s，告警看 p99 是否越过几十毫秒。
var schedBounds = [...]float64{0.0001, 0.0005, 0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1}

// RenderSchedLatency 把运行时的调度延迟直方图输出成 relay_sched_latency_seconds（Prometheus 文本格式）。
// 运行时的桶比这里细，这里只做累计：上界不超过 le 的运行时桶计进 le，所以各桶是偏小的下界估计；
// 运行时不给总和，所以没有 _sum。读不到（运行时不支持）时答空串。
func RenderSchedLatency() string {
	sample := []metrics.Sample{{Name: schedLatencyName}}
	metrics.Read(sample)
	if sample[0].Value.Kind() != metrics.KindFloat64Histogram {
		return ""
	}
	return renderSchedLatency(sample[0].Value.Float64Histogram())
}

func renderSchedLatency(h *metrics.Float64Histogram) string {
	var b strings.Builder
	b.WriteString("# HELP relay_sched_latency_seconds Go scheduler latency: time goroutines spend runnable before running.\n")
	b.WriteString("# TYPE relay_sched_latency_seconds histogram\n")
	var total uint64
	for _, c := range h.Counts {
		total += c
	}
	for _, bound := range schedBounds {
		var n uint64
		for i, c := range h.Counts {
			if i+1 < len(h.Buckets) && h.Buckets[i+1] <= bound {
				n += c
			}
		}
		b.WriteString(`relay_sched_latency_seconds_bucket{le="` + jsNumber(bound) + `"} ` + strconv.FormatUint(n, 10) + "\n")
	}
	b.WriteString(`relay_sched_latency_seconds_bucket{le="+Inf"} ` + strconv.FormatUint(total, 10) + "\n")
	b.WriteString("relay_sched_latency_seconds_count " + strconv.FormatUint(total, 10) + "\n")
	return b.String()
}
