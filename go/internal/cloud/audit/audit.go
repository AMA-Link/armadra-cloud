// Package audit 写审计与账号安全日志（设计 §4.1、§6.1，迁移 0008）。只追加，只记主体、动作、对象、时间、
// jti、IP 前缀与运营理由；不记任何内容、口令与令牌。读取与保留期随 S7。
package audit

import (
	"context"
	"net/netip"
	"unicode/utf8"

	"github.com/AMA-Link/armadra-cloud/go/internal/clientip"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// 账号安全日志的动作（action 以 security. 开头；本人在 me.securityLog 里看到）。
const (
	SecurityRegister         = "security.register"
	SecurityLogin            = "security.login"
	SecurityNewDevice        = "security.newDevice"
	SecurityPasswordChanged  = "security.passwordChanged"
	SecurityPasswordReset    = "security.passwordReset"
	SecurityEmailVerified    = "security.emailVerified"
	SecurityEmailChangeAsked = "security.emailChangeRequested"
	SecurityEmailChanged     = "security.emailChanged"
	SecurityDeviceApprove    = "security.deviceApprove"
	SecurityDeviceDeny       = "security.deviceDeny"
	deviceNameMax            = 128
)

// IPPrefix 是写进日志的来源 IP 前缀：IPv4 取 /24，IPv6 取 /48；不是 IP 答空串（不记）。
func IPPrefix(ip string) string {
	normalized, ok := clientip.NormalizeIP(ip)
	if !ok {
		return ""
	}
	addr, err := netip.ParseAddr(normalized)
	if err != nil {
		return ""
	}
	bits := 48
	if addr.Is4() {
		bits = 24
	}
	return netip.PrefixFrom(addr, bits).Masked().String()
}

// truncate 把设备名截到库里的长度上限（按码点）。
func truncate(name string) string {
	if utf8.RuneCountInString(name) <= deviceNameMax {
		return name
	}
	return string([]rune(name)[:deviceNameMax])
}

// Write 在业务事务里追加一行：补时间与事件 id，设备名截到库里的长度上限，source_id、team_id 原样写入。
// IPPrefix 由调用方用 IPPrefix(ip) 给出（这里不接触完整地址）。
func Write(ctx context.Context, tx saas.Tx, tenantID string, nowMs int64, entry saas.AuditEntry) error {
	entry.AtMs, entry.EventID = nowMs, core.RandomToken(16)
	entry.DeviceName = truncate(entry.DeviceName)
	return tx.Audit().Append(ctx, tenantID, entry)
}

// Security 是一条账号安全日志的来源：谁、哪台设备、从哪里。
type Security struct {
	AccountID  string
	DeviceName string
	IP         string
}

// WriteSecurity 写一条账号安全日志（actor 是账号本人）。设备名存快照，设备改名或删除后日志里仍是当时的名字。
func WriteSecurity(ctx context.Context, tx saas.Tx, tenantID string, nowMs int64, who Security, action string) error {
	return Write(ctx, tx, tenantID, nowMs, saas.AuditEntry{
		AccountID: who.AccountID, ActorKind: saas.ActorAccount, ActorID: who.AccountID, Action: action,
		IPPrefix: IPPrefix(who.IP), DeviceName: who.DeviceName,
	})
}
