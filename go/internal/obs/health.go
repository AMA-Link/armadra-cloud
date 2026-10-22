package obs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/httperr"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

// protocolHeaderValue 是 `armadra-protocol` 头的值（协议版本 major.minor）。
var protocolHeaderValue = strconv.Itoa(gen.ProtocolMajor) + "." + strconv.Itoa(gen.ProtocolMinor)

// DefaultProbeTimeout 是单个探测的缺省超时。
const DefaultProbeTimeout = 2 * time.Second

// sendJSON 写 JSON 答复：content-type、content-length、cache-control: no-store，再加 extra。
func sendJSON(w http.ResponseWriter, status int, v any, extra map[string]string) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	payload := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	h := w.Header()
	h.Set("content-type", "application/json; charset=utf-8")
	h.Set("content-length", strconv.Itoa(len(payload)))
	h.Set("cache-control", "no-store")
	for k, val := range extra {
		h.Set(k, val)
	}
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}

func getOnly(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		httperr.Write(w, gen.ErrNotFound, httperr.Options{})
		return false
	}
	return true
}

// HealthOptions 是 /health 的内容。
type HealthOptions struct {
	Mode, Node, Version string
	Counts              func() (tunnels, streams int)
}

type healthBody struct {
	OK      bool   `json:"ok"`
	Mode    string `json:"mode"`
	Node    string `json:"node"`
	Version string `json:"version"`
	Tunnels int    `json:"tunnels"`
	Streams int    `json:"streams"`
}

// HealthHandler 答 `{ok,mode,node,version,tunnels,streams}`，并带 armadra-protocol 头。
func HealthHandler(o HealthOptions) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !getOnly(w, r) {
			return
		}
		body := healthBody{OK: true, Mode: o.Mode, Node: o.Node, Version: o.Version}
		if o.Counts != nil {
			body.Tunnels, body.Streams = o.Counts()
		}
		sendJSON(w, http.StatusOK, body, map[string]string{gen.HeaderProtocol: protocolHeaderValue})
	})
}

// Probe 是一个依赖的就绪探测；返回 nil 表示可用。
type Probe func(ctx context.Context) error

// Check 是一个探测的结果；不含错误原文（连接串可能出现在驱动的错误消息里）。
type Check struct {
	OK bool  `json:"ok"`
	MS int64 `json:"ms"`
}

// Readiness 是 /ready 的结果。encoding/json 按键名排序输出 checks，输出确定。
type Readiness struct {
	OK     bool             `json:"ok"`
	Checks map[string]Check `json:"checks"`
}

// RunProbes 并行跑全部探测，每个带超时（timeout <= 0 取 2 秒）。
func RunProbes(ctx context.Context, probes map[string]Probe, timeout time.Duration) Readiness {
	if timeout <= 0 {
		timeout = DefaultProbeTimeout
	}
	result := Readiness{OK: true, Checks: make(map[string]Check, len(probes))}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for name, probe := range probes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			started := time.Now()
			pctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				defer func() {
					if recover() != nil {
						done <- context.Canceled
					}
				}()
				done <- probe(pctx)
			}()
			ok := false
			select {
			case err := <-done:
				ok = err == nil
			case <-pctx.Done():
			}
			check := Check{OK: ok, MS: time.Since(started).Milliseconds()}
			mu.Lock()
			result.Checks[name] = check
			if !ok {
				result.OK = false
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	return result
}

// ReadyHandler 逐项跑探测，全部通过答 200，否则 503。
func ReadyHandler(probes map[string]Probe) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !getOnly(w, r) {
			return
		}
		result := RunProbes(r.Context(), probes, DefaultProbeTimeout)
		status := http.StatusOK
		if !result.OK {
			status = http.StatusServiceUnavailable
		}
		sendJSON(w, status, result, map[string]string{gen.HeaderProtocol: protocolHeaderValue})
	})
}

// MetricsHandler 只答 GET /metrics；token 非空时要求 `Authorization: Bearer <token>`，
// 按 SHA-256 摘要做常量时间比较。
func MetricsHandler(render func() string, token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/metrics" {
			httperr.Write(w, gen.ErrNotFound, httperr.Options{})
			return
		}
		if token != "" && !sameSecret(r.Header.Get("Authorization"), "Bearer "+token) {
			httperr.Write(w, gen.ErrUnauthenticated, httperr.Options{})
			return
		}
		body := render()
		h := w.Header()
		h.Set("content-type", "text/plain; version=0.0.4; charset=utf-8")
		h.Set("content-length", strconv.Itoa(len(body)))
		h.Set("cache-control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = strings.NewReader(body).WriteTo(w)
	})
}

func sameSecret(given, expected string) bool {
	a, b := sha256.Sum256([]byte(given)), sha256.Sum256([]byte(expected))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}
