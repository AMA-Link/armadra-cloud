package saas

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	goredis "github.com/redis/go-redis/v9"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil/relayenv"
)

const domain = "example.test"

// rig 是一套 SaaS 节点测试环境：测试签发者（JWKS 走 HTTP）、一个隔离前缀的 Redis。
type rig struct {
	t      *testing.T
	env    *relayenv.Env
	redis  string
	prefix string
	admin  *goredis.Client
	jwks   *httptest.Server
	// jwksHits 是签发者 JWKS 被拉了几次（共享缓存的验收）。
	jwksHits atomic.Int32
}

func redisURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("REDIS_URL")
	if url == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("CI 里没有 REDIS_URL")
		}
		t.Skip("没有 Redis：设置 REDIS_URL")
	}
	return url
}

func newRig(t *testing.T) *rig {
	t.Helper()
	url := redisURL(t)
	opts, err := goredis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, env: relayenv.New(t, relayenv.Options{}), redis: url, prefix: "relay-test:" + randomHex(6) + ":", admin: goredis.NewClient(opts)}
	t.Cleanup(func() {
		ctx := context.Background()
		iter := r.admin.Scan(ctx, 0, r.prefix+"*", 200).Iterator()
		for iter.Next(ctx) {
			r.admin.Del(ctx, iter.Val())
		}
		_ = r.admin.Close()
	})
	body, _ := json.Marshal(map[string]any{"keys": []map[string]any{{
		"kty": "OKP", "crv": "Ed25519", "x": base64.RawURLEncoding.EncodeToString(r.env.Issuer.Public), "kid": r.env.Issuer.Kid,
	}}})
	r.jwks = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.jwksHits.Add(1)
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(r.jwks.Close)
	return r
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

// running 是一个跑着的节点。
type running struct {
	*Node
	name string
	logs *syncBuffer
	stop func()
}

func (r *rig) config(name string, over func(*Config)) Config {
	r.t.Helper()
	addr := freeAddr(r.t)
	cfg, issues := LoadConfig(map[string]string{
		"RELAY_LISTEN": addr, "RELAY_NODE_ID": name, "RELAY_DOMAIN": domain,
		"RELAY_CLOUD_ISSUER": r.env.Issuer.Issuer, "RELAY_REDIS_URL": r.redis,
		"RELAY_TUNNEL_URL": "ws://" + addr + "/t/v1", "RELAY_ALLOW_INSECURE_TUNNEL": "1",
		"RELAY_JWKS_URL": r.jwks.URL + gen.PathJwksWellKnown, "RELAY_DRAIN_GRACE_MS": "1000",
		"RELAY_NODE_TOKEN": r.nodeToken(name, nil),
	}, func() (string, error) { return "h", nil })
	if len(issues) > 0 {
		r.t.Fatal(issues)
	}
	cfg.RedisPrefix = r.prefix
	if over != nil {
		over(&cfg)
	}
	return cfg
}

// start 起一个节点；测试结束时停掉（已停的不再停）。
func (r *rig) start(name string, over func(*Config)) *running {
	r.t.Helper()
	cfg := r.config(name, over)
	ctx, cancel := context.WithCancel(context.Background())
	logs := &syncBuffer{}
	nodes := make(chan *Node, 1)
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, cfg, "9.9.9", logs, func(n *Node) { nodes <- n }) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					r.t.Error(err)
				}
			case <-time.After(20 * time.Second):
				r.t.Error("节点停不下来")
			}
		})
	}
	select {
	case n := <-nodes:
		run := &running{Node: n, name: name, logs: logs, stop: stop}
		r.t.Cleanup(stop)
		return run
	case err := <-done:
		cancel()
		r.t.Fatalf("节点起不来：%v；日志：%s", err, logs.String())
	case <-time.After(15 * time.Second):
		cancel()
		r.t.Fatal("节点起不来：超时")
	}
	return nil
}

// freeAddr 找一个空闲的回环端口（节点表里要写真实的隧道与 /x/v1 地址）。
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().String()
}

