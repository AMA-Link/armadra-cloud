package saas

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
)

func host() (string, error) { return "my host.local", nil }

func baseEnv() map[string]string {
	return map[string]string{
		"RELAY_DOMAIN":       "src.localhost",
		"RELAY_CLOUD_ISSUER": "http://cloud:8100",
		"RELAY_REDIS_URL":    "redis://127.0.0.1:1",
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, issues := LoadConfig(baseEnv(), host)
	if len(issues) > 0 {
		t.Fatal(issues)
	}
	if cfg.Host != "0.0.0.0" || cfg.Port != 8443 || cfg.NodeID != "my-host.local" || cfg.LogLevel != obs.LevelInfo || cfg.AllowInsecureTunnel {
		t.Fatalf("%+v", cfg)
	}
	env := baseEnv()
	env["RELAY_LISTEN"] = "[::1]:9000"
	env["RELAY_NODE_ID"] = "dev-container-1"
	env["RELAY_ALLOW_INSECURE_TUNNEL"] = "1"
	env["RELAY_LOG_LEVEL"] = "debug"
	cfg, issues = LoadConfig(env, host)
	if len(issues) > 0 || cfg.Host != "::1" || cfg.Port != 9000 || cfg.NodeID != "dev-container-1" || !cfg.AllowInsecureTunnel || cfg.LogLevel != obs.LevelDebug {
		t.Fatalf("%+v %v", cfg, issues)
	}
}

// 同 TS 的 parseConfig：空串当没设，问题只列变量名，不回显取值。
func TestLoadConfigIssues(t *testing.T) {
	_, issues := LoadConfig(map[string]string{"RELAY_DOMAIN": " ", "RELAY_REDIS_URL": "redis://:secret-pass@"}, host)
	joined := strings.Join(issues, "\n")
	for _, key := range []string{"RELAY_DOMAIN", "RELAY_CLOUD_ISSUER"} {
		if !strings.Contains(joined, key) {
			t.Errorf("缺 %s：%s", key, joined)
		}
	}
	if strings.Contains(joined, "secret-pass") {
		t.Fatal("回显了取值")
	}
	env := baseEnv()
	env["RELAY_LISTEN"] = "nope"
	env["RELAY_NODE_ID"] = "has space"
	env["RELAY_CLOUD_ISSUER"] = "not a url"
	env["RELAY_ALLOW_INSECURE_TUNNEL"] = "yes"
	env["RELAY_LOG_LEVEL"] = "trace"
	_, issues = LoadConfig(env, host)
	if len(issues) != 5 {
		t.Fatalf("应有 5 个问题：%v", issues)
	}
}

func TestRunUsageAndConfigErrors(t *testing.T) {
	cases := []struct {
		args    []string
		env     map[string]string
		code    int
		out, er string
	}{
		{[]string{"--help"}, nil, 0, "用法", ""},
		{[]string{"serve", "--help"}, nil, 0, "用法", ""},
		{nil, nil, 64, "", "未知命令：saas"},
		{[]string{"serve", "extra"}, nil, 64, "", "未知命令：saas serve extra"},
		{[]string{"serve"}, map[string]string{}, 64, "", "RELAY_CLOUD_ISSUER"},
	}
	for _, c := range cases {
		var out, errOut bytes.Buffer
		code := Run(c.args, c.env, "dev", "用法: armadra-relay", &out, &errOut)
		if code != c.code || !strings.Contains(out.String(), c.out) || !strings.Contains(errOut.String(), c.er) {
			t.Errorf("%v：code=%d out=%q err=%q", c.args, code, out.String(), errOut.String())
		}
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// serve 起占位，答地址、日志与停止函数（停止后等 Serve 返回）。
func serve(t *testing.T, redisURL string) (string, *syncBuffer, func()) {
	t.Helper()
	cfg, issues := LoadConfig(map[string]string{
		"RELAY_LISTEN": "127.0.0.1:0", "RELAY_NODE_ID": "n1", "RELAY_DOMAIN": "src.localhost",
		"RELAY_CLOUD_ISSUER": "http://cloud:8100", "RELAY_REDIS_URL": redisURL,
	}, host)
	if len(issues) > 0 {
		t.Fatal(issues)
	}
	ctx, cancel := context.WithCancel(context.Background())
	logs := &syncBuffer{}
	urls := make(chan string, 1)
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, cfg, "9.9.9", logs, func(u string) { urls <- u }) }()
	select {
	case u := <-urls:
		return u, logs, func() {
			cancel()
			if err := <-done; err != nil {
				t.Error(err)
			}
		}
	case err := <-done:
		cancel()
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("没起来")
	}
	return "", nil, nil
}

func get(t *testing.T, method, url string) (int, http.Header, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	return resp.StatusCode, resp.Header, body
}

// 与 Node 版空壳同形：/health、/ready（Redis 不可达 503）、其余 404 not_found，起停各一行日志。
func TestServeMatchesNodeShell(t *testing.T) {
	// 一个立刻关掉的端口：Redis 不可达。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "redis://" + ln.Addr().String()
	_ = ln.Close()
	url, logs, stop := serve(t, dead)

	status, header, body := get(t, http.MethodGet, url+"/health")
	want := map[string]any{"ok": true, "mode": "saas", "node": "n1", "version": "9.9.9", "tunnels": 0.0, "streams": 0.0}
	if status != 200 || len(body) != len(want) || header.Get("armadra-protocol") == "" || header.Get("cache-control") != "no-store" {
		t.Fatalf("/health：%d %v %v", status, header, body)
	}
	for k, v := range want {
		if body[k] != v {
			t.Fatalf("/health.%s = %v，应为 %v", k, body[k], v)
		}
	}
	if status, _, _ := get(t, http.MethodHead, url+"/health"); status != 200 {
		t.Fatalf("HEAD /health：%d", status)
	}
	status, _, body = get(t, http.MethodGet, url+"/ready")
	checks, _ := body["checks"].(map[string]any)
	redis, _ := checks["redis"].(map[string]any)
	if status != 503 || body["ok"] != false || redis["ok"] != false {
		t.Fatalf("/ready：%d %v", status, body)
	}
	for _, c := range []struct{ method, path string }{{http.MethodGet, "/"}, {http.MethodGet, "/v1/me"}, {http.MethodPost, "/health"}, {http.MethodGet, "/t/v1"}} {
		status, _, body := get(t, c.method, url+c.path)
		if status != 404 || body["code"] != "not_found" || body["message"] != "Not found" || len(body["requestId"].(string)) != 36 {
			t.Fatalf("%s %s：%d %v", c.method, c.path, status, body)
		}
	}
	stop()
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	var started, stopped map[string]any
	for _, line := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("日志不是 JSON：%q", line)
		}
		if rec["name"] != "relay" {
			t.Fatalf("name：%v", rec)
		}
		switch rec["msg"] {
		case "中继已启动":
			started = rec
		case "中继已停止":
			stopped = rec
		}
	}
	if started == nil || started["url"] != url || started["mode"] != "saas" || started["node"] != "n1" || stopped == nil {
		t.Fatalf("日志：%s", logs.String())
	}
	if strings.Contains(logs.String(), dead) {
		t.Fatal("日志里有 Redis 地址")
	}
}

func TestServeReadyWithRedis(t *testing.T) {
	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("CI 里没有 REDIS_URL")
		}
		t.Skip("没有 Redis")
	}
	url, _, stop := serve(t, redisURL)
	defer stop()
	status, _, body := get(t, http.MethodGet, url+"/ready")
	if status != 200 || body["ok"] != true {
		t.Fatalf("/ready：%d %v", status, body)
	}
}

func TestServeListenError(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	cfg, _ := LoadConfig(baseEnv(), host)
	cfg.Host, cfg.Port = "127.0.0.1", ln.Addr().(*net.TCPAddr).Port
	err = Serve(context.Background(), cfg, "dev", io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "监听失败") {
		t.Fatalf("应报监听失败：%v", err)
	}
	var opErr *net.OpError
	if !errors.As(err, &opErr) {
		t.Fatalf("应保留原错误：%v", err)
	}
}
