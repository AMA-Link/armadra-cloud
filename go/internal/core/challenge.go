package core

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

// 人机挑战（cloud-api §16，中继设计 §4.3）：登录（与可选的接受链接）之前先过 Cloudflare Turnstile。
//
//   - 只有装配方给了 ChallengeGate（配置了 siteKey 与 secret）才要求；不给 = 不要求、platform.info
//     不报 challenge；
//   - 校验经端口 ChallengeVerifier 注入：控制核不发 HTTP 的决策，装配方给一个调 siteverify 的实现
//     （HTTPVerifier 是现成的一个）；
//   - 次序（由调用方保证）：按 IP 限流先计 → 挑战 → 口令 / 链接秘密；挑战失败不计入登录失败次数；
//   - 服务端要求照 Cloudflare 官方 existing-widget flow：令牌缺失 / 空 / 超过 2048 字符即拒绝；
//     siteverify 10 秒超时；失败一律关闭（网络错、非 2xx、答复不成形都答 challenge_invalid）；
//     同时要求 success 为真、action 等于该入口的 action、hostname 在允许列表内（空列表拒绝一切）；
//   - 令牌一次性：重放由提供方拒绝（timeout-or-duplicate）；这里另留一份 300 秒的本地防重放表只是
//     省一次往返，只记令牌的哈希，不记明文；
//   - 提供方没答、答了 5xx：同一个幂等键重试一次，仍不行答 challenge_invalid（客户端换新令牌重来）。

// ChallengeScope 是要挑战的 procedure。
type ChallengeScope string

// 能挑战的 procedure；personal 缺省只要求登录。
const (
	ScopeLogin          ChallengeScope = "auth.login"
	ScopeLinksAccept    ChallengeScope = "links.accept"
	ScopeChangePassword ChallengeScope = "auth.changePassword"
	// 下面两项只有 SaaS 控制面实现（personal 没有注册与找回），不在 ChallengeScopes 里。
	ScopeRegister   ChallengeScope = "auth.register"
	ScopeResetStart ChallengeScope = "auth.passwordResetStart"
)

// TurnstileSiteverifyURL 是 Turnstile 的服务端校验地址。
const TurnstileSiteverifyURL = "https://challenges.cloudflare.com/turnstile/v0/siteverify"

// 挑战常量。
const (
	// ChallengeTokenTTL 是令牌有效期（Turnstile：300 秒），也是本地防重放表的保留时长。
	ChallengeTokenTTL = 300 * time.Second
	// SiteverifyTimeout 是校验请求的超时。
	SiteverifyTimeout = 10 * time.Second
	// ChallengeTokenMax 是令牌长度上限（Turnstile 文档：最长 2048 字符）。
	ChallengeTokenMax = 2048
	// maxSeen 是本地防重放表的上限（超了先清过期的，仍满就整表清空：提供方仍会拒绝重放）。
	maxSeen = 10_000
)

// ChallengeScopes 是 personal 能挑战的 procedure（ParseChallengeScope 的白名单）。
var ChallengeScopes = []ChallengeScope{ScopeLogin, ScopeLinksAccept, ScopeChangePassword}

// DefaultChallengeScope 是缺省要求挑战的 procedure。
var DefaultChallengeScope = []ChallengeScope{ScopeLogin}

// ChallengeActions 是各入口的 Turnstile action：客户端渲染组件时必须传同一个值，
// siteverify 答复的 action 与之不等即拒绝。
var ChallengeActions = map[ChallengeScope]string{
	ScopeLogin:          "armadra-login",
	ScopeLinksAccept:    "armadra-accept",
	ScopeChangePassword: "armadra-password",
	ScopeRegister:       "armadra-register",
	ScopeResetStart:     "armadra-reset",
}

