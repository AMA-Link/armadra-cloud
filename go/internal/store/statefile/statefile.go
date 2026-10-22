// Package statefile 只读导入个人中转的 state.json（schema v1，apps/relay/src/personal/state.ts）。
//
// Go 版不再写状态文件：serve 遇到 state.json 就拒绝，提示用 personal migrate --to sqlite；
// migrate 用这里把文件读成 store.TenantSnapshot，再交给 SQLite 或 PostgreSQL 的 ImportTenant。
// 校验与 TypeScript 一侧的 zod schema 一一对应：解析失败、版本未知、字段不符一律拒绝，绝不重建；
// 多出来的未知键被丢掉（与 zod 默认一致）。错误信息不带文件内容。
package statefile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/AMA-Link/armadra-cloud/go/internal/identity"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/schema"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

// 文件名、版本与服务端名称长度上限（协议包 PLATFORM_NAME_MAX）。
const (
	FileName   = "state.json"
	Version    = 1
	nameMaxLen = 128
)

// Code 是状态文件的问题种类。
type Code string

// 状态文件的问题。
const (
	// NotInitialized：文件不存在。
	NotInitialized Code = "not_initialized"
	// Invalid：解析失败、版本未知或字段不符。
	Invalid Code = "invalid"
)

// Error 是状态文件的问题：CLI 打印 Msg 后退出；不带文件内容。
type Error struct {
	Code Code
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

// Path 是数据目录里状态文件的位置。
func Path(dataDir string) string { return filepath.Join(dataDir, FileName) }

// State 是 state.json 的内容（schema v1）。
type State struct {
	Version            int                      `json:"version"`
	Revision           int64                    `json:"revision"`
	Issuer             string                   `json:"issuer"`
	Account            Account                  `json:"account"`
	SigningKeys        []store.SigningKeyRecord `json:"signingKeys"`
	Sources            []store.SourceRecord     `json:"sources"`
	RegistrationTokens []RegistrationToken      `json:"registrationTokens"`
	Devices            []store.DeviceRecord     `json:"devices"`
	Links              []store.LinkRecord       `json:"links"`
	Settings           Settings                 `json:"settings"`
}

// Account 是唯一的账号；导入时 accountId 取 acct:<账号名>。
type Account struct {
	Name            string                  `json:"name"`
	Password        identity.PasswordRecord `json:"password"`
	CreatedAtMs     int64                   `json:"createdAtMs"`
	UpdatedAtMs     int64                   `json:"updatedAtMs"`
	Failures        int64                   `json:"failures"`
	LockedUntilMs   int64                   `json:"lockedUntilMs"`
	LastFailureAtMs int64                   `json:"lastFailureAtMs"`
}

// RegistrationToken 是注册令牌（只存哈希）；签发人导入时取账号。
type RegistrationToken struct {
	TokenHash   string `json:"tokenHash"`
	ExpiresAtMs int64  `json:"expiresAtMs"`
}

// RateLimits 是边缘限流额度（次 / 分钟），都可省略。
type RateLimits struct {
	Anonymous *int64 `json:"anonymous,omitempty"`
	Preflight *int64 `json:"preflight,omitempty"`
	Token     *int64 `json:"token,omitempty"`
	Account   *int64 `json:"account,omitempty"`
}

// TrustedProxy 是受信代理。
type TrustedProxy struct {
	Proxies []string `json:"proxies"`
	Prefer  *string  `json:"prefer,omitempty"`
}

// Settings 是 state.json 的 settings 段。
type Settings struct {
	AllowGuests    bool          `json:"allowGuests"`
	TrustedOrigins []string      `json:"trustedOrigins"`
	RateLimits     *RateLimits   `json:"rateLimits,omitempty"`
	DisplayName    *string       `json:"displayName,omitempty"`
	TrustedProxy   *TrustedProxy `json:"trustedProxy,omitempty"`
}

func invalid(msg string) *Error { return &Error{Code: Invalid, Msg: msg} }

// Parse 把 state.json 的文本解成 State。
func Parse(data []byte) (State, error) {
	var raw any
	if !utf8.Valid(data) || json.Unmarshal(data, &raw) != nil {
		return State{}, invalid("状态文件不是合法的 JSON，拒绝启动")
	}
	object, _ := raw.(map[string]any)
	if version, _ := object["version"].(float64); version != Version {
		return State{}, invalid(fmt.Sprintf("状态文件版本不认识：%s，拒绝启动", describeVersion(object["version"])))
	}
	var state State
	if err := schema.Decode(stateSchema, data, &state); err != nil {
		return State{}, invalid("状态文件字段不符，拒绝启动")
	}
	if name := state.Settings.DisplayName; name != nil {
		// zod 的 .trim().min(1).max(N)：先去空白再量长度，存的是去过空白的。
		trimmed := strings.TrimSpace(*name)
		if n := utf16Len(trimmed); n < 1 || n > nameMaxLen {
			return State{}, invalid("状态文件字段不符，拒绝启动")
		}
		state.Settings.DisplayName = &trimmed
	}
	if state.Settings.TrustedProxy != nil {
		for _, proxy := range state.Settings.TrustedProxy.Proxies {
			if n := utf16Len(proxy); n < 1 || n > 64 {
				return State{}, invalid("状态文件字段不符，拒绝启动")
			}
		}
	}
	if len(state.SigningKeys) == 0 {
		return State{}, invalid("状态文件字段不符，拒绝启动")
	}
	normalize(&state)
	return state, nil
}

func describeVersion(v any) string {
	if v == nil {
		return "undefined"
	}
	if f, ok := v.(float64); ok {
		return fmt.Sprintf("%v", f)
	}
	return fmt.Sprintf("%v", v)
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// normalize：空数组读成 nil 的地方补成空切片，导入后序列化仍是 []。
func normalize(state *State) {
	state.Settings.TrustedOrigins = store.NonNil(state.Settings.TrustedOrigins)
	for i := range state.Sources {
		state.Sources[i].Capabilities = store.NonNil(state.Sources[i].Capabilities)
		state.Sources[i].TrustedOrigins = store.NonNil(state.Sources[i].TrustedOrigins)
	}
}

// Read 读 dataDir 里的 state.json；不存在答 NotInitialized。
func Read(dataDir string) (State, error) {
	data, err := os.ReadFile(Path(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		return State{}, &Error{Code: NotInitialized, Msg: "还没有初始化：先运行 armadra-relay personal init"}
	}
	if err != nil {
		return State{}, err
	}
	return Parse(data)
}

// Snapshot 把状态文件换成一个租户的快照：账号 id 与注册令牌的签发人都是 acct:<账号名>，
// 租户修订号照文件。与 apps/relay/src/personal/snapshot.ts 的 stateToSnapshot 逐字段对应。
func (s State) Snapshot() (store.TenantSnapshot, error) {
	accountID := store.OwnerSubject(s.Account.Name)
	tenant := store.Tenant{
		TenantSettings: store.TenantSettings{
			Issuer:         s.Issuer,
			AllowGuests:    s.Settings.AllowGuests,
			TrustedOrigins: store.NonNil(s.Settings.TrustedOrigins),
		},
		DisplayName: s.Settings.DisplayName,
		Revision:    s.Revision,
	}
	if s.Settings.RateLimits != nil {
		tenant.RateLimits = store.Marshal(s.Settings.RateLimits)
	}
	if s.Settings.TrustedProxy != nil {
		tenant.TrustedProxy = store.Marshal(s.Settings.TrustedProxy)
	}
	snapshot := store.TenantSnapshot{
		Tenant: tenant,
		Accounts: []store.AccountRecord{{
			AccountID:       accountID,
			Name:            s.Account.Name,
			Password:        s.Account.Password,
			CreatedAtMs:     s.Account.CreatedAtMs,
			UpdatedAtMs:     s.Account.UpdatedAtMs,
			Failures:        s.Account.Failures,
			LockedUntilMs:   s.Account.LockedUntilMs,
			LastFailureAtMs: s.Account.LastFailureAtMs,
		}},
		SigningKeys:   s.SigningKeys,
		Sources:       s.Sources,
		Registrations: make([]store.RegistrationToken, 0, len(s.RegistrationTokens)),
		Devices:       s.Devices,
		Links:         s.Links,
	}
	for _, token := range s.RegistrationTokens {
		snapshot.Registrations = append(snapshot.Registrations,
			store.RegistrationToken{TokenHash: token.TokenHash, ExpiresAtMs: token.ExpiresAtMs, AccountID: accountID})
	}
	return snapshot, nil
}

// ReadSnapshot 读并换成快照（migrate --to sqlite 用）。
func ReadSnapshot(dataDir string) (store.TenantSnapshot, error) {
	state, err := Read(dataDir)
	if err != nil {
		return store.TenantSnapshot{}, err
	}
	return state.Snapshot()
}
