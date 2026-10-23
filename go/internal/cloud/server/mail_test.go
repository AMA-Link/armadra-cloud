package server_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/mail"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/relayctl"
)

// waitMails 等到发给 to 的邮件有 n 封，答这些邮件（按发出顺序）。
func (r *rig) waitMails(to string, n int) []mail.Message {
	r.t.Helper()
	var found []mail.Message
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		found = found[:0]
		for _, m := range r.mail.Messages() {
			if m.To == to {
				found = append(found, m)
			}
		}
		if len(found) >= n {
			return append([]mail.Message(nil), found...)
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.t.Fatalf("等不到发给 %s 的第 %d 封邮件（现有 %d）", to, n, len(found))
	return nil
}

// noMail 断言（给作业一点时间之后）没有发给 to 的邮件。
func (r *rig) noMail(to string) {
	r.t.Helper()
	time.Sleep(300 * time.Millisecond)
	for _, m := range r.mail.Messages() {
		if m.To == to {
			r.t.Fatalf("不该有发给 %s 的邮件：%s", to, m.Subject)
		}
	}
}

var linkPattern = regexp.MustCompile(`https?://\S+`)

// tokenOf 取邮件正文里链接的令牌：必须在 # 片段里，query 里没有。
func tokenOf(t *testing.T, m mail.Message, path string) string {
	t.Helper()
	link := linkPattern.FindString(m.Text)
	u, err := url.Parse(link)
	if err != nil || u.Fragment == "" || u.RawQuery != "" || u.Path != path || u.Scheme+"://"+u.Host != appOrigin {
		t.Fatalf("链接要是 %s%s#令牌：%q", appOrigin, path, link)
	}
	if !strings.Contains(m.HTML, link) {
		t.Fatalf("HTML 部分也要带同一个链接")
	}
	return u.Fragment
}

func hashOf(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// dump 是测试 schema 里所有表的全部行文本，用来扫描有没有令牌明文。
func (r *rig) dump() string {
	r.t.Helper()
	ctx := context.Background()
	rows, err := r.run.DB.Pool().Query(ctx, "SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema() AND table_type = 'BASE TABLE'")
	if err != nil {
		r.t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			r.t.Fatal(err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	var all strings.Builder
	for _, table := range tables {
		var text *string
		if err := r.run.DB.Pool().QueryRow(ctx, fmt.Sprintf(`SELECT string_agg(t::text, E'\n') FROM %q t`, table)).Scan(&text); err != nil {
			r.t.Fatalf("%s: %v", table, err)
		}
		if text != nil {
			all.WriteString(*text)
		}
	}
	return all.String()
}

// noPlaintext 断言库的所有表与日志里没有这些令牌的明文。
func (r *rig) noPlaintext(tokens ...string) {
	r.t.Helper()
	dump, logs := r.dump(), r.log.String()
	for _, token := range tokens {
		if strings.Contains(dump, token) {
			r.t.Fatal("库里出现了邮件令牌明文")
		}
		if strings.Contains(logs, token) {
			r.t.Fatal("日志里出现了邮件令牌明文")
		}
	}
}

func (r *rig) verifyEmail(token string) response {
	return r.do("POST", "/v1/auth/email/verify", map[string]any{"token": token}, nil)
}

func (r *rig) me(s session) response { return r.do("GET", "/v1/me", nil, s.bearer()) }

func TestRegisterVerifyEmailFlow(t *testing.T) {
	r := newRig(t, rigOptions{})
	email := r.email()
	s := r.registerBearer(email)

	letters := r.waitMails(email, 1)
	if letters[0].Subject != "验证你的邮箱" || !strings.Contains(letters[0].Text, "测试用户") {
		t.Fatalf("缺省中文：%+v", letters[0])
	}
	token := tokenOf(t, letters[0], "/verify")

	// 库里只有哈希；令牌明文既不在库里也不在日志里。
	var stored, purpose, to string
	if err := r.run.DB.Pool().QueryRow(context.Background(), "SELECT token_hash, purpose, email FROM email_tokens WHERE account_id = $1", s.accountID).Scan(&stored, &purpose, &to); err != nil {
		t.Fatal(err)
	}
	if stored != hashOf(token) || purpose != "verify" || to != email {
		t.Fatalf("令牌行：%s %s %s", stored, purpose, to)
	}
	r.noPlaintext(token)

	// 发件箱：发出后记 sent_at_ms，邮件头用 <email_id@控制台主机>。
	var sentAt *int64
	var messageID string
	if err := r.run.DB.Pool().QueryRow(context.Background(), "SELECT sent_at_ms, message_id FROM outbox_emails WHERE to_email = $1", email).Scan(&sentAt, &messageID); err != nil || sentAt == nil {
		t.Fatalf("发件箱应记已发：%v %v", sentAt, err)
	}
	if letters[0].MessageID != messageID || !strings.HasSuffix(messageID, "@127.0.0.1:8100") {
		t.Fatalf("Message-ID：%s", messageID)
	}

	if r.me(s).json["emailVerified"] != false {
		t.Fatal("验证前未验证")
	}
	res := r.verifyEmail(token)
	r.expect(res, 200, "")
	if res.json["email"] != email || res.json["emailVerified"] != true {
		t.Fatalf("答复：%s", res.body)
	}
	if r.me(s).json["emailVerified"] != true {
		t.Fatal("验证后已验证")
	}
	// 令牌只能用一次。
	r.expect(r.verifyEmail(token), 400, "email_token_invalid")
	r.expect(r.verifyEmail("not-a-token"), 400, "email_token_invalid")

	// 安全日志：注册与验证；设备名是快照，来源只留前缀。
	var actions []string
	rows, err := r.run.DB.Pool().Query(context.Background(), "SELECT action FROM audit_log WHERE account_id = $1 ORDER BY at_ms, action", s.accountID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var action string
		_ = rows.Scan(&action)
		actions = append(actions, action)
	}
	rows.Close()
	if strings.Join(actions, ",") != "security.emailVerified,security.register" {
		t.Fatalf("安全日志：%v", actions)
	}
	var deviceName, prefix *string
	if err := r.run.DB.Pool().QueryRow(context.Background(), "SELECT device_name, ip_prefix FROM audit_log WHERE account_id = $1 AND action = 'security.register'", s.accountID).Scan(&deviceName, &prefix); err != nil {
		t.Fatal(err)
	}
	if deviceName == nil || *deviceName != "测试浏览器" || prefix == nil || !strings.HasSuffix(*prefix, ".0/24") {
		t.Fatalf("设备名快照与 IP 前缀：%v %v", deviceName, prefix)
	}
}

func TestEnglishLocaleFollowsAcceptLanguage(t *testing.T) {
	r := newRig(t, rigOptions{})
	email := r.email()
	r.expect(r.register(email, hdr("Accept-Language", "en-US,en;q=0.9")), 200, "")
	letter := r.waitMails(email, 1)[0]
	if letter.Subject != "Verify your email" || !strings.Contains(letter.Text, "Open the link below") {
		t.Fatalf("英文：%+v", letter)
	}
	var locale string
	if err := r.run.DB.Pool().QueryRow(context.Background(), "SELECT locale FROM accounts WHERE email = $1", email).Scan(&locale); err != nil || locale != "en" {
		t.Fatalf("账号语言：%s %v", locale, err)
	}
}

func TestResendInvalidatesOldLinkAndIsRateLimited(t *testing.T) {
	r := newRig(t, rigOptions{})
	email := r.email()
	s := r.registerBearer(email)
	first := tokenOf(t, r.waitMails(email, 1)[0], "/verify")

	r.expect(r.do("POST", "/v1/auth/email/resend", nil, s.bearer()), 200, "")
	second := tokenOf(t, r.waitMails(email, 2)[1], "/verify")
	if first == second {
		t.Fatal("重发要换新令牌")
	}
	r.expect(r.verifyEmail(first), 400, "email_token_invalid") // 旧链接作废
	r.expect(r.do("POST", "/v1/auth/email/resend", nil, s.bearer()), 200, "")
	r.expect(r.do("POST", "/v1/auth/email/resend", nil, s.bearer()), 200, "")
	res := r.do("POST", "/v1/auth/email/resend", nil, s.bearer())
	r.expect(res, 429, "rate_limited") // 每小时 3 次
	r.waitMails(email, 4)
	r.noPlaintext(first, second)

	// 已验证后重发答 409。
	third := tokenOf(t, r.mail.Messages()[len(r.mail.Messages())-1], "/verify")
	r.expect(r.verifyEmail(third), 200, "")
	r.expect(r.do("POST", "/v1/auth/email/resend", nil, s.bearer()), 409, "conflict")
	r.expect(r.do("POST", "/v1/auth/email/resend", nil, nil), 401, "unauthenticated")
}

func TestEmailTokenExpiry(t *testing.T) {
	r := newRig(t, rigOptions{})
	email := r.email()
	r.registerBearer(email)
	token := tokenOf(t, r.waitMails(email, 1)[0], "/verify")
	r.clock.Advance(24*time.Hour + time.Second)
	r.expect(r.verifyEmail(token), 410, "email_token_expired")
}

func TestVerifyTokenRaceHasOneWinner(t *testing.T) {
	r := newRig(t, rigOptions{})
	email := r.email()
	r.registerBearer(email)
	token := tokenOf(t, r.waitMails(email, 1)[0], "/verify")
	var wg sync.WaitGroup
	results := make([]int, 8)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = r.verifyEmail(token).status
		}()
	}
	wg.Wait()
	ok := 0
	for _, status := range results {
		if status == 200 {
			ok++
		} else if status != 400 {
			t.Fatalf("输家只能答 400：%v", results)
		}
	}
	if ok != 1 {
		t.Fatalf("同一枚令牌并发使用恰好一个成功：%v", results)
	}
}

func (r *rig) resetStart(email string) response {
	return r.do("POST", "/v1/auth/password-reset/start", map[string]any{"email": email}, nil)
}

func (r *rig) resetFinish(token, pw string) response {
	return r.do("POST", "/v1/auth/password-reset/finish", map[string]any{"token": token, "newPassword": pw}, nil)
}

const newPassword = "gravel-marmot-ember-77"

func TestPasswordResetFlowRevokesOtherSessions(t *testing.T) {
	r := newRig(t, rigOptions{})
	email := r.email()
	a := r.registerBearer(email)
	res := r.login(email, password, nil)
	r.expect(res, 200, "")
	b := parseSession(r, res)
	r.waitMails(email, 1) // 注册的验证邮件
	exp := time.Now().Add(time.Hour).UnixMilli()
	for _, s := range []session{a, b} {
		_ = r.run.Relay.IndexIssued(context.Background(), relayctl.Issued{Subject: s.accountID, SourceID: "srcA", DeviceID: s.deviceID, JTI: "r." + s.deviceID + ".srcA.x", ExpMs: exp})
	}

	r.expect(r.resetStart(email), 200, "")
	letters := r.waitMails(email, 2)
	if letters[1].Subject != "重设口令" {
		t.Fatalf("找回邮件：%+v", letters[1])
	}
	token := tokenOf(t, letters[1], "/reset")
	r.noPlaintext(token)

	// 口令策略不过：令牌不消耗，会话也没被动。
	r.expect(r.resetFinish(token, "short"), 400, "password_too_short")
	r.expect(r.me(a), 200, "")

	r.expect(r.resetFinish(token, newPassword), 200, "")
	// 其他会话经撤销钩子撤销：访问令牌即刻失效，已签中继令牌进了撤销表。
	r.expect(r.me(a), 401, "")
	r.expect(r.me(b), 401, "")
	for _, s := range []session{a, b} {
		if !r.run.Relay.IsRevoked("r." + s.deviceID + ".srcA.x") {
			t.Fatalf("设备 %s 的已签令牌应被撤销", s.deviceID)
		}
	}
	r.expect(r.resetFinish(token, newPassword), 400, "email_token_invalid") // 一次性
	r.expect(r.login(email, password, nil), 401, "credentials_invalid")
	r.expect(r.login(email, newPassword, nil), 200, "")

	// 改口令通知，安全日志。
	changed := r.waitMails(email, 3)[2]
	if changed.Subject != "口令已更改" {
		t.Fatalf("口令已更改通知：%+v", changed)
	}
	var count int
	if err := r.run.DB.Pool().QueryRow(context.Background(), "SELECT count(*) FROM audit_log WHERE account_id = $1 AND action = 'security.passwordReset'", a.accountID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("安全日志：%d %v", count, err)
	}
	r.noPlaintext(token)
}

func TestPasswordResetClearsLockoutAndExpires(t *testing.T) {
	r := newRig(t, rigOptions{})
	email := r.email()
	r.registerBearer(email)
	for range 5 {
		r.login(email, "wrong-password-xx", nil)
	}
	r.expect(r.login(email, password, nil), 429, "account_locked")
	r.expect(r.resetStart(email), 200, "")
	token := tokenOf(t, r.waitMails(email, 2)[1], "/reset")
	r.expect(r.resetFinish(token, newPassword), 200, "")
	r.expect(r.login(email, newPassword, nil), 200, "") // 清锁

	r.expect(r.resetStart(email), 200, "")
	token = tokenOf(t, r.waitMails(email, 4)[3], "/reset")
	r.clock.Advance(31 * time.Minute)
	r.expect(r.resetFinish(token, "another-passphrase-12"), 410, "email_token_expired")
}

func TestPasswordResetStartDoesNotEnumerate(t *testing.T) {
	r := newRig(t, rigOptions{})
	known := r.email()
	r.registerBearer(known)
	r.waitMails(known, 1)
	disabled := r.email()
	s := r.registerBearer(disabled)
	r.exec("UPDATE accounts SET status = 'disabled' WHERE account_id = $1", s.accountID)
	unknown := r.email()

	responses := map[string]response{}
	for name, email := range map[string]string{"known": known, "disabled": disabled, "unknown": unknown} {
		responses[name] = r.resetStart(email)
		r.expect(responses[name], 200, "")
	}
	for name, res := range responses {
		if string(res.body) != string(responses["known"].body) {
			t.Fatalf("%s 的答复体与已注册的不同：%s / %s", name, res.body, responses["known"].body)
		}
		if res.header.Get("Content-Type") != responses["known"].header.Get("Content-Type") {
			t.Fatalf("%s 的答复头不同", name)
		}
	}
	// 已注册的收到邮件；停用的与没注册的都没有，也不在发件箱留下地址。
	r.waitMails(known, 2)
	r.noMail(unknown)
	r.noMail(disabled)
	eventually := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if ok() {
				return
			}
		}
		t.Fatalf("超时：%s", what)
	}
	eventually("占位发件被丢弃", func() bool {
		var n int
		_ = r.run.DB.Pool().QueryRow(context.Background(), "SELECT count(*) FROM outbox_emails WHERE to_email = ANY($1)", []string{unknown, disabled}).Scan(&n)
		return n == 0
	})
	// 三种情形做同样的库写入：各插了一行发件箱和一个 mail.send 作业。
	var jobs int
	if err := r.run.DB.Pool().QueryRow(context.Background(), "SELECT count(*) FROM jobs WHERE kind = 'mail.send'").Scan(&jobs); err != nil || jobs != 5 {
		t.Fatalf("作业数（2 封注册 + 3 次找回）：%d %v", jobs, err)
	}
}

