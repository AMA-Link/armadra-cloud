package personal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/pgtest"
	"github.com/AMA-Link/armadra-cloud/go/internal/traffic"
)

// syncBuffer 是并发安全的日志去处。
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

// startServe 用命令行参数起一个个人中转（监听随机端口）。
func startServe(t *testing.T, dataDir string, extra []string, deps ServeDeps) (*Running, *syncBuffer) {
	t.Helper()
	args := append([]string{"--data-dir", dataDir, "--host", "127.0.0.1", "--listen", "127.0.0.1:0"}, extra...)
	options, err := ParseServe(args, Env{}, t.TempDir())
	if err != nil {
		t.Fatalf("参数：%v", err)
	}
	logs := &syncBuffer{}
	if deps.Log == nil {
		deps.Log = obs.NewLogger(logs, obs.LevelDebug)
	}
	if deps.WatchInterval == 0 {
		deps.WatchInterval = 50 * time.Millisecond
	}
	running, err := Serve(context.Background(), options, deps)
	if err != nil {
		t.Fatalf("Serve：%v", err)
	}
	t.Cleanup(running.Close)
	return running, logs
}

// initAt 先建库；issuer 与稍后 Serve 的 --public-origin 一致，免得触发「对外地址变了」。
func initAt(t *testing.T, issuer string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	st, err := CreateState(context.Background(), CreateStateInput{
		DataDir: dir, Account: "me", Password: testPassword, Issuer: issuer,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	return dir
}

func plainServe(t *testing.T, extra ...string) (*Running, *syncBuffer, string) {
	t.Helper()
	dir := initAt(t, "http://127.0.0.1:1")
	args := append([]string{"--tls", "plain", "--public-origin", "http://127.0.0.1:1"}, extra...)
	running, logs := startServe(t, dir, args, ServeDeps{})
	return running, logs, dir
}

func request(t *testing.T, client *http.Client, method, url string, body any, token string) (int, http.Header, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s：%v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, data
}

type loginResult struct {
	Session struct {
		AccessToken string `json:"accessToken"`
		Device      struct {
			DeviceID string `json:"deviceId"`
		} `json:"device"`
	} `json:"session"`
}

func login(t *testing.T, client *http.Client, base string) loginResult {
	t.Helper()
	status, _, data := request(t, client, "POST", base+"/v1/auth/login", map[string]any{
		"account": "me", "password": testPassword, "device": map[string]string{"platform": "desktop", "name": "测试"},
	}, "")
	if status != 200 {
		t.Fatalf("登录 %d：%s", status, data)
	}
	var out loginResult
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestServePlainBasics(t *testing.T) {
	running, logs, dir := plainServe(t)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	base := running.URL

	status, _, data := request(t, client, "GET", base+"/health", nil, "")
	if status != 200 || !strings.Contains(string(data), `"mode":"personal"`) {
		t.Fatalf("/health %d：%s", status, data)
	}
	status, _, data = request(t, client, "GET", base+"/ready", nil, "")
	if status != 200 || !strings.Contains(string(data), "dataDir") {
		t.Fatalf("/ready %d：%s", status, data)
	}
	for _, path := range []string{"/", "/app"} {
		status, header, _ := request(t, client, "GET", base+path, nil, "")
		if status != 308 || header.Get("Location") != "/app/" {
			t.Fatalf("%s 应 308 到 /app/：%d %v", path, status, header)
		}
	}
	status, _, data = request(t, client, "GET", base+"/app/", nil, "")
	if status != 503 {
		t.Fatalf("没有页面产物时 /app/ 应 503：%d %s", status, data)
	}
	status, _, _ = request(t, client, "GET", base+"/ca.crt", nil, "")
	if status != 404 {
		t.Fatalf("明文模式没有 CA：/ca.crt 应 404，得 %d", status)
	}
	status, _, data = request(t, client, "GET", base+"/.well-known/armadra-platform", nil, "")
	if status != 200 || !strings.Contains(string(data), `"issuer":"http://127.0.0.1:1"`) {
		t.Fatalf("platform.info %d：%s", status, data)
	}
	status, _, data = request(t, client, "POST", base+"/v1/auth/login", map[string]any{
		"account": "me", "password": "wrong-password-xyz", "device": map[string]string{"platform": "desktop", "name": "x"},
	}, "")
	if status != 401 || !strings.Contains(string(data), "credentials_invalid") {
		t.Fatalf("错口令 %d：%s", status, data)
	}
	login(t, client, base)
	if _, err := os.Stat(filepath.Join(dir, RunLockFile)); err != nil {
		t.Fatalf("serve 运行中应有 serve.pid：%v", err)
	}
	// 口令与令牌不进日志。
	if text := logs.String(); strings.Contains(text, testPassword) || !strings.Contains(text, "个人中转已启动") {
		t.Fatalf("日志不对：%s", text)
	}
	running.Close()
	if _, err := os.Stat(filepath.Join(dir, RunLockFile)); !os.IsNotExist(err) {
		t.Fatalf("停止后 serve.pid 应删掉：%v", err)
	}
	if !strings.Contains(logs.String(), "个人中转已停止") {
		t.Fatalf("没有停止日志：%s", logs.String())
	}
}

func TestServeSelfSignedTLS(t *testing.T) {
	dir := initAt(t, "https://127.0.0.1:1")
	running, _ := startServe(t, dir, []string{"--public-origin", "https://127.0.0.1:1"}, ServeDeps{})
	if !strings.HasPrefix(running.URL, "https://") || running.Fingerprint == "" {
		t.Fatalf("自签应是 https 并有指纹：%s %q", running.URL, running.Fingerprint)
	}
	insecure := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // 先取 /ca.crt
	status, header, ca := request(t, insecure, "GET", running.URL+"/ca.crt", nil, "")
	if status != 200 {
		t.Fatalf("/ca.crt %d", status)
	}
	_ = header
	block, _ := pem.Decode(ca)
	if block == nil {
		t.Fatal("/ca.crt 不是 PEM")
	}
	sum := sha256.Sum256(block.Bytes)
	if hex.EncodeToString(sum[:]) != running.Fingerprint {
		t.Fatalf("指纹不对：%x vs %s", sum, running.Fingerprint)
	}
	pool := x509.NewCertPool()
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	pool.AddCert(cert)
	trusted := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	if status, _, _ := request(t, trusted, "GET", running.URL+"/health", nil, ""); status != 200 {
		t.Fatalf("按 CA 校验过的 /health：%d", status)
	}
	// TLS 至少 1.2。
	old := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true, MaxVersion: tls.VersionTLS11, //nolint:gosec // 验证服务端拒绝旧版本
	}}}
	if _, err := old.Get(running.URL + "/health"); err == nil {
		t.Fatal("TLS 1.1 应被拒绝")
	}
}

