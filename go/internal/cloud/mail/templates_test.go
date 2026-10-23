package mail

import (
	"bufio"
	"context"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"strconv"
	"strings"
	"testing"
	"time"
)

var tokenTemplates = map[string]bool{VerifyEmail: true, EmailChange: true, PasswordReset: true, TeamInvitation: true, HostInvitation: true}

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
			data := Data{DisplayName: "小明", NewEmail: "new@example.test", DeviceName: "工作笔记本", Platform: "desktop",
				TeamName: "研发组", HostName: "构建机", InviterName: "小红", RoleLabel: RoleLabel(locale, "member")}
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
			// 令牌邮件可能发给还没验证的地址：不带显示名；其余带。
			if tokenTemplates[name] == (strings.Contains(text, "小明") || strings.Contains(html, "小明")) {
				t.Errorf("%s/%s 的显示名约定不符（令牌邮件不带，其余带）", locale, name)
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
			TeamInvitation: "加入团队「研发」的邀请", HostInvitation: "访问主机「构建机」的邀请",
		},
		LocaleEN: {
			VerifyEmail: "Verify your email", EmailChange: "Confirm your new email address", EmailChanged: "Account email changed",
			PasswordReset: "Reset your password", PasswordChanged: "Password changed", NewDevice: "New device sign-in",
			TeamInvitation: `Invitation to join the team "研发"`, HostInvitation: `Invitation to access the host "构建机"`,
		},
	}
	for locale, byName := range subjects {
		for name, want := range byName {
			subject, _, _, err := templates.Render(locale, name, Data{DisplayName: "Ann", Link: "L", TeamName: "研发", HostName: "构建机"})
			if err != nil || subject != want {
				t.Errorf("%s/%s 主题：%q，应为 %q（%v）", locale, name, subject, want, err)
			}
		}
	}
	_, text, _, _ := templates.Render(LocaleZH, VerifyEmail, Data{DisplayName: "Ann", Link: "https://x.test/verify#T"})
	if text != "你好。\n\n打开下面的链接验证邮箱，24 小时内有效：\nhttps://x.test/verify#T\n\n如果不是你本人的操作，请忽略这封邮件。\n" {
		t.Errorf("中文快照：%q", text)
	}
	_, text, _, _ = templates.Render(LocaleEN, PasswordReset, Data{DisplayName: "Ann", Link: "https://x.test/reset#T"})
	if text != "Hello,\n\nOpen the link below to reset your password. It expires in 30 minutes:\nhttps://x.test/reset#T\n\nIf this was not you, ignore this email. Your password stays the same.\n" {
		t.Errorf("英文快照：%q", text)
	}
	_, text, _, _ = templates.Render(LocaleZH, TeamInvitation, Data{TeamName: "研发", InviterName: "小红", RoleLabel: RoleLabel(LocaleZH, "guest"), Link: "https://x.test/invite/i1#T"})
	if text != "你好。\n\n小红 邀请你以访客身份加入团队「研发」。打开下面的链接查看并接受，7 天内有效：\nhttps://x.test/invite/i1#T\n\n如果你不认识邀请人，或不打算加入，请忽略这封邮件。\n" {
		t.Errorf("邀请中文快照：%q", text)
	}
	_, text, _, _ = templates.Render(LocaleEN, TeamInvitation, Data{TeamName: "R&D", RoleLabel: RoleLabel(LocaleEN, "admin"), Link: "https://x.test/invite/i1#T"})
	if !strings.Contains(text, "You are invited to join the team \"R&D\" as an admin.") {
		t.Errorf("邀请英文快照（没有邀请人）：%q", text)
	}
	_, text, _, _ = templates.Render(LocaleZH, HostInvitation, Data{HostName: "构建机", InviterName: "小红", RoleLabel: RoleLabel(LocaleZH, "editor"), Link: "https://x.test/invite/i2#T"})
	if text != "你好。\n\n小红 邀请你以编辑者身份访问主机「构建机」。打开下面的链接查看并接受，7 天内有效：\nhttps://x.test/invite/i2#T\n\n如果你不认识邀请人，或不打算接受，请忽略这封邮件。\n" {
		t.Errorf("主机邀请中文快照：%q", text)
	}
	_, text, _, _ = templates.Render(LocaleEN, HostInvitation, Data{HostName: "build box", RoleLabel: RoleLabel(LocaleEN, "driver"), Link: "https://x.test/invite/i2#T"})
	if !strings.Contains(text, "You are invited to access the host \"build box\" as a driver.") {
		t.Errorf("主机邀请英文快照（没有邀请人）：%q", text)
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

func TestSanitizeLabel(t *testing.T) {
	for in, want := range map[string]string{
		"我的 MacBook": "我的 MacBook",
		"点击 https://evil.example/login 领奖": "点击 领奖",
		"www.evil.test 办公室":                "办公室",
		"bob@evil.test 的电脑":                "的电脑",
		"login-now.evil.com/x":             "",
		"evil . com 抽奖":                    "抽奖",
		"evil。com 抽奖":                      "抽奖",
		"evil．io":                          "",
		"团队 a-b.net/x?y=1":                 "团队",
		"v1.2 版":                           "v1.2 版",
		"a<b>c\r\nd":                       "a b c d",
		strings.Repeat("机", 100):           strings.Repeat("机", 64),
	} {
		if got := SanitizeLabel(in, labelMax); got != want {
			t.Errorf("SanitizeLabel(%q) = %q，应为 %q", in, got, want)
		}
	}
}

func TestAttemptMessageID(t *testing.T) {
	if got := attemptMessageID("abc@app.example.test", 2); got != "abc.2@app.example.test" {
		t.Fatalf("%q", got)
	}
	if attemptMessageID("noat", 1) != "noat" {
		t.Fatal("没有 @ 原样")
	}
}

// 数据已被服务器接收（DATA 以 250 收尾）之后 QUIT 出错不算发信失败。
func TestSMTPIgnoresQuitFailureAfterDataAccepted(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		r := bufio.NewReader(conn)
		reply := func(line string) { _, _ = conn.Write([]byte(line + "\r\n")) }
		reply("220 test")
		inData := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			cmd := strings.ToUpper(strings.TrimSpace(line))
			switch {
			case inData:
				if cmd == "." {
					inData = false
					reply("250 queued")
				}
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
				reply("250 test")
			case strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"):
				reply("250 ok")
			case cmd == "DATA":
				inData = true
				reply("354 go")
			case cmd == "QUIT":
				return // 不答就断开
			}
		}
	}()
	host, portText, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portText)
	sender, _ := NewSMTP(SMTPOptions{Host: host, Port: port, From: "no-reply@armadra.test", TLS: "none", Timeout: 5 * time.Second})
	if err := sender.Send(context.Background(), Message{MessageID: "a@b", To: "ann@example.test", Subject: "x", Text: "t", HTML: "<p>t</p>"}); err != nil {
		t.Fatalf("QUIT 失败不该让已被接收的邮件算失败：%v", err)
	}
}

func TestGreetingWithoutNameIsGeneric(t *testing.T) {
	templates, _ := LoadTemplates()
	for _, locale := range Locales {
		for _, name := range []string{EmailChanged, PasswordChanged, NewDevice} {
			_, text, html, err := templates.Render(locale, name, Data{NewEmail: "n@example.test", DeviceName: "笔记本", Platform: "desktop"})
			if err != nil || strings.Contains(text, "，你好") && strings.HasPrefix(text, "，") || strings.Contains(text+html, "Hi ,") {
				t.Fatalf("%s/%s 没有名字时用通用问候：%q %v", locale, name, text, err)
			}
			want := "你好。"
			if locale == LocaleEN {
				want = "Hello,"
			}
			if !strings.HasPrefix(text, want) {
				t.Errorf("%s/%s 应以 %q 开头：%q", locale, name, want, text)
			}
		}
	}
}
