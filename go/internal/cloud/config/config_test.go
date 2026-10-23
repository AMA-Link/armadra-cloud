package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/config"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
)

const key = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

func base() config.MapEnv {
	return config.MapEnv{
		"ARMADRA_CLOUD_ISSUER":       "https://app.armadra.test",
		"ARMADRA_CLOUD_APP_ORIGIN":   "https://app.armadra.test",
		"ARMADRA_CLOUD_DATABASE_URL": "postgres://u:p@127.0.0.1:5432/db",
		"ARMADRA_CLOUD_REDIS_URL":    "redis://127.0.0.1:6379",
		"ARMADRA_CLOUD_MASTER_KEY":   key,
		"ARMADRA_CLOUD_RELAY_DOMAIN": "armadra.test",
	}
}

func with(changes map[string]string) config.MapEnv {
	env := base()
	for name, value := range changes {
		if value == "-" {
			delete(env, "ARMADRA_CLOUD_"+name)
		} else {
			env["ARMADRA_CLOUD_"+name] = value
		}
	}
	return env
}

func TestLoadDefaults(t *testing.T) {
	c, err := config.Load(base())
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "0.0.0.0:8100" || !c.RequireEmailVerified || c.JobWorkers != 2 || c.TrafficRetentionDays != 400 || c.LogLevel != "info" {
		t.Fatalf("缺省值：%+v", c)
	}
	if c.SMTP != nil || c.Turnstile != nil || c.TrustedProxy != nil || len(c.MasterKey) != 32 || len(c.Warnings) != 0 {
		t.Fatalf("可选项缺省为空：%+v", c)
	}
}

func TestLoadRejects(t *testing.T) {
	cases := []struct {
		name    string
		changes map[string]string
		want    string
	}{
		{"缺 ISSUER", map[string]string{"ISSUER": "-"}, "ISSUER 必填"},
		{"ISSUER 不是 https", map[string]string{"ISSUER": "http://app.armadra.test", "APP_ORIGIN": "http://app.armadra.test"}, "https 来源"},
		{"ISSUER 带路径", map[string]string{"ISSUER": "https://app.armadra.test/x", "APP_ORIGIN": "https://app.armadra.test/x"}, "不带路径"},
		{"APP_ORIGIN 不等于 ISSUER", map[string]string{"APP_ORIGIN": "https://console.armadra.test"}, "APP_ORIGIN 必须等于 ISSUER"},
		{"缺 DATABASE_URL", map[string]string{"DATABASE_URL": "-"}, "DATABASE_URL 必填"},
		{"远程库不带 sslmode", map[string]string{"DATABASE_URL": "postgres://u:p@db.example.com/db"}, "sslmode=verify-full 或 require"},
		{"远程库 sslmode=disable", map[string]string{"DATABASE_URL": "postgres://u:p@db.example.com/db?sslmode=disable"}, "sslmode=verify-full 或 require"},
		{"DATABASE_URL 不是 postgres", map[string]string{"DATABASE_URL": "mysql://u@h/db"}, "postgres://"},
		{"缺 REDIS_URL", map[string]string{"REDIS_URL": "-"}, "REDIS_URL 必填"},
		{"REDIS_URL 不合法", map[string]string{"REDIS_URL": "http://x"}, "REDIS_URL 不合法"},
		{"缺主密钥", map[string]string{"MASTER_KEY": "-"}, "MASTER_KEY 或 MASTER_KEY_FILE 必填"},
		{"主密钥太短", map[string]string{"MASTER_KEY": "abcd"}, "64 位十六进制"},
		{"缺 RELAY_DOMAIN", map[string]string{"RELAY_DOMAIN": "-"}, "RELAY_DOMAIN 必填"},
		{"RELAY_DOMAIN 不合法", map[string]string{"RELAY_DOMAIN": "-bad_.com"}, "RELAY_DOMAIN 不是合法的域名"},
		{"LISTEN 不合法", map[string]string{"LISTEN": "8100"}, "主机:端口"},
		{"REQUIRE_EMAIL_VERIFIED 不合法", map[string]string{"REQUIRE_EMAIL_VERIFIED": "maybe"}, "1 或 0"},
		{"JOB_WORKERS 不合法", map[string]string{"JOB_WORKERS": "0"}, "正整数"},
		{"LOG_LEVEL 不合法", map[string]string{"LOG_LEVEL": "trace"}, "LOG_LEVEL"},
		{"只配了 Turnstile 站点钥", map[string]string{"TURNSTILE_SITE_KEY": "k"}, "两个都配"},
		{"受信代理不合法", map[string]string{"TRUSTED_PROXY": "not-an-ip"}, "TRUSTED_PROXY 不合法"},
		{"受信代理偏好不合法", map[string]string{"TRUSTED_PROXY": "10.0.0.0/8", "TRUSTED_PROXY_PREFER": "x"}, "TRUSTED_PROXY_PREFER"},
		{"指标监听非回环", map[string]string{"METRICS_LISTEN": "0.0.0.0:9100"}, "只许回环"},
		{"SMTP none 非回环", map[string]string{"SMTP_HOST": "smtp.example.com", "SMTP_FROM": "a@b.c", "SMTP_TLS": "none"}, "只许回环"},
		{"SMTP 缺 From", map[string]string{"SMTP_HOST": "smtp.example.com"}, "SMTP_FROM 必填"},
		{"HIBP 不是 https", map[string]string{"HIBP_URL": "http://x"}, "HIBP_URL"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := config.Load(with(c.changes))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
			if strings.Contains(err.Error(), key) || strings.Contains(err.Error(), "u:p@") {
				t.Fatalf("错误里不该有秘密：%v", err)
			}
		})
	}
}

