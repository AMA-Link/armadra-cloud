package hosts

import (
	"context"
	"encoding/json"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/AMA-Link/armadra-cloud/go/internal/api"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/relayctl"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/teams"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

const (
	defaultPageLimit = 50
	// batchSize 是按在线状态筛选时一次从库里取的行数。
	batchSize = 200
	// maxMeSources 是 me.sources 一次最多答的主机数（它不分页）。
	maxMeSources = 2000
	maxOrigins   = 16
	maxTags      = 16
)

var tagPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)

// hostSettings 是主机设置的线上形状（协议包 hostSettingsSchema）。
type hostSettings struct {
	LinkCreators string `json:"linkCreators"`
	AllowGuests  bool   `json:"allowGuests"`
	DefaultRole  string `json:"defaultRole"`
}

var (
	linkCreators = []string{"owner", "owner_admin", "driver"}
	hostRoles    = []string{"viewer", "editor", "operator", "driver"}
)

// GuestsAllowed 报告主机设置是否允许授予访客（allowGuests，缺省 true）。
func GuestsAllowed(h saas.Host) bool { return settingsOf(h).AllowGuests }

// settingsOf 是主机的生效设置：缺省 ← 团队的 defaultHostRole（只作 defaultRole 的预选值）← 主机自己写过的项。
func settingsOf(h saas.Host) hostSettings {
	out := hostSettings{LinkCreators: "owner_admin", AllowGuests: true, DefaultRole: "viewer"}
	if slices.Contains(hostRoles, h.TeamDefaultRole) {
		out.DefaultRole = h.TeamDefaultRole
	}
	if len(h.Settings) > 0 {
		_ = json.Unmarshal(h.Settings, &out) // 只覆盖写过的项；库里的值坏了（不该发生）保持缺省
	}
	return out
}

type spaceOut struct {
	Kind   string `json:"kind"`
	TeamID string `json:"teamId,omitempty"`
}

// summaryOut 是 sourceSummary 的线上形状。
type summaryOut struct {
	SourceID     string   `json:"sourceId"`
	Name         string   `json:"name"`
	Kind         string   `json:"kind"`
	Online       bool     `json:"online"`
	LastSeenAtMs *int64   `json:"lastSeenAtMs"`
	Owner        bool     `json:"owner"`
	OwnerName    string   `json:"ownerName"`
	Via          string   `json:"via"`
	RelayOrigin  string   `json:"relayOrigin"`
	CoreVersion  string   `json:"coreVersion"`
	Space        spaceOut `json:"space"`
	Tags         []string `json:"tags"`
}

// detailOut 是 sourceDetail 的线上形状。
type detailOut struct {
	summaryOut
	TrustedOrigins []string     `json:"trustedOrigins"`
	Capabilities   []string     `json:"capabilities"`
	RegisteredAtMs int64        `json:"registeredAtMs"`
	Settings       hostSettings `json:"settings"`
}

func (s *Service) summary(ctx context.Context, viewerID string, h saas.Host) (summaryOut, error) {
	coreVersion, online, err := s.relay.Route(ctx, h.SourceID)
	if err != nil {
		return summaryOut{}, err
	}
	out := summaryOut{
		SourceID: h.SourceID, Name: h.Name, Kind: h.Kind, Online: online, LastSeenAtMs: h.LastSeenAtMs,
		Owner: h.OwnerAccountID == viewerID, OwnerName: h.OwnerName, Via: "owner", RelayOrigin: s.c.RelayOrigin(h.SourceID),
		CoreVersion: h.CoreVersion, Space: spaceOut{Kind: saas.SpacePersonal}, Tags: store.NonNil(h.Tags),
	}
	if h.TeamID != "" {
		out.Space = spaceOut{Kind: saas.SpaceTeam, TeamID: h.TeamID}
		if !out.Owner {
			out.Via = "team"
		}
	}
	if online {
		now := s.nowMs()
		out.LastSeenAtMs = &now
		out.CoreVersion = coreVersion
	}
	return out, nil
}

func (s *Service) detail(ctx context.Context, viewerID string, h saas.Host) (detailOut, error) {
	summary, err := s.summary(ctx, viewerID, h)
	if err != nil {
		return detailOut{}, err
	}
	return detailOut{
		summaryOut: summary, TrustedOrigins: store.NonNil(h.TrustedOrigins), Capabilities: store.NonNil(h.Capabilities),
		RegisteredAtMs: h.RegisteredAtMs, Settings: settingsOf(h),
	}, nil
}

