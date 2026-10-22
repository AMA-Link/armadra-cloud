package core

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/jose"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

// 会话（中继设计 §6.2）：访问令牌（EdDSA JWT，15 分钟，aud "cloud"，sid = deviceId）与刷新令牌
// （30 天，<deviceId>.<随机>，存储只存哈希，每次刷新旋转）。
//
//   - 旧刷新令牌再来 = 重放 → 撤销整台设备，答 session_revoked；
//   - 访问令牌校验除了签名与声明，还要求 sid 指向的设备未撤销、未过期——登出 / 改口令 /
//     撤销链接即时生效，不等 15 分钟；
//   - lastSeenAtMs 只在内存里累计，每 5 分钟批量写一次。

// 会话常量（协议包 identity-vectors 的 SESSION）。
const (
	AccessTTL         = time.Duration(gen.IdentitySessionAccessTTLMs) * time.Millisecond
	RefreshTTL        = time.Duration(gen.IdentitySessionRefreshTTLMs) * time.Millisecond
	WSTicketTTL       = time.Duration(gen.IdentitySessionWSTicketTTLMs) * time.Millisecond
	LastSeenFlushEach = 5 * time.Minute
)

// Role 是调用方的角色。
type Role string

// 角色：owner 是账号本人，guest 是经链接进来的访客。
const (
	RoleOwner Role = "owner"
	RoleGuest Role = "guest"
)

// Transport 是会话的传输：bearer（原生、core、CLI，访问令牌放 Authorization 头）与 cookie（网页控制台）。
// 设备建立时定下，访问令牌只认建立它的那种传输（SaaS 实现方案 §5.1）。
type Transport string

// 会话的传输。
const (
	TransportBearer Transport = "bearer"
	TransportCookie Transport = "cookie"
)

// SessionScope 是会话的范围：普通会话与运营会话（1 小时绝对寿命）。与传输正交。
type SessionScope string

// 会话的范围。
const (
	SessionScopeSession  SessionScope = "session"
	SessionScopeOperator SessionScope = "operator"
)

// OperatorSessionTTL 是运营会话的绝对寿命，刷新不延长（SaaS 实现方案 §4.2、§4.9）。
const OperatorSessionTTL = time.Hour

// TransportOf 是设备的传输；记录里为空 = bearer。
func TransportOf(device store.DeviceRecord) Transport {
	if device.Transport == string(TransportCookie) {
		return TransportCookie
	}
	return TransportBearer
}

// ScopeOf 是设备的范围；记录里为空 = session。
func ScopeOf(device store.DeviceRecord) SessionScope {
	if device.Scope == string(SessionScopeOperator) {
		return SessionScopeOperator
	}
	return SessionScopeSession
}

// Caller 是已认证的调用方。
type Caller struct {
	Subject  string
	DeviceID string
	Role     Role
	// LinkID 是访客的链接；账号本人为空。
	LinkID string
	// AccountID 是账号本人的 sub（访客为空）；personal 里与 Subject 相同。
	AccountID string
	// Transport 与 Scope 来自设备记录；personal 恒为 bearer / session。
	Transport Transport
	Scope     SessionScope
	// ElevatedUntilMs 是 auth.stepUp 提权到的时刻；0 = 未提权。
	ElevatedUntilMs int64
}

// Elevated 报告会话此刻是否处于 auth.stepUp 提权的有效期内。
func (c Caller) Elevated(nowMs int64) bool { return c.ElevatedUntilMs > nowMs }

// IsLiveDevice：设备未撤销且未过期。
func IsLiveDevice(device store.DeviceRecord, nowMs int64) bool {
	return store.IsLiveDevice(device, nowMs)
}

// NewDevice 是建设备的纯函数：答设备记录与刷新令牌明文（只此一次），不读写存储，持久化由调用方
// （在自己的事务里）做。ttl 为 0 时取刷新令牌寿命；bearer / session 在记录里留空（个人中转的形状不变）。
func NewDevice(subject string, device DeviceInput, transport Transport, scope SessionScope, nowMs int64, ttl time.Duration) (store.DeviceRecord, string) {
	if ttl == 0 {
		ttl = RefreshTTL
	}
	deviceID := RandomHex(16)
	refreshToken := deviceID + "." + RandomToken(32)
	record := store.DeviceRecord{
		DeviceID:     deviceID,
		Subject:      subject,
		Platform:     device.Platform,
		Name:         device.Name,
		RefreshHash:  RefreshHash(refreshToken),
		Rotation:     1,
		CreatedAtMs:  nowMs,
		ExpiresAtMs:  nowMs + ttl.Milliseconds(),
		LastSeenAtMs: &nowMs,
	}
	if transport == TransportCookie {
		record.Transport = string(TransportCookie)
	}
	if scope == SessionScopeOperator {
		record.Scope = string(SessionScopeOperator)
	}
	return record, refreshToken
}