// nodeToken 签一枚节点令牌（sub = 节点名）。
func (r *rig) nodeToken(name string, over relayenv.Claims) string {
	iat := time.Now().Unix()
	claims := relayenv.Claims{
		"iss": r.env.Issuer.Issuer, "sub": name, "aud": gen.TokenAudienceRelayNode,
		"iat": iat, "exp": iat + 3600, "jti": "n." + name + "." + randomHex(6), "ver": 1,
	}
	for k, v := range over {
		claims[k] = v
	}
	return r.env.Sign(r.t, gen.TokenTypeNode, claims)
}

// source 造另一个源（同一个测试签发者签它的令牌）。
func (r *rig) source() *relayenv.Env {
	r.t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		r.t.Fatal(err)
	}
	id := randomHex(16)
	return &relayenv.Env{
		Issuer: r.env.Issuer, Source: testutil.Key{Kid: id, Public: pub, Private: priv},
		SourceID: id, Control: r.env.Control, Auth: r.env.Auth,
	}
}

// relayToken 签一枚中继令牌（源是测试源）。
func (r *rig) relayToken(over relayenv.Claims) string {
	return r.env.RelayToken(r.t, over)
}

// client 是一条客户端通道。
type client struct {
	c      *websocket.Conn
	msgs   chan []byte
	closed chan int
}

// channel 经节点 n 开一条到测试源的端到端通道（Host 寻址）。
func (r *rig) channel(n *running, token string) *client {
	r.t.Helper()
	return r.channelTo(n, r.env.SourceID, token)
}

// channelTo 同 channel，源自选。
func (r *rig) channelTo(n *running, sourceID, token string) *client {
	r.t.Helper()
	header := http.Header{"Host": {sourceID + ".src." + domain}}
	d := websocket.Dialer{Subprotocols: []string{gen.E2ESubprotocol, gen.SubprotocolRelay + token}, HandshakeTimeout: 5 * time.Second}
	c, _, err := d.Dial(strings.Replace(n.URL, "http://", "ws://", 1)+gen.E2EPath, header)
	if err != nil {
		r.t.Fatalf("拨号失败：%v", err)
	}
	r.t.Cleanup(func() { _ = c.Close() })
	cl := &client{c: c, msgs: make(chan []byte, 1024), closed: make(chan int, 1)}
	go func() {
		for {
			mt, b, err := c.ReadMessage()
			if err != nil {
				code := 1006
				var ce *websocket.CloseError
				if errors.As(err, &ce) {
					code = ce.Code
				}
				cl.closed <- code
				return
			}
			if mt == websocket.BinaryMessage {
				cl.msgs <- b
			}
		}
	}()
	return cl
}

// outcome：开成答 0，被拒答关闭码（被拒的通道也先完成升级再关）。
func (c *client) outcome() int {
	select {
	case code := <-c.closed:
		c.closed <- code
		return code
	case <-time.After(300 * time.Millisecond):
		return 0
	}
}

func (c *client) wait(t *testing.T) int {
	t.Helper()
	select {
	case code := <-c.closed:
		c.closed <- code
		return code
	case <-time.After(10 * time.Second):
		t.Fatal("等通道关闭超时")
		return 0
	}
}

// echo 发一段字节，等回显收全。
func (c *client) echo(t *testing.T, payload []byte) []byte {
	t.Helper()
	go func() {
		for off := 0; off < len(payload); off += 60000 {
			if err := c.c.WriteMessage(websocket.BinaryMessage, payload[off:min(off+60000, len(payload))]); err != nil {
				return
			}
		}
	}()
	var got []byte
	deadline := time.After(30 * time.Second)
	for len(got) < len(payload) {
		select {
		case b := <-c.msgs:
			got = append(got, b...)
		case code := <-c.closed:
			t.Fatalf("回显途中通道关闭：%d", code)
		case <-deadline:
			t.Fatal("回显超时")
		}
	}
	return got
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("等不到：%s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func getJSON(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
}
