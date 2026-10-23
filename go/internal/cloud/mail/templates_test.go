package mail

import (
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"testing"
	"time"
)

var tokenTemplates = map[string]bool{VerifyEmail: true, EmailChange: true, PasswordReset: true}

func TestBothLocalesHaveTheSameTemplates(t *testing.T) {
	templates, err := LoadTemplates()
	if err != nil {
		t.Fatal(err)
	}
	zh, en := templates.Names(LocaleZH), templates.Names(LocaleEN)
	if strings.Join(zh, ",") != strings.Join(en, ",") {
		t.Fatalf("两种语言的模板集合必须相同：%v / %v", zh, en)
	}
	for _, name := range []string{VerifyEmail, EmailChange, EmailChanged, PasswordReset, PasswordChanged, NewDevice} {
		found := false
		for _, have := range zh {
			found = found || have == name
		}
		if !found {
			t.Errorf("代码用到的模板 %s 不存在", name)
		}
	}
}

func TestRenderEveryTemplateInBothLocales(t *testing.T) {
	templates, err := LoadTemplates()
	if err != nil {
		t.Fatal(err)
	}
	const link = "https://app.example.test/verify#TOKEN-IN-FRAGMENT"
	for _, locale := range Locales {
		for _, name := range templates.Names(locale) {
			data := Data{DisplayName: "小明", NewEmail: "new@example.test", DeviceName: "工作笔记本", Platform: "desktop"}
			if tokenTemplates[name] {
				data.Link = link
			}
			subject, text, html, err := templates.Render(locale, name, data)
			if err != nil {
				t.Fatalf("%s/%s：%v", locale, name, err)
			}
			for _, part := range []string{subject, text, html} {
				if part == "" || strings.Contains(part, "<no value>") || strings.Contains(part, "{{") {
					t.Fatalf("%s/%s 渲染不完整：%q", locale, name, part)
				}
			}
			if !strings.Contains(text, "小明") || !strings.Contains(html, "小明") {
				t.Errorf("%s/%s 应带显示名", locale, name)
			}
			if strings.ContainsAny(subject, "\r\n") {
				t.Errorf("%s/%s 主题不能换行", locale, name)
			}
			// 令牌只在链接的 # 片段里；不带令牌的模板里没有链接。
			if tokenTemplates[name] != strings.Contains(text, link) || tokenTemplates[name] != strings.Contains(html, link) {
				t.Errorf("%s/%s 的链接与带令牌的约定不符", locale, name)
			}
			if !tokenTemplates[name] && strings.Contains(text+html, "#") {
				t.Errorf("%s/%s 不带令牌，不该有片段", locale, name)
			}
		}
	}
}

// 双语模板的渲染快照（不含令牌）。
func TestRenderSnapshots(t *testing.T) {
	templates, _ := LoadTemplates()
	subjects := map[string]map[string]string{
		LocaleZH: {
			VerifyEmail: "验证你的邮箱", EmailChange: "确认新的邮箱地址", EmailChanged: "账号邮箱已更改",
			PasswordReset: "重设口令", PasswordChanged: "口令已更改", NewDevice: "新设备登录",
		},
		LocaleEN: {
			VerifyEmail: "Verify your email", EmailChange: "Confirm your new email address", EmailChanged: "Account email changed",
			PasswordReset: "Reset your password", PasswordChanged: "Password changed", NewDevice: "New device sign-in",
		},
	}
	for locale, byName := range subjects {
		for name, want := range byName {
			subject, _, _, err := templates.Render(locale, name, Data{DisplayName: "Ann", Link: "L"})
			if err != nil || subject != want {
				t.Errorf("%s/%s 主题：%q，应为 %q（%v）", locale, name, subject, want, err)
			}
		}
	}
	_, text, _, _ := templates.Render(LocaleZH, VerifyEmail, Data{DisplayName: "Ann", Link: "https://x.test/verify#T"})
	if text != "Ann，你好。\n\n打开下面的链接验证邮箱，24 小时内有效：\nhttps://x.test/verify#T\n\n如果不是你本人的操作，请忽略这封邮件。\n" {
		t.Errorf("中文快照：%q", text)
	}
	_, text, _, _ = templates.Render(LocaleEN, PasswordReset, Data{DisplayName: "Ann", Link: "https://x.test/reset#T"})
	if text != "Hi Ann,\n\nOpen the link below to reset your password. It expires in 30 minutes:\nhttps://x.test/reset#T\n\nIf this was not you, ignore this email. Your password stays the same.\n" {
		t.Errorf("英文快照：%q", text)
	}
	// 语言不认识时退回中文。
	subject, _, _, _ := templates.Render("fr", VerifyEmail, Data{})
	if subject != "验证你的邮箱" {
		t.Errorf("退回中文：%q", subject)
	}
}

