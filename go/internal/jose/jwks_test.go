package jose_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/jose"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil"
)

// clock 是手动拨的时钟。
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func jwks(t *testing.T, keys ...testutil.Key) []byte {
	t.Helper()
	type jwk struct {
		Kty string `json:"kty"`
		Crv string `json:"crv"`
		X   string `json:"x"`
		Kid string `json:"kid,omitempty"`
		Alg string `json:"alg"`
		Use string `json:"use"`
	}
	set := struct {
		Keys []jwk `json:"keys"`
	}{Keys: []jwk{}}
	for _, k := range keys {
		set.Keys = append(set.Keys, jwk{"OKP", "Ed25519", base64.RawURLEncoding.EncodeToString(k.Public), k.Kid, "EdDSA", "sig"})
	}
	raw, err := json.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// source 是可以改内容、可以让它失败的假 JWKS 端点，并数着被取了几次。
type source struct {
	mu    sync.Mutex
	body  []byte
	err   error
	calls atomic.Int32
	gate  chan struct{}
}

func (s *source) fetch(ctx context.Context) ([]byte, error) {
	s.calls.Add(1)
	if s.gate != nil {
		select {
		case <-s.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.body, s.err
}

func (s *source) set(body []byte, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.body, s.err = body, err
}

func newCache(src *source, clk *clock) *jose.JWKSCache {
	return jose.NewJWKSCache(jose.JWKSOptions{Fetch: src.fetch, Now: clk.Now})
}

func lookup(t *testing.T, c *jose.JWKSCache, kid string) (found bool, err error) {
	t.Helper()
	_, found, err = c.Key(context.Background(), kid)
	return found, err
}

func TestJWKSCacheTTLAndUnknownKid(t *testing.T) {
	issuer, source2 := testutil.Keys(t)
	src := &source{body: jwks(t, issuer)}
	clk := &clock{t: time.UnixMilli(testutil.FixedNow)}
	c := newCache(src, clk)

	// 第一次查钥才去取；之后在 TTL 内命中缓存。
	if found, err := lookup(t, c, issuer.Kid); !found || err != nil {
		t.Fatalf("第一次：%v %v", found, err)
	}
	if found, _ := lookup(t, c, issuer.Kid); !found || src.calls.Load() != 1 {
		t.Fatalf("命中缓存不该再取：%d 次", src.calls.Load())
	}

	// 未知 kid：距上次刷新不足 10 秒，不刷新。
	clk.Advance(9 * time.Second)
	if found, err := lookup(t, c, source2.Kid); found || err != nil || src.calls.Load() != 1 {
		t.Fatalf("10 秒内不该刷新：found=%v err=%v 取了 %d 次", found, err, src.calls.Load())
	}
	// 满 10 秒后，未知 kid 触发一次刷新；刷新后端点里有了这把钥。
	clk.Advance(time.Second)
	src.set(jwks(t, issuer, source2), nil)
	if found, err := lookup(t, c, source2.Kid); !found || err != nil || src.calls.Load() != 2 {
		t.Fatalf("应刷新并找到：found=%v err=%v 取了 %d 次", found, err, src.calls.Load())
	}
	// 另一个未知 kid 紧接着来：又在 10 秒内，不再刷新。
	for i := 0; i < 5; i++ {
		if found, _ := lookup(t, c, "nobody"); found {
			t.Fatal("不该找到")
		}
	}
	if src.calls.Load() != 2 {
		t.Fatalf("未知 kid 狂刷也只能 10 秒一次：取了 %d 次", src.calls.Load())
	}
	clk.Advance(10 * time.Second)
	lookup(t, c, "nobody")
	if src.calls.Load() != 3 {
		t.Fatalf("再过 10 秒可以刷新一次：取了 %d 次", src.calls.Load())
	}

	// TTL 10 分钟：到期后查已知钥也会刷新（钥轮换后旧钥消失）。
	src.set(jwks(t, source2), nil)
	clk.Advance(10 * time.Minute)
	if found, _ := lookup(t, c, issuer.Kid); found {
		t.Fatal("轮换后旧钥应不在了")
	}
	if src.calls.Load() != 4 {
		t.Fatalf("TTL 到期应刷新：取了 %d 次", src.calls.Load())
	}
	if found, _ := lookup(t, c, source2.Kid); !found {
		t.Fatal("新钥应在")
	}
}

func TestJWKSCacheKeepsOldKeysWhenRefreshFails(t *testing.T) {
	issuer, _ := testutil.Keys(t)
	src := &source{body: jwks(t, issuer)}
	clk := &clock{t: time.UnixMilli(testutil.FixedNow)}
	c := newCache(src, clk)
	if found, _ := lookup(t, c, issuer.Kid); !found {
		t.Fatal("起步应成功")
	}

	src.set(nil, errors.New("端点挂了"))
	clk.Advance(11 * time.Minute) // TTL 过期，查已知钥要刷新
	pub, found, err := c.Key(context.Background(), issuer.Kid)
	if !found || err != nil || string(pub) != string(issuer.Public) {
		t.Fatalf("刷新失败应继续用旧钥：found=%v err=%v", found, err)
	}
	if src.calls.Load() != 2 {
		t.Fatalf("取了 %d 次", src.calls.Load())
	}
	// 失败也算一次尝试：10 秒内不再取，仍用旧钥。
	for i := 0; i < 3; i++ {
		if found, _ := lookup(t, c, issuer.Kid); !found {
			t.Fatal("仍应用旧钥")
		}
	}
	if src.calls.Load() != 2 {
		t.Fatalf("失败后 10 秒内不该重试：取了 %d 次", src.calls.Load())
	}
	// 未知 kid + 刷新失败：找不到，也不把旧钥丢掉。
	clk.Advance(10 * time.Second)
	if found, _ := lookup(t, c, "nobody"); found {
		t.Fatal("不该找到")
	}
	if found, _ := lookup(t, c, issuer.Kid); !found {
		t.Fatal("旧钥应还在")
	}
	// 端点恢复后下一次刷新成功。
	src.set(jwks(t, issuer), nil)
	clk.Advance(10 * time.Second)
	if found, err := lookup(t, c, "nobody"); found || err != nil {
		t.Fatalf("%v %v", found, err)
	}
	if src.calls.Load() != 4 {
		t.Fatalf("取了 %d 次", src.calls.Load())
	}
}

func TestJWKSCacheInitialFailureReturnsError(t *testing.T) {
	issuer, _ := testutil.Keys(t)
	boom := errors.New("连不上")
	src := &source{err: boom}
	clk := &clock{t: time.UnixMilli(testutil.FixedNow)}
	c := newCache(src, clk)
	if _, err := lookup(t, c, issuer.Kid); !errors.Is(err, boom) {
		t.Fatalf("一次都没成功过，应带出取钥的错误：%v", err)
	}
	// 10 秒内不重试，仍报同一个错误。
	if _, err := lookup(t, c, issuer.Kid); !errors.Is(err, boom) || src.calls.Load() != 1 {
		t.Fatalf("%v，取了 %d 次", err, src.calls.Load())
	}
	clk.Advance(10 * time.Second)
	src.set(jwks(t, issuer), nil)
	if found, err := lookup(t, c, issuer.Kid); !found || err != nil {
		t.Fatalf("恢复后应成功：%v %v", found, err)
	}
}

// 同一时刻只有一个刷新在飞：很多 goroutine 同时查，端点只被取一次。
func TestJWKSCacheSingleFlight(t *testing.T) {
	issuer, _ := testutil.Keys(t)
	src := &source{body: jwks(t, issuer), gate: make(chan struct{})}
	clk := &clock{t: time.UnixMilli(testutil.FixedNow)}
	c := newCache(src, clk)

	const n = 64
	var wg sync.WaitGroup
	results := make(chan bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, found, err := c.Key(context.Background(), issuer.Kid)
			results <- found && err == nil
		}()
	}
	// 等取钥开始，再放行。
	deadline := time.Now().Add(5 * time.Second)
	for src.calls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("取钥没有开始")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(src.gate)
	wg.Wait()
	close(results)
	for ok := range results {
		if !ok {
			t.Fatal("所有等待者都应拿到钥")
		}
	}
	if src.calls.Load() != 1 {
		t.Fatalf("应只取一次，实际 %d 次", src.calls.Load())
	}
}

// 调用方取消不影响在飞的刷新：别的等待者照样拿到结果。
func TestJWKSCacheCancelledWaiter(t *testing.T) {
	issuer, _ := testutil.Keys(t)
	src := &source{body: jwks(t, issuer), gate: make(chan struct{})}
	clk := &clock{t: time.UnixMilli(testutil.FixedNow)}
	c := newCache(src, clk)

	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error, 1)
	go func() {
		_, _, err := c.Key(ctx, issuer.Kid)
		errs <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for src.calls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("取钥没有开始")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-errs; !errors.Is(err, context.Canceled) {
		t.Fatalf("取消的调用方应拿到 context 错误：%v", err)
	}
	close(src.gate)
	if found, err := lookup(t, c, issuer.Kid); !found || err != nil {
		t.Fatalf("在飞的刷新应照常完成：%v %v", found, err)
	}
	if src.calls.Load() != 1 {
		t.Fatalf("取了 %d 次", src.calls.Load())
	}
}

func TestParseJWKS(t *testing.T) {
	issuer, source2 := testutil.Keys(t)
	keys, err := jose.ParseJWKS(jwks(t, issuer, source2))
	if err != nil || len(keys) != 2 || string(keys[issuer.Kid]) != string(issuer.Public) {
		t.Fatalf("%v %v", keys, err)
	}
	// 没有 kid 的钥跳过，不报错。
	noKid := []byte(`{"keys":[{"kty":"OKP","crv":"Ed25519","x":"0EqyMnQrtKs6E2i9RhXk5tAiSrcaAWuvhSCjMsl3hzc"}]}`)
	if keys, err := jose.ParseJWKS(noKid); err != nil || len(keys) != 0 {
		t.Fatalf("%v %v", keys, err)
	}
	for name, bad := range map[string]string{
		"不是 JSON":        "keys",
		"缺 keys":         `{}`,
		"kty 不对":         `{"keys":[{"kty":"RSA","crv":"Ed25519","x":"0EqyMnQrtKs6E2i9RhXk5tAiSrcaAWuvhSCjMsl3hzc","kid":"a"}]}`,
		"x 长度不对":         `{"keys":[{"kty":"OKP","crv":"Ed25519","x":"AAAA","kid":"a"}]}`,
		"x 末尾多余位（43 字符）": `{"keys":[{"kty":"OKP","crv":"Ed25519","x":"0EqyMnQrtKs6E2i9RhXk5tAiSrcaAWuvhSCjMsl3hzd","kid":"a"}]}`,
	} {
		if _, err := jose.ParseJWKS([]byte(bad)); err == nil {
			t.Errorf("%s 应被拒", name)
		}
	}
}

func TestHTTPFetch(t *testing.T) {
	issuer, _ := testutil.Keys(t)
	var status atomic.Int32
	status.Store(http.StatusOK)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/jwks.json" {
			http.NotFound(w, r)
			return
		}
		if s := int(status.Load()); s != http.StatusOK {
			w.WriteHeader(s)
			return
		}
		_, _ = w.Write(jwks(t, issuer))
	}))
	defer server.Close()

	clk := &clock{t: time.UnixMilli(testutil.FixedNow)}
	c := jose.NewJWKSCache(jose.JWKSOptions{
		Fetch: jose.HTTPFetch(server.Client(), server.URL+"/.well-known/jwks.json"), Now: clk.Now,
	})
	if found, err := lookup(t, c, issuer.Kid); !found || err != nil {
		t.Fatalf("%v %v", found, err)
	}

	bad := jose.HTTPFetch(server.Client(), server.URL+"/nope")
	if _, err := bad(context.Background()); err == nil {
		t.Error("404 应报错")
	}
	status.Store(http.StatusInternalServerError)
	if _, err := jose.HTTPFetch(server.Client(), server.URL+"/.well-known/jwks.json")(context.Background()); err == nil {
		t.Error("500 应报错")
	}
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, 1<<20+1))
	}))
	defer big.Close()
	if _, err := jose.HTTPFetch(big.Client(), big.URL)(context.Background()); err == nil {
		t.Error("超过 1 MiB 应报错")
	}
}

// 缓存当 Keys 用：已知 kid 验得过，未知 kid 报 unknown_key。
func TestJWKSCacheAsVerifyKeys(t *testing.T) {
	e := newEnv(t)
	src := &source{body: jwks(t, e.issuer)}
	clk := &clock{t: time.UnixMilli(testutil.FixedNow)}
	o := accessOptions(e)
	o.Keys = newCache(src, clk)
	if _, err := jose.Verify(context.Background(), sign(t, e, "armadra-access", validClaims()), o); err != nil {
		t.Fatal(err)
	}
	stranger, err := jose.Sign(e.source.Private, jose.Header{Kid: "stranger", Typ: "armadra-access"}, validClaims())
	if err != nil {
		t.Fatal(err)
	}
	expectFailure(t, stranger, o, jose.FailUnknownKey)
}
