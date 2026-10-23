package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/api"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/config"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/mail"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/server"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/pgtest"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/postgres"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil/schema"
)

const (
	appOrigin = "http://127.0.0.1:8100"
	password  = "plover-quartz-lantern-41"
	masterHex = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
)

// clock 是可控的时钟：整个控制面（令牌、会话寿命、锁定、提权）都读它。
type clock struct {
	mu sync.Mutex
	at time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.at = c.at.Add(d)
	c.mu.Unlock()
}

func (c *clock) Ms() int64 { return c.Now().UnixMilli() }

// syncBuffer 收日志，测试结束时扫描里面有没有秘密。
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

type rigOptions struct {
	turnstile bool
	verifier  core.ChallengeVerifier
	extra     map[string]api.Handler
	env       map[string]string
	// mail 换掉发信方（缺省内存发信方 rig.mail）。
	mail mail.Sender
	// grantsHook 在断言签发的交错点停住。
	grantsHook func(stage string)
}

// rig 是一套真实的控制面：PostgreSQL（随机 schema）+ Redis + 完整路由，经 HTTP 访问。
type rig struct {
	t      *testing.T
	run    *server.Running
	clock  *clock
	log    *syncBuffer
	cfg    *config.Config
	url    string
	ip     string
	router *api.Router
	oapi   *schema.OpenAPI
	// mail 是缺省的内存发信方；换了发信方时为 nil。
	mail *mail.MemorySender
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

func newRig(t *testing.T, options rigOptions) *rig {
	t.Helper()
	dbURL, schemaName := pgtest.Schema(t)
	sep := "?"
	if strings.Contains(dbURL, "?") {
		sep = "&"
	}
	dbURL += sep + "search_path=" + schemaName
	ctx := context.Background()
	migrated, err := postgres.Open(ctx, postgres.Options{URL: dbURL})
	if err != nil {
		t.Fatal(err)
	}
	_ = migrated.Close()

	env := config.MapEnv{
		"ARMADRA_CLOUD_ISSUER":        appOrigin,
		"ARMADRA_CLOUD_APP_ORIGIN":    appOrigin,
		"ARMADRA_CLOUD_LISTEN":        "127.0.0.1:0",
		"ARMADRA_CLOUD_DATABASE_URL":  dbURL,
		"ARMADRA_CLOUD_REDIS_URL":     redisURL(t),
		"ARMADRA_CLOUD_MASTER_KEY":    masterHex,
		"ARMADRA_CLOUD_RELAY_DOMAIN":  "src.localhost",
		"ARMADRA_CLOUD_TRUSTED_PROXY": "127.0.0.1/32,::1/128",
		"ARMADRA_CLOUD_NAME":          "测试控制面",
	}
	if options.turnstile {
		secret := filepath.Join(t.TempDir(), "turnstile")
		if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
			t.Fatal(err)
		}
		env["ARMADRA_CLOUD_TURNSTILE_SITE_KEY"] = "site-key"
		env["ARMADRA_CLOUD_TURNSTILE_SECRET_FILE"] = secret
		env["ARMADRA_CLOUD_CHALLENGE_SCOPE"] = "auth.login,auth.changePassword"
	}
	for name, value := range options.env {
		env["ARMADRA_CLOUD_"+name] = value
	}
	cfg, err := config.Load(env)
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{at: time.Now().Truncate(time.Second)}
	logs := &syncBuffer{}
	var memory *mail.MemorySender
	sender := options.mail
	if sender == nil && cfg.SMTP == nil {
		memory = &mail.MemorySender{}
		sender = memory
	}
	run, err := server.Serve(ctx, cfg, server.Deps{
		Log: obs.NewNamedLogger(logs, obs.LevelDebug, "cloud"), Now: clk.Now, Version: "test",
		Challenge: options.verifier, ExtraHandlers: options.extra, Mail: sender, GrantsHook: options.grantsHook,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(run.Close)
	router, err := api.NewRouter(nil)
	if err != nil {
		t.Fatal(err)
	}
	oapi := schema.Load(t, filepath.Join(testutil.ProtocolDir(t), "schema", "cloud-openapi.json"))
	return &rig{
		t: t, run: run, clock: clk, log: logs, cfg: cfg, url: run.URL, router: router, oapi: oapi, mail: memory,
		// 每个测试一个随机的客户端 IP（经受信代理头），限流桶在 Redis 里互不干扰。
		ip: fmt.Sprintf("10.%d.%d.%d", rand.IntN(256), rand.IntN(256), rand.IntN(256)),
	}
}

type response struct {
	status int
	header http.Header
	body   []byte
	json   map[string]any
}

// headers 是请求头；同名多值各写一行。
type headers map[string][]string

func hdr(kv ...string) headers {
	h := headers{}
	for i := 0; i+1 < len(kv); i += 2 {
		h[kv[i]] = append(h[kv[i]], kv[i+1])
	}
	return h
}

func (h headers) add(kv ...string) headers {
	out := headers{}
	for name, values := range h {
		out[name] = append([]string(nil), values...)
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i]] = append(out[kv[i]], kv[i+1])
	}
	return out
}

