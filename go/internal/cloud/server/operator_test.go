package server_test

import (
	"bytes"
	"encoding/base32"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/admin"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/cli"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/config"
)

// opAccount 是一个登记了两步验证的运营账号：邮箱、口令与 TOTP 种子。
type opAccount struct {
	email     string
	accountID string
	seed      []byte
}

// code 是此刻（受控时钟）的验证码，delta 是相对当前时间步的偏移。
func (r *rig) code(op opAccount, delta int64) string {
	return admin.TOTPCode(op.seed, admin.TOTPStep(r.clock.Now())+delta)
}

// seedFromURI 取 otpauth 地址里的种子。
func seedFromURI(t *testing.T, uri string) []byte {
	t.Helper()
	parsed, err := url.Parse(strings.TrimSpace(uri))
	if err != nil || parsed.Scheme != "otpauth" {
		t.Fatalf("不是 otpauth 地址：%q", uri)
	}
	seed, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(parsed.Query().Get("secret"))
	if err != nil {
		t.Fatal(err)
	}
	return seed
}

// operatorAccount 经真实的命令行路径（operators add / totp）造一个运营账号。
func (r *rig) operatorAccount() opAccount {
	r.t.Helper()
	email := r.email()
	s := r.registerBearer(email)
	env := config.MapEnv{"ARMADRA_CLOUD_DATABASE_URL": r.cfg.DatabaseURL, "ARMADRA_CLOUD_MASTER_KEY": masterHex}
	run := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := cli.Run(args, env, &out, &errOut, cli.Options{Version: "test", Now: r.clock.Now})
		return code, out.String(), errOut.String()
	}
	if code, out, errOut := run("operators", "add", "--email", email); code != 0 {
		r.t.Fatalf("operators add：%d %s %s", code, out, errOut)
	}
	code, out, errOut := run("operators", "totp", "--email", email)
	if code != 0 {
		r.t.Fatalf("operators totp：%d %s %s", code, out, errOut)
	}
	uri := out[strings.Index(out, "otpauth://"):]
	return opAccount{email: email, accountID: s.accountID, seed: seedFromURI(r.t, uri)}
}

// operatorLogin 走完整的 login → mfaVerify；cookie = true 用控制台来源（Cookie 传输），否则 Bearer。
func (r *rig) operatorLogin(op opAccount, cookie bool) session {
	r.t.Helper()
	h := headers(nil)
	if cookie {
		h = hdr("Origin", appOrigin)
	}
	res := r.do("POST", "/v1/auth/login", map[string]any{"account": op.email, "password": password, "device": device, "scope": "operator"}, h)
	r.expect(res, 200, "")
	challenge := res.json["mfa"].(map[string]any)["challengeId"].(string)
	res = r.do("POST", "/v1/auth/mfa/verify", map[string]any{"challengeId": challenge, "code": r.code(op, 0)}, h)
	r.expect(res, 200, "")
	return parseSession(r, res)
}

func (r *rig) login2(op opAccount, h headers) response {
	return r.do("POST", "/v1/auth/login", map[string]any{"account": op.email, "password": password, "device": device, "scope": "operator"}, h)
}

func (r *rig) verify(challenge, code string, h headers) response {
	return r.do("POST", "/v1/auth/mfa/verify", map[string]any{"challengeId": challenge, "code": code}, h)
}

func challengeOf(res response) string {
	return res.json["mfa"].(map[string]any)["challengeId"].(string)
}

