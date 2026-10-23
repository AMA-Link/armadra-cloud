package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/cli"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/config"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/server"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/pgtest"
)

const masterKey = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

func run(t *testing.T, env config.MapEnv, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = cli.Run(args, env, &out, &errOut, cli.Options{Version: "9.9.9"})
	return code, out.String(), errOut.String()
}

func redisURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("REDIS_URL")
	if url == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("CI 里没有 REDIS_URL")
		}
		t.Skip("没有 Redis：设置 REDIS_URL，跳过")
	}
	return url
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port
}

// testEnv 给一个隔离 schema 的完整环境（schema 经连接串的 search_path 传入）。
func testEnv(t *testing.T) config.MapEnv {
	t.Helper()
	url, schema := pgtest.Schema(t)
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	return config.MapEnv{
		"ARMADRA_CLOUD_ISSUER":       "http://127.0.0.1:8100",
		"ARMADRA_CLOUD_APP_ORIGIN":   "http://127.0.0.1:8100",
		"ARMADRA_CLOUD_LISTEN":       "127.0.0.1:" + strconv.Itoa(freePort(t)),
		"ARMADRA_CLOUD_DATABASE_URL": url + sep + "search_path=" + schema,
		"ARMADRA_CLOUD_REDIS_URL":    redisURL(t),
		"ARMADRA_CLOUD_MASTER_KEY":   masterKey,
		"ARMADRA_CLOUD_RELAY_DOMAIN": "src.localhost",
	}
}

func TestUsageAndSimpleCommands(t *testing.T) {
	if code, out, _ := run(t, config.MapEnv{}); code != 64 || !strings.Contains(out, "用法: armadra-cloud <命令> [选项]") {
		t.Fatalf("无参数：%d %q", code, out)
	}
	if code, out, _ := run(t, config.MapEnv{}, "--help"); code != 0 || !strings.Contains(out, "keys rotate") {
		t.Fatalf("--help：%d", code)
	}
	if code, out, _ := run(t, config.MapEnv{}, "version"); code != 0 || strings.TrimSpace(out) != "9.9.9" {
		t.Fatalf("version：%d %q", code, out)
	}
	if code, _, errOut := run(t, config.MapEnv{}, "bogus"); code != 64 || !strings.Contains(errOut, "未知命令：bogus") {
		t.Fatalf("未知命令：%d %q", code, errOut)
	}
	if code, _, errOut := run(t, config.MapEnv{}, "keys"); code != 64 || !strings.Contains(errOut, "未知命令") {
		t.Fatalf("keys 缺子命令：%d %q", code, errOut)
	}
	for _, args := range [][]string{{"nodes", "token"}, {"nodes", "revoke"}} {
		if code, _, errOut := run(t, config.MapEnv{}, args...); code != 64 || !strings.Contains(errOut, "这个命令还没有提供") {
			t.Fatalf("%v：%d %q", args, code, errOut)
		}
	}
	for _, args := range [][]string{{"serve", "--x"}, {"migrate", "extra"}, {"health", "x"}, {"keys", "rotate", "x"}} {
		if code, _, _ := run(t, config.MapEnv{}, args...); code != 64 {
			t.Fatalf("%v 不收参数：%d", args, code)
		}
	}
}

func TestConfigErrorsExit64WithoutSecrets(t *testing.T) {
	env := config.MapEnv{"ARMADRA_CLOUD_DATABASE_URL": "postgres://user:s3cret@db.example.com/x"}
	code, _, errOut := run(t, env, "migrate")
	if code != 64 || !strings.Contains(errOut, "sslmode") || strings.Contains(errOut, "s3cret") {
		t.Fatalf("migrate 配置错误：%d %q", code, errOut)
	}
	code, _, errOut = run(t, config.MapEnv{}, "serve")
	if code != 64 || !strings.Contains(errOut, "ARMADRA_CLOUD_ISSUER 必填") {
		t.Fatalf("serve 缺配置：%d %q", code, errOut)
	}
	code, _, errOut = run(t, config.MapEnv{}, "keys", "rotate")
	if code != 64 || !strings.Contains(errOut, "DATABASE_URL 必填") {
		t.Fatalf("keys rotate 缺配置：%d %q", code, errOut)
	}
}