func TestHTMLEscapesParams(t *testing.T) {
	templates, _ := LoadTemplates()
	_, _, html, err := templates.Render(LocaleEN, NewDevice, Data{DisplayName: "<b>x</b>", DeviceName: `"><script>`, Platform: "web"})
	if err != nil || strings.Contains(html, "<script>") || strings.Contains(html, "<b>x</b>") {
		t.Fatalf("HTML 要转义参数：%s %v", html, err)
	}
}

func TestSMTPMessageIsWellFormed(t *testing.T) {
	sender, err := NewSMTP(SMTPOptions{Host: "127.0.0.1", Port: 25, From: "Armadra <no-reply@example.test>", TLS: "none",
		Now: func() time.Time { return time.Date(2026, 10, 23, 1, 2, 3, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	s := sender.(*smtpSender)
	to, _ := mail.ParseAddress("ann@example.test")
	raw, err := s.build(Message{MessageID: "abc@app.example.test", To: "ann@example.test", Subject: "验证你的邮箱", Text: "你好。\n链接：https://x.test/verify#T\n", HTML: "<p>你好。</p>\n"}, to)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := new(mime.WordDecoder).DecodeHeader(parsed.Header.Get("Subject"))
	if err != nil || decoded != "验证你的邮箱" {
		t.Fatalf("主题：%q %v", decoded, err)
	}
	if parsed.Header.Get("Message-ID") != "<abc@app.example.test>" || parsed.Header.Get("Auto-Submitted") != "auto-generated" || parsed.Header.Get("To") == "" {
		t.Fatalf("头：%v", parsed.Header)
	}
	mediaType, params, err := mime.ParseMediaType(parsed.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/alternative" {
		t.Fatalf("Content-Type：%s %v", mediaType, err)
	}
	reader := multipart.NewReader(parsed.Body, params["boundary"])
	var texts []string
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(quotedprintable.NewReader(part))
		texts = append(texts, string(body))
	}
	if len(texts) != 2 || !strings.Contains(texts[0], "https://x.test/verify#T") || !strings.Contains(texts[1], "<p>") {
		t.Fatalf("两个部分：%q", texts)
	}
}

func TestSMTPRejectsHeaderInjection(t *testing.T) {
	sender, _ := NewSMTP(SMTPOptions{Host: "127.0.0.1", Port: 25, From: "no-reply@example.test", TLS: "none"})
	s := sender.(*smtpSender)
	to, _ := mail.ParseAddress("ann@example.test")
	for _, m := range []Message{
		{MessageID: "a@b", Subject: "x\r\nBcc: evil@example.test"},
		{MessageID: "a@b>\r\nBcc: evil@example.test", Subject: "x"},
	} {
		if _, err := s.build(m, to); err == nil {
			t.Errorf("应拒绝头注入：%+v", m)
		}
	}
	if _, err := NewSMTP(SMTPOptions{From: "not an address"}); err == nil {
		t.Error("发件人地址不合法应拒绝")
	}
}