func TestPasswordResetStartRateLimitsAreUniform(t *testing.T) {
	r := newRig(t, rigOptions{})
	known := r.email()
	r.registerBearer(known)
	unknown := r.email()
	// 按邮箱每小时 3 次：已注册与没注册的一样在第 4 次答 429。
	for _, email := range []string{known, unknown} {
		for range 3 {
			r.expect(r.resetStart(email), 200, "")
		}
		res := r.resetStart(email)
		r.expect(res, 429, "rate_limited")
		if _, ok := res.json["details"].(map[string]any)["retryAfterMs"]; !ok {
			t.Fatalf("带 retryAfterMs：%s", res.body)
		}
	}
}

func TestEmailChangeRejections(t *testing.T) {
	r := newRig(t, rigOptions{})
	oldEmail := r.email()
	s := r.registerBearer(oldEmail)
	other := r.email()
	r.registerBearer(other)
	change := func(email, pw string) response {
		return r.do("POST", "/v1/me/email", map[string]any{"newEmail": email, "password": pw}, s.bearer())
	}
	r.expect(r.do("POST", "/v1/me/email", map[string]any{"newEmail": r.email(), "password": password}, nil), 401, "unauthenticated")
	r.expect(change(r.email(), "wrong-password-xx"), 401, "credentials_invalid")
	r.expect(change(other, password), 409, "email_taken")
	r.expect(change(strings.ToUpper(oldEmail), password), 400, "bad_request")
	// 每次尝试都计入按账号每小时 3 次的限额（与重发验证邮件同一个桶）。
	r.expect(change(r.email(), password), 429, "rate_limited")
}

