package accounts

import (
	"context"
	"strings"

	"github.com/AMA-Link/armadra-cloud/go/internal/api"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

type teamOut struct {
	TeamID string `json:"teamId"`
	Slug   string `json:"slug"`
	Name   string `json:"name"`
	Role   string `json:"role"`
}

// meOut 是 me.get / me.update 的答复（字段顺序同协议包的 meSchema）。
type meOut struct {
	AccountID     string    `json:"accountId"`
	Email         *string   `json:"email"`
	DisplayName   string    `json:"displayName"`
	EmailVerified bool      `json:"emailVerified"`
	Orgs          []any     `json:"orgs"`
	Teams         []teamOut `json:"teams"`
	MFAEnabled    bool      `json:"mfaEnabled"`
	Scopes        []string  `json:"scopes"`
	CSRFToken     string    `json:"csrfToken,omitempty"`
}

func (s *Service) meView(ctx context.Context, caller core.Caller, account *saas.Account) (meOut, error) {
	memberships, err := s.store.Read().Memberships(ctx, s.tenant(), account.AccountID)
	if err != nil {
		return meOut{}, err
	}
	teams := make([]teamOut, 0, len(memberships))
	for _, m := range memberships {
		teams = append(teams, teamOut{TeamID: m.TeamID, Slug: m.Slug, Name: m.Name, Role: string(m.Role)})
	}
	mfa, err := s.store.Read().HasCredential(ctx, s.tenant(), account.AccountID, "totp")
	if err != nil {
		return meOut{}, err
	}
	email := account.Email
	out := meOut{
		AccountID: account.AccountID, Email: &email, DisplayName: account.DisplayName,
		EmailVerified: account.EmailVerifiedAtMs != nil, Orgs: []any{}, Teams: teams, MFAEnabled: mfa,
		Scopes: append([]string{}, account.Scopes...),
	}
	if caller.Transport == core.TransportCookie {
		out.CSRFToken = s.gate.CSRFToken(caller.DeviceID)
	}
	return out, nil
}

func (s *Service) meGet(ctx context.Context, call *api.Call) (any, error) {
	caller, err := call.Caller(ctx)
	if err != nil {
		return nil, err
	}
	account, err := s.owner(ctx, caller)
	if err != nil {
		return nil, err
	}
	return s.meView(ctx, caller, account)
}

func (s *Service) meUpdate(ctx context.Context, call *api.Call) (any, error) {
	caller, err := call.Caller(ctx)
	if err != nil {
		return nil, err
	}
	var in struct {
		DisplayName *string `json:"displayName"`
	}
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	account, err := s.owner(ctx, caller)
	if err != nil {
		return nil, err
	}
	if in.DisplayName != nil {
		name := strings.TrimSpace(*in.DisplayName)
		if name == "" {
			return nil, core.Fail(gen.ErrBadRequest)
		}
		nowMs := s.nowMs()
		if err := s.store.Tx(ctx, func(tx saas.Tx) error {
			return tx.Accounts().SetDisplayName(ctx, s.tenant(), account.AccountID, name, nowMs)
		}); err != nil {
			return nil, err
		}
		account.DisplayName, account.UpdatedAtMs = name, nowMs
	}
	return s.meView(ctx, caller, account)
}

type deviceSummary struct {
	DeviceID     string `json:"deviceId"`
	Platform     string `json:"platform"`
	Name         string `json:"name"`
	LastSeenAtMs *int64 `json:"lastSeenAtMs"`
	Current      bool   `json:"current"`
}

func (s *Service) meDevices(ctx context.Context, call *api.Call) (any, error) {
	caller, err := call.Caller(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := s.owner(ctx, caller); err != nil {
		return nil, err
	}
	rows, err := s.c.Ports.Devices.ListBySubject(ctx, s.tenant(), caller.Subject)
	if err != nil {
		return nil, err
	}
	nowMs := s.nowMs()
	out := make([]deviceSummary, 0, len(rows))
	for _, row := range rows {
		// 运营会话属于 admin.*，不出现在普通设备列表里。
		if core.IsLiveDevice(row, nowMs) && core.ScopeOf(row) == core.SessionScopeSession {
			out = append(out, deviceSummary{
				DeviceID: row.DeviceID, Platform: row.Platform, Name: row.Name,
				LastSeenAtMs: row.LastSeenAtMs, Current: row.DeviceID == caller.DeviceID,
			})
		}
	}
	return map[string]any{"devices": out}, nil
}

func (s *Service) meDeviceRevoke(ctx context.Context, call *api.Call) (any, error) {
	caller, err := call.Caller(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := s.owner(ctx, caller); err != nil {
		return nil, err
	}
	deviceID, _ := call.Input["deviceId"].(string)
	device, err := s.c.Ports.Devices.Get(ctx, s.tenant(), deviceID)
	if err != nil {
		return nil, err
	}
	if device == nil || device.Subject != caller.Subject || core.ScopeOf(*device) != core.SessionScopeSession {
		return nil, core.Fail(gen.ErrNotFound)
	}
	revoked, err := core.RevokeDevice(ctx, s.c, device.DeviceID)
	if err != nil {
		return nil, err
	}
	if !revoked {
		return nil, core.Fail(gen.ErrNotFound)
	}
	// 撤销的是自己这一台：Cookie 会话把两枚 Cookie 一起清掉。
	if device.DeviceID == caller.DeviceID && caller.Transport == core.TransportCookie {
		s.gate.ClearSession(call.Header, caller.Scope)
	}
	return map[string]any{}, nil
}
