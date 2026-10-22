package core

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"regexp"
	"strings"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/jose"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/schema"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

// 源（中继设计 §6.4，平台设计 §17.3 / §17.5）：注册令牌、注册、源 JWS 换隧道令牌、源目录、
// 撤销、访问断言 + 中继令牌。撤销判断见 revocation.go。

// 源相关常量。
const (
	// MaxRegistrationTokens 是同时有效的注册令牌上限。
	MaxRegistrationTokens = 20
	registrationTTL       = time.Duration(gen.IdentityRegistrationTokenTTLMs) * time.Millisecond
	assertionTTL          = time.Duration(gen.IdentityAssertionTTLMs) * time.Millisecond
	relayTokenTTL         = time.Duration(gen.IdentityRelayTokenTTLMs) * time.Millisecond
	tunnelTokenTTL        = time.Duration(gen.IdentityTunnelTokenTTLMs) * time.Millisecond
	sourceJWSTTLMs        = int64(gen.IdentitySourceJWSTTLMs)
)

// RelayBaseURL 是源在中继上的根地址：<issuer>/s/<sourceId>。
func RelayBaseURL(issuer, sourceID string) string { return issuer + "/s/" + sourceID }

// TunnelURL 是隧道节点：issuer 的 https → wss（http → ws）+ /t/v1。
func TunnelURL(issuer string) string {
	switch {
	case strings.HasPrefix(issuer, "https"):
		issuer = "wss" + strings.TrimPrefix(issuer, "https")
	case strings.HasPrefix(issuer, "http"):
		issuer = "ws" + strings.TrimPrefix(issuer, "http")
	}
	return issuer + gen.PathTunnel
}

// TrustedOriginsOf 是页面来源：设置里的可信来源 + issuer 本身（托管页面在同一来源的 /app/）。
func TrustedOriginsOf(settings store.TenantSettings) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, origin := range append(append([]string{}, settings.TrustedOrigins...), settings.Issuer) {
		if !seen[origin] {
			seen[origin] = true
			out = append(out, origin)
		}
	}
	return out
}

// RequireOwner：只有账号本人能做。
func RequireOwner(caller Caller) error {
	if caller.Role != RoleOwner {
		return Fail(gen.ErrForbidden)
	}
	return nil
}

// RegisterInput 是 sources.register 的入参。
type RegisterInput struct {
	RegistrationToken string
	SourceID          string
	PublicKey         gen.Ed25519PublicJWK
	Name              string
	Kind              string
	CoreVersion       string
	Capabilities      []string
	ProtocolMajor     int64
	ProtocolMinor     int64
}

// GrantExtra 是签断言时的附加信息。Team、Role、Email、EmailVerified 与 TrackJTI 只有 SaaS 的访问策略
// 会填（个人中转都不带，签出的字节与 0.2.x 同形）。
type GrantExtra struct {
	Name   string
	Link   *gen.AssertionClaimsLink
	Device *DeviceInput
	Scope  *store.LinkScope
	// Team 是团队主机的团队与调用者在团队里的角色：进断言的 team 声明，teamId 也进中继令牌。
	Team *gen.AssertionClaimsTeam
	// Role 是有效角色（断言的 role 声明）；空 = 不带。
	Role string
	// Email 非空时进断言的 email 与 emailVerified。
	Email         string
	EmailVerified bool
	// TrackJTI 为真时断言的 jti 带前缀 s.<deviceId>.<sourceId>（签发索引按它过滤，SaaS 实现方案 §4.6）。
	TrackJTI bool
}

// Sources 是源服务。
type Sources struct {
	c      *Context
	replay *ReplayCache
}

// NewSources 建源服务。
func NewSources(c *Context) *Sources {
	return &Sources{c: c, replay: NewReplayCache(2*sourceJWSTTLMs+gen.IdentityAssertionSkewMs, 0)}
}

func (s *Sources) liveSource(ctx context.Context, sourceID string) (*store.SourceRecord, error) {
	source, err := s.c.Ports.Sources.Get(ctx, s.c.TenantID, sourceID)
	if err != nil || source == nil || source.RevokedAtMs != nil {
		return nil, err
	}
	return source, nil
}