func TestServeRefusals(t *testing.T) {
	t.Run("状态文件拒绝启动并提示迁移", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, StateFile), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		options, err := ParseServe([]string{"--data-dir", dir, "--host", "127.0.0.1", "--tls", "plain", "--listen", "127.0.0.1:0"}, Env{}, dir)
		if err != nil {
			t.Fatal(err)
		}
		_, err = Serve(context.Background(), options, ServeDeps{Log: obs.Nop()})
		if err == nil || !strings.Contains(err.Error(), "personal migrate --to sqlite") {
			t.Fatalf("应拒绝并提示迁移：%v", err)
		}
	})
	t.Run("没有存储也没给账号", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "d")
		options, _ := ParseServe([]string{"--data-dir", dir, "--host", "127.0.0.1", "--tls", "plain", "--listen", "127.0.0.1:0"}, Env{}, dir)
		_, err := Serve(context.Background(), options, ServeDeps{Log: obs.Nop()})
		var state *StateError
		if err == nil || !asState(err, &state) || state.Code != NotInitialized {
			t.Fatalf("应是 not_initialized：%v", err)
		}
	})
	t.Run("Redis 连不上拒绝启动且不带地址", func(t *testing.T) {
		dir := initAt(t, "http://127.0.0.1:1")
		options, err := ParseServe([]string{
			"--data-dir", dir, "--host", "127.0.0.1", "--tls", "plain", "--listen", "127.0.0.1:0",
			"--public-origin", "http://127.0.0.1:1", "--control", "redis", "--redis-url", "redis://secretuser:secretpass@127.0.0.1:1",
		}, Env{}, dir)
		if err != nil {
			t.Fatal(err)
		}
		_, err = Serve(context.Background(), options, ServeDeps{Log: obs.Nop()})
		if err == nil || strings.Contains(err.Error(), "secretpass") || !strings.Contains(err.Error(), "Redis") {
			t.Fatalf("应拒绝且不带口令：%v", err)
		}
	})
}

