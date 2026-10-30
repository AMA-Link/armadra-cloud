// Package events 是 SaaS 控制面的领域事件与跨实例分发（设计 §7.1）：领域服务在事务提交后发事件，
// 事件经 Redis 频道 cloud:events 到每个实例，再按受众投递给本机的 me-stream 连接。
//
// 事件只带标识，不带内容（客户端收到后重拉对应列表）；受众由发事件的一方在提交后算好、随事件一起发出。
package events

import (
	"encoding/json"
	"fmt"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
)

// 事件的 type（协议包 meStreamEventSchema）。
const (
	TypeMemberChanged     = "memberChanged"
	TypeInvitationChanged = "invitationChanged"
	TypeGrantChanged      = "grantChanged"
	// 设备信任（契约 §31、§32）：新设备待背书、待信任请求已处理。
	TypeDeviceTrustRequested = "deviceTrustRequested"
	TypeDeviceTrustResolved  = "deviceTrustResolved"
)

// 待信任请求的处理结果（deviceTrustResolved 的 outcome）。
const (
	TrustOutcomeTrusted  = "trusted"
	TrustOutcomeRejected = "rejected"
	TrustOutcomeExpired  = "expired"
)

// 成员变化的种类。
const (
	MemberAdded       = "added"
	MemberRemoved     = "removed"
	MemberRoleChanged = "roleChanged"
)

// MemberChanged：团队成员变化；团队被删除时不带 accountId。
type MemberChanged struct {
	Type      string `json:"type"`
	TeamID    string `json:"teamId"`
	AccountID string `json:"accountId,omitempty"`
	Change    string `json:"change"`
	AtMs      int64  `json:"atMs"`
}

// InvitationChanged：邀请状态变化（pending / accepted / declined / revoked / expired）。
type InvitationChanged struct {
	Type         string `json:"type"`
	InvitationID string `json:"invitationId"`
	TeamID       string `json:"teamId,omitempty"`
	SourceID     string `json:"sourceId,omitempty"`
	State        string `json:"state"`
	AtMs         int64  `json:"atMs"`
}

// GrantChanged：主机的授予变化。
type GrantChanged struct {
	Type     string `json:"type"`
	SourceID string `json:"sourceId"`
	GrantID  string `json:"grantId,omitempty"`
	TeamID   string `json:"teamId,omitempty"`
	AtMs     int64  `json:"atMs"`
}

// DeviceTrustRequested：有新设备待背书（详情用 trust.list，事件里没有校验码）。
type DeviceTrustRequested struct {
	Type      string `json:"type"`
	RequestID string `json:"requestId"`
	SourceID  string `json:"sourceId"`
	TeamID    string `json:"teamId,omitempty"`
	AtMs      int64  `json:"atMs"`
}

// DeviceTrustResolved：待信任请求已处理（trusted / rejected / expired）。
type DeviceTrustResolved struct {
	Type      string `json:"type"`
	RequestID string `json:"requestId"`
	Outcome   string `json:"outcome"`
	AtMs      int64  `json:"atMs"`
}

// EventSource 实现 core.Event。成员、邀请与设备信任事件按账号投递（受众直接按账号算），不参与主机可见性。
func (MemberChanged) EventSource() string        { return "" }
func (InvitationChanged) EventSource() string    { return "" }
func (e GrantChanged) EventSource() string       { return e.SourceID }
func (DeviceTrustRequested) EventSource() string { return "" }
func (DeviceTrustResolved) EventSource() string  { return "" }

// NewMemberChanged 等是带正确 type 的构造函数。
func NewMemberChanged(teamID, accountID, change string, atMs int64) MemberChanged {
	return MemberChanged{Type: TypeMemberChanged, TeamID: teamID, AccountID: accountID, Change: change, AtMs: atMs}
}

// NewInvitationChanged 构造 invitationChanged。
func NewInvitationChanged(invitationID, teamID, sourceID, state string, atMs int64) InvitationChanged {
	return InvitationChanged{Type: TypeInvitationChanged, InvitationID: invitationID, TeamID: teamID, SourceID: sourceID, State: state, AtMs: atMs}
}