// Verdict 是一次校验的结论。
type Verdict struct {
	OK bool
	// Reasons 是不通过的原因：提供方的 error-codes（如 timeout-or-duplicate），或本地的
	// network / bad-response / replayed。不含令牌与 secret。
	Reasons []string
	// Retryable：提供方没给出结论（网络错、5xx、答复不成形），可以用同一个幂等键再问一次。
	Retryable bool
}

// VerifyRequest 是一次校验请求。
type VerifyRequest struct {
	Token          string
	RemoteIP       string
	IdempotencyKey string
	// Action：答复的 action 必须等于它。
	Action string
	// Hostnames：答复的 hostname 必须在其中（已小写；空 = 拒绝一切）。
	Hostnames []string
}

// ChallengeVerifier 是提供方校验端口。
type ChallengeVerifier interface {
	Verify(ctx context.Context, request VerifyRequest) (Verdict, error)
}

// PlatformChallenge 是 platform.info.challenge。
type PlatformChallenge struct {
	Provider string           `json:"provider"`
	SiteKey  string           `json:"siteKey"`
	Scope    []ChallengeScope `json:"scope"`
}

// SiteverifyBody 是 siteverify 的请求体（application/x-www-form-urlencoded）。
func SiteverifyBody(secret, token, remoteIP, idempotencyKey string) url.Values {
	body := url.Values{}
	body.Set("secret", secret)
	body.Set("response", token)
	if remoteIP != "" && remoteIP != "unknown" {
		body.Set("remoteip", remoteIP)
	}
	body.Set("idempotency_key", idempotencyKey)
	return body
}

// ReadSiteverify 解读 siteverify 的答复（HTTP 状态 + JSON 体）。失败关闭：非 2xx、不成形、
// success 不是 true、action 或 hostname 对不上，都不通过。
func ReadSiteverify(status int, body []byte, action string, hostnames []string) Verdict {
	if status >= 500 {
		return Verdict{Reasons: []string{"network"}, Retryable: true}
	}
	if status < 200 || status >= 300 {
		return Verdict{Reasons: []string{"bad-status"}}
	}
	var answer struct {
		Success    *bool    `json:"success"`
		Action     *string  `json:"action"`
		Hostname   *string  `json:"hostname"`
		ErrorCodes []string `json:"error-codes"`
	}
	if json.Unmarshal(body, &answer) != nil || answer.Success == nil {
		return Verdict{Reasons: []string{"bad-response"}}
	}
	if !*answer.Success {
		reasons := answer.ErrorCodes
		if len(reasons) > 8 {
			reasons = reasons[:8]
		}
		retryable := false
		for _, reason := range reasons {
			if reason == "internal-error" {
				retryable = true
			}
		}
		if len(reasons) == 0 {
			reasons = []string{"rejected"}
		}
		// 提供方自己的内部错误可以重试；其余是令牌本身的问题。
		return Verdict{Reasons: reasons, Retryable: retryable}
	}
	if answer.Action == nil || *answer.Action != action {
		return Verdict{Reasons: []string{"action-mismatch"}}
	}
	hostname := ""
	if answer.Hostname != nil {
		hostname = strings.ToLower(strings.TrimSpace(*answer.Hostname))
	}
	if hostname == "" {
		return Verdict{Reasons: []string{"hostname-mismatch"}}
	}
	for _, allowed := range hostnames {
		if allowed == hostname {
			return Verdict{OK: true}
		}
	}
	return Verdict{Reasons: []string{"hostname-mismatch"}}
}

var hostnamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)

// ParseChallengeHostnames 解逗号分隔的主机名白名单（配置里用）：小写、去重；不能带协议、端口或路径。
// 空串给空列表（闸会拒绝一切）。allowLocal 为假（生产）时 localhost、127.0.0.1、::1 直接拒绝。
func ParseChallengeHostnames(text string, allowLocal bool) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, item := range strings.Split(text, ",") {
		item = strings.ToLower(strings.TrimSpace(item))
		if item == "" {
			continue
		}
		if !hostnamePattern.MatchString(item) && item != "::1" {
			return nil, fmt.Errorf("挑战主机名不合法：%s", item)
		}
		if !allowLocal && (item == "localhost" || strings.HasSuffix(item, ".localhost") || item == "::1" || strings.HasPrefix(item, "127.")) {
			return nil, fmt.Errorf("生产环境的挑战主机名不能含本机地址：%s", item)
		}
		if !seen[item] {
			seen[item] = true
			out = append(out, item)
		}
	}
	return out, nil
}

