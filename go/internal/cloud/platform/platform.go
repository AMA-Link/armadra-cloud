// Package platform 是 platform.info 与 platform.jwks（设计 §5.3）：服务端的自我介绍与签名公钥集。
package platform

import (
	"context"
	"unicode/utf16"

	"github.com/AMA-Link/armadra-cloud/go/internal/api"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// fixedCapabilities 是不随配置变的能力（设计 §5.3），接在按配置组装的认证能力之后。
// 不报 auth.passkey、auth.totp、groups、access-requests、api-tokens、links.team-join、
// me.traffic、sources.hosted、orgs。
var fixedCapabilities = []string{
	"auth.device-code",
	"links.source-invite",
	"links.update",
	"links.scope",
	"me.stream",
	"teams",
	"invitations",
	"grants",
	"audit",
	"billing",
}

// Capabilities 按配置组装控制面报的能力：口令登录开着才报 auth.password 与 auth.password-change；
// 配了任一第三方登录提供方报 auth.oauth、对应的 auth.oauth.<provider>（providers 按配置顺序）与 auth.step-up-oauth。
// 配了挑战时另加 challenge.turnstile（info 里追加）。
func Capabilities(passwordAuth bool, providers []string) []string {
	out := []string{}
	if passwordAuth {
		out = append(out, "auth.password", "auth.password-change")
	}
	if len(providers) > 0 {
		out = append(out, "auth.oauth")
		for _, provider := range providers {
			out = append(out, "auth.oauth."+provider)
		}
		// 提权要用已绑定的提供方再授权一次，配了提供方就能用（auth.stepUp 的 oauth 分支）。
		out = append(out, "auth.step-up-oauth")
	}
	return append(out, fixedCapabilities...)
}

// nameMax 是服务端名称长度上限（协议包 PLATFORM_NAME_MAX，按 UTF-16 码元）。
const nameMax = 128

// Options 是装配。
type Options struct {
	Core  *core.Context
	Store saas.Store
	// Name 是服务端名称，空 = 不报。
	Name string
	// WebApp 是托管页面的来源；空 = 没有页面（报 null）。
	WebApp string
	// PasswordOff 是普通账号的口令登录已关闭（配置 PASSWORD_AUTH=off）：不报 auth.password、auth.password-change。
	PasswordOff bool
	// OAuthProviders 是配了的第三方登录提供方（github、google），决定 auth.oauth 与 auth.oauth.<provider>。
	OAuthProviders []string
}

// Handlers 是 platform.info 与 platform.jwks 的处理器。
func Handlers(options Options) map[string]api.Handler {
	return map[string]api.Handler{
		"platform.info": func(ctx context.Context, _ *api.Call) (any, error) { return info(ctx, options) },
		"platform.jwks": func(ctx context.Context, _ *api.Call) (any, error) { return options.Core.Signer.JWKS(ctx) },
	}
}

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
	WebApp            *string                 `json:"webApp"`
	RegistrationsOpen bool                    `json:"registrationsOpen"`
	Challenge         *core.PlatformChallenge `json:"challenge,omitempty"`
}

func info(ctx context.Context, options Options) (any, error) {
	c := options.Core
	issuer, err := c.Signer.Issuer(ctx)
	if err != nil {
		return nil, err
	}
	incident, err := options.Store.Read().Incident(ctx, c.TenantID)
	if err != nil {
		return nil, err
	}
	out := platformInfo{Mode: "saas", Name: truncateUTF16(options.Name, nameMax), Issuer: issuer, RegistrationsOpen: !incident.RegistrationsPaused}
	out.Protocol.Major, out.Protocol.Minor = gen.ProtocolMajor, gen.ProtocolMinor
	// 每个源地址是 <sourceId>.src.<域>：子域寻址。
	out.Relay.Addressing = "subdomain"
	out.Capabilities = Capabilities(!options.PasswordOff, options.OAuthProviders)
	if options.WebApp != "" {
		out.WebApp = &options.WebApp
	}
	if c.Challenge != nil {
		out.Capabilities = append(out.Capabilities, "challenge.turnstile")
		challenge := c.Challenge.Info()
		out.Challenge = &challenge
	}
	return out, nil
}

// truncateUTF16 按 UTF-16 码元截断，不把一个码点劈成两半。
func truncateUTF16(s string, limit int) string {
	units := 0
	for index, r := range s {
		width := len(utf16.Encode([]rune{r}))
		if units+width > limit {
			return s[:index]
		}
		units += width
	}
	return s
}
