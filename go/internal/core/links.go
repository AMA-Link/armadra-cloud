package core

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"strings"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

// 分享链接与访客（中继设计 §6.5，平台设计 §17.3）。
//
//   - Create 答 secret（32 字节 base64url），存储只存哈希。分享出去的链接是
//     https://<中继>/j/<linkId>#<secret>.<core 邀请令牌>：秘密只在 # 片段里，浏览器不会把它发给任何
//     服务器；中继自己拼的 url 不带片段；
//   - Accept { secret }：按 IP 限流（link.accept.ip，20 次 / 分钟）→ 人机挑战（scope 含 links.accept
//     时，cloud-api §16）→ 链接在、未撤销 → 秘密比对 → 未过期、次数未满 → uses++ → 建访客设备
//     （guest:<linkId>:<随机>，30 天）→ 签带 link 声明的断言与中继令牌；
//   - Update { label }：只改备注（落地页与访客的源名用它）；已撤销的也能改，不动秘密、次数与期限；
//   - Revoke：撤销链接与它名下的全部访客设备，断开访客经中继的 WS；
//   - scope（0.3.0 起，SaaS 设计 §3.3 (b)）：工作空间 / 会话 / 只读。访客的断言带 scp，只读时中继令牌
//     带 ro: true（边缘不据它过滤，读写由 core 判定）。缺省 = 整台、按角色（旧行为）。

// LinkMaxTTL 是链接最长有效期：30 天（与访客的刷新令牌同寿）。
const LinkMaxTTL = RefreshTTL

// LinkURL 是链接的页面地址：<issuer>/j/<linkId>。
func LinkURL(issuer, linkID string) string { return issuer + "/j/" + linkID }

func toLinkSummary(issuer string, link store.LinkRecord) LinkSummary {
	return LinkSummary{
		LinkID: link.LinkID, Kind: "source_invite", Label: link.Label, Role: link.Role, SourceID: link.SourceID,
		Scope: copyScope(link.Scope), URL: LinkURL(issuer, link.LinkID), Uses: link.Uses, MaxUses: link.MaxUses,
		ExpiresAtMs: link.ExpiresAtMs, CreatedAtMs: link.CreatedAtMs, RevokedAtMs: link.RevokedAtMs,
	}
}

// RevokeLink 撤销链接与它的访客设备；答链接指向的源，没有这条有效链接 ok 为 false。
func RevokeLink(ctx context.Context, c *Context, linkID string, nowMs int64) (sourceID string, ok bool, err error) {
	return c.Ports.Links.Revoke(ctx, c.TenantID, linkID, nowMs)
}

// ListLinks 是全部链接（可按源过滤），按创建顺序。
func ListLinks(ctx context.Context, c *Context, sourceID string) ([]LinkSummary, error) {
	settings, err := c.Ports.Tenants.Get(ctx, c.TenantID)
	if err != nil {
		return nil, err
	}
	links, err := c.Ports.Links.List(ctx, c.TenantID, sourceID)
	if err != nil {
		return nil, err
	}
	out := make([]LinkSummary, 0, len(links))
	for _, link := range links {
		out = append(out, toLinkSummary(settings.Issuer, link))
	}
	return out, nil
}

// CreateLinkInput 是 links.create 的入参（source_invite）。
type CreateLinkInput struct {
	// OrgJoin 为真是 org_join 种类：个人中转不支持，答 bad_request。
	OrgJoin      bool
	SourceID     string
	InvitationID string
	Label        string
	Role         string
	ExpiresAtMs  int64
	MaxUses      *int64
	Scope        *LinkScopeInput
}

// AcceptInput 是 links.accept 的入参。
type AcceptInput struct {
	LinkID    string
	Secret    string
	Device    *DeviceInput
	Challenge *ChallengeToken
}

// Links 是链接服务。
type Links struct {
	c       *Context
	sources *Sources
}

// NewLinks 建链接服务。
func NewLinks(c *Context, sources *Sources) *Links { return &Links{c: c, sources: sources} }