func TestEmailChangeFlow(t *testing.T) {
	r := newRig(t, rigOptions{})
	oldEmail, newEmail := r.email(), r.email()
	s := r.registerBearer(oldEmail)
	r.waitMails(oldEmail, 1)
	other := r.email()
	r.registerBearer(other)

	change := func(email, pw string) response {
		return r.do("POST", "/v1/me/email", map[string]any{"newEmail": email, "password": pw}, s.bearer())
	}
	r.expect(change(strings.ToUpper(newEmail), password), 200, "")
	letter := r.waitMails(newEmail, 1)[0]
	if letter.Subject != "确认新的邮箱地址" {
		t.Fatalf("确认邮件发往新地址：%+v", letter)
	}
	token := tokenOf(t, letter, "/verify")
	// 验证前旧邮箱照常生效。
	r.expect(r.login(oldEmail, password, nil), 200, "")
	r.expect(r.login(newEmail, password, nil), 401, "credentials_invalid")

	// 验证前新地址被别人占了：确认失败，令牌留着。
	r.exec("UPDATE accounts SET email = $1 WHERE email = $2", "moved-"+other, other)
	r.exec("UPDATE accounts SET email = $1 WHERE email = $2", newEmail, "moved-"+other)
	r.expect(r.verifyEmail(token), 409, "email_taken")
	r.exec("UPDATE accounts SET email = $1 WHERE email = $2", other, newEmail)

	res := r.verifyEmail(token)
	r.expect(res, 200, "")
	if res.json["email"] != newEmail || res.json["emailVerified"] != true {
		t.Fatalf("答复：%s", res.body)
	}
	if got := r.me(s).json["email"]; got != newEmail {
		t.Fatalf("me.get 邮箱：%v", got)
	}
	r.expect(r.login(newEmail, password, nil), 200, "")
	r.expect(r.login(oldEmail, password, nil), 401, "credentials_invalid")
	// 旧地址收到通知，里面是新地址、没有链接。
	notice := r.waitMails(oldEmail, 2)[1]
	if notice.Subject != "账号邮箱已更改" || !strings.Contains(notice.Text, newEmail) || linkPattern.MatchString(notice.Text) {
		t.Fatalf("旧地址通知：%+v", notice)
	}
	r.expect(r.verifyEmail(token), 400, "email_token_invalid")
	r.noPlaintext(token)
}