// page 取一页：online 非 nil 时按在线状态再筛（在线状态在中继，不在库里），逐批取到凑满一页。
// 答这一页与下一页的起点（没有下一页为 nil）。
func (s *Service) page(ctx context.Context, filter saas.HostFilter, online *bool, limit int) ([]saas.Host, *saas.Host, error) {
	if online == nil {
		filter.Limit = limit + 1
		rows, err := s.dir.Hosts(ctx, s.store.Read(), filter)
		if err != nil || len(rows) <= limit {
			return rows, nil, err
		}
		return rows[:limit], &rows[limit-1], nil
	}
	var matched []saas.Host
	for {
		filter.Limit = batchSize
		rows, err := s.dir.Hosts(ctx, s.store.Read(), filter)
		if err != nil {
			return nil, nil, err
		}
		for _, row := range rows {
			_, isOnline, err := s.relay.Route(ctx, row.SourceID)
			if err != nil {
				return nil, nil, err
			}
			if isOnline == *online {
				matched = append(matched, row)
				if len(matched) > limit {
					return matched[:limit], &matched[limit-1], nil
				}
			}
		}
		if len(rows) < batchSize {
			return matched, nil, nil
		}
		last := rows[len(rows)-1]
		filter.After = &saas.HostCursor{RegisteredAtMs: last.RegisteredAtMs, SourceID: last.SourceID}
	}
}

// list 是 sources.list：space、status、tag（且）、q、cursor、limit；缺省列全部可达的空间里未撤销的主机。
func (s *Service) list(ctx context.Context, call *api.Call) (any, error) {
	var in struct {
		Status string   `json:"status"`
		Tag    []string `json:"tag"`
		Q      string   `json:"q"`
		Cursor string   `json:"cursor"`
		Limit  int      `json:"limit"`
	}
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	space, hasSpace, err := spaceOf(call)
	if err != nil {
		return nil, err
	}
	account, err := s.me(ctx, call)
	if err != nil {
		return nil, err
	}
	filter := saas.HostFilter{ViewerID: account.AccountID, Revoked: in.Status == "revoked", Tags: in.Tag, Query: in.Q}
	if hasSpace {
		filter.SpaceKind, filter.TeamID = space.Kind, space.TeamID
	}
	if in.Cursor != "" {
		if filter.After, err = saas.DecodeHostCursor(in.Cursor); err != nil {
			return nil, core.Fail(gen.ErrBadRequest)
		}
	}
	var online *bool
	if in.Status == "online" || in.Status == "offline" {
		want := in.Status == "online"
		online = &want
	}
	limit := in.Limit
	if limit <= 0 {
		limit = defaultPageLimit
	}
	rows, more, err := s.page(ctx, filter, online, limit)
	if err != nil {
		return nil, err
	}
	out := make([]summaryOut, 0, len(rows))
	for _, row := range rows {
		summary, err := s.summary(ctx, account.AccountID, row)
		if err != nil {
			return nil, err
		}
		out = append(out, summary)
	}
	var next any
	if more != nil {
		next = saas.HostCursor{RegisteredAtMs: more.RegisteredAtMs, SourceID: more.SourceID}.Encode()
	}
	return map[string]any{"sources": out, "nextCursor": next}, nil
}

// meSources 是 me.sources：调用者能看到的全部未撤销的主机（不分页）。
func (s *Service) meSources(ctx context.Context, call *api.Call) (any, error) {
	account, err := s.me(ctx, call)
	if err != nil {
		return nil, err
	}
	out := []summaryOut{}
	filter := saas.HostFilter{ViewerID: account.AccountID, Limit: batchSize}
	for len(out) < maxMeSources {
		rows, err := s.dir.Hosts(ctx, s.store.Read(), filter)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			summary, err := s.summary(ctx, account.AccountID, row)
			if err != nil {
				return nil, err
			}
			out = append(out, summary)
		}
		if len(rows) < batchSize {
			break
		}
		last := rows[len(rows)-1]
		filter.After = &saas.HostCursor{RegisteredAtMs: last.RegisteredAtMs, SourceID: last.SourceID}
	}
	return map[string]any{"sources": out}, nil
}

// find 读调用者看得到的一台未撤销的主机；看不到（不存在、别人的、别的团队的）答 not_found。
func (s *Service) find(ctx context.Context, r saas.Reader, viewerID, sourceID string) (saas.Host, error) {
	rows, err := s.dir.Hosts(ctx, r, saas.HostFilter{ViewerID: viewerID, SourceID: sourceID, Limit: 1})
	if err != nil {
		return saas.Host{}, err
	}
	if len(rows) == 0 {
		return saas.Host{}, core.Fail(gen.ErrNotFound)
	}
	return rows[0], nil
}

