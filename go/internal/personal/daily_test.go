package personal

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/traffic"
)

func TestArgsServeDaily(t *testing.T) {
	base := args("--data-dir", "d", "--host", "127.0.0.1")
	o := mustServe(t, base, Env{})
	if o.DailyRequests != 0 || o.DailyBytes != 0 {
		t.Fatalf("缺省不限：%+v", o)
	}
	cases := []struct {
		bytes string
		want  int64
	}{
		{"0", 0}, {"1000", 1000}, {"10G", 10 << 30}, {"10g", 10 << 30}, {"10GB", 10 << 30}, {"10GiB", 10 << 30},
		{"500M", 500 << 20}, {"2K", 2048}, {"1T", 1 << 40}, {" 5M ", 5 << 20},
	}
	for _, c := range cases {
		got := mustServe(t, append(append([]string{}, base...), "--daily-bytes", c.bytes), Env{})
		if got.DailyBytes != c.want {
			t.Errorf("--daily-bytes %q = %d，要 %d", c.bytes, got.DailyBytes, c.want)
		}
	}
	// 环境变量；命令行优先；环境变量为空当作没给。
	got := mustServe(t, base, Env{"RELAY_DAILY_REQUESTS": "500", "RELAY_DAILY_BYTES": "1G"})
	if got.DailyRequests != 500 || got.DailyBytes != 1<<30 {
		t.Fatalf("环境变量：%+v", got)
	}
	got = mustServe(t, append(append([]string{}, base...), "--daily-requests", "7"), Env{"RELAY_DAILY_REQUESTS": "500"})
	if got.DailyRequests != 7 {
		t.Fatalf("命令行优先：%+v", got)
	}
	got = mustServe(t, base, Env{"RELAY_DAILY_REQUESTS": "", "RELAY_DAILY_BYTES": ""})
	if got.DailyRequests != 0 || got.DailyBytes != 0 {
		t.Fatalf("空环境变量当没给：%+v", got)
	}
	for _, bad := range []string{"-1", "abc", "1.5", "10X", "G", "", "99999999999999999999", "2000000T"} {
		_, err := ParseServe(append(append([]string{}, base...), "--daily-bytes", bad), Env{}, "/w")
		wantUsage(t, err, "--daily-bytes")
	}
	for _, bad := range []string{"-1", "abc", "1.5", "10G", ""} {
		_, err := ParseServe(append(append([]string{}, base...), "--daily-requests", bad), Env{}, "/w")
		wantUsage(t, err, "--daily-requests")
	}
	// 要靠流量统计恢复：和 --traffic-retention-days 0 同用是配置错误；不限时可以。
	_, err := ParseServe(append(append([]string{}, base...), "--daily-requests", "5", "--traffic-retention-days", "0"), Env{}, "/w")
	wantUsage(t, err, "--traffic-retention-days 0")
	if _, err := ParseServe(append(append([]string{}, base...), "--daily-requests", "0", "--traffic-retention-days", "0"), Env{}, "/w"); err != nil {
		t.Fatalf("0 = 不限，可以不记流量：%v", err)
	}
}

// 缺省关闭：指标里没有配额项，也没有准入。
func TestServeDailyOffByDefault(t *testing.T) {
	running, _, _ := plainServe(t, "--metrics-listen", "127.0.0.1:0")
	_, _, data := request(t, http.DefaultClient, "GET", running.MetricsURL+"/metrics", nil, "")
	if strings.Contains(string(data), "relay_daily") {
		t.Fatalf("缺省不该有配额指标：%s", data)
	}
}

// 开了配额：指标报当日用量与拒绝次数；重启后从流量账本恢复当天用量（含关闭时落库的）。
func TestServeDailyRestoresAfterRestart(t *testing.T) {
	dir := initAt(t, "http://127.0.0.1:1")
	args := []string{"--tls", "plain", "--public-origin", "http://127.0.0.1:1", "--metrics-listen", "127.0.0.1:0",
		"--daily-requests", "5", "--daily-bytes", "1M"}
	first, logs := startServe(t, dir, args, ServeDeps{})
	if !strings.Contains(logs.String(), "已开启每日配额") {
		t.Fatalf("应记一条已开启：%s", logs.String())
	}
	_, _, data := request(t, http.DefaultClient, "GET", first.MetricsURL+"/metrics", nil, "")
	for _, line := range []string{"relay_daily_requests_used 0", "relay_daily_requests_limit 5", "relay_daily_bytes_limit 1048576", "relay_daily_budget_rejected_total 0"} {
		if !strings.Contains(string(data), line+"\n") {
			t.Fatalf("缺 %q：%s", line, data)
		}
	}
	// 流经计量器的字节会累加进配额；关闭时落库。
	first.Meter.Add(traffic.Sample{Key: traffic.Key{SourceID: "s1", Subject: "acct:me", Kind: traffic.KindE2E}, At: time.Now(), Connections: 3, BytesToSource: 400, BytesToClient: 600})
	_, _, data = request(t, http.DefaultClient, "GET", first.MetricsURL+"/metrics", nil, "")
	if !strings.Contains(string(data), "relay_daily_bytes_used 1000\n") {
		t.Fatalf("字节应累加：%s", data)
	}
	first.Close()

	second, _ := startServe(t, dir, args, ServeDeps{})
	_, _, data = request(t, http.DefaultClient, "GET", second.MetricsURL+"/metrics", nil, "")
	for _, line := range []string{"relay_daily_requests_used 3", "relay_daily_bytes_used 1000"} {
		if !strings.Contains(string(data), line+"\n") {
			t.Fatalf("重启后应从账本恢复，缺 %q：%s", line, data)
		}
	}
}

// 程序化给了配额却关了流量统计：拒绝启动。
func TestServeDailyNeedsTraffic(t *testing.T) {
	dir := initAt(t, "http://127.0.0.1:1")
	options, err := ParseServe([]string{"--data-dir", dir, "--host", "127.0.0.1", "--listen", "127.0.0.1:0", "--tls", "plain", "--public-origin", "http://127.0.0.1:1"}, Env{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	options.DailyRequests, options.TrafficRetentionDays = 1, 0
	if _, err := Serve(t.Context(), options, ServeDeps{}); err == nil {
		t.Fatal("没开流量统计应拒绝")
	}
}
