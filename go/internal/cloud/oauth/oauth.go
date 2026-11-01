// Package oauth 是第三方登录（GitHub、Google）的提供方一侧（设计 §4.3）：授权码 + PKCE（S256），
// 标准库实现。它只管「拼授权地址」和「用 code 换出一个已验证的身份」；账号怎么建、怎么绑在 accounts 里。
// 访问令牌只在一次换取里用一下就丢，不落库、不进日志、不进答复。
package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 提供方的名字（与协议包 OAUTH_PROVIDERS 一致）。
const (
	GitHub = "github"
	Google = "google"
)

// Endpoints 是提供方的各个地址。缺省是官方地址；测试用 httptest 的假提供方覆盖。
type Endpoints struct {
	// Authorize 是授权页，Token 是换令牌的地址。
	Authorize string
	Token     string
	// User 与 Emails 是 GitHub 的 /user 与 /user/emails；UserInfo 是 Google 的 OpenID Connect userinfo。
	User     string
	Emails   string
	UserInfo string
}

// DefaultEndpoints 是官方地址。
func DefaultEndpoints(provider string) (Endpoints, bool) {
	switch provider {
	case GitHub:
		return Endpoints{
			Authorize: "https://github.com/login/oauth/authorize",
			Token:     "https://github.com/login/oauth/access_token",
			User:      "https://api.github.com/user",
			Emails:    "https://api.github.com/user/emails",
		}, true
	case Google:
		return Endpoints{
			Authorize: "https://accounts.google.com/o/oauth2/v2/auth",
			Token:     "https://oauth2.googleapis.com/token",
			UserInfo:  "https://openidconnect.googleapis.com/v1/userinfo",
		}, true
	}
	return Endpoints{}, false
}

// Provider 是一个配好的提供方。
type Provider struct {
	ClientID     string
	ClientSecret string
	// Endpoints 的空项取官方地址。
	Endpoints Endpoints
}

// Identity 是提供方给出的已登录身份。
type Identity struct {
	// Subject 是提供方内不变的主体（GitHub 的数字 id、Google 的 sub）。
	Subject string
	// Email 只在提供方确认已验证时才有值（EmailVerified 为真）；Login 是 GitHub 的登录名，Name 是显示名。
	Email         string
	EmailVerified bool
	Login         string
	Name          string
}

// ErrRejected：提供方不认这次授权（code 无效、过期、已用，或用户拒绝）。
var ErrRejected = errors.New("提供方拒绝了这次授权")

// ErrUnavailable：提供方不可达或答复不合格式。
var ErrUnavailable = errors.New("提供方不可用")

// responseLimit 是读提供方答复的上限。
const responseLimit = 1 << 20

// Client 按提供方配置换身份。
type Client struct {
	providers map[string]Provider
	http      *http.Client
}

// Options 是 Client 的装配。
type Options struct {
	// Providers 按名字给配好的提供方；没配的提供方不在表里。
	Providers map[string]Provider
	// HTTP 缺省是 10 秒超时、不跟随重定向的客户端。
	HTTP *http.Client
}

