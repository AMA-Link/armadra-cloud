package accounts

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/api"
	"github.com/AMA-Link/armadra-cloud/go/internal/clientip"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/httpx"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// 设备码登录（设计 §4.3 设备码表、能力设计 §11.5）：原生 App / CLI 没有浏览器会话，
// 先 deviceStart 拿 deviceCode 与 userCode，用户在已登录的浏览器里核对设备再批准，发起设备轮询到会话。

const (
	deviceCodeTTL      = time.Duration(gen.IdentityDeviceCodeTTLMs) * time.Millisecond
	deviceCodeInterval = time.Duration(gen.IdentityDeviceCodeIntervalMs) * time.Millisecond
	// userCodeAttempts 是用户码与待处理的冲突时最多重抽几次；20^8 的空间里连冲突两次几乎不可能。
	userCodeAttempts = 8
)

// DeviceDecision 是批准或拒绝设备码时交给钩子的事件。
type DeviceDecision struct {
	AccountID string
	// Approved 为 false 表示拒绝。
	Approved bool
	// Platform 与 Name 是发起设备报的，用于安全日志的设备名快照。
	Platform string
	Name     string
}

// deviceCodeHash 是设备码明文的哈希（库里只存它）。设备码是 32 字节随机数，不需要慢哈希；
// 前缀用来把它和别的令牌哈希隔开。
func deviceCodeHash(deviceCode string) string {
	sum := sha256.Sum256([]byte("armadra/device-code\x00" + deviceCode))
	return hex.EncodeToString(sum[:])
}

// newUserCode 生成 XXXX-XXXX：每位从字母表里无偏地抽（拒绝采样，丢掉 ≥ 240 的字节）。
func newUserCode() string {
	alphabet := gen.IdentityDeviceCodeUserCodeAlphabet
	limit := byte(256 - 256%len(alphabet))
	out := make([]byte, 0, gen.IdentityDeviceCodeUserCodeLength+1)
	var buf [16]byte
	for len(out) < gen.IdentityDeviceCodeUserCodeLength+1 {
		if _, err := rand.Read(buf[:]); err != nil {
			panic("accounts: 系统随机源不可用：" + err.Error())
		}
		for _, b := range buf {
			if b >= limit {
				continue
			}
			if len(out) == gen.IdentityDeviceCodeUserCodeLength/2 {
				out = append(out, '-')
			}
			if len(out) < gen.IdentityDeviceCodeUserCodeLength+1 {
				out = append(out, alphabet[int(b)%len(alphabet)])
			}
		}
	}
	return string(out)
}

// normalizeUserCode 把用户输入规整成 XXXX-XXXX：去空白、转大写、缺连字符时补上；
// 字符不在字母表里答 false（调用方当作 not_found）。
func normalizeUserCode(input string) (string, bool) {
	code := strings.ToUpper(strings.Join(strings.Fields(input), ""))
	if len(code) == gen.IdentityDeviceCodeUserCodeLength {
		code = code[:4] + "-" + code[4:]
	}
	if len(code) != gen.IdentityDeviceCodeUserCodeLength+1 || code[4] != '-' {
		return "", false
	}
	for i := 0; i < len(code); i++ {
		if i != 4 && !strings.ContainsRune(gen.IdentityDeviceCodeUserCodeAlphabet, rune(code[i])) {
			return "", false
		}
	}
	return code, true
}

