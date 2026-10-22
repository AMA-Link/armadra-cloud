package obs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func parseLine(t *testing.T, line string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("不是 JSON：%q: %v", line, err)
	}
	return m
}

// 对应 log.test.ts「日志只写白名单字段，非标量与未知字段被丢掉」：
// Go 里白名单由类型保证，这里验证输出只含构造函数给出的字段，且不含敏感字符串。
func TestLoggerWhitelist(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(&buf, LevelInfo)
	log.Info("流结束", SourceID("s"), BytesIn(3), Code(42), Code(3.5), Code(struct{}{}), Error(nil))
	line := strings.TrimSuffix(buf.String(), "\n")
	if strings.Contains(line, "\n") {
		t.Fatal("一条记录只能占一行")
	}
	rec := parseLine(t, line)
	if rec["msg"] != "流结束" || rec["sourceId"] != "s" || rec["bytesIn"] != float64(3) || rec["code"] != float64(42) {
		t.Fatalf("字段不对：%v", rec)
	}
	for _, k := range []string{"path", "headers", "error"} {
		if _, ok := rec[k]; ok {
			t.Errorf("不应有字段 %s", k)
		}
	}
	if len(rec) != 7 {
		t.Fatalf("字段数 = %d：%v", len(rec), rec)
	}
}

func TestLoggerKeyOrderAndTime(t *testing.T) {
	var buf bytes.Buffer
	now := func() time.Time { return time.Date(2026, 10, 23, 8, 9, 10, 123456789, time.FixedZone("x", 8*3600)) }
	log := NewLoggerWithClock(&buf, LevelDebug, now)
	log.Warn("a\"b\n<>", Tunnels(1), Streams(2), StreamID(7), Code("rst"), DurationMs(9), Mode("saas"), Listen(":80"), URL("u"), Node("n"), Kind("ws"), Result("ok"), Reason("r"), BytesOut(5), TunnelID("t"))
	want := `{"time":"2026-10-23T00:09:10.123Z","level":"warn","name":"relay","msg":"a\"b\n<>","tunnels":1,"streams":2,"streamId":7,"code":"rst","durationMs":9,"mode":"saas","listen":":80","url":"u","node":"n","kind":"ws","result":"ok","reason":"r","bytesOut":5,"tunnelId":"t"}` + "\n"
	if buf.String() != want {
		t.Fatalf("got  %s\nwant %s", buf.String(), want)
	}
}

func TestLoggerEmptyStringWritten(t *testing.T) {
	var buf bytes.Buffer
	NewLogger(&buf, LevelInfo).Info("m", SourceID(""))
	if !strings.Contains(buf.String(), `"sourceId":""`) {
		t.Fatal(buf.String())
	}
}

func TestLoggerLevelFilter(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(&buf, LevelWarn)
	log.Debug("d")
	log.Info("i")
	log.Warn("w")
	log.Error("e")
	if n := strings.Count(buf.String(), "\n"); n != 2 {
		t.Fatalf("应只剩 warn 与 error：%q", buf.String())
	}
	Nop().Error("x")
	var nilLog *Logger
	nilLog.Error("x")
}

// 对应 log.test.ts「errorName 只给类名」。
func TestErrorName(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(&buf, LevelInfo)
	log.Info("m", Error(&net.OpError{Op: "dial", Err: errors.New("令牌 abc")}))
	rec := parseLine(t, buf.String())
	if rec["error"] != "*net.OpError" {
		t.Fatalf("error = %v", rec["error"])
	}
	if strings.Contains(buf.String(), "abc") {
		t.Fatal("不能记 message")
	}
}

func TestLoggerConcurrent(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(&buf, LevelInfo)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				log.Info("m", Streams(j))
			}
		}()
	}
	wg.Wait()
	for _, line := range strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n") {
		parseLine(t, line)
	}
}

func TestLoggerAllocs(t *testing.T) {
	log := NewLogger(io.Discard, LevelInfo)
	n := testing.AllocsPerRun(100, func() { log.Info("m", SourceID("s"), BytesIn(1)) })
	if n > 1 {
		t.Fatalf("分配 %v 次", n)
	}
}

