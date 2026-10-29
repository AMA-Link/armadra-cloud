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
	// Access 答调用方能否访问这一台主机（Get / Assertion 用）；看不到答 ok=false，不报错。
	// 不能用 Visible 的第一页代替：SaaS 的可见集合可能大于一页。
	Access(ctx context.Context, caller Caller, sourceID string) (visible VisibleSource, ok bool, err error)
	// CanManage 判定管理权；没有答 forbidden。target 只填 LinkID 的动作（links.update / revoke）由实现
	// 自己从链接解析出所属的源再判；target 为空只可能来自不带过滤的 ManageLinkList，SaaS 实现必须拒绝
	// 或自行收窄到调用方有管理权的主机，不得放行为「全租户」。
	CanManage(ctx context.Context, caller Caller, target ManageTarget, action ManageAction) error
	// Watch 解析调用方的事件可见性，连接建立时调用一次；库出错时返回错误。
	Watch(ctx context.Context, caller Caller) (Watcher, error)
}

// GuestHosts 答链接访客此刻仍站得住时链接指向的主机；站不住答带错误码的错误。SaaS 的访问策略实现它
// （访客的刷新与 me.sources 用），personal 不用。
type GuestHosts interface {
	GuestHost(ctx context.Context, caller Caller) (store.SourceRecord, error)
}

// ManageTarget 是管理动作的对象：针对主机填 SourceID，针对链接填 LinkID（links.update / revoke），
// 不带过滤的 links.list 两者皆空。
type ManageTarget struct {
	SourceID string
	LinkID   string
}

// Watcher 是一个 me-stream 连接的事件可见性。三个方法都不做 I/O，也不阻塞。
type Watcher interface {
	// Sees 答这个连接能否看到该主机的事件。
	Sees(sourceID string) bool
	// Refresh 在每个事件分发前收到它，让实现据此更新可见集合（主机授予、撤销等）。
	Refresh(event Event)
	// Close 在连接关闭时调用，释放实现持有的订阅。
	Close()
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

// Access：在 Visible 的结果里找这一台。
func (p personalPolicy) Access(ctx context.Context, caller Caller, sourceID string) (VisibleSource, bool, error) {
	page, _, err := p.Visible(ctx, caller, SourceFilter{})
	if err != nil {
		return VisibleSource{}, false, err
	}
	for _, entry := range page {
		if entry.Source.SourceID == sourceID {
			return entry, true, nil
		}
	}
	return VisibleSource{}, false, nil
}

// CanManage：只有账号本人能管理。
func (personalPolicy) CanManage(_ context.Context, caller Caller, _ ManageTarget, _ ManageAction) error {
	return RequireOwner(caller)
}

// personalWatcher：owner 看全部；访客只看链接指向的那个源（连接建立时解析一次，撤销由 accessRevoked 事件负责）。
type personalWatcher struct {
	all        bool
	linkSource string
}

func (w personalWatcher) Sees(sourceID string) bool {
	return w.all || (w.linkSource != "" && w.linkSource == sourceID)
}
func (personalWatcher) Refresh(Event) {}
func (personalWatcher) Close()        {}

// Watch 解析可见性。
func (p personalPolicy) Watch(ctx context.Context, caller Caller) (Watcher, error) {
	if caller.Role == RoleOwner {
		return personalWatcher{all: true}, nil
	}
	w := personalWatcher{}
	if caller.LinkID != "" {
		link, err := p.c.Ports.Links.Get(ctx, p.c.TenantID, caller.LinkID)
		if err != nil {
			return nil, err
		}
		if link != nil {
			w.linkSource = link.SourceID
		}
	}
	return w, nil
}
