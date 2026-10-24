package server_test

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

type saasTx = saas.Tx

func newDeviceCodeRow(hash, userCode string, nowMs int64) saas.DeviceCode {
	return saas.DeviceCode{DeviceCodeHash: hash, UserCode: userCode, Platform: "desktop", Name: "x", CreatedAtMs: nowMs, ExpiresAtMs: nowMs + 600000}
}

var userCodePattern = regexp.MustCompile(`^[BCDFGHJKLMNPQRSTVWXZ]{4}-[BCDFGHJKLMNPQRSTVWXZ]{4}$`)

const pollInterval = 5 * time.Second

// startDevice 以匿名身份发起设备码（原生 App / CLI 的做法）。
func (r *rig) startDevice(h headers) (deviceCode, userCode string) {
	r.t.Helper()
	res := r.do("POST", "/v1/auth/device/start", map[string]any{"device": map[string]any{"platform": "desktop", "name": "我的 Mac"}}, h)
	r.expect(res, 200, "")
	return res.json["deviceCode"].(string), res.json["userCode"].(string)
}

func (r *rig) poll(deviceCode string) response {
	r.t.Helper()
	return r.do("POST", "/v1/auth/device/poll", map[string]any{"deviceCode": deviceCode}, nil)
}

func (r *rig) pollStatus(deviceCode string) string {
	r.t.Helper()
	res := r.poll(deviceCode)
	r.expect(res, 200, "")
	return res.json["status"].(string)
}

func (r *rig) approve(s session, userCode string, approve bool) response {
	r.t.Helper()
	return r.do("POST", "/v1/auth/device/approve", map[string]any{"userCode": userCode, "approve": approve}, s.write())
}

func (r *rig) deviceGet(s session, userCode string) response {
	r.t.Helper()
	return r.do("GET", "/v1/auth/device?userCode="+url.QueryEscape(userCode), nil, s.cookieRead())
}

// cookieRead 是 Cookie 会话的读请求头（GET 不要 CSRF 与 Origin）。
func (s session) cookieRead() headers {
	return hdr("Cookie", s.cookieHeader("__Host-armadra-cloud"))
}