// 对应 log.test.ts「指标是文本格式，计数按标签累加」。
func TestMetricsText(t *testing.T) {
	m := NewMetrics(func() (int, int) { return 1, 2 })
	m.AddBytes(ToClient, 10)
	m.AddBytes(ToClient, 5)
	m.EdgeConnection("ok")
	m.EdgeConnection("ok")
	m.EdgeConnection("source_offline")
	m.HandshakeDone(20 * time.Millisecond)
	m.HeartbeatMiss()
	text := m.Render()
	for _, want := range []string{
		"relay_tunnels 1\n",
		"relay_streams 2\n",
		`relay_bytes_total{direction="to_client"} 15`,
		`relay_bytes_total{direction="to_source"} 0`,
		`relay_edge_connections_total{result="ok"} 2`,
		`relay_edge_connections_total{result="source_offline"} 1`,
		`relay_handshake_total{result="ok"} 1`,
		`relay_handshake_seconds_bucket{le="0.05"} 1`,
		`relay_handshake_seconds_bucket{le="0.01"} 0`,
		"relay_heartbeat_miss_total 1",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("缺 %q", want)
		}
	}
}

// 黄金串：同一场景由 TS 渲染得到，Go 输出必须逐字节相同。
func TestMetricsGolden(t *testing.T) {
	m := NewMetrics(func() (int, int) { return 1, 2 })
	m.AddBytes(ToClient, 10)
	m.AddBytes(ToClient, 5)
	m.AddBytes(ToSource, 7)
	m.EdgeConnection("ok")
	m.EdgeConnection("ok")
	m.EdgeConnection("source_offline")
	m.EdgeStream("http")
	m.EdgeStream("http")
	m.EdgeStream("ws")
	m.HandshakeDone(20 * time.Millisecond)
	m.HandshakeDone(3 * time.Second)
	m.Handshake("bad_token")
	m.HeartbeatMiss()
	if got := m.Render(); got != goldenMetrics {
		t.Fatalf("got:\n%s\nwant:\n%s", got, goldenMetrics)
	}
}

func TestMetricsSnapshot(t *testing.T) {
	m := NewMetrics(func() (int, int) { return 3, 4 })
	m.AddBytes(ToSource, 2)
	m.EdgeStream("e2e")
	m.Handshake("x")
	s := m.Snapshot()
	if s.Tunnels != 3 || s.Streams != 4 || s.Bytes[ToSource] != 2 || s.EdgeStreams["e2e"] != 1 || s.Handshakes["x"] != 1 {
		t.Fatalf("%+v", s)
	}
}

func TestMetricsConcurrent(t *testing.T) {
	m := NewMetrics(nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				m.AddBytes(ToClient, 1)
				m.EdgeConnection("ok")
			}
		}()
	}
	wg.Wait()
	s := m.Snapshot()
	if s.Bytes[ToClient] != 8000 || s.Edge["ok"] != 8000 {
		t.Fatalf("%+v", s)
	}
}

func TestJSNumber(t *testing.T) {
	cases := map[float64]string{0: "0", 0.01: "0.01", 2.5: "2.5", 10: "10", 3.02: "3.02", 1e-7: "1e-7", 1.5e21: "1.5e+21", 1e-6: "0.000001", 123456789012345680000: "123456789012345680000", -0.5: "-0.5"}
	for in, want := range cases {
		if got := jsNumber(in); got != want {
			t.Errorf("%v: %q != %q", in, got, want)
		}
	}
}