// NewGrantChanged 构造 grantChanged。
func NewGrantChanged(sourceID, grantID, teamID string, atMs int64) GrantChanged {
	return GrantChanged{Type: TypeGrantChanged, SourceID: sourceID, GrantID: grantID, TeamID: teamID, AtMs: atMs}
}

// NewDeviceTrustRequested 构造 deviceTrustRequested。
func NewDeviceTrustRequested(requestID, sourceID, teamID string, atMs int64) DeviceTrustRequested {
	return DeviceTrustRequested{Type: TypeDeviceTrustRequested, RequestID: requestID, SourceID: sourceID, TeamID: teamID, AtMs: atMs}
}

// NewDeviceTrustResolved 构造 deviceTrustResolved。
func NewDeviceTrustResolved(requestID, outcome string, atMs int64) DeviceTrustResolved {
	return DeviceTrustResolved{Type: TypeDeviceTrustResolved, RequestID: requestID, Outcome: outcome, AtMs: atMs}
}

// ---- 线上形状：{origin, event, audience} ----

type audienceWire struct {
	LinkID     string   `json:"linkId,omitempty"`
	AccountIDs []string `json:"accountIds,omitempty"`
}

type envelope struct {
	Origin   string          `json:"origin"`
	Event    json.RawMessage `json:"event"`
	Audience *audienceWire   `json:"audience,omitempty"`
}

// decoders 按 type 解事件。新的领域事件（信任请求、计划变更等）在这里登记；不认识的 type 被忽略（别的版本发的）。
var decoders = map[string]func([]byte) (core.Event, error){
	"sourceRevoked": decodeAs[core.SourceRevoked],
	"accessRevoked": decodeAs[core.AccessRevoked],
	"linkUsed":      decodeAs[core.LinkUsed],
	"sourceOnline":  decodeAs[core.SourceOnline],
	"sourceOffline": decodeAs[core.SourceOffline],

	TypeMemberChanged:     decodeAs[MemberChanged],
	TypeInvitationChanged: decodeAs[InvitationChanged],
	TypeGrantChanged:      decodeAs[GrantChanged],

	TypeDeviceTrustRequested: decodeAs[DeviceTrustRequested],
	TypeDeviceTrustResolved:  decodeAs[DeviceTrustResolved],
}

func decodeAs[E core.Event](data []byte) (core.Event, error) {
	var e E
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, err
	}
	return e, nil
}

// Register 登记一种新的领域事件（S6 / S7 的事件在各自的包里调用）；同名覆盖。
func Register[E core.Event](eventType string) { decoders[eventType] = decodeAs[E] }

// Encode 编广播消息。
func Encode(origin string, event core.Event, audience *core.Audience) ([]byte, error) {
	body, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}
	env := envelope{Origin: origin, Event: body}
	if audience != nil {
		env.Audience = &audienceWire{LinkID: audience.LinkID, AccountIDs: audience.AccountIDs}
	}
	return json.Marshal(env)
}

// ErrUnknown 表示消息的形状或事件 type 不认识（别的版本的实例发的，忽略）。
var ErrUnknown = fmt.Errorf("不认识的事件消息")

// Decode 解广播消息。
func Decode(data []byte) (origin string, event core.Event, audience *core.Audience, err error) {
	var env envelope
	if json.Unmarshal(data, &env) != nil || env.Origin == "" || len(env.Event) == 0 {
		return "", nil, nil, ErrUnknown
	}
	var head struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(env.Event, &head) != nil {
		return "", nil, nil, ErrUnknown
	}
	decode, ok := decoders[head.Type]
	if !ok {
		return "", nil, nil, ErrUnknown
	}
	event, err = decode(env.Event)
	if err != nil {
		return "", nil, nil, ErrUnknown
	}
	if env.Audience != nil {
		audience = &core.Audience{LinkID: env.Audience.LinkID, AccountIDs: env.Audience.AccountIDs}
	}
	return env.Origin, event, audience, nil
}