func TestNewDeviceReminder(t *testing.T) {
	r := newRig(t, rigOptions{})
	email := r.email()
	r.registerBearer(email)
	r.waitMails(email, 1)
	login := func(name string) {
		res := r.do("POST", "/v1/auth/login", map[string]any{"account": email, "password": password, "device": map[string]any{"platform": "desktop", "name": name}}, nil)
		r.expect(res, 200, "")
	}
	login("笔记本")
	letter := r.waitMails(email, 2)[1]
	if letter.Subject != "新设备登录" || !strings.Contains(letter.Text, "笔记本") || !strings.Contains(letter.Text, "desktop") {
		t.Fatalf("新设备提醒：%+v", letter)
	}
	login("笔记本") // 同平台同名 30 天内出现过：不提醒
	login("台式机")
	r.waitMails(email, 3)
	time.Sleep(300 * time.Millisecond)
	if n := len(r.mail.Messages()); n != 3 {
		t.Fatalf("同名设备不重复提醒：%d 封", n)
	}
	r.clock.Advance(31 * 24 * time.Hour)
	login("笔记本") // 31 天后又算新设备
	r.waitMails(email, 4)

	var login1, newDevice int
	_ = r.run.DB.Pool().QueryRow(context.Background(), "SELECT count(*) FILTER (WHERE action = 'security.login'), count(*) FILTER (WHERE action = 'security.newDevice') FROM audit_log WHERE actor_id = (SELECT account_id FROM accounts WHERE email = $1)", email).Scan(&login1, &newDevice)
	if login1 != 4 || newDevice != 3 {
		t.Fatalf("安全日志 login / newDevice：%d / %d", login1, newDevice)
	}
}