func (s *Sources) summary(ctx context.Context, issuer string, visible VisibleSource) (SourceSummary, error) {
	source := visible.Source
	coreVersion, online, err := s.c.route(ctx, source.SourceID)
	if err != nil {
		return SourceSummary{}, err
	}
	out := SourceSummary{
		SourceID: source.SourceID, Name: source.Name, Kind: source.Kind, Online: online,
		LastSeenAtMs: source.LastSeenAtMs, Owner: visible.Owner, Via: visible.Via,
		RelayOrigin: s.c.relayOrigin(issuer, source.SourceID), CoreVersion: source.CoreVersion,
	}
	if online {
		now := s.c.NowMs()
		out.LastSeenAtMs = &now
		out.CoreVersion = coreVersion
	}
	return out, nil
}

// Grant 签断言 + 中继令牌（owner 或访客，已核过可见性）。scope（链接的分享范围）→ 断言 scp；
// 只读 → 中继令牌 ro: true。没有范围时两者都不出现（与 0.2.x 签出的字节同形）。中继令牌带 dev
// （1.3）：这台设备的 id，与断言的 device.deviceId 相同，中继只用它计数。
func (s *Sources) Grant(ctx context.Context, source store.SourceRecord, device store.DeviceRecord, extra GrantExtra) (Grant, error) {
	issuer, err := s.c.Signer.Issuer(ctx)
	if err != nil {
		return Grant{}, err
	}
	scp := ScopeClaims(extra.Scope)
	readOnly := false
	for _, entry := range scp {
		readOnly = readOnly || entry == gen.ScopeReadOnly
	}
	platform, name := device.Platform, device.Name
	if extra.Device != nil {
		platform, name = extra.Device.Platform, extra.Device.Name
	}
	claims := Claims{{"sub", device.Subject}, {"aud", source.SourceID}}
	if extra.Name != "" {
		claims = append(claims, Claim{"name", extra.Name})
	}
	if extra.Email != "" {
		claims = append(claims, Claim{"email", extra.Email}, Claim{"emailVerified", extra.EmailVerified})
	}
	if extra.Team != nil {
		claims = append(claims, Claim{"team", extra.Team})
	}
	if extra.Role != "" {
		claims = append(claims, Claim{"role", extra.Role})
	}
	if extra.Link != nil {
		claims = append(claims, Claim{"link", extra.Link})
	}
	if len(scp) > 0 {
		claims = append(claims, Claim{"scp", scp})
	}
	claims = append(claims, Claim{"device", gen.AssertionClaimsDevice{DeviceID: device.DeviceID, Platform: platform, Name: name}})
	assertionOptions := SignOptions{TTL: assertionTTL}
	if extra.TrackJTI {
		assertionOptions.JTIPrefix = "s." + device.DeviceID + "." + source.SourceID
	}
	assertion, err := s.c.Signer.Sign(ctx, gen.TokenTypeAssertion, claims, assertionOptions)
	if err != nil {
		return Grant{}, err
	}
	relayClaims := Claims{{"sub", device.Subject}, {"aud", gen.TokenAudienceRelay}, {"src", source.SourceID}}
	if extra.Team != nil {
		relayClaims = append(relayClaims, Claim{"team", extra.Team.TeamID})
	}
	if readOnly {
		relayClaims = append(relayClaims, Claim{"ro", true})
	}
	relayClaims = append(relayClaims, Claim{"dev", device.DeviceID})
	relay, err := s.c.Signer.Sign(ctx, gen.TokenTypeRelay, relayClaims, SignOptions{
		TTL: relayTokenTTL, JTIPrefix: "r." + device.DeviceID + "." + source.SourceID,
	})
	if err != nil {
		return Grant{}, err
	}
	_, online, err := s.c.route(ctx, source.SourceID)
	if err != nil {
		return Grant{}, err
	}
	return Grant{
		Assertion: assertion.Token, AssertionExpiresAtMs: assertion.ExpiresAtMs,
		RelayToken: relay.Token, RelayTokenExpiresAtMs: relay.ExpiresAtMs,
		RelayOrigin: s.c.relayOrigin(issuer, source.SourceID), RelayBaseURL: s.c.relayBaseURL(issuer, source.SourceID), Online: online,
		AssertionJTI: assertion.JTI, RelayTokenJTI: relay.JTI,
	}, nil
}