// deviceStart 是 auth.deviceStart：按 IP 限流 → 生成设备码（只存哈希）与用户码（与待处理的冲突就重抽）。
func (s *Service) deviceStart(ctx context.Context, call *api.Call) (any, error) {
	var in struct {
		Device core.DeviceInput `json:"device"`
	}
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	if err := s.limits.Check(ctx, httpx.LimitDeviceStart, call.IP); err != nil {
		return nil, err
	}
	deviceCode := core.RandomToken(32)
	nowMs := s.nowMs()
	row := saas.DeviceCode{
		DeviceCodeHash: deviceCodeHash(deviceCode), Platform: in.Device.Platform, Name: strings.TrimSpace(in.Device.Name),
		PushPublicKey: in.Device.PushPublicKey, IPPrefix: clientip.RateKey(call.IP),
		CreatedAtMs: nowMs, ExpiresAtMs: nowMs + deviceCodeTTL.Milliseconds(),
	}
	if row.Name == "" {
		return nil, core.Fail(gen.ErrBadRequest)
	}
	for attempt := 0; attempt < userCodeAttempts; attempt++ {
		row.UserCode = newUserCode()
		var inserted bool
		err := s.store.Tx(ctx, func(tx saas.Tx) (err error) {
			inserted, err = tx.DeviceCodes().Create(ctx, s.tenant(), row, nowMs)
			return err
		})
		if err != nil {
			return nil, err
		}
		if inserted {
			return map[string]any{
				"deviceCode": deviceCode, "userCode": row.UserCode,
				"verificationUrl": s.appOrigin + "/device?code=" + row.UserCode,
				"expiresAtMs":     row.ExpiresAtMs, "intervalMs": deviceCodeInterval.Milliseconds(),
			}, nil
		}
	}
	return nil, core.Fail(gen.ErrInternal)
}

// pollOutcome 是 devicePoll 在事务里算出的结果。
type pollOutcome struct {
	status string
	wait   time.Duration
	// session 在提交之前就签好（签发不依赖库）；事务回滚或重试时丢弃。
	session core.Session
}

// devicePoll 是 auth.devicePoll。状态机（行锁下）：
//
//	不存在 / consumed / expired → expired（兑换只此一次，之后与未知码同答，不泄露码是否存在过）
//	过期的 pending / approved → 落成 expired，答 expired
//	距上次轮询不足 intervalMs → 429 rate_limited（不更新轮询时间，等满间隔就放行）
//	pending → pending；denied → denied
//	approved → 建 bearer 设备、改 consumed、答会话
//
// 兑换时账号若已停用，改成 denied 并答 denied，不发会话。设备永远是 bearer / session：
// 轮询方是原生 App 或 CLI，不是控制台页面，所以不看 Origin。
//
// 加锁顺序（团队 → 账号 → 其余）：先不加锁读出 account_id，approved 时先 LockAccount，再 FOR UPDATE 读设备码行
// 并复核状态（不加锁读到的只是线索，一切以加锁后的为准）。批准 / 拒绝只锁设备码行，不拿账号锁，所以不会反序。
// 每个 IP 另有轮询桶（device.poll.ip），在开事务之前判。
func (s *Service) devicePoll(ctx context.Context, call *api.Call) (any, error) {
	var in struct {
		DeviceCode string `json:"deviceCode"`
	}
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	if err := s.limits.Check(ctx, httpx.LimitDevicePoll, call.IP); err != nil {
		return nil, err
	}
	hash := deviceCodeHash(in.DeviceCode)
	var out pollOutcome
	err := s.store.Tx(ctx, func(tx saas.Tx) error {
		out = pollOutcome{status: "expired"}
		repo := tx.DeviceCodes()
		hint, err := repo.Find(ctx, s.tenant(), hash)
		if err != nil {
			return err
		}
		if hint == nil {
			return nil
		}
		if hint.Status == saas.DeviceCodeApproved && hint.AccountID != nil {
			if _, err := tx.Accounts().LockAccount(ctx, s.tenant(), *hint.AccountID); err != nil {
				return err
			}
		}
		row, err := repo.Get(ctx, s.tenant(), hash)
		if err != nil {
			return err
		}
		nowMs := s.nowMs()
		if row == nil || row.Status == saas.DeviceCodeConsumed || row.Status == saas.DeviceCodeExpired {
			return nil
		}
		if nowMs >= row.ExpiresAtMs && (row.Status == saas.DeviceCodePending || row.Status == saas.DeviceCodeApproved) {
			row.Status = saas.DeviceCodeExpired
			return repo.Update(ctx, s.tenant(), *row)
		}
		if row.LastPollAtMs != nil {
			if wait := *row.LastPollAtMs + deviceCodeInterval.Milliseconds() - nowMs; wait > 0 {
				out.status, out.wait = "slow", time.Duration(wait)*time.Millisecond
				return nil
			}
		}
		row.LastPollAtMs = &nowMs
		switch row.Status {
		case saas.DeviceCodePending:
			out.status = "pending"
		case saas.DeviceCodeDenied:
			out.status = "denied"
		case saas.DeviceCodeApproved:
			if row.AccountID == nil {
				return core.Fail(gen.ErrInternal)
			}
			if hint.AccountID == nil || *hint.AccountID != *row.AccountID {
				// 不加锁读到的账号与加锁后的不一致（不该发生，批准只写一次）：这次不兑换，下次轮询按新状态来。
				out.status = "pending"
				break
			}
			account, err := tx.Account(ctx, s.tenant(), *row.AccountID)
			if err != nil {
				return err
			}
			if account == nil || account.Status != saas.AccountActive {
				row.Status, out.status = saas.DeviceCodeDenied, "denied"
				break
			}
			device, refresh := core.NewDevice(account.AccountID,
				core.DeviceInput{Platform: row.Platform, Name: row.Name, PushPublicKey: row.PushPublicKey},
				core.TransportBearer, core.SessionScopeSession, nowMs, 0)
			if err := s.recordLogin(ctx, tx, account, device, call.IP, nowMs); err != nil {
				return err
			}
			// 令牌在提交之前签：签发不依赖库，提交失败整个 out 被丢弃，没有悬空的令牌。
			session, err := core.SessionNamed(ctx, s.c, device, refresh, account.DisplayName)
			if err != nil {
				return err
			}
			session.Account = accountInfo(account)
			row.Status = saas.DeviceCodeConsumed
			out.status, out.session = "approved", session
		}
		return repo.Update(ctx, s.tenant(), *row)
	})
	if err != nil {
		return nil, err
	}
	switch out.status {
	case "slow":
		return nil, core.FailAfter(gen.ErrRateLimited, out.wait)
	case "approved":
		return map[string]any{"status": "approved", "session": out.session}, nil
	default:
		return map[string]any{"status": out.status}, nil
	}
}