func TestChangePasswordNotifiesAndLogs(t *testing.T) {
	r := newRig(t, rigOptions{})
	email := r.email()
	s := r.registerBearer(email)
	r.waitMails(email, 1)
	res := r.do("POST", "/v1/auth/password", map[string]any{"password": password, "newPassword": newPassword}, s.bearer())
	r.expect(res, 200, "")
	if letter := r.waitMails(email, 2)[1]; letter.Subject != "口令已更改" {
		t.Fatalf("改口令通知：%+v", letter)
	}
	var n int
	_ = r.run.DB.Pool().QueryRow(context.Background(), "SELECT count(*) FROM audit_log WHERE account_id = $1 AND action = 'security.passwordChanged'", s.accountID).Scan(&n)
	if n != 1 {
		t.Fatalf("安全日志：%d", n)
	}
}

func eventuallyDB(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("超时：%s", what)
}

func TestSendFailureRetriesWithFreshTokenAndSendsOnce(t *testing.T) {
	r := newRig(t, rigOptions{})
	r.mail.FailNext(1)
	email := r.email()
	r.registerBearer(email)
	eventuallyDB(t, "第一次发信失败并记下", func() bool {
		var last *string
		_ = r.run.DB.Pool().QueryRow(context.Background(), "SELECT last_error FROM jobs WHERE kind = 'mail.send'").Scan(&last)
		return last != nil
	})
	if len(r.mail.Messages()) != 0 {
		t.Fatal("第一次没发出")
	}
	var firstHash string
	_ = r.run.DB.Pool().QueryRow(context.Background(), "SELECT token_hash FROM email_tokens WHERE email = $1", email).Scan(&firstHash)
	r.clock.Advance(31 * time.Second) // 退避 30 秒
	letter := r.waitMails(email, 1)[0]
	token := tokenOf(t, letter, "/verify")
	if hashOf(token) == firstHash {
		t.Fatal("重试要换新令牌")
	}
	var live int
	_ = r.run.DB.Pool().QueryRow(context.Background(), "SELECT count(*) FROM email_tokens WHERE email = $1 AND consumed_at_ms IS NULL", email).Scan(&live)
	if live != 1 {
		t.Fatalf("只有最后一枚有效：%d", live)
	}
	r.expect(r.verifyEmail(token), 200, "")
}