// ParseChallengeScope 解逗号分隔的 scope（配置里用）；有不认识的或为空就报错。
func ParseChallengeScope(text string) ([]ChallengeScope, error) {
	var out []ChallengeScope
	seen := map[ChallengeScope]bool{}
	for _, item := range strings.Split(text, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		known := false
		for _, scope := range ChallengeScopes {
			known = known || string(scope) == item
		}
		if !known {
			return nil, fmt.Errorf("挑战范围不认识：%s", item)
		}
		if !seen[ChallengeScope(item)] {
			seen[ChallengeScope(item)] = true
			out = append(out, ChallengeScope(item))
		}
	}
	if len(out) == 0 {
		return nil, errors.New("挑战范围为空")
	}
	return out, nil
}

// ChallengeGateOptions 配置挑战闸。
type ChallengeGateOptions struct {
	SiteKey string
	// Scope 缺省 DefaultChallengeScope。
	Scope []ChallengeScope
	// Hostnames 是 siteverify 答复里 hostname 的允许列表；空 = 拒绝一切。
	Hostnames []string
	Verifier  ChallengeVerifier
	Now       func() time.Time
	// OnRejected 在不通过时记一笔（只给原因，不给令牌）。
	OnRejected func(procedure ChallengeScope, reasons []string)
}

// ChallengeGate 是挑战闸。
type ChallengeGate struct {
	siteKey   string
	scope     []ChallengeScope
	hostnames []string
	verifier  ChallengeVerifier
	now       func() time.Time
	onReject  func(ChallengeScope, []string)

	mu sync.Mutex
	// seen：用过的令牌哈希 → 过期时刻（毫秒）。
	seen map[string]int64
}