func TestDeviceCodeFullFlow(t *testing.T) {
	r := newRig(t, rigOptions{})
	browser := r.registerCookie(r.email())

	// 即使带着控制台的 Origin，匿名发起的设备也永远走 Bearer（轮询方不是控制台页面）。
	res := r.do("POST", "/v1/auth/device/start", map[string]any{"device": map[string]any{"platform": "desktop", "name": "我的 Mac"}}, hdr("Origin", appOrigin))
	r.expect(res, 200, "")
	deviceCode, userCode := res.json["deviceCode"].(string), res.json["userCode"].(string)
	if !userCodePattern.MatchString(userCode) || res.json["verificationUrl"] != appOrigin+"/device?code="+userCode ||
		res.json["intervalMs"].(float64) != 5000 || res.json["expiresAtMs"].(float64) != float64(r.clock.Ms()+10*60*1000) {
		t.Fatalf("发起的答复：%s", res.body)
	}
	if len(setCookies(res)) != 0 {
		t.Fatal("发起不下发 Cookie")
	}

	// 库里只有哈希：设备码明文不在任何列里。
	var plain int
	if err := r.run.DB.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM device_codes WHERE device_code_hash = $1 OR user_code = $1 OR name = $1`, deviceCode).Scan(&plain); err != nil || plain != 0 {
		t.Fatalf("设备码明文落了库：%d %v", plain, err)
	}

	// pending；紧接着再轮询被限速，等满间隔放行。
	if got := r.pollStatus(deviceCode); got != "pending" {
		t.Fatalf("状态 %s", got)
	}
	fast := r.poll(deviceCode)
	r.expect(fast, 429, "rate_limited")
	if fast.header.Get("Retry-After") == "" || fast.json["details"].(map[string]any)["retryAfterMs"].(float64) <= 0 {
		t.Fatalf("429 要带重试时间：%v %s", fast.header, fast.body)
	}
	r.clock.Advance(pollInterval)

	// 授权页：看到发起设备的平台与名字。
	info := r.deviceGet(browser, userCode)
	r.expect(info, 200, "")
	if info.json["platform"] != "desktop" || info.json["name"] != "我的 Mac" {
		t.Fatalf("发起设备：%s", info.body)
	}
	if info.json["ipPrefix"] != r.ip {
		t.Fatalf("IPv4 原样返回：%s", info.body)
	}
	// IPv6 截到 /64。
	_, v6Code := r.startDevice(hdr("X-Forwarded-For", "2001:db8:1:2:3:4:5:6"))
	if got := r.deviceGet(browser, v6Code); got.status != 200 || got.json["ipPrefix"] != "2001:db8:1:2::/64" {
		t.Fatalf("IPv6 前缀：%s", got.body)
	}
	// 未批准前轮询仍是 pending。
	if got := r.pollStatus(deviceCode); got != "pending" {
		t.Fatalf("状态 %s", got)
	}

	r.expect(r.approve(browser, userCode, true), 200, "")
	r.clock.Advance(pollInterval)
	res = r.poll(deviceCode)
	r.expect(res, 200, "")
	if res.json["status"] != "approved" {
		t.Fatalf("批准后：%s", res.body)
	}
	device := parseSession(r, response{status: 200, header: res.header, json: map[string]any{"session": res.json["session"]}})
	if device.refresh == "" || len(setCookies(res)) != 0 || res.json["csrfToken"] != nil {
		t.Fatalf("设备拿到 Bearer 会话（带刷新令牌、不下发 Cookie、没有 csrfToken）：%s", res.body)
	}
	if device.accountID != browser.accountID || device.deviceID == browser.deviceID {
		t.Fatal("会话属于批准的账号、是新设备")
	}
	var transport, scope string
	if err := r.run.DB.Pool().QueryRow(context.Background(),
		`SELECT coalesce(transport, ''), coalesce(scope, '') FROM devices WHERE device_id = $1`, device.deviceID).Scan(&transport, &scope); err != nil {
		t.Fatal(err)
	}
	if (transport != "" && transport != "bearer") || (scope != "" && scope != "session") {
		t.Fatalf("设备的传输 %q 与范围 %q", transport, scope)
	}

	// 会话可用，且在设备列表里（名字与平台来自发起请求）。
	r.expect(r.do("GET", "/v1/me", nil, device.bearer()), 200, "")
	list := r.do("GET", "/v1/me/devices", nil, device.bearer())
	r.expect(list, 200, "")
	if !strings.Contains(string(list.body), device.deviceID) || !strings.Contains(string(list.body), "我的 Mac") {
		t.Fatalf("设备列表：%s", list.body)
	}

	// 只此一次：再轮询与未知码同答 expired。
	r.clock.Advance(pollInterval)
	if got := r.pollStatus(deviceCode); got != "expired" {
		t.Fatalf("兑换后再轮询：%s", got)
	}
	r.expect(r.approve(browser, userCode, true), 404, "not_found")

	// 批准后签出的设备能被撤销（浏览器会话撤销它）：访问令牌与刷新令牌都失效。
	r.expect(r.do("DELETE", "/v1/me/devices/"+device.deviceID, nil, browser.write()), 200, "")
	r.expect(r.do("GET", "/v1/me", nil, device.bearer()), 401, "unauthenticated")
	r.expect(r.do("POST", "/v1/auth/refresh", map[string]any{"refreshToken": device.refresh}, nil), 401, "session_revoked")

	if strings.Contains(r.log.String(), deviceCode) {
		t.Fatal("日志里不该有设备码")
	}
}

func TestDeviceCodeDeny(t *testing.T) {
	r := newRig(t, rigOptions{})
	browser := r.registerCookie(r.email())

	deviceCode, userCode := r.startDevice(nil)
	r.expect(r.do("POST", "/v1/auth/device/deny", map[string]any{"userCode": userCode}, browser.write()), 200, "")
	if got := r.pollStatus(deviceCode); got != "denied" {
		t.Fatalf("拒绝后：%s", got)
	}
	r.clock.Advance(pollInterval)
	if got := r.pollStatus(deviceCode); got != "denied" {
		t.Fatalf("拒绝后再轮询仍是 denied：%s", got)
	}
	// 结案后批准、再拒绝都是 404；授权页也看不到了。
	r.expect(r.approve(browser, userCode, true), 404, "not_found")
	r.expect(r.do("POST", "/v1/auth/device/deny", map[string]any{"userCode": userCode}, browser.write()), 404, "not_found")
	r.expect(r.deviceGet(browser, userCode), 404, "not_found")
	var devices int
	_ = r.run.DB.Pool().QueryRow(context.Background(), `SELECT count(*) FROM devices WHERE subject = $1`, browser.accountID).Scan(&devices)
	if devices != 1 {
		t.Fatalf("拒绝不该建设备：%d", devices)
	}

	// approve: false 等同拒绝。
	deviceCode, userCode = r.startDevice(nil)
	r.expect(r.approve(browser, userCode, false), 200, "")
	if got := r.pollStatus(deviceCode); got != "denied" {
		t.Fatalf("approve=false：%s", got)
	}
}

func TestDeviceCodeExpiry(t *testing.T) {
	r := newRig(t, rigOptions{})
	browser := r.registerCookie(r.email())

	// pending 过期：轮询答 expired，授权页、批准、拒绝答 410。
	deviceCode, userCode := r.startDevice(nil)
	r.clock.Advance(10*time.Minute + time.Second)
	r.expect(r.deviceGet(browser, userCode), 410, "device_code_expired")
	r.expect(r.approve(browser, userCode, true), 410, "device_code_expired")
	r.expect(r.do("POST", "/v1/auth/device/deny", map[string]any{"userCode": userCode}, browser.write()), 410, "device_code_expired")
	if got := r.pollStatus(deviceCode); got != "expired" {
		t.Fatalf("过期：%s", got)
	}
	r.clock.Advance(pollInterval)
	r.expect(r.deviceGet(browser, userCode), 410, "device_code_expired")

	// approved 之后才过期（发起设备迟迟没来取）：不再发会话。
	browser = r.registerCookie(r.email()) // 访问令牌 15 分钟，时钟已经走远
	approver := browser
	deviceCode, userCode = r.startDevice(nil)
	r.expect(r.approve(browser, userCode, true), 200, "")
	r.clock.Advance(10*time.Minute + time.Second)
	if got := r.pollStatus(deviceCode); got != "expired" {
		t.Fatalf("批准后过期：%s", got)
	}
	browser = r.registerCookie(r.email())
	var devices int
	_ = r.run.DB.Pool().QueryRow(context.Background(), `SELECT count(*) FROM devices WHERE subject = $1`, approver.accountID).Scan(&devices)
	if devices != 1 {
		t.Fatalf("过期不该建设备：%d", devices)
	}

	// 未知的设备码与过期同答，不泄露码是否存在。
	if got := r.pollStatus("not-a-device-code"); got != "expired" {
		t.Fatalf("未知码：%s", got)
	}
	// 用户码不存在或形状不对：404。
	r.expect(r.deviceGet(browser, "BCDF-GHJK"), 404, "not_found")
	r.expect(r.deviceGet(browser, "AEIO-0000"), 404, "not_found")
	r.expect(r.deviceGet(browser, ""), 404, "not_found")
}

func TestDeviceCodeUserCodeInput(t *testing.T) {
	r := newRig(t, rigOptions{})
	browser := r.registerCookie(r.email())
	deviceCode, userCode := r.startDevice(nil)
	// 小写、没有连字符、带空白都认。
	typed := strings.ToLower(strings.ReplaceAll(userCode, "-", "")) + " "
	r.expect(r.deviceGet(browser, typed), 200, "")
	r.expect(r.approve(browser, " "+strings.ToLower(userCode), true), 200, "")
	r.clock.Advance(pollInterval)
	if got := r.pollStatus(deviceCode); got != "approved" {
		t.Fatalf("状态 %s", got)
	}
}

func TestDeviceCodeUserCodeReuseAfterExpiry(t *testing.T) {
	r := newRig(t, rigOptions{})
	_, userCode := r.startDevice(nil)
	// 过期但还停在 pending 的行占着唯一索引：新的发起撞上同一个用户码时，旧行先落成 expired 再插入。
	r.clock.Advance(11 * time.Minute)
	var expired int
	_ = r.run.DB.Pool().QueryRow(context.Background(), `SELECT count(*) FROM device_codes WHERE user_code = $1 AND status = 'pending'`, userCode).Scan(&expired)
	if expired != 1 {
		t.Fatalf("前置：%d", expired)
	}
	// 直接用仓储复现冲突：同一用户码再插一行。
	err := r.run.DB.SaaS().Tx(context.Background(), func(tx saasTx) error {
		inserted, err := tx.DeviceCodes().Create(context.Background(), "cloud", newDeviceCodeRow("h2", userCode, r.clock.Ms()), r.clock.Ms())
		if err != nil || !inserted {
			return fmt.Errorf("过期的旧行不该挡住新行：%v %v", inserted, err)
		}
		// 现在有效的行再撞：返回 false，不报错。
		inserted, err = tx.DeviceCodes().Create(context.Background(), "cloud", newDeviceCodeRow("h3", userCode, r.clock.Ms()), r.clock.Ms())
		if err != nil || inserted {
			return fmt.Errorf("有效的行必须挡住：%v %v", inserted, err)
		}
		// 清理作业用的删除：只删过期早于截止时间的行。
		if n, err := tx.DeviceCodes().DeleteExpiredBefore(context.Background(), "cloud", r.clock.Ms()+24*3600*1000); err != nil || n < 1 {
			return fmt.Errorf("清理：%d %v", n, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDeviceCodeSecurity(t *testing.T) {
	r := newRig(t, rigOptions{})
	browser := r.registerCookie(r.email())
	deviceCode, userCode := r.startDevice(nil)
	body := map[string]any{"userCode": userCode, "approve": true}

	// 没有会话 → 401；授权页同样。
	r.expect(r.do("POST", "/v1/auth/device/approve", body, nil), 401, "unauthenticated")
	r.expect(r.do("POST", "/v1/auth/device/deny", map[string]any{"userCode": userCode}, nil), 401, "unauthenticated")
	r.expect(r.do("GET", "/v1/auth/device?userCode="+userCode, nil, nil), 401, "unauthenticated")
	// Cookie 写请求缺 CSRF / CSRF 错值 → 403。
	cookie := hdr("Origin", appOrigin, "Cookie", browser.cookieHeader("__Host-armadra-cloud"))
	r.expect(r.do("POST", "/v1/auth/device/approve", body, cookie), 403, "forbidden")
	r.expect(r.do("POST", "/v1/auth/device/approve", body, cookie.add("X-Armadra-CSRF", "x"+browser.csrf)), 403, "forbidden")
	// Origin 缺失、重复、为兄弟子域 → 403。
	noOrigin := hdr("X-Armadra-CSRF", browser.csrf, "Cookie", browser.cookieHeader("__Host-armadra-cloud"))
	r.expect(r.do("POST", "/v1/auth/device/approve", body, noOrigin), 403, "forbidden")
	r.expect(r.do("POST", "/v1/auth/device/approve", body, noOrigin.add("Origin", appOrigin, "Origin", appOrigin)), 403, "forbidden")
	r.expect(r.do("POST", "/v1/auth/device/approve", body, noOrigin.add("Origin", "https://x.src.localhost")), 403, "forbidden")
	r.expect(r.do("POST", "/v1/auth/device/deny", map[string]any{"userCode": userCode}, noOrigin.add("Origin", "https://x.src.localhost")), 403, "forbidden")
	// Cookie 与 Authorization 同带 → 400。
	r.expect(r.do("POST", "/v1/auth/device/approve", body, browser.write().add("Authorization", "Bearer "+browser.access)), 400, "bad_request")
	// 被拒的尝试什么都没改：仍是 pending。
	if got := r.pollStatus(deviceCode); got != "pending" {
		t.Fatalf("被拒的批准改了状态：%s", got)
	}
	// 另一个账号也能批准（任何已登录账号批准的就是它自己的账号），但会话属于批准者。
	other := r.registerCookie(r.email())
	r.expect(r.approve(other, userCode, true), 200, "")
	r.clock.Advance(pollInterval)
	res := r.poll(deviceCode)
	r.expect(res, 200, "")
	got := parseSession(r, response{status: 200, header: res.header, json: map[string]any{"session": res.json["session"]}})
	if got.accountID != other.accountID {
		t.Fatal("会话该属于批准的账号")
	}
}

func TestDeviceCodeApproverDisabledBeforeRedeem(t *testing.T) {
	r := newRig(t, rigOptions{})
	email := r.email()
	browser := r.registerCookie(email)
	deviceCode, userCode := r.startDevice(nil)
	r.expect(r.approve(browser, userCode, true), 200, "")
	r.exec("UPDATE accounts SET status = 'disabled' WHERE email = $1", email)
	if got := r.pollStatus(deviceCode); got != "denied" {
		t.Fatalf("批准账号被停用后：%s", got)
	}
	var devices int
	_ = r.run.DB.Pool().QueryRow(context.Background(), `SELECT count(*) FROM devices WHERE subject = $1`, browser.accountID).Scan(&devices)
	if devices != 1 {
		t.Fatalf("不该为停用账号建设备：%d", devices)
	}
}

func TestDeviceCodeRateLimits(t *testing.T) {
	r := newRig(t, rigOptions{})
	browser := r.registerCookie(r.email())

	// 发起：每 IP 每小时 30 次；换个 IP 不受影响。
	other := hdr("X-Forwarded-For", "203.0.113.9")
	for i := 0; i < 30; i++ {
		r.startDevice(nil)
	}
	over := r.do("POST", "/v1/auth/device/start", map[string]any{"device": map[string]any{"platform": "desktop", "name": "x"}}, nil)
	r.expect(over, 429, "rate_limited")
	if over.header.Get("Retry-After") == "" {
		t.Fatal("429 要带 Retry-After")
	}
	r.startDevice(other)

	// 猜码的 IP 桶：授权页每 IP 每分钟 30 次，批准与拒绝共用同一个桶。账号桶（10 次失败 / 小时）
	// 会先于它触发，所以这里每个账号只猜 8 次，换账号继续。
	guess := func(n int) session {
		s := r.registerCookie(r.email())
		for i := 0; i < n; i++ {
			r.expect(r.deviceGet(s, "BCDF-GHJK"), 404, "not_found")
		}
		return s
	}
	guess(8)
	guess(8)
	guess(8)
	last := guess(4) // 共 28 次
	r.expect(r.approve(last, "BCDF-GHJL", true), 404, "not_found")
	r.expect(r.deviceGet(last, "BCDF-GHJK"), 404, "not_found")
	r.expect(r.deviceGet(last, "BCDF-GHJK"), 429, "rate_limited")
	r.expect(r.approve(last, "BCDF-GHJM", true), 429, "rate_limited")
	r.expect(r.do("POST", "/v1/auth/device/deny", map[string]any{"userCode": "BCDF-GHJM"}, last.write()), 429, "rate_limited")
	_ = browser
}

func TestDeviceCodeGuessLimitPerAccount(t *testing.T) {
	r := newRig(t, rigOptions{})
	s := r.registerCookie(r.email())
	_, real := r.startDevice(nil)
	// 同一账号每小时 10 次查无此码；换 IP 也一样（账号桶不看 IP）。批准与拒绝的失败同样计数。
	for i := 0; i < 4; i++ {
		r.expect(r.do("GET", "/v1/auth/device?userCode=BCDF-GHJK", nil, s.cookieRead().add("X-Forwarded-For", fmt.Sprintf("198.51.100.%d", i))), 404, "not_found")
	}
	for i := 0; i < 3; i++ {
		r.expect(r.do("POST", "/v1/auth/device/approve", map[string]any{"userCode": "BCDF-GHJL", "approve": true}, s.write().add("X-Forwarded-For", fmt.Sprintf("198.51.101.%d", i))), 404, "not_found")
	}
	for i := 0; i < 3; i++ {
		r.expect(r.do("POST", "/v1/auth/device/deny", map[string]any{"userCode": "BCDF-GHJM"}, s.write().add("X-Forwarded-For", fmt.Sprintf("198.51.102.%d", i))), 404, "not_found")
	}
	other := hdr("X-Forwarded-For", "198.51.103.7")
	blocked := r.do("GET", "/v1/auth/device?userCode="+real, nil, s.cookieRead().add("X-Forwarded-For", "198.51.103.7"))
	r.expect(blocked, 429, "rate_limited")
	if blocked.header.Get("Retry-After") == "" {
		t.Fatal("429 要带 Retry-After")
	}
	// 满了之后连真码也进不去（批准一样）；别的账号不受影响。
	r.expect(r.do("POST", "/v1/auth/device/approve", map[string]any{"userCode": real, "approve": true}, s.write().add("X-Forwarded-For", "198.51.103.8")), 429, "rate_limited")
	_ = other
	fresh := r.registerCookie(r.email())
	r.expect(r.deviceGet(fresh, real), 200, "")
	// 过期（410）不计入账号桶。
	r.clock.Advance(11 * time.Minute)
	fresh = r.registerCookie(r.email())
	for i := 0; i < 12; i++ {
		r.expect(r.deviceGet(fresh, real), 410, "device_code_expired")
	}
}

func TestDeviceCodeApproveNeedsCookieOrElevatedBearer(t *testing.T) {
	r := newRig(t, rigOptions{})
	native := r.registerBearer(r.email())
	deviceCode, userCode := r.startDevice(nil)
	body := map[string]any{"userCode": userCode, "approve": true}

	// Bearer 会话没提权：批准 401 mfa_required；授权页与拒绝不受限。
	r.expect(r.do("POST", "/v1/auth/device/approve", body, native.bearer()), 401, "mfa_required")
	if got := r.pollStatus(deviceCode); got != "pending" {
		t.Fatalf("被拒的批准改了状态：%s", got)
	}
	r.expect(r.do("GET", "/v1/auth/device?userCode="+userCode, nil, native.bearer()), 200, "")
	// 提权后（10 分钟窗口内）可批准；窗口过了又不行。
	r.expect(r.do("POST", "/v1/auth/step-up", map[string]any{"password": password}, native.bearer()), 200, "")
	r.expect(r.do("POST", "/v1/auth/device/approve", body, native.bearer()), 200, "")
	r.clock.Advance(pollInterval)
	if got := r.pollStatus(deviceCode); got != "approved" {
		t.Fatalf("状态 %s", got)
	}
	r.clock.Advance(10 * time.Minute)
	native.access = r.do("POST", "/v1/auth/refresh", map[string]any{"refreshToken": native.refresh}, nil).json["session"].(map[string]any)["accessToken"].(string)
	_, second := r.startDevice(nil)
	r.expect(r.do("POST", "/v1/auth/device/approve", map[string]any{"userCode": second, "approve": true}, native.bearer()), 401, "mfa_required")
	// Bearer 拒绝不需要提权。
	r.expect(r.do("POST", "/v1/auth/device/deny", map[string]any{"userCode": second}, native.bearer()), 200, "")
}

func TestDevicePollIPLimit(t *testing.T) {
	r := newRig(t, rigOptions{})
	// 每 IP 每分钟 120 次，在开事务之前就判；未知码本身不触发别的限速。
	for i := 0; i < 120; i++ {
		if got := r.pollStatus(fmt.Sprintf("unknown-%d", i)); got != "expired" {
			t.Fatalf("状态 %s", got)
		}
	}
	over := r.poll("unknown-x")
	r.expect(over, 429, "rate_limited")
	if over.header.Get("Retry-After") == "" {
		t.Fatal("429 要带 Retry-After")
	}
	r.expect(r.do("POST", "/v1/auth/device/poll", map[string]any{"deviceCode": "y"}, hdr("X-Forwarded-For", "203.0.113.77")), 200, "")
}

func TestDeviceCodeRedeemOnceUnderConcurrency(t *testing.T) {
	r := newRig(t, rigOptions{})
	browser := r.registerCookie(r.email())
	deviceCode, userCode := r.startDevice(nil)
	r.expect(r.approve(browser, userCode, true), 200, "")

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		approved int
	)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := r.poll(deviceCode)
			mu.Lock()
			defer mu.Unlock()
			if res.status == 200 && res.json["status"] == "approved" {
				approved++
			}
		}()
	}
	wg.Wait()
	if approved != 1 {
		t.Fatalf("并发轮询只能兑换一次：%d", approved)
	}
	var devices int
	_ = r.run.DB.Pool().QueryRow(context.Background(), `SELECT count(*) FROM devices WHERE subject = $1`, browser.accountID).Scan(&devices)
	if devices != 2 {
		t.Fatalf("设备数 %d，应为浏览器 + 新设备", devices)
	}
}

func TestDeviceCodeStartValidation(t *testing.T) {
	r := newRig(t, rigOptions{})
	r.expect(r.do("POST", "/v1/auth/device/start", map[string]any{"device": map[string]any{"platform": "toaster", "name": "x"}}, nil), 400, "bad_request")
	r.expect(r.do("POST", "/v1/auth/device/start", map[string]any{"device": map[string]any{"platform": "desktop", "name": "   "}}, nil), 400, "bad_request")
	r.expect(r.do("POST", "/v1/auth/device/start", map[string]any{}, nil), 400, "bad_request")
	r.expect(r.do("POST", "/v1/auth/device/poll", map[string]any{}, nil), 400, "bad_request")
}
