package hosts

import (
	"context"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/api"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/httpx"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/quota"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/relayctl"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/teams"
	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

const (
	registrationTTL = time.Duration(gen.IdentityRegistrationTokenTTLMs) * time.Millisecond
	// maxRegistrationTokens 是一个账号同时有效的注册令牌上限（核心的上限按整个租户计，SaaS 按账号计）。
	maxRegistrationTokens = core.MaxRegistrationTokens
)

// registrationToken 是 sources.registrationToken：邮箱已验证；space = team 时核成员、团队状态与设置，
// 再核主机配额（快速失败，权威检查在 register 里）；令牌带 team_id。
func (s *Service) registrationToken(ctx context.Context, call *api.Call) (any, error) {
	account, err := s.me(ctx, call)
	if err != nil {
		return nil, err
	}
	if s.verified && account.EmailVerifiedAtMs == nil {
		return nil, core.Fail(gen.ErrEmailUnverified)
	}
	space, _, err := spaceOf(call)
	if err != nil {
		return nil, err
	}
	teamID := ""
	if space.Kind == saas.SpaceTeam {
		teamID = space.TeamID
	}
	issuer, err := s.c.Signer.Issuer(ctx)
	if err != nil {
		return nil, err
	}
	nowMs := s.nowMs()
	token := core.RandomToken(32)
	expiresAtMs := nowMs + registrationTTL.Milliseconds()
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		// 锁账号行：同一账号并发要令牌时，令牌数上限与个人空间的配额检查按行排队。
		if _, err := tx.Accounts().LockAccount(ctx, s.tenant(), account.AccountID); err != nil {
			return err
		}
		if teamID == "" {
			used, err := tx.Hosts().CountPersonal(ctx, s.tenant(), account.AccountID, "")
			if err != nil {
				return err
			}
			limit, err := s.hostLimit(ctx, tx, "", account.AccountID)
			if err != nil {
				return err
			}
			if err := quota.Check(quota.Hosts, used, limit); err != nil {
				return err
			}
		} else {
			access, err := teams.Require(ctx, tx, s.tenant(), account.AccountID, teamID, teams.MinStaff)
			if err != nil {
				return err
			}
			if access.Team.Status == saas.TeamSuspended {
				return core.Fail(gen.ErrTeamSuspended)
			}
			if access.Role == saas.RoleMember && !teams.SettingsOf(access.Team).MembersCanRegisterHosts {
				return core.Fail(gen.ErrHostRegistrationDisabled)
			}
			used, err := tx.Hosts().CountTeam(ctx, access.Scope, "")
			if err != nil {
				return err
			}
			limit, err := s.hostLimit(ctx, tx, teamID, "")
			if err != nil {
				return err
			}
			if err := quota.Check(quota.Hosts, used, limit); err != nil {
				return err
			}
		}
		added, err := tx.Hosts().AddRegistration(ctx, s.tenant(), saas.RegistrationToken{
			TokenHash: core.SecretHash(core.SecretRegistration, token), AccountID: account.AccountID, TeamID: teamID, ExpiresAtMs: expiresAtMs,
		}, nowMs, maxRegistrationTokens)
		if err != nil {
			return err
		}
		if !added {
			return core.Fail(gen.ErrLimitReached)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return core.RegistrationTokenResult{RegistrationToken: token, ExpiresAtMs: expiresAtMs, Issuer: issuer}, nil
}

// register 是 sources.register：一个事务里消耗注册令牌、核 core 版本门、锁空间行再查配额、写源与空间行、审计。
// 任何一步失败整个事务回滚，令牌不被消耗（它十分钟后自己过期）。
func (s *Service) register(ctx context.Context, call *api.Call) (any, error) {
	var in struct {
		RegistrationToken string               `json:"registrationToken"`
		SourceID          string               `json:"sourceId"`
		PublicKey         gen.Ed25519PublicJWK `json:"publicKey"`
		Name              string               `json:"name"`
		Kind              string               `json:"kind"`
		CoreVersion       string               `json:"coreVersion"`
		Capabilities      []string             `json:"capabilities"`
		Protocol          struct {
			Major int64 `json:"major"`
			Minor int64 `json:"minor"`
		} `json:"protocol"`
	}
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	if err := s.limits.Check(ctx, httpx.LimitLoginIP, "register:"+call.IP); err != nil {
		return nil, err
	}
	input := core.RegisterInput{
		RegistrationToken: in.RegistrationToken, SourceID: in.SourceID, PublicKey: in.PublicKey, Name: in.Name, Kind: in.Kind,
		CoreVersion: in.CoreVersion, Capabilities: in.Capabilities, ProtocolMajor: in.Protocol.Major, ProtocolMinor: in.Protocol.Minor,
	}
	hash := core.SecretHash(core.SecretRegistration, in.RegistrationToken)
	nowMs := s.nowMs()
	var ownerID string
	err := s.store.Tx(ctx, func(tx saas.Tx) error {
		var err error
		ownerID, err = s.registerIn(ctx, tx, call.IP, hash, input, nowMs)
		return err
	})
	if err != nil {
		return nil, err
	}
	settings, err := s.c.Ports.Tenants.Get(ctx, s.tenant())
	if err != nil {
		return nil, err
	}
	return core.RegisterResult{
		Issuer: settings.Issuer, JWKSURL: settings.Issuer + gen.PathJwksWellKnown,
		RelayOrigins: []string{s.c.RelayOrigin(in.SourceID)}, TrustedOrigins: core.TrustedOriginsOf(settings),
		SourceID: in.SourceID, OwnerAccountID: ownerID,
	}, nil
}

// registerIn 是登记事务的正文。加锁顺序：团队行（团队空间）→ 账号行（个人空间）→ 源。
func (s *Service) registerIn(ctx context.Context, tx saas.Tx, ip, hash string, input core.RegisterInput, nowMs int64) (string, error) {
	invalid := core.Fail(gen.ErrRegistrationTokenInvalid)
	// 先不加锁地看令牌属于哪个空间，再按顺序加锁，最后才消耗（消耗是删除，先删再等团队锁会和清除作业互等）。
	peek, err := tx.Hosts().PeekRegistration(ctx, s.tenant(), hash, nowMs)
	if err != nil {
		return "", err
	}
	if peek == nil {
		return "", invalid
	}
	var access teams.Access
	if peek.TeamID != "" {
		access, err = teams.Lock(ctx, tx, s.tenant(), peek.AccountID, peek.TeamID, teams.MinStaff)
		if err != nil {
			// 令牌签发之后账号不再是团队的可登记成员，或团队没了：令牌作废。
			if code, ok := core.CodeOf(err); ok && (code == gen.ErrNotFound || code == gen.ErrTeamRoleInsufficient || code == gen.ErrTeamDeleted) {
				return "", invalid
			}
			return "", err
		}
	} else if _, err := tx.Accounts().LockAccount(ctx, s.tenant(), peek.AccountID); err != nil {
		return "", err
	}
	token, err := tx.Hosts().ConsumeRegistration(ctx, s.tenant(), hash, nowMs)
	if err != nil {
		return "", err
	}
	if token == nil {
		return "", invalid
	}
	account, err := tx.Account(ctx, s.tenant(), token.AccountID)
	if err != nil {
		return "", err
	}
	if account == nil || account.Status != saas.AccountActive {
		return "", invalid
	}
	if input.ProtocolMajor != gen.ProtocolMajor {
		return "", core.Fail(gen.ErrProtocolUnsupported)
	}
	if token.TeamID != "" {
		if access.Team.Status == saas.TeamSuspended {
			return "", core.Fail(gen.ErrTeamSuspended)
		}
		if access.Role == saas.RoleMember && !teams.SettingsOf(access.Team).MembersCanRegisterHosts {
			return "", invalid
		}
		// 版本门：不让不认团队断言的老 core 进团队（设计 §4.6）。
		if !AtLeast(input.CoreVersion, gen.MinCoreVersionTeamAssertion) {
			return "", core.Fail(gen.ErrProtocolUnsupported)
		}
	}
	if err := tx.Hosts().LockSource(ctx, s.tenant(), input.SourceID); err != nil {
		return "", err
	}
	// 已登记且未撤销的源只能由登记它的人在同一个空间里重新登记；其余答 source_taken。
	existing, err := tx.Ports().Sources.Get(ctx, s.tenant(), input.SourceID)
	if err != nil {
		return "", err
	}
	// 撤销后也只认同一把钥：换钥登记同一个 sourceId 答 source_taken。
	if existing != nil && existing.PublicJWK.X != input.PublicKey.X {
		return "", core.Fail(gen.ErrSourceTaken)
	}
	if existing != nil && existing.RevokedAtMs == nil {
		space, err := tx.SpaceOf(ctx, s.tenant(), input.SourceID)
		if err != nil {
			return "", err
		}
		if space == nil || space.TeamID != token.TeamID || space.OwnerAccountID != token.AccountID {
			return "", core.Fail(gen.ErrSourceTaken)
		}
	}
	// 权威的配额检查：空间行已锁，重登记的这台自己不计。
	var used int
	if token.TeamID != "" {
		used, err = tx.Hosts().CountTeam(ctx, access.Scope, input.SourceID)
	} else {
		used, err = tx.Hosts().CountPersonal(ctx, s.tenant(), token.AccountID, input.SourceID)
	}
	if err != nil {
		return "", err
	}
	limit, err := s.hostLimit(ctx, tx, token.TeamID, token.AccountID)
	if err != nil {
		return "", err
	}
	if err := quota.Check(quota.Hosts, used, limit); err != nil {
		return "", err
	}
	if _, err := tx.Ports().Sources.Put(ctx, s.tenant(), input.SourceID, func(existing *store.SourceRecord) (store.SourceRecord, error) {
		return core.BuildSourceRecord(existing, input, nowMs)
	}); err != nil {
		return "", err
	}
	if err := tx.Hosts().UpsertSpace(ctx, s.tenant(), saas.HostSpace{
		SourceID: input.SourceID, TeamID: token.TeamID, OwnerAccountID: token.AccountID,
	}, nowMs); err != nil {
		return "", err
	}
	if err := s.record(ctx, tx, ip, token.AccountID, token.TeamID, input.SourceID, ActionRegister); err != nil {
		return "", err
	}
	return token.AccountID, nil
}

// heartbeatHint 是 sources.heartbeatHint：源 JWS 换隧道令牌；团队主机的令牌带 team，令牌的 jti 记进签发索引。
func (s *Service) heartbeatHint(ctx context.Context, call *api.Call) (any, error) {
	source, err := s.sources.AuthenticateSource(ctx, call.Authorization)
	if err != nil {
		return nil, err
	}
	space, err := s.store.Read().SpaceOf(ctx, s.tenant(), source.SourceID)
	if err != nil {
		return nil, err
	}
	teamID := ""
	if space != nil {
		teamID = space.TeamID
	}
	info, err := s.sources.RelayForTeam(ctx, source, teamID)
	if err != nil {
		return nil, err
	}
	if err := s.relay.IndexIssued(ctx, relayctl.Issued{
		Subject: relayctl.NoAccount, SourceID: source.SourceID, JTI: info.TunnelJTI, ExpMs: info.ExpiresAtMs,
	}); err != nil {
		return nil, err
	}
	// 签令牌 → 登记签发索引 → 再读源复核：撤销提交之后才会撤索引里的令牌，所以复核看到未撤销，
	// 说明之后的撤销一定会撤到这枚；看到已撤销就自己撤（与并发的 revoke 交错也不漏）。
	fresh, err := s.c.Ports.Sources.Get(ctx, s.tenant(), source.SourceID)
	if err != nil {
		return nil, err
	}
	if fresh == nil || fresh.RevokedAtMs != nil {
		if _, err := s.relay.RevokeWhere(ctx, relayctl.Filter{SourceID: source.SourceID}); err != nil {
			return nil, err
		}
		return nil, core.Fail(gen.ErrSourceRevoked)
	}
	return info, nil
}

// TunnelNodes 是 core.Context.TunnelNodes 的 SaaS 实现：读节点表（去掉排空的），按源做 rendezvous 排序，
// 答前三个（权重 3、2、1），url 是各节点自己的隧道地址；表为空答 503 unavailable，不兜底到共用地址（设计 §7.3）。
func TunnelNodes(relay relayctl.Control) func(ctx context.Context, sourceID string) ([]core.TunnelNode, error) {
	return func(ctx context.Context, sourceID string) ([]core.TunnelNode, error) {
		nodes, err := relay.Nodes(ctx)
		if err != nil {
			return nil, err
		}
		candidates := make([]control.NodeInfo, 0, len(nodes))
		for _, node := range nodes {
			candidates = append(candidates, control.NodeInfo{
				ID: node.ID, URL: node.URL, Weight: node.Weight, Draining: node.Draining,
			})
		}
		ranked := control.RankNodes(sourceID, candidates)
		if len(ranked) == 0 {
			return nil, core.Fail(gen.ErrUnavailable)
		}
		out := make([]core.TunnelNode, 0, len(ranked))
		for _, node := range ranked {
			out = append(out, core.TunnelNode{URL: node.URL, Weight: node.Weight})
		}
		return out, nil
	}
}