// New 建 Client；每个提供方的空地址补官方地址，名字不认识的忽略。
func New(options Options) *Client {
	providers := map[string]Provider{}
	for name, p := range options.Providers {
		defaults, ok := DefaultEndpoints(name)
		if !ok {
			continue
		}
		e := p.Endpoints
		if e.Authorize == "" {
			e.Authorize = defaults.Authorize
		}
		if e.Token == "" {
			e.Token = defaults.Token
		}
		if e.User == "" {
			e.User = defaults.User
		}
		if e.Emails == "" {
			e.Emails = defaults.Emails
		}
		if e.UserInfo == "" {
			e.UserInfo = defaults.UserInfo
		}
		p.Endpoints = e
		providers[name] = p
	}
	client := options.HTTP
	if client == nil {
		client = &http.Client{
			Timeout:       10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	return &Client{providers: providers, http: client}
}

// Configured 报告提供方是否配了。
func (c *Client) Configured(provider string) bool {
	_, ok := c.providers[provider]
	return ok
}

// Names 是配了的提供方，按固定顺序（github、google）。
func (c *Client) Names() []string {
	var out []string
	for _, name := range []string{GitHub, Google} {
		if c.Configured(name) {
			out = append(out, name)
		}
	}
	return out
}

// Challenge 是 PKCE 的 S256 挑战：base64url(sha256(verifier))，无填充。
func Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// AuthorizationURL 拼授权地址：GitHub 要 read:user 与 user:email；Google 走 OpenID Connect（openid email profile），
// 每次让用户选账号。
func (c *Client) AuthorizationURL(provider, state, verifier, redirectURI string) (string, bool) {
	p, ok := c.providers[provider]
	if !ok {
		return "", false
	}
	query := url.Values{
		"client_id":             {p.ClientID},
		"redirect_uri":          {redirectURI},
		"state":                 {state},
		"code_challenge":        {Challenge(verifier)},
		"code_challenge_method": {"S256"},
	}
	switch provider {
	case GitHub:
		query.Set("scope", "read:user user:email")
	case Google:
		query.Set("response_type", "code")
		query.Set("scope", "openid email profile")
		query.Set("prompt", "select_account")
	}
	return p.Endpoints.Authorize + "?" + query.Encode(), true
}

// Exchange 用 code 与 code_verifier 换访问令牌，再取身份；访问令牌只用这一次。
func (c *Client) Exchange(ctx context.Context, provider, code, verifier, redirectURI string) (Identity, error) {
	p, ok := c.providers[provider]
	if !ok {
		return Identity{}, ErrRejected
	}
	token, err := c.exchangeCode(ctx, p, code, verifier, redirectURI, provider)
	if err != nil {
		return Identity{}, err
	}
	if provider == GitHub {
		return c.githubIdentity(ctx, p, token)
	}
	return c.googleIdentity(ctx, p, token)
}

func (c *Client) exchangeCode(ctx context.Context, p Provider, code, verifier, redirectURI, provider string) (string, error) {
	form := url.Values{
		"client_id": {p.ClientID}, "client_secret": {p.ClientSecret}, "code": {code},
		"redirect_uri": {redirectURI}, "code_verifier": {verifier},
	}
	if provider == Google {
		form.Set("grant_type", "authorization_code")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.Endpoints.Token, strings.NewReader(form.Encode()))
	if err != nil {
		return "", ErrUnavailable
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	status, body, err := c.do(req)
	if err != nil {
		return "", err
	}
	if status >= 500 {
		return "", fmt.Errorf("%w：令牌端点答 %d", ErrUnavailable, status)
	}
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if json.Unmarshal(body, &out) != nil {
		if status >= 400 {
			return "", ErrRejected
		}
		return "", fmt.Errorf("%w：令牌答复不合格式（%d）", ErrUnavailable, status)
	}
	switch {
	case out.Error != "" || status >= 400:
		return "", ErrRejected
	case status != http.StatusOK || out.AccessToken == "":
		return "", fmt.Errorf("%w：令牌答复状态 %d", ErrUnavailable, status)
	}
	return out.AccessToken, nil
}

// do 发请求并读限长的答复体；传输错误统一成 ErrUnavailable（不带 URL 之外的东西）。
func (c *Client) do(req *http.Request) (int, []byte, error) {
	res, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%w：%s 请求失败", ErrUnavailable, req.URL.Host)
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(res.Body, responseLimit))
	if err != nil {
		return 0, nil, fmt.Errorf("%w：%s 答复读不完", ErrUnavailable, req.URL.Host)
	}
	return res.StatusCode, body, nil
}

// getJSON 带访问令牌 GET 一个 JSON；非 200 都算不可用（令牌是刚换到的，不会无效）。
func (c *Client) getJSON(ctx context.Context, endpoint, token string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ErrUnavailable
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "armadra-cloud")
	status, body, err := c.do(req)
	if err != nil {
		return err
	}
	if status != http.StatusOK || json.Unmarshal(body, out) != nil {
		return fmt.Errorf("%w：%s 答 %d", ErrUnavailable, req.URL.Host, status)
	}
	return nil
}

// githubIdentity 取 /user（主体与登录名）与 /user/emails（只认 primary 且 verified 的邮箱）。
func (c *Client) githubIdentity(ctx context.Context, p Provider, token string) (Identity, error) {
	var user struct {
		ID    json.Number `json:"id"`
		Login string      `json:"login"`
		Name  *string     `json:"name"`
	}
	if err := c.getJSON(ctx, p.Endpoints.User, token, &user); err != nil {
		return Identity{}, err
	}
	if user.ID.String() == "" || strings.ContainsAny(user.ID.String(), ":. ") {
		return Identity{}, fmt.Errorf("%w：GitHub 用户 id 不合格式", ErrUnavailable)
	}
	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := c.getJSON(ctx, p.Endpoints.Emails, token, &emails); err != nil {
		return Identity{}, err
	}
	id := Identity{Subject: user.ID.String(), Login: user.Login}
	if user.Name != nil {
		id.Name = strings.TrimSpace(*user.Name)
	}
	for _, e := range emails {
		if e.Primary && e.Verified && e.Email != "" {
			id.Email, id.EmailVerified = e.Email, true
			break
		}
	}
	return id, nil
}

// googleIdentity 取 OpenID Connect userinfo（只认 email_verified = true）。
func (c *Client) googleIdentity(ctx context.Context, p Provider, token string) (Identity, error) {
	var info struct {
		Sub           string          `json:"sub"`
		Email         string          `json:"email"`
		EmailVerified json.RawMessage `json:"email_verified"`
		Name          string          `json:"name"`
	}
	if err := c.getJSON(ctx, p.Endpoints.UserInfo, token, &info); err != nil {
		return Identity{}, err
	}
	if info.Sub == "" || strings.Contains(info.Sub, " ") {
		return Identity{}, fmt.Errorf("%w：Google 的 sub 不合格式", ErrUnavailable)
	}
	// email_verified 通常是布尔，个别旧答复是字符串 "true"；其余一律当没验证。
	verified := string(info.EmailVerified) == "true" || string(info.EmailVerified) == `"true"`
	id := Identity{Subject: info.Sub, Name: strings.TrimSpace(info.Name)}
	if verified && info.Email != "" {
		id.Email, id.EmailVerified = info.Email, true
	}
	return id, nil
}
