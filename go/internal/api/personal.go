package api

import (
	"context"
	"strings"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

// personal 实现的 procedure（平台设计 §17.3 的子集表，契约 §10）：服务 → 契约输出形状。
// 没列出的 procedure 由路由器答 404 not_found。

// PersonalCapabilities 是 personal 报的能力（契约 PLATFORM_CAPABILITIES 的子集）。
var PersonalCapabilities = []string{
	"auth.password",
	"auth.password-change",
	"links.source-invite",
	"links.update",
	"links.scope",
	"me.stream",
	"me.traffic",
}

// PersonalProcedures 是 personal 实现的 procedure。
var PersonalProcedures = []string{
	"platform.info", "platform.jwks",
	"auth.login", "auth.refresh", "auth.logout", "auth.changePassword",
	"me.get", "me.sources", "me.devices", "me.deviceRevoke", "me.streamTicket", "me.traffic",
	"sources.registrationToken", "sources.register", "sources.heartbeatHint",
	"sources.list", "sources.get", "sources.revoke", "sources.assertion",
	"links.create", "links.get", "links.accept", "links.list", "links.update", "links.revoke",
}

// platformNameMax 是服务端名称长度上限（协议包 PLATFORM_NAME_MAX）。
const platformNameMax = 128

// PersonalOptions 是 personal 处理器的装配。
type PersonalOptions struct {
	Stream *core.MeStream
	// WebApp 是托管页面的地址；没有页面产物时答空串（platform.info 报 null）。
	WebApp func() string
	// Name 是服务端名称（platform.info.name，0.3.1 起）；不给或为空不报。
	Name func() string
}

// PersonalHandlers 把 personal 的 procedure 接上控制核服务，并答认证器（给 me-stream 兑换前认证用）。
func PersonalHandlers(c *core.Context, options PersonalOptions) (map[string]Handler, *core.Authenticator) {
	authenticator := core.NewAuthenticator(c)
	accounts := core.NewAccounts(c)
	sources := core.NewSources(c)
	links := core.NewLinks(c, sources)
	traffic := core.NewTraffic(c)
	empty := map[string]any{}

	// session 要认证的调用方；失败原样上抛。
	session := func(ctx context.Context, call *Call) (core.Caller, error) { return call.Caller(ctx) }

	handlers := map[string]Handler{
		"platform.info": func(ctx context.Context, _ *Call) (any, error) {
			issuer, err := c.Signer.Issuer(ctx)
			if err != nil {
				return nil, err
			}
			name := ""
			if options.Name != nil {
				name = truncateUTF16(strings.TrimSpace(options.Name()), platformNameMax)
			}
			capabilities := append([]string{}, PersonalCapabilities...)
			info := platformInfo{Mode: "personal", Name: name, Issuer: issuer}
			info.Protocol.Major, info.Protocol.Minor = gen.ProtocolMajor, gen.ProtocolMinor
			info.Relay.Addressing = "path"
			if options.WebApp != nil {
				if app := options.WebApp(); app != "" {
					info.WebApp = &app
				}
			}
			if c.Challenge != nil {
				capabilities = append(capabilities, "challenge.turnstile")
				challenge := c.Challenge.Info()
				info.Challenge = &challenge
			}
			info.Capabilities = capabilities
			return info, nil
		},
		"platform.jwks": func(ctx context.Context, _ *Call) (any, error) { return c.Signer.JWKS(ctx) },

		"auth.login": func(ctx context.Context, call *Call) (any, error) {
			var in struct {
				Account   string               `json:"account"`
				Password  string               `json:"password"`
				Device    core.DeviceInput     `json:"device"`
				Challenge *core.ChallengeToken `json:"challenge"`
			}
			if err := call.Bind(&in); err != nil {
				return nil, err
			}
			result, err := accounts.Login(ctx, core.LoginInput{
				Account: in.Account, Password: in.Password, Device: in.Device, Challenge: in.Challenge,
			}, call.IP)
			return sessionResult(result, err)
		},
		"auth.refresh": func(ctx context.Context, call *Call) (any, error) {
			var in struct {
				RefreshToken string `json:"refreshToken"`
			}
			if err := call.Bind(&in); err != nil {
				return nil, err
			}
			result, err := core.Refresh(ctx, c, in.RefreshToken)
			return sessionResult(result, err)
		},
		"auth.logout": func(ctx context.Context, call *Call) (any, error) {
			caller, err := session(ctx, call)
			if err != nil {
				return nil, err
			}
			if _, err := core.RevokeDevice(ctx, c, caller.DeviceID); err != nil {
				return nil, err
			}
			return empty, nil
		},
		"auth.changePassword": func(ctx context.Context, call *Call) (any, error) {
			caller, err := session(ctx, call)
			if err != nil {
				return nil, err
			}
			var in struct {
				Password    string               `json:"password"`
				NewPassword string               `json:"newPassword"`
				Challenge   *core.ChallengeToken `json:"challenge"`
			}
			if err := call.Bind(&in); err != nil {
				return nil, err
			}
			result, err := accounts.ChangePassword(ctx, caller, core.ChangePasswordInput{
				Password: in.Password, NewPassword: in.NewPassword, Challenge: in.Challenge,
			}, call.IP)
			return sessionResult(result, err)
		},

		"me.get": func(ctx context.Context, call *Call) (any, error) {
			who, err := session(ctx, call)
			if err != nil {
				return nil, err
			}
			displayName := "guest"
			if who.Role == core.RoleOwner {
				displayName = ""
				account, err := c.Ports.Accounts.Get(ctx, c.TenantID, who.Subject)
				if err != nil {
					return nil, err
				}
				if account != nil {
					displayName = account.Name
				}
			} else if who.LinkID != "" {
				link, err := c.Ports.Links.Get(ctx, c.TenantID, who.LinkID)
				if err != nil {
					return nil, err
				}
				if link != nil {
					displayName = link.Label
				}
			}
			return me{AccountID: who.Subject, DisplayName: displayName, Orgs: []any{}}, nil
		},
		"me.sources": func(ctx context.Context, call *Call) (any, error) {
			who, err := session(ctx, call)
			if err != nil {
				return nil, err
			}
			list, err := sources.List(ctx, who)
			return map[string]any{"sources": list}, err
		},
		"me.devices": func(ctx context.Context, call *Call) (any, error) {
			who, err := session(ctx, call)
			if err != nil {
				return nil, err
			}
			nowMs := c.NowMs()
			rows, err := c.Ports.Devices.ListBySubject(ctx, c.TenantID, who.Subject)
			if err != nil {
				return nil, err
			}
			out := make([]deviceSummary, 0, len(rows))
			for _, row := range rows {
				if core.IsLiveDevice(row, nowMs) {
					out = append(out, deviceSummary{
						DeviceID: row.DeviceID, Platform: row.Platform, Name: row.Name,
						LastSeenAtMs: row.LastSeenAtMs, Current: row.DeviceID == who.DeviceID,
					})
				}
			}
			return map[string]any{"devices": out}, nil
		},
		"me.deviceRevoke": func(ctx context.Context, call *Call) (any, error) {
			who, err := session(ctx, call)
			if err != nil {
				return nil, err
			}
			var in struct {
				DeviceID string `json:"deviceId"`
			}
			if err := call.Bind(&in); err != nil {
				return nil, err
			}
			device, err := c.Ports.Devices.Get(ctx, c.TenantID, in.DeviceID)
			if err != nil {
				return nil, err
			}
			if device == nil || device.Subject != who.Subject {
				return nil, core.Fail(gen.ErrNotFound)
			}
			revoked, err := core.RevokeDevice(ctx, c, device.DeviceID)
			if err != nil {
				return nil, err
			}
			if !revoked {
				return nil, core.Fail(gen.ErrNotFound)
			}
			return empty, nil
		},
		"me.streamTicket": func(ctx context.Context, call *Call) (any, error) {
			who, err := session(ctx, call)
			if err != nil {
				return nil, err
			}
			return options.Stream.Ticket(who), nil
		},
		"me.traffic": func(ctx context.Context, call *Call) (any, error) {
			who, err := session(ctx, call)
			if err != nil {
				return nil, err
			}
			var in struct {
				FromMs      int64  `json:"fromMs"`
				ToMs        int64  `json:"toMs"`
				SourceID    string `json:"sourceId"`
				Granularity string `json:"granularity"`
			}
			if err := call.Bind(&in); err != nil {
				return nil, err
			}
			rows, err := traffic.Summary(ctx, who, core.TrafficSummaryInput{
				FromMs: in.FromMs, ToMs: in.ToMs, SourceID: in.SourceID, Granularity: in.Granularity,
			})
			return map[string]any{"buckets": rows}, err
		},

		"sources.registrationToken": func(ctx context.Context, call *Call) (any, error) {
			who, err := session(ctx, call)
			if err != nil {
				return nil, err
			}
			return sources.RegistrationToken(ctx, who)
		},
		"sources.register": func(ctx context.Context, call *Call) (any, error) {
			var in struct {
				RegistrationToken string               `json:"registrationToken"`
				SourceID          string               `json:"sourceId"`
				PublicKey         gen.Ed25519PublicJWK `json:"publicKey"`
				Name              string               `json:"name"`
				Kind              string               `json:"kind"`
				CoreVersion       string               `json:"coreVersion"`
				Capabilities      []string             `json:"capabilities"`
				Protocol          struct {
					Major int64 `json:"major"`
					Minor int64 `json:"minor"`
				} `json:"protocol"`
			}
			if err := call.Bind(&in); err != nil {
				return nil, err
			}
			return sources.Register(ctx, core.RegisterInput{
				RegistrationToken: in.RegistrationToken, SourceID: in.SourceID, PublicKey: in.PublicKey,
				Name: in.Name, Kind: in.Kind, CoreVersion: in.CoreVersion, Capabilities: in.Capabilities,
				ProtocolMajor: in.Protocol.Major, ProtocolMinor: in.Protocol.Minor,
			}, call.IP)
		},
		"sources.heartbeatHint": func(ctx context.Context, call *Call) (any, error) {
			source, err := sources.AuthenticateSource(ctx, call.Authorization)
			if err != nil {
				return nil, err
			}
			return sources.RelayFor(ctx, source)
		},
		"sources.list": func(ctx context.Context, call *Call) (any, error) {
			who, err := session(ctx, call)
			if err != nil {
				return nil, err
			}
			list, err := sources.List(ctx, who)
			return map[string]any{"sources": list}, err
		},
		"sources.get": func(ctx context.Context, call *Call) (any, error) {
			who, err := session(ctx, call)
			if err != nil {
				return nil, err
			}
			return sources.Get(ctx, who, stringInput(call, "sourceId"))
		},
		"sources.revoke": func(ctx context.Context, call *Call) (any, error) {
			who, err := session(ctx, call)
			if err != nil {
				return nil, err
			}
			if err := sources.Revoke(ctx, who, stringInput(call, "sourceId")); err != nil {
				return nil, err
			}
			return empty, nil
		},
		"sources.assertion": func(ctx context.Context, call *Call) (any, error) {
			who, err := session(ctx, call)
			if err != nil {
				return nil, err
			}
			var in struct {
				SourceID string            `json:"sourceId"`
				Device   *core.DeviceInput `json:"device"`
			}
			if err := call.Bind(&in); err != nil {
				return nil, err
			}
			return sources.Assertion(ctx, who, in.SourceID, in.Device)
		},

		"links.create": func(ctx context.Context, call *Call) (any, error) {
			who, err := session(ctx, call)
			if err != nil {
				return nil, err
			}
			var in struct {
				Kind         string           `json:"kind"`
				SourceID     string           `json:"sourceId"`
				InvitationID string           `json:"invitationId"`
				Label        string           `json:"label"`
				Role         string           `json:"role"`
				ExpiresAtMs  int64            `json:"expiresAtMs"`
				MaxUses      *int64           `json:"maxUses"`
				Scope        *store.LinkScope `json:"scope"`
			}
			if err := call.Bind(&in); err != nil {
				return nil, err
			}
			return links.Create(ctx, who, core.CreateLinkInput{
				OrgJoin: in.Kind != "source_invite", SourceID: in.SourceID, InvitationID: in.InvitationID,
				Label: in.Label, Role: in.Role, ExpiresAtMs: in.ExpiresAtMs, MaxUses: in.MaxUses, Scope: in.Scope,
			})
		},
		"links.get": func(ctx context.Context, call *Call) (any, error) {
			return links.Get(ctx, stringInput(call, "linkId"))
		},
		"links.accept": func(ctx context.Context, call *Call) (any, error) {
			var in struct {
				LinkID    string               `json:"linkId"`
				Secret    string               `json:"secret"`
				Device    *core.DeviceInput    `json:"device"`
				Challenge *core.ChallengeToken `json:"challenge"`
			}
			if err := call.Bind(&in); err != nil {
				return nil, err
			}
			return links.Accept(ctx, core.AcceptInput{
				LinkID: in.LinkID, Secret: in.Secret, Device: in.Device, Challenge: in.Challenge,
			}, call.IP)
		},
		"links.list": func(ctx context.Context, call *Call) (any, error) {
			who, err := session(ctx, call)
			if err != nil {
				return nil, err
			}
			list, err := links.List(ctx, who, stringInput(call, "sourceId"))
			return map[string]any{"links": list}, err
		},
		"links.update": func(ctx context.Context, call *Call) (any, error) {
			who, err := session(ctx, call)
			if err != nil {
				return nil, err
			}
			return links.Update(ctx, who, stringInput(call, "linkId"), stringInput(call, "label"))
		},
		"links.revoke": func(ctx context.Context, call *Call) (any, error) {
			who, err := session(ctx, call)
			if err != nil {
				return nil, err
			}
			if err := links.Revoke(ctx, who, stringInput(call, "linkId")); err != nil {
				return nil, err
			}
			return empty, nil
		},
	}
	return handlers, authenticator
}

func stringInput(call *Call, key string) string {
	value, _ := call.Input[key].(string)
	return value
}

func sessionResult(result core.Session, err error) (any, error) {
	if err != nil {
		return nil, err
	}
	return map[string]core.Session{"session": result}, nil
}

// platformInfo 是 platform.info 的输出（字段顺序与 TypeScript 一致）。
type platformInfo struct {
	Mode     string `json:"mode"`
	Name     string `json:"name,omitempty"`
	Issuer   string `json:"issuer"`
	Protocol struct {
		Major int `json:"major"`
		Minor int `json:"minor"`
	} `json:"protocol"`
	Capabilities []string `json:"capabilities"`
	Relay        struct {
		Addressing string `json:"addressing"`
	} `json:"relay"`
	WebApp    *string                 `json:"webApp"`
	Challenge *core.PlatformChallenge `json:"challenge,omitempty"`
}

type me struct {
	AccountID     string  `json:"accountId"`
	Email         *string `json:"email"`
	DisplayName   string  `json:"displayName"`
	EmailVerified bool    `json:"emailVerified"`
	Orgs          []any   `json:"orgs"`
	MFAEnabled    bool    `json:"mfaEnabled"`
}

type deviceSummary struct {
	DeviceID     string `json:"deviceId"`
	Platform     string `json:"platform"`
	Name         string `json:"name"`
	LastSeenAtMs *int64 `json:"lastSeenAtMs"`
	Current      bool   `json:"current"`
}

// truncateUTF16 按 UTF-16 码元截断（TypeScript 的 slice），不把一个码点劈成两半。
func truncateUTF16(s string, limit int) string {
	units := 0
	for index, r := range s {
		width := 1
		if r >= 0x10000 {
			width = 2
		}
		if units+width > limit {
			return s[:index]
		}
		units += width
	}
	return s
}
