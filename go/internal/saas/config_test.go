package saas

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
)

func host() (string, error) { return "my host.local", nil }

func baseEnv() map[string]string {
	return map[string]string{
		"RELAY_DOMAIN":       "Example.Test",
		"RELAY_CLOUD_ISSUER": "https://app.example.test",
		"RELAY_REDIS_URL":    "redis://127.0.0.1:1",
		"RELAY_TUNNEL_URL":   "wss://a.edge.example.test/t/v1",
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, issues := LoadConfig(baseEnv(), host)
	if len(issues) > 0 {
		t.Fatal(issues)
	}
	if cfg.Host != "0.0.0.0" || cfg.Port != 8443 || cfg.NodeID != "my-host-local" || cfg.LogLevel != obs.LevelInfo || cfg.AllowInsecureTunnel {
		t.Fatalf("%+v", cfg)
	}
	if cfg.Domain != "example.test" || cfg.Weight != 1 || cfg.DrainGrace != 30*time.Second {
		t.Fatalf("%+v", cfg)
	}
	if cfg.InternodeURL != "wss://a.edge.example.test/x/v1" || cfg.JWKSURL != "https://app.example.test/.well-known/jwks.json" {
		t.Fatalf("派生地址：%s %s", cfg.InternodeURL, cfg.JWKSURL)
	}
	if cfg.TeamConcurrent != DefaultTeamConcurrent || cfg.TeamConnectPerMinute != DefaultTeamConnectPerMinute || DefaultTeamConcurrent != 10 {
		t.Fatalf("团队额度缺省取最严格的套餐：%+v", cfg)
	}
	if cfg.ForwardedPerAccount != DefaultForwardedPerAccount || DefaultForwardedPerAccount != 64 {
		t.Fatalf("转发名额缺省 64：%+v", cfg)
	}
	if cfg.NodeToken != "" || cfg.DatabaseURL != "" || cfg.TrustedProxy != nil {
		t.Fatalf("%+v", cfg)
	}

	env := baseEnv()
	env["RELAY_LISTEN"] = "[::1]:9000"
	env["RELAY_NODE_ID"] = "a"
	env["RELAY_ALLOW_INSECURE_TUNNEL"] = "1"
	env["RELAY_TUNNEL_URL"] = "ws://a.edge.localhost:8443/t/v1"
	env["RELAY_INTERNODE_URL"] = "ws://relay-a:8443/x/v1"
	env["RELAY_NODE_WEIGHT"] = "3"
	env["RELAY_LOG_LEVEL"] = "debug"
	env["RELAY_TRUSTED_PROXY"] = "10.0.0.0/8, ::1/128"
	env["RELAY_TRUSTED_PROXY_PREFER"] = "real-ip"
	env["RELAY_DRAIN_GRACE_MS"] = "500"
	env["RELAY_METRICS_LISTEN"] = "127.0.0.1:9100"
	env["RELAY_TEAM_CONCURRENT"] = "50"
	env["RELAY_TEAM_CONNECT_PER_MINUTE"] = "3000"
	env["RELAY_FORWARDED_PER_ACCOUNT"] = "8"
	cfg, issues = LoadConfig(env, host)
	if cfg.TeamConcurrent != 50 || cfg.TeamConnectPerMinute != 3000 || cfg.ForwardedPerAccount != 8 {
		t.Fatalf("显式覆盖团队缺省：%+v", cfg)
	}
	if len(issues) > 0 {
		t.Fatal(issues)
	}
	if cfg.Host != "::1" || cfg.Port != 9000 || cfg.NodeID != "a" || !cfg.AllowInsecureTunnel || cfg.LogLevel != obs.LevelDebug ||
		cfg.InternodeURL != "ws://relay-a:8443/x/v1" || cfg.Weight != 3 || cfg.DrainGrace != 500*time.Millisecond ||
		cfg.TrustedProxy == nil || len(cfg.TrustedProxy.Proxies) != 2 || cfg.TrustedProxy.Prefer != "real-ip" || cfg.MetricsListen != "127.0.0.1:9100" {
		t.Fatalf("%+v", cfg)
	}
}

// 秘密从变量或 *_FILE 读，两个都给是配错；问题只列变量名，不回显取值。
func TestLoadConfigSecrets(t *testing.T) {
	files := map[string]string{"/run/token": "node-token-value\n", "/run/db": "postgres://traffic:pw@db/armadra"}
	orig := readFile
	defer func() { readFile = orig }()
	readFile = func(name string) ([]byte, error) {
		if v, ok := files[name]; ok {
			return []byte(v), nil
		}
		return nil, errors.New("no such file")
	}
	env := baseEnv()
	env["RELAY_NODE_TOKEN_FILE"] = "/run/token"
	env["RELAY_DATABASE_URL_FILE"] = "/run/db"
	env["RELAY_METRICS_TOKEN"] = "metrics-secret"
	cfg, issues := LoadConfig(env, host)
	if len(issues) > 0 || cfg.NodeToken != "node-token-value" || cfg.DatabaseURL != "postgres://traffic:pw@db/armadra" || cfg.MetricsToken != "metrics-secret" {
		t.Fatalf("%+v %v", cfg, issues)
	}
	env["RELAY_NODE_TOKEN"] = "inline-secret"
	env["RELAY_DATABASE_URL_FILE"] = "/run/missing"
	_, issues = LoadConfig(env, host)
	joined := strings.Join(issues, "\n")
	if len(issues) != 2 || !strings.Contains(joined, "RELAY_NODE_TOKEN") || !strings.Contains(joined, "RELAY_DATABASE_URL_FILE") {
		t.Fatalf("%v", issues)
	}
	if strings.Contains(joined, "inline-secret") || strings.Contains(joined, "node-token-value") {
		t.Fatal("回显了秘密")
	}
}

func TestLoadConfigIssues(t *testing.T) {
	_, issues := LoadConfig(map[string]string{"RELAY_DOMAIN": " ", "RELAY_REDIS_URL": "redis://:secret-pass@"}, host)
	joined := strings.Join(issues, "\n")
	for _, key := range []string{"RELAY_DOMAIN", "RELAY_CLOUD_ISSUER", "RELAY_TUNNEL_URL"} {
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
	env["RELAY_TUNNEL_URL"] = "ws://a/t/v1" // 没开 insecure 时只许 wss
	env["RELAY_NODE_WEIGHT"] = "0"
	env["RELAY_TRUSTED_PROXY"] = "not-a-cidr"
	env["RELAY_DRAIN_GRACE_MS"] = "-1"
	env["RELAY_METRICS_LISTEN"] = "x"
	env["RELAY_TEAM_CONCURRENT"] = "0"
	_, issues = LoadConfig(env, host)
	if len(issues) != 11 {
		t.Fatalf("应有 11 个问题：%v", issues)
	}
}

// 节点名与契约的 relayNodeIdSchema 同一条规则；缺省由主机名换出。
func TestNodeID(t *testing.T) {
	for in, want := range map[string]string{
		"my host.local":         "my-host-local",
		"Relay_A.Example":       "relay-a-example",
		"--edge-1":              "edge-1",
		strings.Repeat("a", 80): strings.Repeat("a", 63),
		"..":                    "",
		"node-7":                "node-7",
	} {
		if got := NodeIDFromHost(func() (string, error) { return in, nil }); got != want {
			t.Errorf("%q → %q，应为 %q", in, got, want)
		}
	}
	for id, ok := range map[string]bool{
		"a": true, "relay-a": true, strings.Repeat("a", 63): true,
		"Relay-A": false, "relay.a": false, "relay_a": false, "-a": false, strings.Repeat("a", 64): false,
	} {
		env := baseEnv()
		env["RELAY_NODE_ID"] = id
		_, issues := LoadConfig(env, host)
		if (len(issues) == 0) != ok {
			t.Errorf("%q：%v", id, issues)
		}
	}
	// 主机名换不出合法节点名又没显式设置：配置错误。
	if _, issues := LoadConfig(baseEnv(), func() (string, error) { return "...", nil }); len(issues) != 1 || !strings.Contains(issues[0], "RELAY_NODE_ID") {
		t.Fatalf("%v", issues)
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

// Redis 连不上：启动失败（退出 1），错误里没有地址。
func TestServeFailsWithoutRedis(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "redis://:pw-secret@" + ln.Addr().String()
	_ = ln.Close()
	env := baseEnv()
	env["RELAY_REDIS_URL"] = dead
	cfg, issues := LoadConfig(env, host)
	if len(issues) > 0 {
		t.Fatal(issues)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err = Serve(ctx, cfg, "dev", io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "控制面（Redis）起不来") || strings.Contains(err.Error(), "pw-secret") || strings.Contains(err.Error(), ln.Addr().String()) {
		t.Fatalf("%v", err)
	}
}

// Redis 地址带口令时用 RELAY_REDIS_URL_FILE 给；与 RELAY_REDIS_URL 同给是配错，问题不回显取值。
func TestLoadConfigRedisURLFile(t *testing.T) {
	orig := readFile
	defer func() { readFile = orig }()
	readFile = func(name string) ([]byte, error) {
		if name == "/run/redis" {
			return []byte("redis://:redis-pass@redis:6379\n"), nil
		}
		return nil, errors.New("no such file")
	}
	env := baseEnv()
	delete(env, "RELAY_REDIS_URL")
	env["RELAY_REDIS_URL_FILE"] = "/run/redis"
	cfg, issues := LoadConfig(env, host)
	if len(issues) > 0 || cfg.RedisURL != "redis://:redis-pass@redis:6379" {
		t.Fatalf("%+v %v", cfg, issues)
	}
	env["RELAY_REDIS_URL"] = "redis://inline:6379"
	_, issues = LoadConfig(env, host)
	if len(issues) != 1 || !strings.Contains(issues[0], "RELAY_REDIS_URL") || strings.Contains(strings.Join(issues, ""), "redis-pass") {
		t.Fatalf("%v", issues)
	}
	delete(env, "RELAY_REDIS_URL")
	delete(env, "RELAY_REDIS_URL_FILE")
	if _, issues = LoadConfig(env, host); len(issues) != 1 || !strings.Contains(issues[0], "必填") {
		t.Fatalf("%v", issues)
	}
}

// 指标监听：回环地址不要令牌；非回环（空主机 :端口、0.0.0.0、[::]、别的地址）必须配 RELAY_METRICS_TOKEN
// 或 RELAY_METRICS_TOKEN_FILE，否则配置无效、不启动（与控制面同一条规则）。
func TestMetricsListenNeedsTokenOffLoopback(t *testing.T) {
	orig := readFile
	defer func() { readFile = orig }()
	readFile = func(name string) ([]byte, error) {
		switch name {
		case "/run/metrics_token":
			return []byte("metrics-secret\n"), nil
		case "/run/empty":
			return []byte("\n"), nil
		}
		return nil, errors.New("no such file")
	}
	const want = "RELAY_METRICS_LISTEN：不是回环地址时必须配 RELAY_METRICS_TOKEN 或 RELAY_METRICS_TOKEN_FILE"
	for _, c := range []struct {
		listen, tokenKey, token string
		ok                      bool
	}{
		{":9100", "", "", false},
		{"0.0.0.0:9100", "", "", false},
		{"[::]:9100", "", "", false},
		{"10.0.0.5:9100", "", "", false},
		{":9100", "RELAY_METRICS_TOKEN_FILE", "/run/empty", false},
		{":9100", "RELAY_METRICS_TOKEN_FILE", "/run/metrics_token", true},
		{"0.0.0.0:9100", "RELAY_METRICS_TOKEN_FILE", "/run/metrics_token", true},
		{"0.0.0.0:9100", "RELAY_METRICS_TOKEN", "metrics-secret", true},
		{"127.0.0.1:9100", "", "", true},
		{"[::1]:9100", "", "", true},
		{"localhost:9100", "", "", true},
	} {
		env := baseEnv()
		env["RELAY_METRICS_LISTEN"] = c.listen
		if c.tokenKey != "" {
			env[c.tokenKey] = c.token
		}
		cfg, issues := LoadConfig(env, host)
		if c.ok {
			if len(issues) > 0 || cfg.MetricsListen != c.listen {
				t.Errorf("%s（%s）应放行：%v", c.listen, c.tokenKey, issues)
			}
			continue
		}
		if len(issues) != 1 || issues[0] != want || cfg.MetricsListen != "" {
			t.Errorf("%s（%s）应拒绝：%v", c.listen, c.tokenKey, issues)
		}
	}
	// serve 拒绝启动：报这一句，不回显令牌。
	var out, errOut bytes.Buffer
	env := baseEnv()
	env["RELAY_METRICS_LISTEN"] = "0.0.0.0:9100"
	if code := Run([]string{"serve"}, env, "dev", "用法: armadra-relay", &out, &errOut); code != 64 || !strings.Contains(errOut.String(), want) {
		t.Fatalf("code=%d err=%q", code, errOut.String())
	}
}