// Create 建链接。
func (l *Links) Create(ctx context.Context, caller Caller, input CreateLinkInput) (LinkCreated, error) {
	if err := l.c.access().CanManage(ctx, caller, ManageTarget{SourceID: input.SourceID}, ManageLinkCreate); err != nil {
		return LinkCreated{}, err
	}
	if input.OrgJoin {
		return LinkCreated{}, Fail(gen.ErrBadRequest)
	}
	scope, err := NormalizeLinkScope(input.Scope)
	if err != nil {
		return LinkCreated{}, err
	}
	nowMs := l.c.NowMs()
	if input.ExpiresAtMs <= nowMs || input.ExpiresAtMs > nowMs+LinkMaxTTL.Milliseconds() {
		return LinkCreated{}, Fail(gen.ErrBadRequest)
	}
	linkID := RandomHex(8)
	secret := RandomToken(32)
	settings, err := l.c.Ports.Tenants.Get(ctx, l.c.TenantID)
	if err != nil {
		return LinkCreated{}, err
	}
	if !settings.AllowGuests {
		return LinkCreated{}, Fail(gen.ErrForbidden)
	}
	source, err := l.sources.liveSource(ctx, input.SourceID)
	if err != nil {
		return LinkCreated{}, err
	}
	if source == nil {
		return LinkCreated{}, Fail(gen.ErrSourceAccessDenied)
	}
	err = l.c.Ports.Links.Insert(ctx, l.c.TenantID, store.LinkRecord{
		LinkID: linkID, SourceID: input.SourceID, InvitationID: input.InvitationID, Label: input.Label,
		Role: input.Role, SecretHash: SecretHash(SecretLink, secret), MaxUses: input.MaxUses, Uses: 0,
		CreatedAtMs: nowMs, ExpiresAtMs: input.ExpiresAtMs, Scope: scope,
	})
	if err != nil {
		return LinkCreated{}, err
	}
	return LinkCreated{LinkID: linkID, URL: LinkURL(settings.Issuer, linkID), Secret: secret, ExpiresAtMs: input.ExpiresAtMs}, nil
}

// Get 是链接的落地页信息（匿名可读）；不在、已撤销或源已撤销答 link_invalid。
func (l *Links) Get(ctx context.Context, linkID string) (LinkInfo, error) {
	link, err := l.c.Ports.Links.Get(ctx, l.c.TenantID, linkID)
	if err != nil {
		return LinkInfo{}, err
	}
	if link == nil || link.RevokedAtMs != nil {
		return LinkInfo{}, Fail(gen.ErrLinkInvalid)
	}
	source, err := l.sources.liveSource(ctx, link.SourceID)
	if err != nil {
		return LinkInfo{}, err
	}
	if source == nil {
		return LinkInfo{}, Fail(gen.ErrLinkInvalid)
	}
	return LinkInfo{
		Kind: "source_invite", Label: link.Label, Role: link.Role, SourceName: source.Name,
		Scope: copyScope(link.Scope), ExpiresAtMs: link.ExpiresAtMs,
		Exhausted: link.MaxUses != nil && link.Uses >= *link.MaxUses, RequiresAccount: false,
	}, nil
}

// Accept 接受链接，建访客设备，签断言与中继令牌。
func (l *Links) Accept(ctx context.Context, input AcceptInput, ip string) (LinkAccepted, error) {
	if err := l.c.hit(ctx, LimitLinkAcceptIP, ip); err != nil {
		return LinkAccepted{}, err
	}
	if err := l.c.Challenge.Check(ctx, ScopeLinksAccept, input.Challenge, ip); err != nil {
		return LinkAccepted{}, err
	}
	nowMs := l.c.NowMs()
	device := DeviceInput{Platform: "browser", Name: "guest"}
	if input.Device != nil {
		device = *input.Device
	}
	found, err := l.c.Ports.Links.Get(ctx, l.c.TenantID, input.LinkID)
	if err != nil {
		return LinkAccepted{}, err
	}
	var source *store.SourceRecord
	if found != nil {
		if source, err = l.sources.liveSource(ctx, found.SourceID); err != nil {
			return LinkAccepted{}, err
		}
	}
	settings, err := l.c.Ports.Tenants.Get(ctx, l.c.TenantID)
	if err != nil {
		return LinkAccepted{}, err
	}
	if found == nil || found.RevokedAtMs != nil || source == nil || !settings.AllowGuests {
		return LinkAccepted{}, Fail(gen.ErrLinkInvalid)
	}
	if input.Secret == "" || !SameHex(SecretHash(SecretLink, input.Secret), found.SecretHash) {
		return LinkAccepted{}, Fail(gen.ErrLinkSecretInvalid)
	}
	// 次数与期限在原子的读-改-写里再核一遍，并发的 accept 不会超额。
	var link store.LinkRecord
	err = l.c.Ports.Links.Update(ctx, l.c.TenantID, input.LinkID, func(draft *store.LinkRecord) error {
		switch {
		case draft == nil || draft.RevokedAtMs != nil:
			return Fail(gen.ErrLinkInvalid)
		case draft.ExpiresAtMs <= nowMs:
			return Fail(gen.ErrLinkExpired)
		case draft.MaxUses != nil && draft.Uses >= *draft.MaxUses:
			return Fail(gen.ErrLinkExhausted)
		}
		draft.Uses++
		link = *draft
		return nil
	})
	if err != nil {
		return LinkAccepted{}, err
	}
	created, refresh, err := CreateDevice(ctx, l.c, GuestPrefix(link.LinkID)+RandomHex(4), device, nowMs, min(RefreshTTL, LinkMaxTTL))
	if err != nil {
		return LinkAccepted{}, err
	}
	granted, err := l.sources.Grant(ctx, *source, created, GrantExtra{
		Name: link.Label, Link: &gen.AssertionClaimsLink{LinkID: link.LinkID, InvitationID: link.InvitationID}, Scope: link.Scope,
	})
	if err != nil {
		return LinkAccepted{}, err
	}
	guest := true
	l.c.Emit(LinkUsed{Type: "linkUsed", LinkID: link.LinkID, SourceID: source.SourceID, By: LinkUsedBy{Guest: &guest}, AtMs: nowMs}, nil)
	public, err := base64.RawURLEncoding.DecodeString(source.PublicJWK.X)
	if err != nil {
		return LinkAccepted{}, err
	}
	session, err := SessionFor(ctx, l.c, created, refresh)
	if err != nil {
		return LinkAccepted{}, err
	}
	return LinkAccepted{
		SourceID: source.SourceID, RelayOrigin: granted.RelayOrigin, RelayBaseURL: granted.RelayBaseURL,
		Assertion: granted.Assertion, RelayToken: granted.RelayToken,
		// cloud-api §20：源登记的公钥就是它端到端通道的身份；只是提示，链接片段第三段（源自己给的）
		// 才是访客钉扎的依据。
		IdentityFp:   IdentityFingerprint(public),
		GuestSession: session,
	}, nil
}