// NewChallengeGate 建挑战闸。
func NewChallengeGate(options ChallengeGateOptions) (*ChallengeGate, error) {
	siteKey := strings.TrimSpace(options.SiteKey)
	if siteKey == "" {
		return nil, errors.New("挑战缺 siteKey")
	}
	scope := options.Scope
	if scope == nil {
		scope = DefaultChallengeScope
	}
	if len(scope) == 0 {
		return nil, errors.New("挑战范围为空")
	}
	if options.Verifier == nil {
		return nil, errors.New("挑战缺 Verifier")
	}
	var hostnames []string
	for _, item := range options.Hostnames {
		if item = strings.ToLower(strings.TrimSpace(item)); item != "" {
			hostnames = append(hostnames, item)
		}
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &ChallengeGate{
		siteKey: siteKey, scope: append([]ChallengeScope(nil), scope...), hostnames: hostnames,
		verifier: options.Verifier, now: now, onReject: options.OnRejected, seen: map[string]int64{},
	}, nil
}

// Info 是 platform.info.challenge。
func (g *ChallengeGate) Info() PlatformChallenge {
	return PlatformChallenge{Provider: "turnstile", SiteKey: g.siteKey, Scope: append([]ChallengeScope(nil), g.scope...)}
}

func challengeTokenHash(token string) string {
	sum := sha256.Sum256([]byte("armadra/relay/v1/challenge\x00" + token))
	return hex.EncodeToString(sum[:])
}

// claim 是「查」和「记」合成的一步：这个令牌哈希还没用过就占下并答 true；用过（且未过期）答 false。
// 并发的同一令牌只有一个能占到，去问提供方。
func (g *ChallengeGate) claim(hash string, nowMs int64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if until, ok := g.seen[hash]; ok && until > nowMs {
		return false
	}
	if len(g.seen) >= maxSeen {
		for key, until := range g.seen {
			if until <= nowMs {
				delete(g.seen, key)
			}
		}
	}
	if len(g.seen) >= maxSeen {
		g.seen = map[string]int64{}
	}
	g.seen[hash] = nowMs + ChallengeTokenTTL.Milliseconds()
	return true
}

func (g *ChallengeGate) reject(procedure ChallengeScope, reasons []string) error {
	if g.onReject != nil {
		g.onReject(procedure, reasons)
	}
	return Fail(gen.ErrChallengeInvalid)
}

func (g *ChallengeGate) inScope(procedure ChallengeScope) bool {
	for _, scope := range g.scope {
		if scope == procedure {
			return true
		}
	}
	return false
}

// Check：这条 procedure 要挑战就校验；不通过抛 challenge_required / challenge_invalid。
// 调用方在按 IP 限流之后、口令或秘密之前调。nil 闸等于不要求。
func (g *ChallengeGate) Check(ctx context.Context, procedure ChallengeScope, challenge *ChallengeToken, ip string) error {
	if g == nil || !g.inScope(procedure) {
		return nil
	}
	if challenge == nil {
		return Fail(gen.ErrChallengeRequired)
	}
	if challenge.Provider != "turnstile" {
		return g.reject(procedure, []string{"provider"})
	}
	token := challenge.Token
	if token == "" || len(token) > ChallengeTokenMax {
		return g.reject(procedure, []string{"bad-token"})
	}
	nowMs := g.now().UnixMilli()
	hash := challengeTokenHash(token)
	// 查与记是一步：并发的同一令牌只有一个去问提供方，其余按重放拒绝。
	if !g.claim(hash, nowMs) {
		return g.reject(procedure, []string{"replayed"})
	}
	request := VerifyRequest{
		Token: token, RemoteIP: ip, IdempotencyKey: newUUID(),
		Action: ChallengeActions[procedure], Hostnames: g.hostnames,
	}
	ask := func() Verdict {
		verdict, err := g.verifier.Verify(ctx, request)
		if err != nil {
			return Verdict{Reasons: []string{"network"}, Retryable: true}
		}
		return verdict
	}
	verdict := ask()
	if !verdict.OK && verdict.Retryable {
		verdict = ask()
	}
	if !verdict.OK {
		return g.reject(procedure, verdict.Reasons)
	}
	return nil
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// HTTPVerifier 调 Turnstile 的 siteverify。secret 只在请求体里，不进日志与错误。
type HTTPVerifier struct {
	Secret string
	// URL 缺省 TurnstileSiteverifyURL（测试换成本地服务）。
	URL string
	// Client 缺省带 10 秒超时的 http.Client。
	Client *http.Client
}

// Verify 实现 ChallengeVerifier。
func (v HTTPVerifier) Verify(ctx context.Context, request VerifyRequest) (Verdict, error) {
	target := v.URL
	if target == "" {
		target = TurnstileSiteverifyURL
	}
	client := v.Client
	if client == nil {
		client = &http.Client{Timeout: SiteverifyTimeout}
	}
	ctx, cancel := context.WithTimeout(ctx, SiteverifyTimeout)
	defer cancel()
	body := SiteverifyBody(v.Secret, request.Token, request.RemoteIP, request.IdempotencyKey).Encode()
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(body))
	if err != nil {
		return Verdict{}, errors.New("siteverify 请求建不出来")
	}
	httpRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := client.Do(httpRequest)
	if err != nil {
		// 网络错误的文本可能带地址：不转述。
		return Verdict{Reasons: []string{"network"}, Retryable: true}, nil
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	if err != nil {
		return Verdict{Reasons: []string{"network"}, Retryable: true}, nil
	}
	return ReadSiteverify(response.StatusCode, data, request.Action, request.Hostnames), nil
}
