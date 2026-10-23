package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// mailpit 集成：真发 SMTP，经 mailpit 的 HTTP API 取信。变量来自 pnpm dev:up 的实例：
// MAILPIT_SMTP_ADDR（127.0.0.1:1026）与 MAILPIT_API_URL（http://127.0.0.1:8026）；没有就跳过，CI 里一律失败。
// mailpit 是多个测试共用的：每个测试用随机收件地址，只按 to: 查自己的信，不清空信箱。

type mailpitClient struct {
	t   *testing.T
	api string
}

func mailpitEnv(t *testing.T) (smtpAddr string, client *mailpitClient) {
	t.Helper()
	smtpAddr, api := os.Getenv("MAILPIT_SMTP_ADDR"), os.Getenv("MAILPIT_API_URL")
	if smtpAddr == "" || api == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("CI 里没有 MAILPIT_SMTP_ADDR / MAILPIT_API_URL")
		}
		t.Skip("没有 mailpit：设置 MAILPIT_SMTP_ADDR 与 MAILPIT_API_URL（pnpm dev:up 的实例），跳过")
	}
	return smtpAddr, &mailpitClient{t: t, api: strings.TrimRight(api, "/")}
}

func (m *mailpitClient) get(path string, out any) {
	m.t.Helper()
	res, err := http.Get(m.api + path)
	if err != nil {
		m.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	data, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || json.Unmarshal(data, out) != nil {
		m.t.Fatalf("mailpit %s：%d %s", path, res.StatusCode, data)
	}
}

type mailpitMessage struct {
	ID        string
	MessageID string
	Subject   string
	Text      string
	HTML      string
	To        []struct{ Address string }
	From      struct{ Address string }
}

// wait 等到发给 to 的信有 n 封，答完整内容（按时间升序）。
func (m *mailpitClient) wait(to string, n int) []mailpitMessage {
	m.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var list struct{ Messages []struct{ ID string } }
		m.get("/api/v1/search?query="+url.QueryEscape("to:"+to), &list)
		if len(list.Messages) >= n {
			var out []mailpitMessage
			for i := len(list.Messages) - 1; i >= 0; i-- { // 搜索结果新的在前
				var full mailpitMessage
				m.get("/api/v1/message/"+list.Messages[i].ID, &full)
				out = append(out, full)
			}
			return out
		}
		time.Sleep(50 * time.Millisecond)
	}
	m.t.Fatalf("mailpit 里等不到发给 %s 的 %d 封信", to, n)
	return nil
}

func mailpitRig(t *testing.T) (*rig, *mailpitClient) {
	smtpAddr, client := mailpitEnv(t)
	host, port, _ := strings.Cut(smtpAddr, ":")
	r := newRig(t, rigOptions{env: map[string]string{
		"SMTP_HOST": host, "SMTP_PORT": port, "SMTP_TLS": "none", "SMTP_FROM": "Armadra <no-reply@armadra.test>",
	}})
	return r, client
}

func TestMailpitRegisterVerifyAndReset(t *testing.T) {
	r, mailpit := mailpitRig(t)
	email := r.email()
	first := r.register(email, hdr("Accept-Language", "en"))
	r.expect(first, 200, "")
	s := parseSession(r, first)

	verify := mailpit.wait(email, 1)[0]
	if verify.Subject != "Verify your email" || verify.From.Address != "no-reply@armadra.test" || len(verify.To) != 1 || verify.To[0].Address != email {
		t.Fatalf("验证邮件：%+v", verify)
	}
	if !strings.HasSuffix(verify.MessageID, "@127.0.0.1:8100") {
		t.Fatalf("Message-ID：%q", verify.MessageID)
	}
	token1 := linkToken(t, verify.Text, "/verify")
	if !strings.Contains(verify.HTML, appOrigin+"/verify#"+token1) {
		t.Fatalf("HTML 部分带同一个链接：%s", verify.HTML)
	}

	// 重发后旧链接失效，新链接可用。
	r.expect(r.do("POST", "/v1/auth/email/resend", nil, s.bearer()), 200, "")
	token2 := linkToken(t, mailpit.wait(email, 2)[1].Text, "/verify")
	r.expect(r.verifyEmail(token1), 400, "email_token_invalid")
	r.expect(r.verifyEmail(token2), 200, "")

	// 找回口令：英文邮件，完成后收到口令已更改通知。
	r.expect(r.resetStart(email), 200, "")
	reset := mailpit.wait(email, 3)[2]
	if reset.Subject != "Reset your password" {
		t.Fatalf("找回邮件：%+v", reset)
	}
	resetToken := linkToken(t, reset.Text, "/reset")
	r.expect(r.resetFinish(resetToken, newPassword), 200, "")
	if done := mailpit.wait(email, 4)[3]; done.Subject != "Password changed" {
		t.Fatalf("通知：%+v", done)
	}
	r.expect(r.me(s), 401, "")
	r.noPlaintext(token1, token2, resetToken)
}

func TestMailpitChineseMessageAndNewDevice(t *testing.T) {
	r, mailpit := mailpitRig(t)
	email := r.email()
	r.registerBearer(email)
	verify := mailpit.wait(email, 1)[0]
	if verify.Subject != "验证你的邮箱" || !strings.Contains(verify.Text, "测试用户，你好。") || !strings.Contains(verify.HTML, "<p>") {
		t.Fatalf("中文邮件（RFC 2047 主题与 UTF-8 正文）：%+v", verify)
	}
	res := r.do("POST", "/v1/auth/login", map[string]any{"account": email, "password": password, "device": map[string]any{"platform": "desktop", "name": "办公室电脑"}}, nil)
	r.expect(res, 200, "")
	device := mailpit.wait(email, 2)[1]
	if device.Subject != "新设备登录" || !strings.Contains(device.Text, "办公室电脑") {
		t.Fatalf("新设备提醒：%+v", device)
	}
}

func linkToken(t *testing.T, body, path string) string {
	t.Helper()
	link := linkPattern.FindString(body)
	u, err := url.Parse(link)
	if err != nil || u.Fragment == "" || u.RawQuery != "" || u.Path != path {
		t.Fatalf("链接要是 …%s#令牌：%q", path, link)
	}
	return u.Fragment
}