// RegistrationToken 签发一个注册令牌（10 分钟，同时最多 20 个）。
func (s *Sources) RegistrationToken(ctx context.Context, caller Caller) (RegistrationTokenResult, error) {
	if err := RequireOwner(caller); err != nil {
		return RegistrationTokenResult{}, err
	}
	nowMs := s.c.NowMs()
	token := RandomToken(32)
	expiresAtMs := nowMs + registrationTTL.Milliseconds()
	added, err := s.c.Ports.Registrations.Add(ctx, s.c.TenantID, store.RegistrationToken{
		TokenHash: SecretHash(SecretRegistration, token), ExpiresAtMs: expiresAtMs, AccountID: caller.Subject,
	}, nowMs, MaxRegistrationTokens)
	if err != nil {
		return RegistrationTokenResult{}, err
	}
	if !added {
		return RegistrationTokenResult{}, Fail(gen.ErrLimitReached)
	}
	issuer, err := s.c.Signer.Issuer(ctx)
	if err != nil {
		return RegistrationTokenResult{}, err
	}
	return RegistrationTokenResult{RegistrationToken: token, ExpiresAtMs: expiresAtMs, Issuer: issuer}, nil
}

// BuildSourceRecord 是登记的纯函数：existing 是库里已有的记录（可为 nil），答要写入的记录。
// 已有未撤销的记录公钥不同答 source_taken；同钥或已撤销的重登记保留 lastSeenAtMs 与 trustedOrigins，
// 未撤销时保留 registeredAtMs。不读写任何存储，调用方在自己的事务里持久化。
func BuildSourceRecord(existing *store.SourceRecord, input RegisterInput, nowMs int64) (store.SourceRecord, error) {
	publicJWK := gen.Ed25519PublicJWK{Kty: "OKP", Crv: "Ed25519", X: input.PublicKey.X}
	if existing != nil && existing.RevokedAtMs == nil && existing.PublicJWK.X != publicJWK.X {
		return store.SourceRecord{}, Fail(gen.ErrSourceTaken)
	}
	record := store.SourceRecord{
		SourceID: input.SourceID, Name: input.Name, Kind: input.Kind, PublicJWK: publicJWK,
		CoreVersion: input.CoreVersion, Capabilities: store.NonNil(input.Capabilities),
		RegisteredAtMs: nowMs, TrustedOrigins: []string{},
	}
	if existing != nil {
		if existing.RevokedAtMs == nil {
			record.RegisteredAtMs = existing.RegisteredAtMs
		}
		record.LastSeenAtMs = existing.LastSeenAtMs
		record.TrustedOrigins = store.NonNil(existing.TrustedOrigins)
	}
	return record, nil
}

// Register 用注册令牌登记一个源。
func (s *Sources) Register(ctx context.Context, input RegisterInput, ip string) (RegisterResult, error) {
	if err := s.c.hit(ctx, LimitLoginIP, "register:"+ip); err != nil {
		return RegisterResult{}, err
	}
	nowMs := s.c.NowMs()
	hash := SecretHash(SecretRegistration, input.RegistrationToken)
	// 一次性：先消耗，后面就算拒绝也不能再用。
	ownerID, ok, err := s.c.Ports.Registrations.Consume(ctx, s.c.TenantID, hash, nowMs)
	if err != nil {
		return RegisterResult{}, err
	}
	if !ok {
		return RegisterResult{}, Fail(gen.ErrRegistrationTokenInvalid)
	}
	if input.ProtocolMajor != gen.ProtocolMajor {
		return RegisterResult{}, Fail(gen.ErrProtocolUnsupported)
	}
	_, err = s.c.Ports.Sources.Put(ctx, s.c.TenantID, input.SourceID, func(existing *store.SourceRecord) (store.SourceRecord, error) {
		return BuildSourceRecord(existing, input, nowMs)
	})
	if err != nil {
		return RegisterResult{}, err
	}
	settings, err := s.c.Ports.Tenants.Get(ctx, s.c.TenantID)
	if err != nil {
		return RegisterResult{}, err
	}
	return RegisterResult{
		Issuer: settings.Issuer, JWKSURL: settings.Issuer + gen.PathJwksWellKnown,
		RelayOrigins: []string{s.c.relayOrigin(settings.Issuer, input.SourceID)}, TrustedOrigins: TrustedOriginsOf(settings),
		SourceID: input.SourceID, OwnerAccountID: ownerID,
	}, nil
}

var sourceHeader = regexp.MustCompile(`^Source\s+(\S+)$`)