// CreateDevice 建一台 bearer / session 设备并存下，答它与刷新令牌明文（只返回这一次）。ttl 为 0 时取刷新令牌寿命。
func CreateDevice(ctx context.Context, c *Context, subject string, device DeviceInput, nowMs int64, ttl time.Duration) (store.DeviceRecord, string, error) {
	record, refreshToken := NewDevice(subject, device, TransportBearer, SessionScopeSession, nowMs, ttl)
	if err := c.Ports.Devices.Insert(ctx, c.TenantID, record, nowMs); err != nil {
		return store.DeviceRecord{}, "", err
	}
	return record, refreshToken, nil
}

// DisplayNameOf 是线上显示名：账号本人是账号名，访客是链接备注（没有就是 guest）。
func DisplayNameOf(ctx context.Context, c *Context, subject string) (string, error) {
	account, err := c.Ports.Accounts.Get(ctx, c.TenantID, subject)
	if err != nil {
		return "", err
	}
	if account != nil {
		return account.Name, nil
	}
	if linkID, ok := GuestLinkID(subject); ok {
		link, err := c.Ports.Links.Get(ctx, c.TenantID, linkID)
		if err != nil {
			return "", err
		}
		if link != nil && link.Label != "" {
			return link.Label, nil
		}
	}
	return "guest", nil
}

// SessionFor 给一台设备签访问令牌，拼成线上的 Session。
func SessionFor(ctx context.Context, c *Context, device store.DeviceRecord, refreshToken string) (Session, error) {
	access, err := c.Signer.Sign(ctx, gen.TokenTypeAccess, Claims{
		{"sub", device.Subject}, {"aud", gen.TokenAudienceAccess}, {"sid", device.DeviceID},
	}, SignOptions{TTL: AccessTTL, JTIPrefix: "a." + device.DeviceID})
	if err != nil {
		return Session{}, err
	}
	name, err := DisplayNameOf(ctx, c, device.Subject)
	if err != nil {
		return Session{}, err
	}
	return Session{
		AccessToken:       access.Token,
		RefreshToken:      refreshToken,
		AccessExpiresAtMs: access.ExpiresAtMs,
		ExpiresAtMs:       device.ExpiresAtMs,
		Account:           AccountInfo{AccountID: device.Subject, DisplayName: name},
		Device:            DeviceRef{DeviceID: device.DeviceID},
	}, nil
}

var deviceIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Refresh 用刷新令牌换新会话：旋转刷新令牌；旧令牌再来是重放，撤销整台设备。
func Refresh(ctx context.Context, c *Context, refreshToken string) (Session, error) {
	deviceID, _, _ := strings.Cut(refreshToken, ".")
	if !deviceIDPattern.MatchString(deviceID) {
		return Session{}, Fail(gen.ErrSessionRevoked)
	}
	hash := RefreshHash(refreshToken)
	nowMs := c.NowMs()
	var (
		failure  gen.ErrorCode
		updated  store.DeviceRecord
		next     string
		replayed string
	)
	err := c.Ports.Devices.Update(ctx, c.TenantID, deviceID, func(device *store.DeviceRecord) error {
		switch {
		case device == nil || device.RevokedAtMs != nil:
			failure = gen.ErrSessionRevoked
		case device.ExpiresAtMs <= nowMs:
			failure = gen.ErrSessionExpired
		case SameHex(hash, device.RefreshHash):
			next = device.DeviceID + "." + RandomToken(32)
			previous := device.RefreshHash
			device.PreviousRefreshHash = &previous
			device.RefreshHash = RefreshHash(next)
			device.Rotation++
			device.LastSeenAtMs = &nowMs
			updated = *device
		case device.PreviousRefreshHash != nil && SameHex(hash, *device.PreviousRefreshHash):
			// 用过的令牌又来了：要么被偷，要么被重放。整台设备作废。
			device.RevokedAtMs = &nowMs
			replayed = device.Subject
			failure = gen.ErrSessionRevoked
		default:
			failure = gen.ErrSessionRevoked
		}
		return nil
	})
	if err != nil {
		return Session{}, err
	}
	if replayed != "" {
		c.deviceRevoked(ctx, replayed, deviceID)
	}
	if failure != "" {
		return Session{}, Fail(failure)
	}
	return SessionFor(ctx, c, updated, next)
}