func sourceIDOf(call *api.Call) string {
	id, _ := call.Input["sourceId"].(string)
	return id
}

// get 是 sources.get。
func (s *Service) get(ctx context.Context, call *api.Call) (any, error) {
	account, err := s.me(ctx, call)
	if err != nil {
		return nil, err
	}
	h, err := s.find(ctx, s.store.Read(), account.AccountID, sourceIDOf(call))
	if err != nil {
		return nil, err
	}
	return s.detail(ctx, account.AccountID, h)
}

// manage 在事务里核管理权：团队主机先锁团队行（团队 → 账号 → 其余的顺序），再重读主机，
// 看不到答 not_found，看得到但管不了答 forbidden。
func (s *Service) manage(ctx context.Context, tx saas.Tx, accountID, sourceID string) (saas.Host, error) {
	first, err := s.find(ctx, tx, accountID, sourceID)
	if err != nil {
		return saas.Host{}, err
	}
	if first.TeamID != "" {
		if _, err := teams.Lock(ctx, tx, s.tenant(), accountID, first.TeamID, teams.MinStaff); err != nil {
			return saas.Host{}, err
		}
	}
	if err := tx.Hosts().LockSource(ctx, s.tenant(), sourceID); err != nil {
		return saas.Host{}, err
	}
	h, err := s.find(ctx, tx, accountID, sourceID)
	if err != nil {
		return saas.Host{}, err
	}
	if !h.CanManage(accountID) {
		return saas.Host{}, core.Fail(gen.ErrForbidden)
	}
	return h, nil
}

// update 是 sources.update：名称、可信来源、标签（整组替换）、主机设置（逐项改）；管理权。
func (s *Service) update(ctx context.Context, call *api.Call) (any, error) {
	var in struct {
		Name           *string   `json:"name"`
		TrustedOrigins *[]string `json:"trustedOrigins"`
		Tags           *[]string `json:"tags"`
	}
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	account, err := s.me(ctx, call)
	if err != nil {
		return nil, err
	}
	name, origins, tags := "", []string(nil), []string(nil)
	if in.Name != nil {
		if name = strings.TrimSpace(*in.Name); name == "" {
			return nil, core.Fail(gen.ErrBadRequest)
		}
	}
	if in.TrustedOrigins != nil {
		if origins, err = cleanOrigins(*in.TrustedOrigins); err != nil {
			return nil, err
		}
	}
	if in.Tags != nil {
		if tags, err = cleanTags(*in.Tags); err != nil {
			return nil, err
		}
	}
	patch, err := settingsPatch(call)
	if err != nil {
		return nil, err
	}
	sourceID := sourceIDOf(call)
	var out detailOut
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		h, err := s.manage(ctx, tx, account.AccountID, sourceID)
		if err != nil {
			return err
		}
		nowMs := s.nowMs()
		if in.Name != nil || in.TrustedOrigins != nil {
			err := tx.Ports().Sources.Update(ctx, s.tenant(), sourceID, func(source *store.SourceRecord) error {
				if source == nil || source.RevokedAtMs != nil {
					return core.Fail(gen.ErrNotFound)
				}
				if in.Name != nil {
					source.Name = name
				}
				if in.TrustedOrigins != nil {
					source.TrustedOrigins = origins
				}
				return nil
			})
			if err != nil {
				return err
			}
		}
		hostPatch := saas.HostPatch{}
		if in.Tags != nil {
			hostPatch.Tags = &tags
		}
		if patch != nil {
			merged, err := mergeSettings(h.Settings, patch)
			if err != nil {
				return err
			}
			hostPatch.Settings = merged
		}
		if hostPatch.Tags != nil || hostPatch.Settings != nil {
			ok, err := tx.Hosts().Patch(ctx, s.tenant(), sourceID, h.TeamID, hostPatch, nowMs)
			if err != nil {
				return err
			}
			if !ok {
				return core.Fail(gen.ErrNotFound)
			}
		}
		if err := s.record(ctx, tx, call.IP, account.AccountID, h.TeamID, sourceID, ActionUpdate); err != nil {
			return err
		}
		fresh, err := s.find(ctx, tx, account.AccountID, sourceID)
		if err != nil {
			return err
		}
		out, err = s.detail(ctx, account.AccountID, fresh)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// cleanTags 校验并去重（保序）：协议包的 hostTagsSchema，至多 16 个。
func cleanTags(tags []string) ([]string, error) {
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		if !tagPattern.MatchString(tag) {
			return nil, core.Fail(gen.ErrBadRequest)
		}
		if !slices.Contains(out, tag) {
			out = append(out, tag)
		}
	}
	if len(out) > maxTags {
		return nil, core.Fail(gen.ErrBadRequest)
	}
	return out, nil
}