// do 发一个请求；200 的答复按 OpenAPI 的输出 schema 校验。
func (r *rig) do(method, path string, body any, h headers) response {
	r.t.Helper()
	var reader io.Reader
	switch typed := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(typed)
	default:
		data, err := json.Marshal(body)
		if err != nil {
			r.t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, r.url+path, reader)
	if err != nil {
		r.t.Fatal(err)
	}
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if _, ok := h["X-Forwarded-For"]; !ok {
		req.Header.Set("X-Forwarded-For", r.ip)
	}
	for name, values := range h {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	res, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		r.t.Fatal(err)
	}
	out := response{status: res.StatusCode, header: res.Header, body: data}
	_ = json.Unmarshal(data, &out.json)
	lookup := method
	if lookup == http.MethodHead {
		lookup = http.MethodGet
	}
	stub := strings.HasPrefix(path, "/v1/admin/") // 测试挂的假处理器，不按契约验形状
	if match := r.router.Match(lookup, strings.SplitN(path, "?", 2)[0]); match != nil && !stub && res.StatusCode == http.StatusOK && method != http.MethodHead {
		if err := r.oapi.ValidateOutput(match.Route.Procedure, data); err != nil {
			r.t.Fatalf("%s %s 的答复不符合 %s 的输出 schema：%v\n%s", method, path, match.Route.Procedure, err, data)
		}
	}
	return out
}

func (r *rig) expect(res response, status int, code string) {
	r.t.Helper()
	if res.status != status {
		r.t.Fatalf("状态码 %d，应为 %d：%s", res.status, status, res.body)
	}
	if code != "" && res.json["code"] != code {
		r.t.Fatalf("错误码 %v，应为 %s：%s", res.json["code"], code, res.body)
	}
}

func (r *rig) email() string { return fmt.Sprintf("u%08x@example.com", rand.Uint32()) }

var device = map[string]any{"platform": "browser", "name": "测试浏览器"}

// session 是登录类答案里的会话。
type session struct {
	access, refresh, deviceID, accountID, csrf string
	cookies                                    map[string]string
	expiresAtMs                                int64
}

func (s session) bearer() headers { return hdr("Authorization", "Bearer "+s.access) }

// cookieHeader 是 Cookie 请求头的值（按名字顺序）。
func (s session) cookieHeader(names ...string) string {
	var parts []string
	for _, name := range names {
		parts = append(parts, name+"="+s.cookies[name])
	}
	return strings.Join(parts, "; ")
}

// write 是 Cookie 会话的写请求头：Origin、CSRF 与访问 Cookie。
func (s session) write() headers {
	return hdr("Origin", appOrigin, "X-Armadra-CSRF", s.csrf, "Cookie", s.cookieHeader("__Host-armadra-cloud"))
}

func parseSession(r *rig, res response) session {
	r.t.Helper()
	body, ok := res.json["session"].(map[string]any)
	if !ok {
		r.t.Fatalf("答复里没有会话：%s", res.body)
	}
	account := body["account"].(map[string]any)
	dev := body["device"].(map[string]any)
	s := session{
		access: body["accessToken"].(string), deviceID: dev["deviceId"].(string), accountID: account["accountId"].(string),
		cookies: map[string]string{}, expiresAtMs: int64(body["expiresAtMs"].(float64)),
	}
	if refresh, ok := body["refreshToken"].(string); ok {
		s.refresh = refresh
	}
	if csrf, ok := res.json["csrfToken"].(string); ok {
		s.csrf = csrf
	}
	for _, line := range res.header.Values("Set-Cookie") {
		name, rest, _ := strings.Cut(line, "=")
		value, _, _ := strings.Cut(rest, ";")
		if !strings.Contains(line, "Max-Age=0") {
			s.cookies[name] = value
		}
	}
	return s
}

func (r *rig) register(email string, h headers) response {
	r.t.Helper()
	return r.do("POST", "/v1/auth/register", map[string]any{
		"email": email, "password": password, "displayName": "测试用户", "device": device,
	}, h)
}

// registerBearer 注册一个 Bearer 传输的账号。
func (r *rig) registerBearer(email string) session {
	r.t.Helper()
	res := r.register(email, nil)
	r.expect(res, 200, "")
	return parseSession(r, res)
}

// registerCookie 注册一个 Cookie 传输的账号（带控制台 Origin、不带 Authorization）。
func (r *rig) registerCookie(email string) session {
	r.t.Helper()
	res := r.register(email, hdr("Origin", appOrigin))
	r.expect(res, 200, "")
	return parseSession(r, res)
}

func (r *rig) login(email, pw string, h headers) response {
	r.t.Helper()
	return r.do("POST", "/v1/auth/login", map[string]any{"account": email, "password": pw, "device": device}, h)
}

func (r *rig) exec(sql string, args ...any) {
	r.t.Helper()
	if _, err := r.run.DB.Pool().Exec(context.Background(), sql, args...); err != nil {
		r.t.Fatalf("%s: %v", sql, err)
	}
}

// execTx 在一个事务里执行几条语句（团队与 owner 成员要同事务插，约束在提交时检查）。
func (r *rig) execTx(statements ...[2]any) {
	r.t.Helper()
	ctx := context.Background()
	tx, err := r.run.DB.Pool().Begin(ctx)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, statement := range statements {
		var args []any
		if statement[1] != nil {
			args = statement[1].([]any)
		}
		if _, err := tx.Exec(ctx, statement[0].(string), args...); err != nil {
			r.t.Fatalf("%v: %v", statement[0], err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		r.t.Fatal(err)
	}
}