// 命令行 operators add / totp：幂等加范围、种子只打印一次、已登记要 --reset、非运营账号拒绝。
func TestOperatorsCommandLine(t *testing.T) {
	r := newRig(t, rigOptions{})
	env := config.MapEnv{"ARMADRA_CLOUD_DATABASE_URL": r.cfg.DatabaseURL, "ARMADRA_CLOUD_MASTER_KEY": masterHex}
	run := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := cli.Run(args, env, &out, &errOut, cli.Options{Version: "test", Now: r.clock.Now})
		return code, out.String(), errOut.String()
	}
	email := r.email()
	s := r.registerBearer(email)

	if code, _, errOut := run("operators", "add"); code != 64 || !strings.Contains(errOut, "用法: armadra-cloud operators add --email <地址>") {
		t.Fatalf("缺 --email：%d %q", code, errOut)
	}
	if code, _, errOut := run("operators", "totp", "--email", email, "--bogus"); code != 64 || !strings.Contains(errOut, "用法") {
		t.Fatalf("多余参数：%d %q", code, errOut)
	}
	if code, _, errOut := run("operators", "add", "--email", "nobody@example.com"); code != 1 || !strings.Contains(errOut, "没有这个账号") {
		t.Fatalf("没有的账号：%d %q", code, errOut)
	}
	// 还不是运营账号：不能登记两步验证。
	if code, _, errOut := run("operators", "totp", "--email", email); code != 1 || !strings.Contains(errOut, "还不是运营账号") {
		t.Fatalf("非运营账号：%d %q", code, errOut)
	}
	for i := 0; i < 2; i++ { // 幂等
		if code, out, errOut := run("operators", "add", "--email="+strings.ToUpper(email)); code != 0 {
			t.Fatalf("add：%d %s %s", code, out, errOut)
		}
	}
	if got := r.queryString("SELECT array_to_string(scopes, ',') FROM accounts WHERE account_id = $1", s.accountID); got != "operator" {
		t.Fatalf("范围：%q", got)
	}
	code, out, errOut := run("operators", "totp", "--email", email)
	if code != 0 || !strings.Contains(out, "otpauth://totp/Armadra:"+email) || errOut != "" {
		t.Fatalf("totp：%d %q %q", code, out, errOut)
	}
	seed := seedFromURI(t, out[strings.Index(out, "otpauth://"):])
	// 种子不以明文落库、不进日志；库里是主密钥包裹的。
	wrap := r.queryString("SELECT secret_wrap FROM account_credentials WHERE account_id = $1 AND kind = 'totp'", s.accountID)
	if !strings.HasPrefix(wrap, "aes256gcm-v1:") || strings.Contains(r.dump(), base32.StdEncoding.EncodeToString(seed)[:16]) {
		t.Fatalf("种子要包裹：%q", wrap)
	}
	// 已登记：不带 --reset 拒绝，且没有换掉种子。
	if code, _, errOut := run("operators", "totp", "--email", email); code != 1 || !strings.Contains(errOut, "--reset") {
		t.Fatalf("重复登记：%d %q", code, errOut)
	}
	if got := r.queryString("SELECT secret_wrap FROM account_credentials WHERE account_id = $1 AND kind = 'totp'", s.accountID); got != wrap {
		t.Fatal("拒绝时不能换掉种子")
	}
	// --reset 换种子：旧种子的码不再通过。
	code, out, _ = run("operators", "totp", "--email", email, "--reset")
	if code != 0 {
		t.Fatalf("--reset：%d", code)
	}
	fresh := seedFromURI(t, out[strings.Index(out, "otpauth://"):])
	if bytes.Equal(fresh, seed) {
		t.Fatal("--reset 要换新种子")
	}
	res := r.login2(opAccount{email: email}, nil)
	r.expect(res, 200, "")
	r.expect(r.verify(challengeOf(res), r.code(opAccount{seed: seed}, 0), nil), 401, "mfa_invalid")
	if r.auditCount("action IN ('operator.add', 'operator.totp') AND actor_kind = 'system' AND target_id = $1", s.accountID) != 4 {
		t.Fatal("命令行动作写系统审计（add 两次、totp 两次）")
	}
	if strings.Contains(r.log.String(), base32.StdEncoding.EncodeToString(fresh)[:16]) {
		t.Fatal("种子不进日志")
	}
}