func asState(err error, target **StateError) bool {
	s, ok := err.(*StateError)
	if ok {
		*target = s
	}
	return ok
}

func TestServeAutoInit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	pw := passwordFile(t, testPassword)
	running, logs := startServe(t, dir, []string{
		"--tls", "plain", "--account", "me", "--password-file", pw, "--public-origin", "http://127.0.0.1:1",
	}, ServeDeps{})
	login(t, http.DefaultClient, running.URL)
	if !strings.Contains(logs.String(), "已初始化个人中转") {
		t.Fatalf("应记自动初始化：%s", logs.String())
	}
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("数据目录应是 0700：%v %v", info, err)
	}
}

func TestServeIssuerChangeWarns(t *testing.T) {
	dir := initAt(t, "http://127.0.0.1:1")
	running, logs := startServe(t, dir, []string{"--tls", "plain", "--public-origin", "http://127.0.0.1:2"}, ServeDeps{})
	if running.Issuer != "http://127.0.0.1:2" || running.Store.Tenant().Issuer != "http://127.0.0.1:2" {
		t.Fatalf("issuer 应改成新的：%s", running.Issuer)
	}
	if !strings.Contains(logs.String(), "对外地址变了") {
		t.Fatalf("应警告：%s", logs.String())
	}
}

// --node-id 同时作用于隧道层与控制面（Node 版只作用于控制面，隧道层用 host）。
func TestServeNodeIDAppliesToTunnelLayer(t *testing.T) {
	running, _, _ := plainServe(t, "--node-id", "node-7")
	_, _, data := request(t, http.DefaultClient, "GET", running.URL+"/health", nil, "")
	if !strings.Contains(string(data), `"node":"node-7"`) {
		t.Fatalf("/health 应报 node-7：%s", data)
	}
}

func TestServeMetricsAndWebRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("<html>app</html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	running, _, _ := plainServe(t, "--web-root", root, "--metrics-listen", "127.0.0.1:0")
	status, _, data := request(t, http.DefaultClient, "GET", running.URL+"/app/anything", nil, "")
	if status != 200 || !strings.Contains(string(data), "app") {
		t.Fatalf("SPA 回退 %d：%s", status, data)
	}
	_, _, info := request(t, http.DefaultClient, "GET", running.URL+"/.well-known/armadra-platform", nil, "")
	if !strings.Contains(string(info), `"webApp":"http://127.0.0.1:1/app/"`) {
		t.Fatalf("platform.info 应报 webApp：%s", info)
	}
	if running.MetricsURL == "" {
		t.Fatal("应有指标地址")
	}
	status, _, data = request(t, http.DefaultClient, "GET", running.MetricsURL+"/metrics", nil, "")
	if status != 200 || !strings.Contains(string(data), "relay_tunnels") {
		t.Fatalf("/metrics %d：%s", status, data)
	}
}

func wsURL(base string) string { return "ws" + strings.TrimPrefix(base, "http") + MeStreamPath }

func dialStream(t *testing.T, base, ticket string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	dialer := websocket.Dialer{Subprotocols: []string{gen.SubprotocolTicket + ticket}, HandshakeTimeout: 3 * time.Second}
	return dialer.Dial(wsURL(base), nil)
}

