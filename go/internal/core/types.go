package core

import "github.com/AMA-Link/armadra-cloud/go/internal/store"

// 服务的输出形状：线上 JSON（camelCase）与协议包 cloud-api 的 zod schema 一一对应，
// 字段顺序与 TypeScript 一致。可缺的字段用 omitempty，可为 null 的用指针。

// AccountInfo 是会话里的账号。
type AccountInfo struct {
	AccountID     string  `json:"accountId"`
	Email         *string `json:"email"`
	DisplayName   string  `json:"displayName"`
	EmailVerified bool    `json:"emailVerified"`
}

// DeviceRef 指向一台设备。
type DeviceRef struct {
	DeviceID string `json:"deviceId"`
}

// Session 是云会话。
type Session struct {
	AccessToken       string      `json:"accessToken"`
	RefreshToken      string      `json:"refreshToken,omitempty"`
	AccessExpiresAtMs int64       `json:"accessExpiresAtMs"`
	ExpiresAtMs       int64       `json:"expiresAtMs"`
	Account           AccountInfo `json:"account"`
	Device            DeviceRef   `json:"device"`
}

// DeviceInput 是客户端报的设备信息。
type DeviceInput struct {
	Platform      string  `json:"platform"`
	Name          string  `json:"name"`
	PushPublicKey *string `json:"pushPublicKey,omitempty"`
}

// ChallengeToken 是请求里带的挑战令牌。
type ChallengeToken struct {
	Provider string `json:"provider"`
	Token    string `json:"token"`
}

// SourceSummary 是源的摘要。
type SourceSummary struct {
	SourceID     string `json:"sourceId"`
	Name         string `json:"name"`
	Kind         string `json:"kind"`
	Online       bool   `json:"online"`
	LastSeenAtMs *int64 `json:"lastSeenAtMs"`
	Owner        bool   `json:"owner"`
	Via          string `json:"via"`
	RelayOrigin  string `json:"relayOrigin"`
	CoreVersion  string `json:"coreVersion"`
}

// SourceDetail 是源的详情。
type SourceDetail struct {
	SourceSummary
	TrustedOrigins []string `json:"trustedOrigins"`
	Capabilities   []string `json:"capabilities"`
	RegisteredAtMs int64    `json:"registeredAtMs"`
}

// LinkSummary 是链接的摘要。
type LinkSummary struct {
	LinkID      string           `json:"linkId"`
	Kind        string           `json:"kind"`
	Label       string           `json:"label"`
	Role        string           `json:"role,omitempty"`
	SourceID    string           `json:"sourceId,omitempty"`
	Scope       *store.LinkScope `json:"scope,omitempty"`
	URL         string           `json:"url"`
	Uses        int64            `json:"uses"`
	MaxUses     *int64           `json:"maxUses"`
	ExpiresAtMs int64            `json:"expiresAtMs"`
	CreatedAtMs int64            `json:"createdAtMs"`
	RevokedAtMs *int64           `json:"revokedAtMs"`
}

// RegistrationTokenResult 是 sources.registrationToken 的答复。
type RegistrationTokenResult struct {
	RegistrationToken string `json:"registrationToken"`
	ExpiresAtMs       int64  `json:"expiresAtMs"`
	Issuer            string `json:"issuer"`
}

// RegisterResult 是 sources.register 的答复。
type RegisterResult struct {
	Issuer         string   `json:"issuer"`
	JWKSURL        string   `json:"jwksUrl"`
	RelayOrigins   []string `json:"relayOrigins"`
	TrustedOrigins []string `json:"trustedOrigins"`
	SourceID       string   `json:"sourceId"`
	OwnerAccountID string   `json:"ownerAccountId"`
}

// Grant 是 sources.assertion 的答复：断言 + 中继令牌。
type Grant struct {
	Assertion             string `json:"assertion"`
	AssertionExpiresAtMs  int64  `json:"assertionExpiresAtMs"`
	RelayToken            string `json:"relayToken"`
	RelayTokenExpiresAtMs int64  `json:"relayTokenExpiresAtMs"`
	RelayOrigin           string `json:"relayOrigin"`
	RelayBaseURL          string `json:"relayBaseUrl"`
	Online                bool   `json:"online"`
	// 两个 jti 不上线，SaaS 记进签发索引（撤销时按它过滤）。
	AssertionJTI  string `json:"-"`
	RelayTokenJTI string `json:"-"`
}

// TunnelNode 是隧道节点。
type TunnelNode struct {
	URL    string `json:"url"`
	Weight int    `json:"weight"`
}

// TunnelLimits 是隧道限制。
type TunnelLimits struct {
	MaxStreams    int `json:"maxStreams"`
	StreamWindow  int `json:"streamWindow"`
	TunnelWindow  int `json:"tunnelWindow"`
	MaxFrameBytes int `json:"maxFrameBytes"`
}

// RelayInfo 是 sources.heartbeatHint 的答复。
type RelayInfo struct {
	TunnelToken string       `json:"tunnelToken"`
	ExpiresAtMs int64        `json:"expiresAtMs"`
	Nodes       []TunnelNode `json:"nodes"`
	Limits      TunnelLimits `json:"limits"`
	// TunnelJTI 是隧道令牌的 jti，SaaS 记进签发索引；不进答复。
	TunnelJTI string `json:"-"`
}

// LinkCreated 是 links.create 的答复。
type LinkCreated struct {
	LinkID      string `json:"linkId"`
	URL         string `json:"url"`
	Secret      string `json:"secret"`
	ExpiresAtMs int64  `json:"expiresAtMs"`
}

// LinkInfo 是 links.get 的答复（落地页用，匿名可读）。
type LinkInfo struct {
	Kind            string           `json:"kind"`
	Label           string           `json:"label"`
	Role            string           `json:"role"`
	SourceName      string           `json:"sourceName"`
	Scope           *store.LinkScope `json:"scope,omitempty"`
	ExpiresAtMs     int64            `json:"expiresAtMs"`
	Exhausted       bool             `json:"exhausted"`
	RequiresAccount bool             `json:"requiresAccount"`
}

// LinkAccepted 是 links.accept 的答复。
type LinkAccepted struct {
	SourceID     string  `json:"sourceId"`
	RelayOrigin  string  `json:"relayOrigin"`
	RelayBaseURL string  `json:"relayBaseUrl"`
	Assertion    string  `json:"assertion"`
	RelayToken   string  `json:"relayToken"`
	IdentityFp   string  `json:"identityFp"`
	GuestSession Session `json:"guestSession"`
}