// 运营登录：非运营账号 403、没有 TOTP operator_mfa_required；挑战 → 验证码 → 运营会话（Cookie 与 Bearer）。
func TestOperatorLoginAndMFAVerify(t *testing.T) {
	r := newRig(t, rigOptions{})
	// 非运营账号：口令对了也是 403 forbidden；口令错仍是 credentials_invalid（不借此枚举运营账号）。
	plainEmail := r.email()
	r.registerBearer(plainEmail)
	r.expect(r.login2(opAccount{email: plainEmail}, nil), 403, "forbidden")
	r.expect(r.do("POST", "/v1/auth/login", map[string]any{"account": plainEmail, "password": "x", "device": device, "scope": "operator"}, nil), 401, "credentials_invalid")
	// 运营账号但没登记 TOTP。
	noTOTP := r.email()
	s := r.registerBearer(noTOTP)
	r.exec("UPDATE accounts SET scopes = '{operator}' WHERE account_id = $1", s.accountID)
	r.expect(r.login2(opAccount{email: noTOTP}, nil), 403, "operator_mfa_required")

	op := r.operatorAccount()
	// 口令错：不发挑战。
	r.expect(r.do("POST", "/v1/auth/login", map[string]any{"account": op.email, "password": "wrong-password-1", "device": device, "scope": "operator"}, nil), 401, "credentials_invalid")

	// Cookie 传输：Origin = 控制台、不带 Authorization。
	res := r.login2(op, hdr("Origin", appOrigin))
	r.expect(res, 200, "")
	if methods := res.json["mfa"].(map[string]any)["methods"]; fmt.Sprint(methods) != "[totp]" || res.json["session"] != nil || len(setCookies(res)) != 0 {
		t.Fatalf("口令对了只发挑战，不建会话：%s", res.body)
	}
	challenge := challengeOf(res)
	r.expect(r.verify(challenge, "000000", hdr("Origin", appOrigin)), 401, "mfa_invalid")
	res = r.verify(challenge, r.code(op, 0), hdr("Origin", appOrigin))
	r.expect(res, 200, "")
	cookieOp := parseSession(r, res)
	if cookieOp.csrf == "" || cookieOp.refresh != "" || cookieOp.cookies["__Host-armadra-ops"] == "" || cookieOp.cookies["__Host-armadra-ops-refresh"] == "" ||
		cookieOp.cookies["__Host-armadra-cloud"] != "" {
		t.Fatalf("Cookie 传输：只下发运营那一对，体里没有 refreshToken：%s %v", res.body, res.header.Values("Set-Cookie"))
	}
	for _, line := range res.header.Values("Set-Cookie") {
		if !strings.Contains(line, "Secure") || !strings.Contains(line, "HttpOnly") || !strings.Contains(line, "SameSite=Strict") || !strings.Contains(line, "Path=/") ||
			strings.Contains(line, "Domain") || !strings.HasPrefix(line, "__Host-armadra-ops") {
			t.Fatalf("Cookie 属性：%s", line)
		}
	}
	if want := r.clock.Ms() + time.Hour.Milliseconds(); cookieOp.expiresAtMs != want {
		t.Fatalf("运营会话 1 小时绝对寿命：%d / %d", cookieOp.expiresAtMs, want)
	}
	r.expect(r.do("GET", "/v1/admin/incident", nil, hdr("Cookie", cookieOp.cookieHeader("__Host-armadra-ops"))), 200, "")
	// 运营会话用不了普通过程；同一个挑战用过即作废。
	r.expect(r.do("GET", "/v1/me", nil, hdr("Cookie", cookieOp.cookieHeader("__Host-armadra-ops"))), 401, "unauthenticated")
	r.expect(r.verify(challenge, r.code(op, 1), hdr("Origin", appOrigin)), 410, "mfa_expired")

	// Bearer 传输。
	r.clock.Advance(time.Minute) // 换一个时间步，上面的码已用过
	bearer := r.operatorLogin(op, false)
	if bearer.refresh == "" || bearer.csrf != "" {
		t.Fatalf("Bearer 传输带 refreshToken、没有 csrfToken：%+v", bearer)
	}
	r.expect(r.do("GET", "/v1/admin/incident", nil, bearer.bearer()), 200, "")
	// 登录后账号本人的安全日志里有运营登录。
	if n := r.auditCount("account_id = $1 AND action = 'security.operatorLogin'", op.accountID); n != 2 {
		t.Fatalf("security.operatorLogin：%d", n)
	}
}

