package personal

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func runCLI(args []string, env Env) (code int, out, errOut string) {
	var stdout, stderr bytes.Buffer
	code = RunCLI(args, env, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestRunCLIExitCodes(t *testing.T) {
	dir := t.TempDir()
	pw := passwordFile(t, testPassword)
	cases := []struct {
		name    string
		args    []string
		code    int
		out     string
		errText string
	}{
		{"没有子命令", nil, 64, "用法: armadra-relay personal", ""},
		{"--help", []string{"--help"}, 0, "用法: armadra-relay personal", ""},
		{"-h", []string{"-h"}, 0, "用法: armadra-relay personal", ""},
		{"子命令带 --help", []string{"serve", "--help"}, 0, "用法: armadra-relay personal", ""},
		{"未知子命令", []string{"nope"}, 64, "", "未知子命令：personal nope"},
		{"缺 --data-dir", []string{"status"}, 64, "", "缺少 --data-dir（或 RELAY_DATA_DIR）"},
		{"未知选项", []string{"status", "--data-dir", dir, "--bogus", "1"}, 64, "", "未知选项：--bogus"},
		{"没初始化", []string{"status", "--data-dir", filepath.Join(dir, "none")}, 64, "", "还没有初始化"},
		{"serve 缺 host", []string{"serve", "--data-dir", dir}, 64, "", "缺少 --host"},
		{"serve 指标监听非回环没令牌", []string{"serve", "--data-dir", dir, "--host", "127.0.0.1", "--tls", "plain", "--metrics-listen", ":9100"}, 64, "", "--metrics-listen 不是回环地址时必须配 RELAY_METRICS_TOKEN：:9100"},
		{"init 成功", []string{"init", "--data-dir", filepath.Join(dir, "d"), "--account", "me", "--password-file", pw, "--host", "127.0.0.1", "--tls", "plain"}, 0, "已初始化：", ""},
		{"init 再来一次", []string{"init", "--data-dir", filepath.Join(dir, "d"), "--account", "me", "--password-file", pw, "--host", "127.0.0.1", "--tls", "plain"}, 64, "", "已经初始化过"},
		{"status 成功", []string{"status", "--data-dir", filepath.Join(dir, "d")}, 0, "账号：me", ""},
		{"links 缺动作", []string{"links", "--data-dir", dir}, 64, "", "links 只有 list 与 revoke"},
		{"links revoke 不存在", []string{"links", "revoke", "0123456789abcdef", "--data-dir", filepath.Join(dir, "d")}, 64, "", "没有这条有效链接"},
		{"migrate --to file", []string{"migrate", "--data-dir", filepath.Join(dir, "d"), "--to", "file"}, 64, "", "不再写状态文件"},
		{"doctor", []string{"doctor", "--data-dir", filepath.Join(dir, "d"), "--tls", "plain", "--host", "127.0.0.1"}, 0, "检查完成：", ""},
		{"doctor 数据目录不存在", []string{"doctor", "--data-dir", filepath.Join(dir, "none"), "--tls", "plain"}, 1, "失败  数据目录", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, out, errOut := runCLI(c.args, Env{})
			if code != c.code || !strings.Contains(out, c.out) || !strings.Contains(errOut, c.errText) {
				t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
			}
		})
	}
}

func TestRunCLIEnvDataDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "d")
	pw := passwordFile(t, testPassword)
	env := Env{"RELAY_DATA_DIR": dir, "RELAY_HOST": "127.0.0.1", "RELAY_PERSONAL_ACCOUNT": "me", "RELAY_PERSONAL_PASSWORD_FILE": pw, "RELAY_TLS": "plain"}
	if code, _, errOut := runCLI([]string{"init"}, env); code != 0 {
		t.Fatalf("init 走环境变量：%d %s", code, errOut)
	}
	if code, out, _ := runCLI([]string{"status"}, env); code != 0 || !strings.Contains(out, "账号：me") {
		t.Fatalf("status：%d %s", code, out)
	}
}

func TestHealth(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer ok.Close()
	port := ok.Listener.Addr().(*net.TCPAddr).Port
	if code := Health(Env{"RELAY_LISTEN": "0.0.0.0:" + strconv.Itoa(port)}); code != 0 {
		t.Fatalf("明文服务 200 应退出 0：%d", code)
	}
	secure := httptest.NewTLSServer(ok.Config.Handler)
	defer secure.Close()
	port = secure.Listener.Addr().(*net.TCPAddr).Port
	if code := Health(Env{"RELAY_LISTEN": "0.0.0.0:" + strconv.Itoa(port)}); code != 0 {
		t.Fatalf("自签 HTTPS 200 应退出 0：%d", code)
	}
	unavailable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }))
	defer unavailable.Close()
	port = unavailable.Listener.Addr().(*net.TCPAddr).Port
	if code := Health(Env{"RELAY_LISTEN": "0.0.0.0:" + strconv.Itoa(port)}); code != 1 {
		t.Fatalf("503 应退出 1：%d", code)
	}
	if code := Health(Env{"RELAY_LISTEN": "127.0.0.1:1"}); code != 1 {
		t.Fatalf("连不上应退出 1：%d", code)
	}
}

// deploy/README.md 的 personal serve 参数表里的每个参数与环境变量，命令行都认识。
func TestFlagsMatchDeployReadme(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	start := strings.Index(text, "中继（`armadra-relay personal serve`")
	end := strings.Index(text, "限流额度也可写在状态文件")
	if start < 0 || end < start {
		t.Fatal("deploy/README.md 里找不到 personal serve 的参数表")
	}
	table := text[start:end]
	flagRe := regexp.MustCompile("`--([a-z][a-z-]*)")
	envRe := regexp.MustCompile("`?(RELAY_[A-Z_]+)")
	args, err := os.ReadFile("args.go")
	if err != nil {
		t.Fatal(err)
	}
	var flags, envs int
	for _, m := range flagRe.FindAllStringSubmatch(table, -1) {
		flags++
		if !serveFlags[m[1]] {
			t.Errorf("README 里有 --%s，命令行不认识", m[1])
		}
		if !strings.Contains(PersonalUsage, "--"+m[1]) {
			t.Errorf("用法里没写 --%s", m[1])
		}
	}
	for _, m := range envRe.FindAllStringSubmatch(table, -1) {
		envs++
		if !strings.Contains(string(args), `"`+m[1]+`"`) {
			t.Errorf("README 里有 %s，args.go 没读", m[1])
		}
	}
	if flags < 30 || envs < 30 {
		t.Fatalf("参数表解析出的条目太少：flags=%d envs=%d", flags, envs)
	}
	// 反过来：命令行认识的 serve 参数，README 都写了（--database-url-file 等在表里）。
	for name := range serveFlags {
		if !strings.Contains(table, "--"+name) {
			t.Errorf("命令行认识 --%s，README 没写", name)
		}
	}
}
