package server_test

import (
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

// 控制面的 /metrics（METRICS_LISTEN）除了 up 与登录失败关闭的计数，还有发信的四个计数器（只有次数）。
func TestMetricsListenerExportsMailCounters(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	newRig(t, rigOptions{env: map[string]string{"METRICS_LISTEN": addr}})
	resp, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码：%d", resp.StatusCode)
	}
	for _, line := range []string{
		"armadra_cloud_up 1",
		"armadra_cloud_lockout_unavailable_total 0",
		"# TYPE armadra_cloud_mail_sent_total counter",
		"armadra_cloud_mail_sent_total 0",
		"armadra_cloud_mail_send_failures_total 0",
		"armadra_cloud_mail_abandoned_total 0",
		"armadra_cloud_mail_backlog_dropped_total 0",
	} {
		if !strings.Contains(text, line+"\n") {
			t.Fatalf("缺少 %q：\n%s", line, text)
		}
	}
}
