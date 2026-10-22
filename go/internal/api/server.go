package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

// MaxBodyBytes 是 JSON 体上限。
const MaxBodyBytes = 64 * 1024

// Mode 是装配模式，决定没有处理器的（预留）过程怎么答。
type Mode int

const (
	// ModePersonal：没实现的过程答 404 not_found。
	ModePersonal Mode = iota
	// ModeCloud：契约里有、还没实现的过程答 501 not_implemented。
	ModeCloud
)

var corsHeaders = map[string]string{
	"Access-Control-Allow-Origin":  "*",
	"Access-Control-Allow-Methods": "GET, POST, PATCH, DELETE, OPTIONS",
	"Access-Control-Allow-Headers": "authorization, content-type",
	"Access-Control-Max-Age":       "7200",
}

// Call 是一次过程调用（与传输无关）。
type Call struct {
	// Input 是路径参数 + query / JSON 体，已按入参 schema 校验并剔掉未列出的字段。
	Input map[string]any
	// IP 是客户端 IP（装配方按受信代理规则取）。
	IP string
	// Authorization 是 Authorization 头原文（sources.heartbeatHint 用 Source <jws>）。
	Authorization string

	authenticate  func(ctx context.Context) (core.Caller, error)
	caller        *core.Caller
	callerErr     error
	authenticated bool
}

// Caller 按 Authorization: Bearer 认证调用方（惰性、只认证一次）。
func (c *Call) Caller(ctx context.Context) (core.Caller, error) {
	if !c.authenticated {
		c.authenticated = true
		caller, err := c.authenticate(ctx)
		c.callerErr = err
		if err == nil {
			c.caller = &caller
		}
	}
	if c.callerErr != nil {
		return core.Caller{}, c.callerErr
	}
	return *c.caller, nil
}