func TestMailSendReplayIsIdempotent(t *testing.T) {
	r := newRig(t, rigOptions{})
	email := r.email()
	r.registerBearer(email)
	r.waitMails(email, 1)
	eventuallyDB(t, "作业完成", func() bool {
		var done *int64
		_ = r.run.DB.Pool().QueryRow(context.Background(), "SELECT done_at_ms FROM jobs WHERE kind = 'mail.send'").Scan(&done)
		return done != nil
	})
	// 假装作业完成没记上、被别的实例重放。
	r.exec("UPDATE jobs SET done_at_ms = NULL, locked_by = NULL, locked_until_ms = NULL WHERE kind = 'mail.send'")
	eventuallyDB(t, "重放后再次完成", func() bool {
		var done *int64
		_ = r.run.DB.Pool().QueryRow(context.Background(), "SELECT done_at_ms FROM jobs WHERE kind = 'mail.send'").Scan(&done)
		return done != nil
	})
	if n := len(r.mail.Messages()); n != 1 {
		t.Fatalf("重放不该重发：%d 封", n)
	}
}

func TestGarbageCollectionJobs(t *testing.T) {
	r := newRig(t, rigOptions{})
	email := r.email()
	r.registerBearer(email)
	token := tokenOf(t, r.waitMails(email, 1)[0], "/verify")
	r.expect(r.verifyEmail(token), 200, "")
	eventuallyDB(t, "发件箱记已发", func() bool {
		var n int
		_ = r.run.DB.Pool().QueryRow(context.Background(), "SELECT count(*) FROM outbox_emails WHERE sent_at_ms IS NOT NULL").Scan(&n)
		return n == 1
	})
	r.clock.Advance(8 * 24 * time.Hour)
	eventuallyDB(t, "完成满 7 天的作业与过期满 24 小时的令牌被清", func() bool {
		var tokens, doneOld int
		_ = r.run.DB.Pool().QueryRow(context.Background(), "SELECT count(*) FROM email_tokens").Scan(&tokens)
		_ = r.run.DB.Pool().QueryRow(context.Background(), "SELECT count(*) FROM jobs WHERE kind = 'mail.send'").Scan(&doneOld)
		return tokens == 0 && doneOld == 0
	})
	var outbox int
	_ = r.run.DB.Pool().QueryRow(context.Background(), "SELECT count(*) FROM outbox_emails").Scan(&outbox)
	if outbox != 1 {
		t.Fatalf("发件箱满 30 天才清，8 天时还在：%d", outbox)
	}
	r.clock.Advance(23 * 24 * time.Hour)
	eventuallyDB(t, "发件箱满 30 天被清", func() bool {
		var n int
		_ = r.run.DB.Pool().QueryRow(context.Background(), "SELECT count(*) FROM outbox_emails").Scan(&n)
		return n == 0
	})
}