func TestMigrateThenServeRefusesBehindLedger(t *testing.T) {
	env := testEnv(t)
	// 没迁移：serve 拒绝并提示先迁移，退出码 1。
	code, _, errOut := run(t, env, "serve")
	if code != 1 || !strings.Contains(errOut, "数据库版本落后，先运行 armadra-cloud migrate") {
		t.Fatalf("serve 不自动迁移：%d %q", code, errOut)
	}
	code, out, errOut := run(t, env, "migrate")
	if code != 0 || !strings.Contains(out, "本次新增 10 个") {
		t.Fatalf("migrate：%d %q %q", code, out, errOut)
	}
	// 再迁移一次是空操作。
	code, out, _ = run(t, env, "migrate")
	if code != 0 || !strings.Contains(out, "本次新增 0 个") {
		t.Fatalf("重复迁移：%d %q", code, out)
	}
}

// start 在后台跑 serve，答运行信息与「停」的函数。
func start(t *testing.T, env config.MapEnv) (*server.Running, func() int) {
	t.Helper()
	ready := make(chan *server.Running, 1)
	signals := make(chan os.Signal, 1)
	exit := make(chan int, 1)
	var out, errOut bytes.Buffer
	go func() {
		exit <- cli.Run([]string{"serve"}, env, &out, &errOut, cli.Options{
			Version: "9.9.9", Signals: signals, OnReady: func(r *server.Running) { ready <- r },
		})
	}()
	select {
	case r := <-ready:
		var once sync.Once
		stop := func() int {
			var code int
			once.Do(func() {
				signals <- os.Interrupt
				select {
				case code = <-exit:
				case <-time.After(20 * time.Second):
					t.Fatal("serve 没有按时退出")
				}
			})
			return code
		}
		t.Cleanup(func() { stop() })
		return r, stop
	case code := <-exit:
		t.Fatalf("serve 提前退出：%d %s", code, errOut.String())
	case <-time.After(30 * time.Second):
		t.Fatal("serve 没有按时就绪")
	}
	return nil, nil
}

func getJSON(t *testing.T, url string) (int, map[string]any, http.Header) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	return resp.StatusCode, body, resp.Header
}

func TestServeBootstrapsHealthReadyAndAnswers501(t *testing.T) {
	env := testEnv(t)
	if code, _, errOut := run(t, env, "migrate"); code != 0 {
		t.Fatal(errOut)
	}
	running, stop := start(t, env)

	status, body, header := getJSON(t, running.URL+"/health")
	if status != 200 || body["ok"] != true || body["mode"] != "saas" || body["version"] != "9.9.9" || header.Get("Armadra-Protocol") == "" {
		t.Fatalf("/health：%d %v", status, body)
	}
	status, body, _ = getJSON(t, running.URL+"/ready")
	if status != 200 || body["ok"] != true {
		t.Fatalf("/ready：%d %v", status, body)
	}
	// 还没实现的过程答 501 not_implemented；未知路径 404。
	for _, path := range []string{"/v1/source-transfers", "/v1/api-tokens", "/v1/teams/x/groups"} {
		status, body, _ = getJSON(t, running.URL+path)
		if status != 501 || body["code"] != "not_implemented" {
			t.Fatalf("%s 应答 501：%d %v", path, status, body)
		}
	}
	if status, body, _ = getJSON(t, running.URL+"/nope"); status != 404 || body["code"] != "not_found" {
		t.Fatalf("未知路径：%d %v", status, body)
	}
	// 引导出的租户：issuer 与配置一致、一把包裹的签名钥、三个月审计分区。
	tenant, err := running.DB.ReadTenant(context.Background(), server.TenantID)
	if err != nil || tenant == nil || tenant.Issuer != "http://127.0.0.1:8100" {
		t.Fatalf("租户：%+v %v", tenant, err)
	}
	var wrapped, partitions int
	_ = running.DB.Pool().QueryRow(context.Background(), "SELECT count(*)::int FROM signing_keys WHERE private_key_wrap = 'aes256gcm-v1'").Scan(&wrapped)
	_ = running.DB.Pool().QueryRow(context.Background(), `SELECT count(*)::int FROM pg_inherits i JOIN pg_class p ON p.oid = i.inhparent
	  JOIN pg_class c ON c.oid = i.inhrelid WHERE p.oid = to_regclass('audit_log') AND c.relname LIKE 'audit_log_2%'`).Scan(&partitions)
	if wrapped != 1 || partitions != 3 {
		t.Fatalf("引导结果：包裹的钥 %d，分区 %d", wrapped, partitions)
	}
	// health 命令探活 LISTEN 的端口。
	if code, _, _ := run(t, env, "health"); code != 0 {
		t.Fatal("health 应成功")
	}
	if code := stop(); code != 0 {
		t.Fatalf("退出码：%d", code)
	}
	if code, _, _ := run(t, env, "health"); code != 1 {
		t.Fatal("停了之后 health 应失败")
	}
}

