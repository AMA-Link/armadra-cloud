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
	if cfg.Host != "0.0.0.0" || cfg.Port != 8443 || cfg.NodeID != "my-host.local" || cfg.LogLevel != obs.LevelInfo || cfg.AllowInsecureTunnel {
		t.Fatalf("%+v", cfg)
	}
	if cfg.Domain != "example.test" || cfg.Weight != 1 || cfg.DrainGrace != 30*time.Second {
		t.Fatalf("%+v", cfg)
	}
	if cfg.InternodeURL != "wss://a.edge.example.test/x/v1" || cfg.JWKSURL != "https://app.example.test/.well-known/jwks.json" {
		t.Fatalf("派生地址：%s %s", cfg.InternodeURL, cfg.JWKSURL)
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
	cfg, issues = LoadConfig(env, host)
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
	_, issues = LoadConfig(env, host)
	if len(issues) != 10 {
		t.Fatalf("应有 10 个问题：%v", issues)
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