func TestLoopbackAllowsHTTPAndPlainDatabase(t *testing.T) {
	c, err := config.Load(with(map[string]string{
		"ISSUER": "http://127.0.0.1:8100", "APP_ORIGIN": "http://127.0.0.1:8100", "RELAY_DOMAIN": "src.localhost",
		"SMTP_HOST": "127.0.0.1", "SMTP_PORT": "1026", "SMTP_FROM": "noreply@armadra.test", "SMTP_TLS": "none",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Issuer != "http://127.0.0.1:8100" || c.SMTP == nil || c.SMTP.Port != 1026 || c.SMTP.TLS != "none" {
		t.Fatalf("%+v", c)
	}
}

func TestRemoteDatabaseWithSSL(t *testing.T) {
	for _, mode := range []string{"verify-full", "require"} {
		if _, err := config.Load(with(map[string]string{"DATABASE_URL": "postgres://u:p@db.example.com/db?sslmode=" + mode})); err != nil {
			t.Fatalf("%s 应通过：%v", mode, err)
		}
	}
}

func TestSecretsFromFilesAndConflicts(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "master")
	if err := os.WriteFile(keyFile, []byte(key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(with(map[string]string{"MASTER_KEY": "-", "MASTER_KEY_FILE": keyFile}))
	if err != nil || len(c.MasterKey) != 32 {
		t.Fatalf("从文件读主密钥：%v", err)
	}
	if _, err := config.Load(with(map[string]string{"MASTER_KEY_FILE": keyFile})); err == nil || !strings.Contains(err.Error(), "只能给一个") {
		t.Fatalf("MASTER_KEY 与 MASTER_KEY_FILE 同给应拒绝：%v", err)
	}
	if _, err := config.Load(with(map[string]string{"MASTER_KEY": "-", "MASTER_KEY_FILE": filepath.Join(dir, "none")})); err == nil {
		t.Fatal("文件不存在应拒绝")
	}
	secret := filepath.Join(dir, "smtp")
	if err := os.WriteFile(secret, []byte("hunter2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err = config.Load(with(map[string]string{"SMTP_HOST": "smtp.example.com", "SMTP_FROM": "a@b.c", "SMTP_PASSWORD_FILE": secret}))
	if err != nil || c.SMTP.Password != "hunter2" || c.SMTP.TLS != "starttls" || c.SMTP.Port != 587 {
		t.Fatalf("SMTP：%+v %v", c.SMTP, err)
	}
	// 配置对象的字符串形式不含秘密。
	for _, text := range []string{c.String(), strings.ReplaceAll(strings.ReplaceAll(c.String(), "Config", ""), "{", "")} {
		if strings.Contains(text, key) || strings.Contains(text, "hunter2") || strings.Contains(text, "u:p") {
			t.Fatalf("字符串里不该有秘密：%s", text)
		}
	}
}

func TestChallengeScopes(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "turnstile")
	if err := os.WriteFile(secret, []byte("s"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(with(map[string]string{"TURNSTILE_SITE_KEY": "site", "TURNSTILE_SECRET_FILE": secret}))
	if err != nil {
		t.Fatal(err)
	}
	want := []core.ChallengeScope{core.ScopeLogin, core.ScopeRegister, core.ScopeResetStart, core.ScopeLinksAccept}
	if !reflect.DeepEqual(c.Turnstile.Scope, want) {
		t.Fatalf("缺省范围四项都生效：%v", c.Turnstile.Scope)
	}
	if len(c.Warnings) != 0 {
		t.Fatalf("缺省范围不再有启动警告：%v", c.Warnings)
	}
	if _, err := config.Load(with(map[string]string{"TURNSTILE_SITE_KEY": "site", "TURNSTILE_SECRET_FILE": secret, "CHALLENGE_SCOPE": "auth.nope"})); err == nil {
		t.Fatal("不认识的范围应拒绝")
	}
	c, err = config.Load(with(map[string]string{"TURNSTILE_SITE_KEY": "site", "TURNSTILE_SECRET_FILE": secret, "CHALLENGE_SCOPE": "auth.changePassword,auth.login"}))
	if err != nil || len(c.Turnstile.Scope) != 2 || len(c.Warnings) != 0 {
		t.Fatalf("显式范围：%+v %v", c.Turnstile, err)
	}
	if _, err := config.Load(with(map[string]string{"TURNSTILE_SITE_KEY": "site", "TURNSTILE_SECRET_FILE": filepath.Join(dir, "no")})); err == nil {
		t.Fatal("密钥文件不存在应拒绝")
	}
}

func TestTrustedProxyAndUnknownVariables(t *testing.T) {
	env := with(map[string]string{"TRUSTED_PROXY": "10.0.0.0/8, 127.0.0.1/32", "TRUSTED_PROXY_PREFER": "real-ip"})
	env["ARMADRA_CLOUD_OAUTH_GITHUB_ID"] = "x"
	env["UNRELATED"] = "y"
	c, err := config.Load(env)
	if err != nil {
		t.Fatal(err)
	}
	if c.TrustedProxy == nil || len(c.TrustedProxy.Proxies) != 2 || c.TrustedProxy.Prefer != "real-ip" {
		t.Fatalf("受信代理：%+v", c.TrustedProxy)
	}
	if len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "ARMADRA_CLOUD_OAUTH_GITHUB_ID") {
		t.Fatalf("未知变量只记一条警告：%v", c.Warnings)
	}
	c, _ = config.Load(with(map[string]string{"TRUSTED_PROXY": "10.0.0.1"}))
	if c.TrustedProxy.Prefer != "forwarded-for" {
		t.Fatalf("缺省偏好：%q", c.TrustedProxy.Prefer)
	}
}
