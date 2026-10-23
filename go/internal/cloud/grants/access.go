package grants

import (
	"context"

	"github.com/AMA-Link/armadra-cloud/go/internal/api"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

type accessRow struct {
	AccountID    string   `json:"accountId"`
	Email        *string  `json:"email"`
	Via          string   `json:"via"`
	LinkID       *string  `json:"linkId"`
	GrantedAtMs  int64    `json:"grantedAtMs"`
	GrantID      *string  `json:"grantId,omitempty"`
	Role         string   `json:"role"`
	WorkspaceIDs []string `json:"workspaceIds"`
	ExpiresAtMs  *int64   `json:"expiresAtMs,omitempty"`
}

// accessBatch 是访问清单一次合成的账号数。
const accessBatch = 200

// accessList 是 sources.accessList：谁能访问这台主机——所有者与每个有有效权限的账号（团队授予展开到成员），
// 合成后的角色、范围与到期；grantId 取贡献最高角色的那条授予。管理权。
func (s *Service) accessList(ctx context.Context, call *api.Call) (any, error) {
	account, err := s.me(ctx, call)
	if err != nil {
		return nil, err
	}
	sourceID, _ := call.Input["sourceId"].(string)
	h, err := s.manageRead(ctx, account.AccountID, sourceID)
	if err != nil {
		return nil, err
	}
	r := s.store.Read()
	candidates, err := r.AccessAccounts(ctx, s.tenant(), sourceID)
	if err != nil {
		return nil, err
	}
	rows := []accessRow{}
	// 候选账号按批合成有效权限（一批固定几条查询），够了 maxAccessRows 行就停。
	var effective map[string]Effective
	for i, id := range candidates {
		if len(rows) >= maxAccessRows {
			break
		}
		if i%accessBatch == 0 {
			effective, err = s.effectiveMany(ctx, r, sourceID, candidates[i:min(i+accessBatch, len(candidates))], s.nowMs())
			if err != nil {
				return nil, err
			}
		}
		eff := effective[id]
		if eff.None() {
			continue
		}
		member, err := r.Account(ctx, s.tenant(), id)
		if err != nil {
			return nil, err
		}
		row := accessRow{
			AccountID: id, Via: "link", GrantedAtMs: h.RegisteredAtMs, Role: eff.Role, WorkspaceIDs: nonNil(eff.WorkspaceIDs), ExpiresAtMs: eff.ExpiresAtMs,
		}
		if member != nil {
			email := member.Email
			row.Email = &email
		}
		if h.TeamID != "" || eff.Owner() {
			row.Via = ViaTeam
		}
		if eff.Owner() {
			row.Via = ViaOwner
		}
		best := -1
		for _, source := range eff.From {
			if source.GrantID == "" || eff.Owner() { // 所有者的行不挂授予（他本来就是 driver 整台）
				continue
			}
			if roleRank[source.Role] > best {
				best, row.GrantedAtMs = roleRank[source.Role], source.CreatedAtMs
				grantID := source.GrantID
				row.GrantID = &grantID
			}
		}
		rows = append(rows, row)
	}
	return map[string]any{"access": rows}, nil
}

// accessRevoke 是 sources.accessRevoke：撤销一个账号在这台主机上以他本人为主体（member / guest）的全部授予；
// 团队授予（团队全员）撤不掉单个人，要走 grants.revoke。没有撤到任何授予答 404。收窄了就撤他已签的令牌并踢人。管理权。
func (s *Service) accessRevoke(ctx context.Context, call *api.Call) (any, error) {
	account, err := s.me(ctx, call)
	if err != nil {
		return nil, err
	}
	sourceID, _ := call.Input["sourceId"].(string)
	target, _ := call.Input["accountId"].(string)
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		h, err := s.manage(ctx, tx, account.AccountID, sourceID)
		if err != nil {
			return err
		}
		before, err := s.snapshot(ctx, tx, sourceID, []string{target})
		if err != nil {
			return err
		}
		nowMs := s.nowMs()
		revoked, err := tx.Grants().RevokeAccount(ctx, s.tenant(), sourceID, target, account.AccountID, nowMs)
		if err != nil {
			return err
		}
		if len(revoked) == 0 {
			return core.Fail(gen.ErrNotFound)
		}
		for _, id := range revoked {
			if err := tx.Jobs().Cancel(ctx, s.tenant(), JobExpire, id); err != nil {
				return err
			}
		}
		after, err := s.snapshot(ctx, tx, sourceID, []string{target})
		if err != nil {
			return err
		}
		s.revokeAccess(tx, sourceID, narrowedAccounts(before, after), "access_revoked")
		return s.record(ctx, tx, call.IP, saas.ActorAccount, account.AccountID, h.TeamID, sourceID, ActionAccessRevoke, "account", target)
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{}, nil
}