func get(h http.Handler, method, target string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// 对应 dispatch.test.ts「/health 报模式、节点与计数」。
func TestHealth(t *testing.T) {
	h := HealthHandler(HealthOptions{Mode: "saas", Node: "node-a", Version: "0.1.0", Counts: func() (int, int) { return 2, 5 }})
	rec := get(h, "GET", "/health", nil)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	want := `{"ok":true,"mode":"saas","node":"node-a","version":"0.1.0","tunnels":2,"streams":5}`
	if rec.Body.String() != want {
		t.Fatalf("body = %s", rec.Body.String())
	}
	for k, v := range map[string]string{"armadra-protocol": "1.3", "content-type": "application/json; charset=utf-8", "cache-control": "no-store"} {
		if rec.Header().Get(k) != v {
			t.Errorf("%s = %q", k, rec.Header().Get(k))
		}
	}
	if get(h, "POST", "/health", nil).Code != 404 {
		t.Error("非 GET 应 404")
	}
}

// 对应 dispatch.test.ts「/ready 跟随 Redis」。
func TestReady(t *testing.T) {
	down := ReadyHandler(map[string]Probe{"redis": func(context.Context) error { return errors.New("x") }})
	rec := get(down, "GET", "/ready", nil)
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), `"ok":false`) {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	up := ReadyHandler(map[string]Probe{"redis": func(context.Context) error { return nil }})
	if rec := get(up, "GET", "/ready", nil); rec.Code != 200 || rec.Header().Get("armadra-protocol") != "1.3" {
		t.Fatalf("%d", rec.Code)
	}
}

// 对应 health.test.ts「任一探测失败或超时则整体不就绪」。
func TestRunProbesFailures(t *testing.T) {
	result := RunProbes(context.Background(), map[string]Probe{
		"good": func(context.Context) error { return nil },
		"bad":  func(context.Context) error { return errors.New("postgres://user:secret@host/db refused") },
		"slow": func(context.Context) error { select {} },
	}, 50*time.Millisecond)
	if result.OK || !result.Checks["good"].OK || result.Checks["bad"].OK || result.Checks["slow"].OK {
		t.Fatalf("%+v", result)
	}
	raw, _ := json.Marshal(result)
	if strings.Contains(string(raw), "secret") {
		t.Fatal("不能回显错误原文")
	}
}

// 对应 health.test.ts「全部通过则就绪」。
func TestRunProbesAllPass(t *testing.T) {
	if !RunProbes(context.Background(), map[string]Probe{"a": func(context.Context) error { return nil }}, 0).OK {
		t.Fatal("应就绪")
	}
}

func TestRunProbesPanicAndOrder(t *testing.T) {
	result := RunProbes(context.Background(), map[string]Probe{
		"b": func(context.Context) error { panic("boom") },
		"a": func(context.Context) error { return nil },
	}, time.Second)
	if result.OK || result.Checks["b"].OK {
		t.Fatalf("%+v", result)
	}
	raw, _ := json.Marshal(result)
	if !strings.HasPrefix(string(raw), `{"ok":false,"checks":{"a":{"ok":true,"ms":`) {
		t.Fatalf("check 名应排序：%s", raw)
	}
}

func TestMetricsHandler(t *testing.T) {
	h := MetricsHandler(func() string { return "x 1\n" }, "s3cret")
	if get(h, "GET", "/health", nil).Code != 404 || get(h, "POST", "/metrics", nil).Code != 404 {
		t.Error("非 GET /metrics 应 404")
	}
	rec := get(h, "GET", "/metrics", nil)
	if rec.Code != 401 || !strings.Contains(rec.Body.String(), `"unauthenticated"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if get(h, "GET", "/metrics", map[string]string{"Authorization": "Bearer wrong"}).Code != 401 {
		t.Error("错误令牌应 401")
	}
	rec = get(h, "GET", "/metrics?x=1", map[string]string{"Authorization": "Bearer s3cret"})
	if rec.Code != 200 || rec.Body.String() != "x 1\n" ||
		rec.Header().Get("content-type") != "text/plain; version=0.0.4; charset=utf-8" ||
		rec.Header().Get("cache-control") != "no-store" {
		t.Fatalf("%d %v %q", rec.Code, rec.Header(), rec.Body.String())
	}
	open := MetricsHandler(func() string { return "y\n" }, "")
	if get(open, "GET", "/metrics", nil).Code != 200 {
		t.Error("无令牌应放行")
	}
}