// openRequest 在行锁下读用户码对应的设备码，要求它是还没过期的 pending：
// 用户码格式不对、没有这一行、已被批准 / 拒绝 / 兑换 → 404 not_found；过期（含时间到了还没落库）→ 410 device_code_expired。
func (s *Service) openRequest(ctx context.Context, tx saas.Tx, userCode string) (*saas.DeviceCode, error) {
	code, ok := normalizeUserCode(userCode)
	if !ok {
		return nil, core.Fail(gen.ErrNotFound)
	}
	row, err := tx.DeviceCodes().GetByUserCode(ctx, s.tenant(), code)
	if err != nil {
		return nil, err
	}
	switch {
	case row == nil:
		return nil, core.Fail(gen.ErrNotFound)
	case row.Status == saas.DeviceCodeExpired:
		return nil, core.Fail(gen.ErrDeviceCodeExpired)
	case row.Status != saas.DeviceCodePending:
		return nil, core.Fail(gen.ErrNotFound)
	case s.nowMs() >= row.ExpiresAtMs:
		return nil, core.Fail(gen.ErrDeviceCodeExpired)
	}
	return row, nil
}

// deviceGet 是 auth.deviceGet：要会话 → 按 IP 限流（防猜码）→ 答发起设备的平台、名字与时间。
func (s *Service) deviceGet(ctx context.Context, call *api.Call) (any, error) {
	caller, err := call.Caller(ctx)
	if err != nil {
		return nil, err
	}
	var in struct {
		UserCode string `json:"userCode"`
	}
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	account, err := s.owner(ctx, caller)
	if err != nil {
		return nil, err
	}
	if err := s.guessGate(ctx, call, account.AccountID); err != nil {
		return nil, err
	}
	var row *saas.DeviceCode
	err = s.store.Tx(ctx, func(tx saas.Tx) (err error) {
		row, err = s.openRequest(ctx, tx, in.UserCode)
		return err
	})
	if err != nil {
		return nil, s.countGuess(ctx, account.AccountID, err)
	}
	out := map[string]any{"platform": row.Platform, "name": row.Name, "requestedAtMs": row.CreatedAtMs, "expiresAtMs": row.ExpiresAtMs}
	if row.IPPrefix != "" {
		out["ipPrefix"] = row.IPPrefix
	}
	return out, nil
}

