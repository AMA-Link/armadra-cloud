package saas

import "strings"

// CredentialOAuth 是 account_credentials.kind 里第三方登录身份的取值。
const CredentialOAuth = "oauth"

// OAuthIdentity 是账号绑定的一个第三方登录身份（account_credentials，kind = oauth，迁移 0011）：
// external_id = "<提供方>:<主体>"，public_json = { provider, email, login / name }，不存任何令牌。
type OAuthIdentity struct {
	CredentialID string
	AccountID    string
	// Provider 是提供方（github / google），Subject 是提供方内不变的主体（GitHub 的数字 id、Google 的 sub）。
	Provider string
	Subject  string
	// Email 是绑定时提供方给的已验证邮箱；Login 是 GitHub 的登录名，Name 是显示名，都只是展示用。
	Email       string
	Login       string
	Name        string
	CreatedAtMs int64
}

// OAuthExternalID 是 account_credentials.external_id：<提供方>:<主体>。
func OAuthExternalID(provider, subject string) string { return provider + ":" + subject }

// SplitOAuthExternalID 把 external_id 拆成提供方与主体；不合形状答 false。
func SplitOAuthExternalID(externalID string) (provider, subject string, ok bool) {
	provider, subject, ok = strings.Cut(externalID, ":")
	return provider, subject, ok && provider != "" && subject != ""
}
