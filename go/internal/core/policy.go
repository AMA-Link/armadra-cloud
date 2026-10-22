package core

import (
	"context"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

// 访问策略（SaaS 实现方案 §4.2）：谁能看到、管理、访问哪台主机。core 不知道团队；personal 用
// personalPolicy（账号本人看到全部，访客只看链接指向的源），SaaS 由 internal/cloud 另行实现。

// ManageAction 是 CanManage 判定的管理动作。
type ManageAction string

// 管理动作。
const (
	ManageSourceRevoke ManageAction = "sources.revoke"
	ManageLinkCreate   ManageAction = "links.create"
	ManageLinkList     ManageAction = "links.list"
	ManageLinkUpdate   ManageAction = "links.update"
	ManageLinkRevoke   ManageAction = "links.revoke"
)

// SourceFilter 是 Visible 的过滤与分页。personal 不分页，全量返回、next 恒空，忽略 Cursor 与 Limit。
type SourceFilter struct {
	Cursor string
	Limit  int
}

// VisibleSource 是调用方看到的一台主机与来路。
type VisibleSource struct {
	Source store.SourceRecord
	// Via 是来路：owner / link（SaaS 另有 team / member / guest）。
	Via string
	// Owner 为真表示调用方是这台主机的所有者。
	Owner bool
}

// AccessPolicy 决定谁能看到、管理、访问哪台主机。
type AccessPolicy interface {
	// Visible 答调用方能看到的主机与来路，按 filter 分页；next 为空表示没有下一页。
	Visible(ctx context.Context, caller Caller, filter SourceFilter) (page []VisibleSource, next string, err error)
	// Intent 答签断言要带的意图（名字、链接、范围）；没有访问权答 source_access_denied。
	Intent(ctx context.Context, caller Caller, source store.SourceRecord) (GrantExtra, error)
	// CanManage 判定管理权；没有答 forbidden。sourceID 为空表示动作不针对某台已知主机（按链接 id 的
	// 更新与撤销、不带过滤的链接列表），由实现自行解析。
	CanManage(ctx context.Context, caller Caller, sourceID string, action ManageAction) error
	// Watch 解析调用方的事件可见性：答一个判定函数，me-stream 对每个事件调用它（不做 I/O）。
	// 连接建立时调用一次；库出错时返回错误。
	Watch(ctx context.Context, caller Caller) (sees func(sourceID string) bool, err error)
}

// personalPolicy 是个人中转的策略：原有的可见性与管理权判断原样搬进来。
type personalPolicy struct{ c *Context }

// NewPersonalPolicy 建 personal 的访问策略。
func NewPersonalPolicy(c *Context) AccessPolicy { return personalPolicy{c: c} }

// access 是生效的策略；没有装配时取 personal。
func (c *Context) access() AccessPolicy {
	if c.Policy == nil {
		return personalPolicy{c: c}
	}
	return c.Policy
}

func (p personalPolicy) liveSource(ctx context.Context, sourceID string) (*store.SourceRecord, error) {
	source, err := p.c.Ports.Sources.Get(ctx, p.c.TenantID, sourceID)
	if err != nil || source == nil || source.RevokedAtMs != nil {
		return nil, err
	}
	return source, nil
}

// Visible：owner → 全部未撤销的；访客 → 链接指向的那一个。
func (p personalPolicy) Visible(ctx context.Context, caller Caller, _ SourceFilter) ([]VisibleSource, string, error) {
	if caller.Role == RoleOwner {
		all, err := p.c.Ports.Sources.List(ctx, p.c.TenantID)
		if err != nil {
			return nil, "", err
		}
		out := make([]VisibleSource, 0, len(all))
		for _, source := range all {
			if source.RevokedAtMs == nil {
				out = append(out, VisibleSource{Source: source, Via: "owner", Owner: true})
			}
		}
		return out, "", nil
	}
	if caller.LinkID == "" {
		return nil, "", nil
	}
	link, err := p.c.Ports.Links.Get(ctx, p.c.TenantID, caller.LinkID)
	if err != nil || link == nil || link.RevokedAtMs != nil {
		return nil, "", err
	}
	source, err := p.liveSource(ctx, link.SourceID)
	if err != nil || source == nil {
		return nil, "", err
	}
	return []VisibleSource{{Source: *source, Via: "link"}}, "", nil
}

// Intent：访客带链接的名字、链接与范围；owner 带账号名。
func (p personalPolicy) Intent(ctx context.Context, caller Caller, source store.SourceRecord) (GrantExtra, error) {
	var extra GrantExtra
	if caller.Role == RoleGuest && caller.LinkID != "" {
		link, err := p.c.Ports.Links.Get(ctx, p.c.TenantID, caller.LinkID)
		if err != nil {
			return GrantExtra{}, err
		}
		if link != nil {
			extra.Name = link.Label
			extra.Link = &gen.AssertionClaimsLink{LinkID: link.LinkID, InvitationID: link.InvitationID}
			extra.Scope = link.Scope
		}
	}
	if caller.Role == RoleOwner {
		owner, err := p.c.Ports.Accounts.Get(ctx, p.c.TenantID, caller.Subject)
		if err != nil {
			return GrantExtra{}, err
		}
		if owner != nil {
			extra.Name = owner.Name
		}
	}
	return extra, nil
}

// CanManage：只有账号本人能管理。
func (personalPolicy) CanManage(_ context.Context, caller Caller, _ string, _ ManageAction) error {
	return RequireOwner(caller)
}

// Watch：owner 看全部；访客只看链接指向的那个源（连接建立时解析一次）。
func (p personalPolicy) Watch(ctx context.Context, caller Caller) (func(string) bool, error) {
	if caller.Role == RoleOwner {
		return func(string) bool { return true }, nil
	}
	linkSource := ""
	if caller.LinkID != "" {
		link, err := p.c.Ports.Links.Get(ctx, p.c.TenantID, caller.LinkID)
		if err != nil {
			return nil, err
		}
		if link != nil {
			linkSource = link.SourceID
		}
	}
	return func(sourceID string) bool { return linkSource != "" && linkSource == sourceID }, nil
}