// Bind 把入参解进处理器自己的结构体（入参已校验过，这里只管取值）。
func (c *Call) Bind(out any) error {
	data, err := json.Marshal(c.Input)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

// Handler 处理一个过程，答输出对象（序列化成 200 的 JSON）或错误。
type Handler func(ctx context.Context, call *Call) (any, error)

// Options 配置 API。
type Options struct {
	Mode     Mode
	Handlers map[string]Handler
	// Exclude 是不进路由表的过程。
	Exclude []string
	// Authenticator 认证 Bearer 访问令牌；nil 时需要认证的过程答 unauthenticated。
	Authenticator *core.Authenticator
	// ClientIP 取客户端 IP；缺省直连对端地址。
	ClientIP func(*http.Request) string
	// OnError 在处理器意外出错时记一笔（只给错误，不含请求内容）。
	OnError func(error)
}

// API 是 /v1/* 与 /.well-known/* 的 HTTP 外壳。
type API struct {
	router *Router
	opts   Options
}

// New 建 API。
func New(options Options) (*API, error) {
	router, err := NewRouter(options.Handlers, options.Exclude...)
	if err != nil {
		return nil, err
	}
	if options.ClientIP == nil {
		options.ClientIP = remoteIP
	}
	return &API{router: router, opts: options}, nil
}

// Router 是路由表。
func (a *API) Router() *Router { return a.router }

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Claims：这条路径归 API 管吗（/v1/*，以及 /.well-known/ 下的平台信息与 JWKS）。其余交给别的处理器。
func (a *API) Claims(path string) bool {
	if strings.HasPrefix(path, "/v1/") {
		return true
	}
	return path == gen.PathPlatformWellKnown || path == gen.PathJwksWellKnown
}

func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func protocolHeader() string { return fmt.Sprintf("%d.%d", gen.ProtocolMajor, gen.ProtocolMinor) }

func writeJSON(w http.ResponseWriter, status int, body any, extra map[string]string) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(body); err != nil {
		status = http.StatusInternalServerError
		buf.Reset()
		buf.WriteString(`{"code":"internal","message":"Internal error"}`)
	}
	payload := bytes.TrimRight(buf.Bytes(), "\n")
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(payload)))
	h.Set("Cache-Control", "no-store")
	h.Set(gen.HeaderProtocol, protocolHeader())
	for key, value := range corsHeaders {
		h.Set(key, value)
	}
	for key, value := range extra {
		h.Set(key, value)
	}
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}

// errorBody 是线上错误形状 { code, message, requestId, details? }。
type errorBody struct {
	Code      gen.ErrorCode `json:"code"`
	Message   string        `json:"message"`
	RequestID string        `json:"requestId"`
	Details   *errorDetails `json:"details,omitempty"`
}

type errorDetails struct {
	RetryAfterMs int64 `json:"retryAfterMs"`
}

// WriteError 按错误码写错误答复；retryAfter 非零时带 details.retryAfterMs 与 Retry-After 头。
func WriteError(w http.ResponseWriter, code gen.ErrorCode, requestID string, retryAfter time.Duration) {
	status, ok := code.HTTPStatus()
	if !ok {
		status = http.StatusInternalServerError
	}
	body := errorBody{Code: code, Message: core.MessageOf(code), RequestID: requestID}
	extra := map[string]string{}
	if retryAfter > 0 {
		body.Details = &errorDetails{RetryAfterMs: retryAfter.Milliseconds()}
		extra["Retry-After"] = strconv.FormatInt(max(1, int64((retryAfter+time.Second-1)/time.Second)), 10)
	}
	writeJSON(w, status, body, extra)
}

func (a *API) fail(w http.ResponseWriter, err error, requestID string) {
	var failure *core.Error
	if errors.As(err, &failure) {
		WriteError(w, failure.Code, requestID, failure.RetryAfter)
		return
	}
	if a.opts.OnError != nil {
		a.opts.OnError(err)
	}
	WriteError(w, gen.ErrInternal, requestID, 0)
}

func readBody(r *http.Request) (map[string]any, error) {
	if r.Body == nil {
		return map[string]any{}, nil
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes+1))
	if err != nil || len(data) > MaxBodyBytes {
		return nil, core.Fail(gen.ErrBadRequest)
	}
	if len(data) == 0 {
		return map[string]any{}, nil
	}
	var parsed any
	if json.Unmarshal(data, &parsed) != nil {
		return nil, core.Fail(gen.ErrBadRequest)
	}
	object, ok := parsed.(map[string]any)
	if !ok {
		return nil, core.Fail(gen.ErrBadRequest)
	}
	return object, nil
}

// ServeHTTP 处理 /v1/* 与 /.well-known/*。
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		for key, value := range corsHeaders {
			w.Header().Set(key, value)
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	requestID := newRequestID()
	method := r.Method
	if method == http.MethodHead {
		method = http.MethodGet
	}
	match := a.router.Match(method, r.URL.EscapedPath())
	if match == nil || match.Handler == nil {
		if r.Body != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, MaxBodyBytes))
		}
		code := gen.ErrNotFound
		if match != nil && a.opts.Mode == ModeCloud {
			code = gen.ErrNotImplemented
		}
		WriteError(w, code, requestID, 0)
		return
	}
	var raw map[string]any
	if method == http.MethodGet || method == http.MethodDelete {
		raw = queryObject(r.URL.Query(), match.Route.Input)
	} else {
		var err error
		if raw, err = readBody(r); err != nil {
			a.fail(w, err, requestID)
			return
		}
	}
	for name, value := range match.Params {
		raw[name] = value
	}
	input, err := decodeInput(match.Route.Input, raw)
	if err != nil {
		a.fail(w, core.Fail(gen.ErrBadRequest), requestID)
		return
	}
	authorization := r.Header.Get("Authorization")
	call := &Call{Input: input, IP: a.opts.ClientIP(r), Authorization: authorization}
	call.authenticate = func(ctx context.Context) (core.Caller, error) {
		if a.opts.Authenticator == nil {
			return core.Caller{}, core.Fail(gen.ErrUnauthenticated)
		}
		return a.opts.Authenticator.Authenticate(ctx, authorization)
	}
	output, err := match.Handler(r.Context(), call)
	if err != nil {
		a.fail(w, err, requestID)
		return
	}
	writeJSON(w, http.StatusOK, output, nil)
}