// deviceApprove 是 auth.deviceApprove：approve = false 等同拒绝。
func (s *Service) deviceApprove(ctx context.Context, call *api.Call) (any, error) {
	caller, err := call.Caller(ctx)
	if err != nil {
		return nil, err
	}
	var in struct {
		UserCode string `json:"userCode"`
		Approve  bool   `json:"approve"`
	}
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	return s.decide(ctx, call, caller, in.UserCode, in.Approve)
}

// deviceDeny 是 auth.deviceDeny：发起设备的 devicePoll 随后答 denied。
func (s *Service) deviceDeny(ctx context.Context, call *api.Call) (any, error) {
	caller, err := call.Caller(ctx)
	if err != nil {
		return nil, err
	}
	var in struct {
		UserCode string `json:"userCode"`
	}
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	return s.decide(ctx, call, caller, in.UserCode, false)
}

// decide 批准或拒绝：要会话（Cookie 会话的 Origin 与 CSRF 已在认证闸里核过）→ 与 deviceGet 共用 IP 桶限流
// （批准也是一次猜码机会：猜中别人的用户码就能把别人的设备登进自己的账号）→ 只对 pending 生效。
// 钩子在同一事务里记安全日志（security.deviceApprove，S2 接审计后补上）。
func (s *Service) decide(ctx context.Context, call *api.Call, caller core.Caller, userCode string, approve bool) (any, error) {
	account, err := s.owner(ctx, caller)
	if err != nil {
		return nil, err
	}
	// 批准要么是 Cookie 会话（页面里的人点的，Origin 与 CSRF 已核），要么是提权窗口内的 Bearer 会话：
	// 一个被盗的 Bearer 令牌不能静默地把别的设备登进来。拒绝不需要。用 mfa_required（设计 Q7 的「未提权」码）。
	if approve && caller.Transport != core.TransportCookie && !caller.Elevated(s.nowMs()) {
		return nil, core.Fail(gen.ErrMfaRequired)
	}
	if err := s.guessGate(ctx, call, account.AccountID); err != nil {
		return nil, err
	}
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		row, err := s.openRequest(ctx, tx, userCode)
		if err != nil {
			return err
		}
		nowMs := s.nowMs()
		row.Status, row.AccountID, row.DecidedAtMs = saas.DeviceCodeDenied, &account.AccountID, &nowMs
		if approve {
			row.Status = saas.DeviceCodeApproved
		}
		if err := tx.DeviceCodes().Update(ctx, s.tenant(), *row); err != nil {
			return err
		}
		if s.onDeviceDecision != nil {
			return s.onDeviceDecision(ctx, tx, DeviceDecision{AccountID: account.AccountID, Approved: approve, Platform: row.Platform, Name: row.Name})
		}
		return nil
	})
	if err != nil {
		return nil, s.countGuess(ctx, account.AccountID, err)
	}
	return map[string]any{}, nil
}

// guessGate 是猜用户码的两道闸：账号桶（只统计 not_found 的失败，同一账号换 IP 也逃不掉）已满直接 429；
// IP 桶每次都计（deviceGet、批准、拒绝共用）。
func (s *Service) guessGate(ctx context.Context, call *api.Call, accountID string) error {
	if err := s.limits.Blocked(ctx, httpx.LimitDeviceGuess, accountID); err != nil {
		return err
	}
	return s.limits.Check(ctx, httpx.LimitDeviceGet, call.IP)
}

// countGuess 在查无此码（not_found）时给账号桶记一次失败；过期（410）与成功不计。原样返回 err。
func (s *Service) countGuess(ctx context.Context, accountID string, err error) error {
	if code, ok := core.CodeOf(err); ok && code == gen.ErrNotFound {
		if recordErr := s.limits.Record(ctx, httpx.LimitDeviceGuess, accountID); recordErr != nil {
			return recordErr
		}
	}
	return err
}