// AuthenticateSource：Authorization: Source <jws> → 已登记的源；失败 unauthenticated / source_revoked。
func (s *Sources) AuthenticateSource(ctx context.Context, header string) (store.SourceRecord, error) {
	match := sourceHeader.FindStringSubmatch(header)
	if match == nil {
		return store.SourceRecord{}, Fail(gen.ErrUnauthenticated)
	}
	parts, err := jose.Split(match[1])
	if err != nil {
		return store.SourceRecord{}, Fail(gen.ErrUnauthenticated)
	}
	// typ 可以不写，写了就必须是 armadra-source；写成空串同样拒绝（与 TypeScript 一侧一致）。
	if parts.TypPresent && parts.Header.Typ != gen.TokenTypeSource {
		return store.SourceRecord{}, Fail(gen.ErrUnauthenticated)
	}
	source, err := s.c.Ports.Sources.Get(ctx, s.c.TenantID, parts.Header.Kid)
	if err != nil {
		return store.SourceRecord{}, err
	}
	if source == nil {
		return store.SourceRecord{}, Fail(gen.ErrUnauthenticated)
	}
	public, err := base64.RawURLEncoding.DecodeString(source.PublicJWK.X)
	if err != nil || len(public) != ed25519.PublicKeySize || !ed25519.Verify(public, parts.SigningInput, parts.Signature) {
		return store.SourceRecord{}, Fail(gen.ErrUnauthenticated)
	}
	if source.RevokedAtMs != nil {
		return store.SourceRecord{}, Fail(gen.ErrSourceRevoked)
	}
	var claims gen.SourceJWSClaims
	if schema.Decode(gen.SourceJWSClaimsSchema, parts.Payload, &claims) != nil {
		return store.SourceRecord{}, Fail(gen.ErrUnauthenticated)
	}
	issuer, err := s.c.Signer.Issuer(ctx)
	if err != nil {
		return store.SourceRecord{}, err
	}
	nowMs := s.c.NowMs()
	if claims.Iss != source.SourceID || claims.Aud != issuer {
		return store.SourceRecord{}, Fail(gen.ErrUnauthenticated)
	}
	// 与 TypeScript 的偏差：TypeScript 不看 nbf，这里看——声明了 nbf 就不能早于它（容差同 iat），
	// 只会比 TypeScript 更严；合法的源不写 nbf 或写成 iat，两边结果一致。
	if !timesOK(claims.Iat, claims.Exp, claims.Nbf, nowMs) {
		return store.SourceRecord{}, Fail(gen.ErrUnauthenticated)
	}
	if (claims.Exp-claims.Iat)*1000 > 2*sourceJWSTTLMs {
		return store.SourceRecord{}, Fail(gen.ErrUnauthenticated)
	}
	if s.replay.Seen(source.SourceID+":"+claims.Jti, claims.Exp*1000, nowMs) {
		return store.SourceRecord{}, Fail(gen.ErrUnauthenticated)
	}
	return *source, nil
}

// timesOK：exp / iat / nbf 是整数秒。过期是 now - skew >= exp；未生效是 iat 或 nbf 晚于 now + skew。
func timesOK(iat, exp int64, nbf *int64, nowMs int64) bool {
	skew := int64(gen.IdentityAssertionSkewMs)
	if nowMs-skew >= exp*1000 {
		return false
	}
	if iat*1000 > nowMs+skew {
		return false
	}
	return nbf == nil || *nbf*1000 <= nowMs+skew
}

type tunnelCnf struct {
	JWK tunnelJWK `json:"jwk"`
}

type tunnelJWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
}

// RelayFor 是 GET /v1/sources/me/relay：签隧道令牌（cnf.jwk = 登记的公钥）。
func (s *Sources) RelayFor(ctx context.Context, source store.SourceRecord) (RelayInfo, error) {
	return s.RelayForTeam(ctx, source, "")
}

// RelayForTeam 同 RelayFor；teamID 非空（团队主机）时隧道令牌带 team 声明。
func (s *Sources) RelayForTeam(ctx context.Context, source store.SourceRecord, teamID string) (RelayInfo, error) {
	issuer, err := s.c.Signer.Issuer(ctx)
	if err != nil {
		return RelayInfo{}, err
	}
	claims := Claims{
		{"sub", source.SourceID}, {"aud", gen.TokenAudienceTunnel},
		{"cnf", tunnelCnf{JWK: tunnelJWK{Kty: source.PublicJWK.Kty, Crv: source.PublicJWK.Crv, X: source.PublicJWK.X}}},
	}
	if teamID != "" {
		claims = append(claims, Claim{"team", teamID})
	}
	tunnel, err := s.c.Signer.Sign(ctx, gen.TokenTypeTunnel, claims, SignOptions{TTL: tunnelTokenTTL, JTIPrefix: "t." + source.SourceID})
	if err != nil {
		return RelayInfo{}, err
	}
	nodes, err := s.c.tunnelNodes(ctx, issuer, source.SourceID)
	if err != nil {
		return RelayInfo{}, err
	}
	return RelayInfo{
		TunnelToken: tunnel.Token, ExpiresAtMs: tunnel.ExpiresAtMs,
		Nodes: nodes,
		Limits: TunnelLimits{
			MaxStreams: gen.LimitMaxStreams, StreamWindow: gen.LimitStreamWindow,
			TunnelWindow: gen.LimitTunnelWindow, MaxFrameBytes: gen.LimitMaxFrameBytes,
		},
	}, nil
}