// List 是全部链接（账号本人），可按源过滤。
func (l *Links) List(ctx context.Context, caller Caller, sourceID string) ([]LinkSummary, error) {
	if err := l.c.access().CanManage(ctx, caller, ManageTarget{SourceID: sourceID}, ManageLinkList); err != nil {
		return nil, err
	}
	return ListLinks(ctx, l.c, sourceID)
}

// Update 只改链接的备注。
func (l *Links) Update(ctx context.Context, caller Caller, linkID, label string) (LinkSummary, error) {
	if err := l.c.access().CanManage(ctx, caller, ManageTarget{LinkID: linkID}, ManageLinkUpdate); err != nil {
		return LinkSummary{}, err
	}
	label = trimJS(label)
	var link store.LinkRecord
	err := l.c.Ports.Links.Update(ctx, l.c.TenantID, linkID, func(draft *store.LinkRecord) error {
		if draft == nil {
			return Fail(gen.ErrNotFound)
		}
		draft.Label = label
		link = *draft
		return nil
	})
	if err != nil {
		return LinkSummary{}, err
	}
	settings, err := l.c.Ports.Tenants.Get(ctx, l.c.TenantID)
	if err != nil {
		return LinkSummary{}, err
	}
	return toLinkSummary(settings.Issuer, link), nil
}

// Revoke 撤销链接与它名下的访客设备，断开访客的 WS，通知访客。
func (l *Links) Revoke(ctx context.Context, caller Caller, linkID string) error {
	if err := l.c.access().CanManage(ctx, caller, ManageTarget{LinkID: linkID}, ManageLinkRevoke); err != nil {
		return err
	}
	sourceID, ok, err := RevokeLink(ctx, l.c, linkID, l.c.NowMs())
	if err != nil {
		return err
	}
	if !ok {
		return Fail(gen.ErrNotFound)
	}
	if l.c.Relay != nil {
		l.c.Relay.Disconnect(ConnectionFilter{SubjectPrefix: GuestPrefix(linkID)})
	}
	l.c.Emit(NewAccessRevoked(sourceID), &Audience{LinkID: linkID})
	return nil
}

// trimJS 去掉首尾空白（JavaScript 的 trim 把 BOM 与 Unicode 空白都算上）。
func trimJS(s string) string { return strings.TrimFunc(s, isJSSpace) }

func isJSSpace(r rune) bool {
	switch {
	case r >= '\t' && r <= '\r', r == ' ', r == '\u00a0', r == '\ufeff', r == '\u1680',
		r >= '\u2000' && r <= '\u200a', r == '\u2028', r == '\u2029', r == '\u202f', r == '\u205f', r == '\u3000':
		return true
	}
	return false
}

var fingerprintEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// IdentityFingerprint 是身份公钥指纹：base32(SHA-256(pub))，小写、无填充，52 个字符（§4.9）。
func IdentityFingerprint(public []byte) string {
	sum := sha256.Sum256(public)
	return fingerprintEncoding.EncodeToString(sum[:])
}