func TestServeRefusesIssuerChangeAndNoMasterKey(t *testing.T) {
	env := testEnv(t)
	if code, _, errOut := run(t, env, "migrate"); code != 0 {
		t.Fatal(errOut)
	}
	_, stop := start(t, env)
	stop()
	other := config.MapEnv{}
	for name, value := range env {
		other[name] = value
	}
	other["ARMADRA_CLOUD_ISSUER"] = "http://127.0.0.1:9999"
	other["ARMADRA_CLOUD_APP_ORIGIN"] = "http://127.0.0.1:9999"
	code, _, errOut := run(t, other, "serve")
	if code != 1 || !strings.Contains(errOut, "issuer 与库里的不一致") {
		t.Fatalf("issuer 不一致应拒绝：%d %q", code, errOut)
	}
	wrong := config.MapEnv{}
	for name, value := range env {
		wrong[name] = value
	}
	wrong["ARMADRA_CLOUD_MASTER_KEY"] = strings.Repeat("11", 32)
	code, _, errOut = run(t, wrong, "serve")
	if code != 1 || !strings.Contains(errOut, "主密钥不对") {
		t.Fatalf("主密钥不对应拒绝：%d %q", code, errOut)
	}
}

func TestKeysRotateWhileServing(t *testing.T) {
	env := testEnv(t)
	if code, _, errOut := run(t, env, "migrate"); code != 0 {
		t.Fatal(errOut)
	}
	code, _, errOut := run(t, env, "keys", "rotate")
	if code != 1 || !strings.Contains(errOut, "还没有存储租户") {
		t.Fatalf("没引导过不能轮换：%d %q", code, errOut)
	}
	running, _ := start(t, env)
	keys, err := running.DB.Keys(server.TenantID).SigningKeys(context.Background())
	if err != nil || len(keys) != 1 {
		t.Fatalf("初始：%v %v", keys, err)
	}
	code, out, errOut := run(t, env, "keys", "rotate")
	if code != 0 || !strings.Contains(out, "已轮换签名钥：k2") {
		t.Fatalf("keys rotate：%d %q %q", code, out, errOut)
	}
	// 运行中的实例察觉租户修订号变化，重读钥缓存。
	deadline := time.Now().Add(15 * time.Second)
	for {
		keys, _ = running.DB.Keys(server.TenantID).SigningKeys(context.Background())
		if len(keys) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("运行中的实例没有察觉轮换：%d", len(keys))
		}
		time.Sleep(200 * time.Millisecond)
	}
	if keys[0].RetiredAtMs == nil {
		t.Fatal("旧钥应定在 1 小时后退役")
	}
}