// List 是调用方能看到的源。
func (s *Sources) List(ctx context.Context, caller Caller) ([]SourceSummary, error) {
	settings, err := s.c.Ports.Tenants.Get(ctx, s.c.TenantID)
	if err != nil {
		return nil, err
	}
	var visible []VisibleSource
	for cursor := ""; ; {
		page, next, err := s.c.access().Visible(ctx, caller, SourceFilter{Cursor: cursor})
		if err != nil {
			return nil, err
		}
		visible = append(visible, page...)
		if next == "" {
			break
		}
		cursor = next
	}
	out := make([]SourceSummary, 0, len(visible))
	for _, source := range visible {
		summary, err := s.summary(ctx, settings.Issuer, source)
		if err != nil {
			return nil, err
		}
		out = append(out, summary)
	}
	return out, nil
}

// Get 是一个源的详情；看不到答 not_found。
func (s *Sources) Get(ctx context.Context, caller Caller, sourceID string) (SourceDetail, error) {
	settings, err := s.c.Ports.Tenants.Get(ctx, s.c.TenantID)
	if err != nil {
		return SourceDetail{}, err
	}
	entry, ok, err := s.c.access().Access(ctx, caller, sourceID)
	if err != nil {
		return SourceDetail{}, err
	}
	if ok {
		source := entry.Source
		summary, err := s.summary(ctx, settings.Issuer, entry)
		if err != nil {
			return SourceDetail{}, err
		}
		return SourceDetail{
			SourceSummary:  summary,
			TrustedOrigins: append([]string{}, source.TrustedOrigins...),
			Capabilities:   append([]string{}, source.Capabilities...),
			RegisteredAtMs: source.RegisteredAtMs,
		}, nil
	}
	return SourceDetail{}, Fail(gen.ErrNotFound)
}

// Revoke 撤销源：标记、关隧道、断开经中继的客户端 WS、通知 me/stream。
func (s *Sources) Revoke(ctx context.Context, caller Caller, sourceID string) error {
	if err := s.c.access().CanManage(ctx, caller, ManageTarget{SourceID: sourceID}, ManageSourceRevoke); err != nil {
		return err
	}
	nowMs := s.c.NowMs()
	err := s.c.Ports.Sources.Update(ctx, s.c.TenantID, sourceID, func(source *store.SourceRecord) error {
		if source == nil || source.RevokedAtMs != nil {
			return Fail(gen.ErrNotFound)
		}
		source.RevokedAtMs = &nowMs
		return nil
	})
	if err != nil {
		return err
	}
	if s.c.Relay != nil {
		s.c.Relay.CloseTunnel(sourceID)
		s.c.Relay.Disconnect(ConnectionFilter{SourceID: sourceID})
	}
	s.c.sourceRevoked(ctx, sourceID)
	s.c.Emit(NewSourceRevoked(sourceID), nil)
	return nil
}

// Assertion 给调用方签访问某个源的断言 + 中继令牌。
func (s *Sources) Assertion(ctx context.Context, caller Caller, sourceID string, device *DeviceInput) (Grant, error) {
	registered, err := s.c.Ports.Sources.Get(ctx, s.c.TenantID, sourceID)
	if err != nil {
		return Grant{}, err
	}
	if registered != nil && registered.RevokedAtMs != nil {
		return Grant{}, Fail(gen.ErrSourceRevoked)
	}
	entry, ok, err := s.c.access().Access(ctx, caller, sourceID)
	if err != nil {
		return Grant{}, err
	}
	var source *store.SourceRecord
	if ok {
		source = &entry.Source
	}
	if source == nil {
		return Grant{}, Fail(gen.ErrSourceAccessDenied)
	}
	record, err := s.c.Ports.Devices.Get(ctx, s.c.TenantID, caller.DeviceID)
	if err != nil {
		return Grant{}, err
	}
	if record == nil {
		return Grant{}, Fail(gen.ErrUnauthenticated)
	}
	extra, err := s.c.access().Intent(ctx, caller, *source)
	if err != nil {
		return Grant{}, err
	}
	extra.Device = device
	return s.Grant(ctx, *source, *record, extra)
}
