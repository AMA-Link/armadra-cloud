package core

import (
	"context"
	"strings"
)

// 撤销靠 jti 的形状即时生效、重启后仍然有效：
//
//   - 隧道令牌 t.<sourceId>.<随机>：源被撤销或不在登记里 → 撤销；
//   - 中继令牌 r.<deviceId>.<sourceId>.<随机>：设备被撤销 / 过期、源被撤销，或访客的链接被撤销 → 撤销；
//   - 访问令牌 a.<deviceId>.<随机>：同设备（校验在 sessions.go）。

// IsRevokedJTI 按 jti 的形状判断令牌是否已撤销。
func IsRevokedJTI(ctx context.Context, c *Context, jti string, nowMs int64) (bool, error) {
	parts := strings.Split(jti, ".")
	kind := parts[0]
	id, sourceID := "", ""
	if len(parts) > 1 {
		id = parts[1]
	}
	if len(parts) > 2 {
		sourceID = parts[2]
	}
	switch kind {
	case "t":
		if id == "" {
			return true, nil
		}
		source, err := c.Ports.Sources.Get(ctx, c.TenantID, id)
		if err != nil {
			return false, err
		}
		return source == nil || source.RevokedAtMs != nil, nil
	case "r", "a":
		if id == "" {
			return true, nil
		}
		device, err := c.Ports.Devices.Get(ctx, c.TenantID, id)
		if err != nil {
			return false, err
		}
		if device == nil || !IsLiveDevice(*device, nowMs) {
			return true, nil
		}
		if kind == "r" {
			if sourceID == "" {
				return true, nil
			}
			source, err := c.Ports.Sources.Get(ctx, c.TenantID, sourceID)
			if err != nil {
				return false, err
			}
			if source == nil || source.RevokedAtMs != nil {
				return true, nil
			}
		}
		linkID, guest := GuestLinkID(device.Subject)
		if !guest {
			return false, nil
		}
		link, err := c.Ports.Links.Get(ctx, c.TenantID, linkID)
		if err != nil {
			return false, err
		}
		return link == nil || link.RevokedAtMs != nil, nil
	}
	return false, nil
}
