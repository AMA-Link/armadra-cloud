package mail_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/mail"
)

// SMTP 适配器对真的 mailpit：MAILPIT_SMTP_ADDR / MAILPIT_API_URL 来自 pnpm dev:up；没有就跳过，CI 里一律失败。
// mailpit 是共用的：随机收件地址，只按 to: 查自己的信。
func mailpitEnv(t *testing.T) (host string, port int, api string) {
	t.Helper()
	addr, api := os.Getenv("MAILPIT_SMTP_ADDR"), os.Getenv("MAILPIT_API_URL")
	if addr == "" || api == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("CI 里没有 MAILPIT_SMTP_ADDR / MAILPIT_API_URL")
		}
		t.Skip("没有 mailpit：设置 MAILPIT_SMTP_ADDR 与 MAILPIT_API_URL（pnpm dev:up 的实例），跳过")
	}
	h, p, _ := strings.Cut(addr, ":")
	n, err := strconv.Atoi(p)
	if err != nil {
		t.Fatal(err)
	}
	return h, n, strings.TrimRight(api, "/")
}

func apiGet(t *testing.T, api, path string, out any) {
	t.Helper()
	res, err := http.Get(api + path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	data, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || json.Unmarshal(data, out) != nil {
		t.Fatalf("mailpit %s：%d %s", path, res.StatusCode, data)
	}
}

func TestSMTPSenderDeliversToMailpit(t *testing.T) {
	host, port, api := mailpitEnv(t)
	sender, err := mail.NewSMTP(mail.SMTPOptions{Host: host, Port: port, From: "Armadra <no-reply@armadra.test>", TLS: "none"})
	if err != nil {
		t.Fatal(err)
	}
	to := fmt.Sprintf("smtp%08x@example.test", rand.Uint32())
	message := mail.Message{
		MessageID: fmt.Sprintf("%08x@app.example.test", rand.Uint32()), To: to, Subject: "验证你的邮箱",
		Text: "你好。\n链接：https://app.example.test/verify#abc\n", HTML: "<p>你好。</p>\n",
	}
	if err := sender.Send(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	var list struct{ Messages []struct{ ID string } }
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline) && len(list.Messages) == 0; time.Sleep(50 * time.Millisecond) {
		apiGet(t, api, "/api/v1/search?query="+url.QueryEscape("to:"+to), &list)
	}
	if len(list.Messages) != 1 {
		t.Fatalf("mailpit 里该有 1 封：%d", len(list.Messages))
	}
	var got struct {
		MessageID, Subject, Text, HTML string
		From                           struct{ Address string }
	}
	apiGet(t, api, "/api/v1/message/"+list.Messages[0].ID, &got)
	if got.Subject != message.Subject || got.MessageID != message.MessageID || got.From.Address != "no-reply@armadra.test" ||
		!strings.Contains(got.Text, "https://app.example.test/verify#abc") || !strings.Contains(got.HTML, "<p>你好。</p>") {
		t.Fatalf("收到的邮件：%+v", got)
	}
}

func TestSMTPSenderFailsOnClosedPortAndBadRecipient(t *testing.T) {
	host, port, _ := mailpitEnv(t)
	closed, _ := mail.NewSMTP(mail.SMTPOptions{Host: "127.0.0.1", Port: 1, From: "no-reply@armadra.test", TLS: "none", Timeout: 2 * time.Second})
	if err := closed.Send(context.Background(), mail.Message{MessageID: "a@b", To: "ann@example.test", Subject: "x"}); err == nil {
		t.Fatal("连不上应报错")
	}
	sender, _ := mail.NewSMTP(mail.SMTPOptions{Host: host, Port: port, From: "no-reply@armadra.test", TLS: "none"})
	for _, to := range []string{"not an address", "ann@example.test\r\nBcc: evil@example.test"} {
		if err := sender.Send(context.Background(), mail.Message{MessageID: "a@b", To: to, Subject: "x"}); err == nil {
			t.Fatalf("收件地址 %q 应拒绝", to)
		}
	}
}