// RevokeDevice 撤销一台设备；设备不在或早已撤销答 false。
func RevokeDevice(ctx context.Context, c *Context, deviceID string) (bool, error) {
	nowMs := c.NowMs()
	revoked := false
	subject := ""
	err := c.Ports.Devices.Update(ctx, c.TenantID, deviceID, func(device *store.DeviceRecord) error {
		if device == nil || device.RevokedAtMs != nil {
			return nil
		}
		device.RevokedAtMs = &nowMs
		revoked, subject = true, device.Subject
		return nil
	})
	if revoked && err == nil {
		c.deviceRevoked(ctx, subject, deviceID)
	}
	return revoked && err == nil, err
}

var bearerHeader = regexp.MustCompile(`(?i)^Bearer\s+(\S+)$`)

// Authenticator 认证访问令牌。
type Authenticator struct {
	c *Context

	mu   sync.Mutex
	seen map[string]int64
}

// NewAuthenticator 建认证器。
func NewAuthenticator(c *Context) *Authenticator {
	return &Authenticator{c: c, seen: map[string]int64{}}
}

// Authenticate：Authorization: Bearer <访问令牌> → 调用方；缺或无效 → unauthenticated。
func (a *Authenticator) Authenticate(ctx context.Context, header string) (Caller, error) {
	match := bearerHeader.FindStringSubmatch(header)
	if match == nil {
		return Caller{}, Fail(gen.ErrUnauthenticated)
	}
	return a.AuthenticateToken(ctx, match[1], TransportBearer)
}

// AuthenticateToken 认证一枚访问令牌，并要求它来自设备建立时的传输（bearer 的从头来，cookie 的从
// Cookie 来）：对不上答 unauthenticated。
func (a *Authenticator) AuthenticateToken(ctx context.Context, token string, transport Transport) (Caller, error) {
	issuer, err := a.c.Signer.Issuer(ctx)
	if err != nil {
		return Caller{}, err
	}
	nowMs := a.c.NowMs()
	result, err := jose.Verify(ctx, token, jose.Options{
		Typ:         gen.TokenTypeAccess,
		Schema:      gen.AccessClaimsSchema,
		ExpectedAud: gen.TokenAudienceAccess,
		ExpectedIss: issuer,
		Now:         time.UnixMilli(nowMs),
		Keys:        a.c.Signer,
		// 访问令牌的撤销按 jti 形状判（a.<deviceId>.<随机>：设备失效、访客的链接撤销即作废）。
		IsRevoked: func(ctx context.Context, jti string) (bool, error) { return IsRevokedJTI(ctx, a.c, jti, nowMs) },
	})
	if err != nil {
		// 取钥时存储出错不是令牌的问题：原样上抛，别当成未认证。
		if jose.FailureOf(err) == jose.FailUnknownKey {
			var jerr *jose.Error
			if errors.As(err, &jerr) && jerr.Err != nil {
				return Caller{}, jerr.Err
			}
		}
		return Caller{}, Fail(gen.ErrUnauthenticated)
	}
	var claims gen.AccessClaims
	if err := result.Into(&claims); err != nil {
		return Caller{}, Fail(gen.ErrUnauthenticated)
	}
	device, err := a.c.Ports.Devices.Get(ctx, a.c.TenantID, claims.Sid)
	if err != nil {
		return Caller{}, err
	}
	if device == nil || device.Subject != claims.Sub || !IsLiveDevice(*device, nowMs) || TransportOf(*device) != transport {
		return Caller{}, Fail(gen.ErrUnauthenticated)
	}
	account, err := a.c.Ports.Accounts.Get(ctx, a.c.TenantID, claims.Sub)
	if err != nil {
		return Caller{}, err
	}
	caller := Caller{
		Subject: claims.Sub, DeviceID: device.DeviceID, Role: RoleOwner, AccountID: claims.Sub,
		Transport: TransportOf(*device), Scope: ScopeOf(*device),
	}
	if device.ElevatedUntilMs != nil {
		caller.ElevatedUntilMs = *device.ElevatedUntilMs
	}
	if account == nil {
		linkID, ok := GuestLinkID(claims.Sub)
		if !ok {
			return Caller{}, Fail(gen.ErrUnauthenticated)
		}
		caller.Role, caller.LinkID, caller.AccountID = RoleGuest, linkID, ""
	}
	a.Touch(device.DeviceID)
	return caller, nil
}

// Touch 记一次「最近使用」（内存，批量落盘）。
func (a *Authenticator) Touch(deviceID string) {
	a.mu.Lock()
	a.seen[deviceID] = a.c.NowMs()
	a.mu.Unlock()
}

// Flush 把累计的 lastSeenAtMs 写进存储。
func (a *Authenticator) Flush(ctx context.Context) error {
	a.mu.Lock()
	if len(a.seen) == 0 {
		a.mu.Unlock()
		return nil
	}
	batch := a.seen
	a.seen = map[string]int64{}
	a.mu.Unlock()
	return a.c.Ports.Devices.Touch(ctx, a.c.TenantID, batch)
}