// cleanOrigins：页面来源只许 http(s)://主机[:端口]，不带路径、查询与账号信息；去重保序。
func cleanOrigins(origins []string) ([]string, error) {
	out := make([]string, 0, len(origins))
	for _, raw := range origins {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil ||
			(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return nil, core.Fail(gen.ErrBadRequest)
		}
		origin := u.Scheme + "://" + strings.ToLower(u.Host)
		if !slices.Contains(out, origin) {
			out = append(out, origin)
		}
	}
	if len(out) > maxOrigins {
		return nil, core.Fail(gen.ErrBadRequest)
	}
	return out, nil
}

// settingsPatch 取入参里 settings 给了的项并校验取值；没给 settings 答 nil。
func settingsPatch(call *api.Call) (map[string]any, error) {
	raw, ok := call.Input["settings"].(map[string]any)
	if !ok {
		return nil, nil
	}
	patch := map[string]any{}
	for key, value := range raw {
		switch key {
		case "linkCreators":
			text, _ := value.(string)
			if !slices.Contains(linkCreators, text) {
				return nil, core.Fail(gen.ErrBadRequest)
			}
		case "defaultRole":
			text, _ := value.(string)
			if !slices.Contains(hostRoles, text) {
				return nil, core.Fail(gen.ErrBadRequest)
			}
		case "allowGuests":
			if _, isBool := value.(bool); !isBool {
				return nil, core.Fail(gen.ErrBadRequest)
			}
		default:
			continue // 入参 schema 已剔掉未列出的项
		}
		patch[key] = value
	}
	return patch, nil
}

// mergeSettings 把给了的项并进库里已写过的项（库里只存写过的项，没写的走缺省）。
func mergeSettings(stored json.RawMessage, patch map[string]any) (json.RawMessage, error) {
	current := map[string]any{}
	if len(stored) > 0 {
		if err := json.Unmarshal(stored, &current); err != nil {
			current = map[string]any{}
		}
	}
	for key, value := range patch {
		current[key] = value
	}
	return json.Marshal(current)
}

// revoke 是 sources.revoke：管理权；置 revoked_at_ms，提交后撤销该源全部已签令牌（含隧道令牌）、
// 关隧道并踢掉连接、发 sourceRevoked、审计（设计 §4.8）。
func (s *Service) revoke(ctx context.Context, call *api.Call) (any, error) {
	account, err := s.me(ctx, call)
	if err != nil {
		return nil, err
	}
	sourceID := sourceIDOf(call)
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		h, err := s.manage(ctx, tx, account.AccountID, sourceID)
		if err != nil {
			return err
		}
		nowMs := s.nowMs()
		err = tx.Ports().Sources.Update(ctx, s.tenant(), sourceID, func(source *store.SourceRecord) error {
			if source == nil || source.RevokedAtMs != nil {
				return core.Fail(gen.ErrNotFound)
			}
			source.RevokedAtMs = &nowMs
			return nil
		})
		if err != nil {
			return err
		}
		if err := s.record(ctx, tx, call.IP, account.AccountID, h.TeamID, sourceID, ActionRevoke); err != nil {
			return err
		}
		s.afterRevoke(tx, sourceID)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{}, nil
}

// afterRevoke 在提交后撤销令牌、关隧道、踢人、发事件；失败只记日志（重试与重放由 S8 的 relayctl 兜底）。
func (s *Service) afterRevoke(tx saas.Tx, sourceID string) {
	tx.AfterCommit(func(ctx context.Context) {
		if _, err := s.relay.RevokeWhere(ctx, relayctl.Filter{SourceID: sourceID}); err != nil {
			s.log.Warn("撤销主机的已签令牌失败", obs.Operation("hosts.revoke"), obs.SourceID(sourceID), obs.Error(err))
		}
		if err := s.relay.CloseTunnel(ctx, sourceID); err != nil {
			s.log.Warn("关闭主机的隧道失败", obs.Operation("hosts.closeTunnel"), obs.SourceID(sourceID), obs.Error(err))
		}
		if err := s.relay.Kick(ctx, relayctl.KickFilter{SourceID: sourceID}, "source_revoked"); err != nil {
			s.log.Warn("踢掉主机的连接失败", obs.Operation("hosts.kick"), obs.SourceID(sourceID), obs.Error(err))
		}
		s.c.Emit(core.NewSourceRevoked(sourceID), nil)
	})
}
