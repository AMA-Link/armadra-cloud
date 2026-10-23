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

// Capabilities 是 SaaS M1 报的能力（设计 §5.3）。配了挑战时另加 challenge.turnstile。
// 不报 auth.passkey、auth.totp、auth.oauth、groups、access-requests、api-tokens、links.team-join、
// me.traffic、sources.hosted、orgs。
var Capabilities = []string{
	"auth.password",
	"auth.password-change",
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
	out.Capabilities = append([]string{}, Capabilities...)
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