// TOTP 防重放：同一个码（同一时间步）不能用第二次，更早的步也不行；±1 步内的码都认；±2 步不认。
func TestMFAVerifyReplayAndWindow(t *testing.T) {
	r := newRig(t, rigOptions{})
	op := r.operatorAccount()
	attempt := func(code string) response {
		res := r.login2(op, nil)
		r.expect(res, 200, "")
		return r.verify(challengeOf(res), code, nil)
	}
	r.expect(attempt(r.code(op, 0)), 200, "")
	r.expect(attempt(r.code(op, 0)), 401, "mfa_invalid")  // 重放
	r.expect(attempt(r.code(op, -1)), 401, "mfa_invalid") // 更早的步
	r.expect(attempt(r.code(op, 2)), 401, "mfa_invalid")  // 超出 ±1
	r.expect(attempt(r.code(op, 1)), 200, "")             // 更晚的步（时钟漂移）认
	r.expect(attempt(r.code(op, 1)), 401, "mfa_invalid")  // 但它也只能用一次
	// 并发重放：同一个新码同时提交，恰好一个成功。
	r.clock.Advance(2 * time.Minute)
	code := r.code(op, 0)
	var challenges []string
	for i := 0; i < 4; i++ {
		challenges = append(challenges, challengeOf(r.login2(op, nil)))
	}
	results := make(chan int, len(challenges))
	for _, id := range challenges {
		go func() { results <- r.verify(id, code, nil).status }()
	}
	ok := 0
	for range challenges {
		if <-results == 200 {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("同一个码并发提交只能成功一次：%d", ok)
	}
}

// 挑战：每个最多错 5 次（之后连对的码也不认）；5 分钟过期；账号被停用 / 摘掉运营范围后失效。
func TestMFAChallengeLimitsAndExpiry(t *testing.T) {
	r := newRig(t, rigOptions{})
	op := r.operatorAccount()
	id := challengeOf(r.login2(op, nil))
	for i := 0; i < 5; i++ {
		r.expect(r.verify(id, "000000", nil), 401, "mfa_invalid")
	}
	r.expect(r.verify(id, r.code(op, 0), nil), 410, "mfa_expired") // 错满 5 次，挑战已删

	id = challengeOf(r.login2(op, nil))
	r.clock.Advance(6 * time.Minute)
	r.expect(r.verify(id, r.code(op, 0), nil), 410, "mfa_expired")
	r.expect(r.verify("no-such-challenge", "123456", nil), 410, "mfa_expired")

	id = challengeOf(r.login2(op, nil))
	r.exec("UPDATE accounts SET status = 'disabled' WHERE account_id = $1", op.accountID)
	r.expect(r.verify(id, r.code(op, 0), nil), 410, "mfa_expired")
	r.exec("UPDATE accounts SET status = 'active' WHERE account_id = $1", op.accountID)
	id = challengeOf(r.login2(op, nil))
	r.exec("UPDATE accounts SET scopes = '{}' WHERE account_id = $1", op.accountID)
	r.expect(r.verify(id, r.code(op, 0), nil), 410, "mfa_expired")
}

// auth.mfaVerify 按账号限流：10 分钟 10 次（跨挑战累计），超限 429 且连对的码也不认；窗口过后恢复。
func TestMFAVerifyIsRateLimitedPerAccount(t *testing.T) {
	r := newRig(t, rigOptions{})
	op := r.operatorAccount()
	attempts := 0
	for attempts < 10 {
		id := challengeOf(r.login2(op, nil))
		for i := 0; i < 4 && attempts < 10; i++ { // 每个挑战错 4 次，不触发「错满 5 次」
			r.expect(r.verify(id, "000000", nil), 401, "mfa_invalid")
			attempts++
		}
	}
	id := challengeOf(r.login2(op, nil))
	res := r.verify(id, r.code(op, 0), nil)
	r.expect(res, 429, "rate_limited")
	if res.header.Get("Retry-After") == "" {
		t.Fatal("429 带 Retry-After")
	}
	// 另一个运营账号不受影响。
	other := r.operatorAccount()
	r.operatorLogin(other, false)
}

// 运营会话 1 小时绝对寿命：刷新不延长，到点 admin.* 与刷新都失败。
func TestOperatorSessionExpiresAfterOneHour(t *testing.T) {
	r := newRig(t, rigOptions{})
	op := r.operatorAccount()
	s := r.operatorLogin(op, true)
	refresh := func() response {
		return r.do("POST", "/v1/auth/refresh", map[string]any{"scope": "operator"}, hdr("Origin", appOrigin, "Cookie", s.cookieHeader("__Host-armadra-ops-refresh")))
	}
	r.clock.Advance(40 * time.Minute)
	res := refresh()
	r.expect(res, 200, "")
	if parseSession(r, res).expiresAtMs != s.expiresAtMs {
		t.Fatal("刷新不延长")
	}
	s.cookies = parseSession(r, res).cookies
	r.expect(r.do("GET", "/v1/admin/incident", nil, hdr("Cookie", s.cookieHeader("__Host-armadra-ops"))), 200, "")
	r.clock.Advance(21 * time.Minute)
	r.expect(refresh(), 401, "session_expired")
	r.expect(r.do("GET", "/v1/admin/incident", nil, hdr("Cookie", s.cookieHeader("__Host-armadra-ops"))), 401, "")
}

// 运营账号的普通会话可以用 TOTP 做 auth.stepUp；非运营账号没有可验的凭据；同一个码不能用两次。
func TestStepUpWithTOTPForOperators(t *testing.T) {
	r := newRig(t, rigOptions{})
	op := r.operatorAccount()
	normal := r.do("POST", "/v1/auth/login", map[string]any{"account": op.email, "password": password, "device": device}, nil)
	r.expect(normal, 200, "")
	s := parseSession(r, normal)
	res := r.do("POST", "/v1/auth/step-up", map[string]any{"totpCode": r.code(op, 0)}, s.bearer())
	r.expect(res, 200, "")
	if res.json["elevatedUntilMs"] == nil {
		t.Fatalf("提权：%s", res.body)
	}
	r.expect(r.do("POST", "/v1/auth/step-up", map[string]any{"totpCode": r.code(op, 0)}, s.bearer()), 401, "mfa_invalid")
	other := r.registerBearer(r.email())
	r.expect(r.do("POST", "/v1/auth/step-up", map[string]any{"totpCode": "123456"}, other.bearer()), 401, "mfa_invalid")
}