func streamTicket(t *testing.T, base, token string) string {
	t.Helper()
	status, _, data := request(t, http.DefaultClient, "POST", base+"/v1/me/stream/ticket", map[string]any{}, token)
	if status != 200 {
		t.Fatalf("票据 %d：%s", status, data)
	}
	var out struct {
		Ticket string `json:"ticket"`
	}
	_ = json.Unmarshal(data, &out)
	return out.Ticket
}

func TestMeStream(t *testing.T) {
	dir := initAt(t, "http://127.0.0.1:1")
	running, _ := startServe(t, dir, []string{"--tls", "plain", "--public-origin", "http://127.0.0.1:1"},
		ServeDeps{StreamPing: 100 * time.Millisecond})
	session := login(t, http.DefaultClient, running.URL)
	token := session.Session.AccessToken

	t.Run("没有票或票错答 401", func(t *testing.T) {
		_, resp, err := dialStream(t, running.URL, "nope")
		if err == nil || resp == nil || resp.StatusCode != 401 {
			t.Fatalf("应 401：%v %v", err, resp)
		}
	})
	t.Run("票一次性、选中同一个子协议、ping/pong", func(t *testing.T) {
		ticket := streamTicket(t, running.URL, token)
		conn, resp, err := dialStream(t, running.URL, ticket)
		if err != nil {
			t.Fatalf("握手：%v", err)
		}
		defer func() { _ = conn.Close() }()
		if resp.Header.Get("Sec-WebSocket-Protocol") != gen.SubprotocolTicket+ticket {
			t.Fatalf("应选中票据子协议：%v", resp.Header)
		}
		if _, resp, err := dialStream(t, running.URL, ticket); err == nil || resp.StatusCode != 401 {
			t.Fatalf("票只能用一次：%v", err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var event map[string]any
		if err := conn.ReadJSON(&event); err != nil || event["type"] != "ping" {
			t.Fatalf("应先收到服务端 ping：%v %v", event, err)
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"ping"}`)); err != nil {
			t.Fatal(err)
		}
		for {
			if err := conn.ReadJSON(&event); err != nil {
				t.Fatalf("等 pong：%v", err)
			}
			if event["type"] == "pong" {
				break
			}
		}
	})
	t.Run("会话撤销后以 4401 关闭", func(t *testing.T) {
		second := login(t, http.DefaultClient, running.URL)
		conn, _, err := dialStream(t, running.URL, streamTicket(t, running.URL, second.Session.AccessToken))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		status, _, data := request(t, http.DefaultClient, "DELETE", running.URL+"/v1/me/devices/"+second.Session.Device.DeviceID, nil, token)
		if status != 200 {
			t.Fatalf("撤销设备 %d：%s", status, data)
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			_, _, err := conn.ReadMessage()
			if err == nil {
				continue
			}
			closeErr, ok := err.(*websocket.CloseError)
			if !ok || closeErr.Code != gen.CloseUnauthenticated || closeErr.Text != "session_revoked" {
				t.Fatalf("应以 4401 session_revoked 关闭：%v", err)
			}
			break
		}
	})
	t.Run("发送不阻塞：不读的客户端被断开而发事件的一方不卡", func(t *testing.T) {
		conn, _, err := dialStream(t, running.URL, streamTicket(t, running.URL, token))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		started := time.Now()
		for i := 0; i < 5000; i++ {
			running.Core.Emit(core.NewSourceOffline("src", int64(i)), nil)
		}
		if elapsed := time.Since(started); elapsed > 3*time.Second {
			t.Fatalf("扇出被慢客户端拖住了：%v", elapsed)
		}
	})
}

// fakeConn 是登记进边缘的连接替身：只记有没有被断开。
type fakeConn struct{ disconnected atomic.Bool }

func (f *fakeConn) Disconnect() { f.disconnected.Store(true) }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等不到：%s", what)
}

// 别的进程撤销源与链接：一秒内断开该源的客户端、该链接的访客（这里缩短到 50 ms）。
func TestRevocationWatch(t *testing.T) {
	running, logs, dir := plainServe(t)
	ctx := context.Background()
	other, err := OpenStore(ctx, dir, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	nowMs := time.Now().UnixMilli()
	jwk := store.SigningKeyRecord{}
	_ = jwk
	if _, err := other.Ports().Sources.Put(ctx, store.DefaultTenant, "s1", func(*store.SourceRecord) (store.SourceRecord, error) {
		return store.SourceRecord{SourceID: "s1", Name: "n", Kind: "core", CoreVersion: "1", Capabilities: []string{}, RegisteredAtMs: nowMs, TrustedOrigins: []string{}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := other.Ports().Links.Insert(ctx, store.DefaultTenant, store.LinkRecord{
		LinkID: "0123456789abcdef", SourceID: "s1", InvitationID: "i", Label: "l", Role: "viewer", SecretHash: "h",
		CreatedAtMs: nowMs, ExpiresAtMs: nowMs + 3_600_000,
	}); err != nil {
		t.Fatal(err)
	}
	// 重新登记之后 serve 里的「已撤销」基线是空的；等一次轮询让它读到这些写入。
	time.Sleep(200 * time.Millisecond)

	bySource, byLink, bystander := &fakeConn{}, &fakeConn{}, &fakeConn{}
	policy := running.Plane.Edge.Policy()
	policy.Track(bySource, "s1", "acct:me")
	policy.Track(byLink, "s2", core.GuestPrefix("0123456789abcdef")+"abcd1234")
	policy.Track(bystander, "s2", "acct:me")

	if _, ok, err := other.Ports().Links.Revoke(ctx, store.DefaultTenant, "0123456789abcdef", nowMs); err != nil || !ok {
		t.Fatalf("撤销链接：%v %v", ok, err)
	}
	waitFor(t, "链接撤销后访客连接被断开", byLink.disconnected.Load)
	if bystander.disconnected.Load() || bySource.disconnected.Load() {
		t.Fatal("链接撤销不该断开别的连接")
	}
	if err := other.Ports().Sources.Update(ctx, store.DefaultTenant, "s1", func(s *store.SourceRecord) error {
		revoked := nowMs
		s.RevokedAtMs = &revoked
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "源撤销后该源的连接被断开", bySource.disconnected.Load)
	if bystander.disconnected.Load() {
		t.Fatal("源撤销不该断开别的源的连接")
	}
	if !strings.Contains(logs.String(), "存储有外部改动") {
		t.Fatalf("应记外部改动：%s", logs.String())
	}
}

// 源上下线记进内存、Flush 时落库：最近在线与版本。
func TestPresenceFlush(t *testing.T) {
	running, _, _ := plainServe(t)
	ctx := context.Background()
	nowMs := time.Now().UnixMilli()
	if _, err := running.Store.Ports().Sources.Put(ctx, store.DefaultTenant, "s1", func(*store.SourceRecord) (store.SourceRecord, error) {
		return store.SourceRecord{SourceID: "s1", Name: "n", Kind: "core", CoreVersion: "1.0.0", Capabilities: []string{}, RegisteredAtMs: nowMs, TrustedOrigins: []string{}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	pres := running.Cluster.Presence()
	if err := pres.Publish(ctx, control.PresenceEvent{Type: control.PresenceSourceOnline, SourceID: "s1", CoreVersion: "2.0.0", AtMs: nowMs + 5}); err != nil {
		t.Fatal(err)
	}
	running.Flush(ctx)
	source, err := running.Store.Ports().Sources.Get(ctx, store.DefaultTenant, "s1")
	if err != nil || source == nil || source.LastSeenAtMs == nil || *source.LastSeenAtMs != nowMs+5 || source.CoreVersion != "2.0.0" {
		t.Fatalf("最近在线与版本没落库：%+v %v", source, err)
	}
	// 下线只推进时间，不改版本。
	if err := pres.Publish(ctx, control.PresenceEvent{Type: control.PresenceSourceOffline, SourceID: "s1", AtMs: nowMs + 9}); err != nil {
		t.Fatal(err)
	}
	running.Flush(ctx)
	source, _ = running.Store.Ports().Sources.Get(ctx, store.DefaultTenant, "s1")
	if *source.LastSeenAtMs != nowMs+9 || source.CoreVersion != "2.0.0" {
		t.Fatalf("下线只推进时间：%+v", source)
	}
}

// 流量计量器落库到存储的同一张表（同键相加）；保留天数 0 时不计量。
func TestTrafficMeterFlushesToStore(t *testing.T) {
	running, _, _ := plainServe(t)
	ctx := context.Background()
	if running.Meter == nil {
		t.Fatal("缺省应有流量计量器")
	}
	at := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		running.Meter.Add(traffic.Sample{
			Key: traffic.Key{SourceID: "s1", Subject: "acct:me", DeviceID: "d", Kind: "e2e"}, At: at,
			Connections: 2, BytesToSource: 10, BytesToClient: 20,
		})
	}
	if err := running.Meter.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := running.Store.Ports().Traffic.Query(ctx, store.DefaultTenant, store.TrafficQuery{ToMs: at.Add(2 * time.Hour).UnixMilli(), Limit: 10})
	if err != nil || len(got) != 1 || got[0].Connections != 4 || got[0].BytesToSource != 20 || got[0].BytesToClient != 40 || got[0].Node != "127.0.0.1" {
		t.Fatalf("同键相加：%+v %v", got, err)
	}
	off, _, _ := plainServe(t, "--traffic-retention-days", "0")
	if off.Meter != nil {
		t.Fatal("保留天数 0 不记录")
	}
}

func TestEdgeLimitRules(t *testing.T) {
	cli := &RateLimits{Anonymous: 5}
	stored := &RateLimits{Anonymous: 9, Token: 7, Account: 8, Preflight: 6}
	rules := EdgeLimitRules(cli, stored)
	if rules[control.LimitEdgeConnectIP].Max != 5 || rules[control.LimitEdgeConnectToken].Max != 7 ||
		rules[control.LimitEdgeConnectAcct].Max != 8 || rules[control.LimitEdgePreflightIP].Max != 6 {
		t.Fatalf("命令行 > 状态文件：%+v", rules)
	}
	if len(EdgeLimitRules(nil, nil)) != 0 {
		t.Fatal("都没给就不覆盖缺省")
	}
}

// ACME：对 pebble 申请证书后用它起 HTTPS（环境变量没设就跳过，见 tlsx 的同名测试）。
func TestServeACMEWithPebble(t *testing.T) {
	directory := os.Getenv("ARMADRA_CLOUD_TEST_ACME_DIRECTORY")
	ca := os.Getenv("ARMADRA_CLOUD_TEST_ACME_CA")
	if directory == "" || ca == "" {
		t.Skip("没设 ARMADRA_CLOUD_TEST_ACME_DIRECTORY / ARMADRA_CLOUD_TEST_ACME_CA，跳过 pebble 用例")
	}
	dir := initAt(t, "https://relay.test:1")
	args := []string{
		"--tls", "acme", "--acme-email", "ops@relay.test", "--acme-directory", directory, "--acme-ca-bundle", ca,
		"--acme-http-listen", "127.0.0.1:0", "--public-origin", "https://relay.test:1",
	}
	options, err := ParseServe(append([]string{"--data-dir", dir, "--host", "relay.test", "--listen", "127.0.0.1:0"}, args...), Env{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	running, err := Serve(context.Background(), options, ServeDeps{Log: obs.NewLogger(logs, obs.LevelDebug)})
	if err != nil {
		t.Fatalf("Serve：%v", err)
	}
	defer running.Close()
	if running.Fingerprint != "" {
		t.Fatalf("ACME 没有要钉的指纹：%q", running.Fingerprint)
	}
	conn, err := tls.Dial("tcp", strings.TrimPrefix(running.URL, "https://"), &tls.Config{InsecureSkipVerify: true, ServerName: "relay.test"}) //nolint:gosec // pebble 的根不在系统里
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	leaf := conn.ConnectionState().PeerCertificates[0]
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "relay.test" {
		t.Fatalf("证书应覆盖 relay.test：%v", leaf.DNSNames)
	}
}

// 多节点控制面：--control postgres 与 --control redis 起得来，/ready 多报 database / redis；
// 两个节点共用同一个库时，一个节点上的源上线事件另一个节点收得到。
func TestServeClusterControl(t *testing.T) {
	redisURL := os.Getenv("REDIS_URL")
	t.Run("postgres", func(t *testing.T) {
		databaseURL, schema := pgtest.Schema(t)
		{
			db := &DatabaseOptions{URL: databaseURL, Schema: schema}
			dir := filepath.Join(t.TempDir(), "data")
			st, err := CreateState(context.Background(), CreateStateInput{
				DataDir: dir, Account: "me", Password: testPassword, Issuer: "http://127.0.0.1:1", Store: StorePostgres, Database: db,
			})
			if err != nil {
				t.Fatal(err)
			}
			_ = st.Close()
			args := []string{
				"--tls", "plain", "--public-origin", "http://127.0.0.1:1", "--store", "postgres", "--control", "postgres",
				"--database-url", databaseURL, "--database-schema", schema,
			}
			a, _ := startServe(t, dir, append(args, "--node-id", "node-a"), ServeDeps{})
			b, _ := startServe(t, dir, append(args, "--node-id", "node-b"), ServeDeps{})
			_, _, data := request(t, http.DefaultClient, "GET", a.URL+"/ready", nil, "")
			if !strings.Contains(string(data), "database") {
				t.Fatalf("/ready 应报 database：%s", data)
			}
			got := make(chan control.PresenceEvent, 1)
			off := b.Cluster.Presence().Subscribe(func(ev control.PresenceEvent) {
				select {
				case got <- ev:
				default:
				}
			})
			defer off()
			// 订阅连接就绪需要一点时间：重发直到另一个节点收到。
			deadline := time.Now().Add(10 * time.Second)
			for {
				_ = a.Cluster.Presence().Publish(context.Background(), control.PresenceEvent{
					Type: control.PresenceSourceOnline, SourceID: "s1", Node: "node-a", CoreVersion: "1", AtMs: 1,
				})
				select {
				case ev := <-got:
					if ev.SourceID != "s1" || ev.Node != "node-a" {
						t.Fatalf("事件不对：%+v", ev)
					}
					return
				case <-time.After(200 * time.Millisecond):
				}
				if time.Now().After(deadline) {
					t.Fatal("另一个节点没收到在线事件")
				}
			}
		}
	})
	t.Run("redis", func(t *testing.T) {
		if redisURL == "" {
			if os.Getenv("CI") != "" {
				t.Fatal("CI 里 REDIS_URL 必须设")
			}
			t.Skip("没设 REDIS_URL，跳过 Redis 控制面用例")
		}
		{
			dir := initAt(t, "http://127.0.0.1:1")
			a, _ := startServe(t, dir, []string{
				"--tls", "plain", "--public-origin", "http://127.0.0.1:1", "--control", "redis", "--redis-url", redisURL,
				"--node-id", "node-a",
			}, ServeDeps{})
			_, _, data := request(t, http.DefaultClient, "GET", a.URL+"/ready", nil, "")
			if !strings.Contains(string(data), "redis") {
				t.Fatalf("/ready 应报 redis：%s", data)
			}
		}
	})
}

// 关闭时计量器最后落一次库，带上关闭期间累计的字节，且在存储关闭之前。
func TestCloseFlushesTrafficLast(t *testing.T) {
	running, _, dir := plainServe(t)
	at := time.Now()
	running.Meter.Add(traffic.Sample{
		Key: traffic.Key{SourceID: "s1", Subject: "acct:me", Kind: "e2e"}, At: at, Connections: 1, BytesToSource: 7,
	})
	running.Close()
	st, err := OpenStore(context.Background(), dir, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	rows, err := st.Ports().Traffic.Query(context.Background(), store.DefaultTenant, store.TrafficQuery{ToMs: at.Add(2 * time.Hour).UnixMilli(), Limit: 10})
	if err != nil || len(rows) != 1 || rows[0].BytesToSource != 7 {
		t.Fatalf("关闭时应落库：%+v %v", rows, err)
	}
}
