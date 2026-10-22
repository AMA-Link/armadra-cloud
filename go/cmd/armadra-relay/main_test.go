package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestRun(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  string
		wantErr  string
	}{
		{"没有命令", nil, 64, "用法: armadra-relay <命令>", ""},
		{"未知命令", []string{"nope"}, 64, "", "未知命令：nope"},
		{"版本", []string{"version"}, 0, "dev\n", ""},
		{"帮助", []string{"help"}, 0, "用法: armadra-relay", ""},
		{"dev 缺参数", []string{"dev"}, 64, "", "用法: armadra-relay dev --jwks"},
		{"dev 拒绝非回环监听", []string{"dev", "--jwks", "x", "--issuer", "i", "--listen", "0.0.0.0:1"}, 64, "", "明文监听只许回环地址"},
		{"dev 拒绝非回环指标监听", []string{"dev", "--jwks", "x", "--issuer", "i", "--metrics-listen", "10.0.0.1:1"}, 64, "", "明文监听只许回环地址"},
		{"dev 监听地址不合法", []string{"dev", "--jwks", "x", "--issuer", "i", "--listen", "nope"}, 64, "", "监听地址不合法"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			code := run(c.args, &out, &errOut)
			if code != c.wantCode || !strings.Contains(out.String(), c.wantOut) || !strings.Contains(errOut.String(), c.wantErr) {
				t.Fatalf("code=%d out=%q err=%q", code, out.String(), errOut.String())
			}
		})
	}
}

func TestRequireLoopback(t *testing.T) {
	for addr, ok := range map[string]bool{
		"127.0.0.1:0": true, "[::1]:8443": true, "localhost:80": true,
		"0.0.0.0:80": false, "192.168.1.2:80": false, "example.com:80": false, ":80": false,
	} {
		if err := requireLoopback(addr); (err == nil) != ok {
			t.Errorf("%s：err=%v，应为 ok=%v", addr, err, ok)
		}
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// dev 带与不带 --traffic 都能起来、答 /health、收到 SIGTERM 后正常退出。
func TestDevTrafficFlag(t *testing.T) {
	jwks := filepath.Join(t.TempDir(), "jwks.json")
	if err := os.WriteFile(jwks, []byte(`{"keys":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, extra := range [][]string{nil, {"--traffic"}} {
		t.Run(strings.Join(extra, " "), func(t *testing.T) {
			var out, errOut lockedBuffer
			done := make(chan int, 1)
			args := append([]string{"dev", "--jwks", jwks, "--issuer", "https://x"}, extra...)
			go func() { done <- run(args, &out, &errOut) }()
			var ready struct {
				URL string `json:"url"`
			}
			deadline := time.Now().Add(5 * time.Second)
			for ready.URL == "" {
				for _, line := range strings.Split(out.String(), "\n") {
					_ = json.Unmarshal([]byte(line), &ready)
				}
				if time.Now().After(deadline) {
					t.Fatalf("没等到就绪行：%q %q", out.String(), errOut.String())
				}
				time.Sleep(20 * time.Millisecond)
			}
			resp, err := http.Get(ready.URL + "/health")
			if err != nil || resp.StatusCode != 200 {
				t.Fatalf("/health：%v %v", resp, err)
			}
			_ = resp.Body.Close()
			_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
			select {
			case code := <-done:
				if code != 0 {
					t.Fatalf("退出码 %d：%q", code, errOut.String())
				}
			case <-time.After(10 * time.Second):
				t.Fatal("收到 SIGTERM 后没退出")
			}
		})
	}
}
