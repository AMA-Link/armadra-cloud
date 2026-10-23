package mail

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Message 是一封待发的邮件。
type Message struct {
	// MessageID 不含尖括号；重试用同一个，提供方可去重。
	MessageID string
	To        string
	Subject   string
	Text      string
	HTML      string
}

// Sender 把邮件交给提供方；失败返回错误，由作业重试。
type Sender interface {
	Send(ctx context.Context, message Message) error
}

// ErrNotConfigured 是没配 SMTP 时发信的错误：邮件留在发件箱里按退避重试，配好 SMTP 重启后补发。
var ErrNotConfigured = errors.New("没有配置 SMTP")

type disabledSender struct{}

func (disabledSender) Send(context.Context, Message) error { return ErrNotConfigured }

// Disabled 是没配 SMTP 时的发信方：每次都失败，作业按退避重试。
func Disabled() Sender { return disabledSender{} }

// MemorySender 把邮件记在内存里（测试用）。
type MemorySender struct {
	mu       sync.Mutex
	messages []Message
	failNext int
}

var _ Sender = (*MemorySender)(nil)

// Send 实现 Sender。
func (m *MemorySender) Send(_ context.Context, message Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failNext > 0 {
		m.failNext--
		return errors.New("memory sender: 按测试要求失败")
	}
	m.messages = append(m.messages, message)
	return nil
}

// FailNext 让接下来的 n 次发送失败。
func (m *MemorySender) FailNext(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failNext = n
}

// Messages 是已发出的邮件副本。
func (m *MemorySender) Messages() []Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Message(nil), m.messages...)
}

// SMTPOptions 是 SMTP 适配器的配置。
type SMTPOptions struct {
	Host     string
	Port     int
	Username string
	Password string
	// From 形如 "name@example.com" 或 "Name <name@example.com>"。
	From string
	// TLS 取 starttls | tls | none。
	TLS string
	// Timeout 是连接与整封邮件的上限；缺省 30 秒。
	Timeout time.Duration
	// Now 缺省 time.Now（Date 头）。
	Now func() time.Time
}

type smtpSender struct {
	o    SMTPOptions
	from *mail.Address
}

// NewSMTP 建 SMTP 适配器（标准库 net/smtp + crypto/tls）。
func NewSMTP(options SMTPOptions) (Sender, error) {
	from, err := mail.ParseAddress(options.From)
	if err != nil {
		return nil, errors.New("SMTP 发件人地址不合法")
	}
	if options.Timeout <= 0 {
		options.Timeout = 30 * time.Second
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &smtpSender{o: options, from: from}, nil
}

func (s *smtpSender) Send(ctx context.Context, message Message) error {
	to, err := mail.ParseAddress(message.To)
	if err != nil || strings.ContainsAny(message.To, "\r\n") {
		return errors.New("收件地址不合法")
	}
	body, err := s.build(message, to)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, s.o.Timeout)
	defer cancel()
	addr := net.JoinHostPort(s.o.Host, strconv.Itoa(s.o.Port))
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	// 超时或取消时关连接，打断阻塞中的读写。
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	tlsConfig := &tls.Config{ServerName: s.o.Host, MinVersion: tls.VersionTLS12}
	if s.o.TLS == "tls" {
		conn = tls.Client(conn, tlsConfig)
	}
	client, err := smtp.NewClient(conn, s.o.Host)
	if err != nil {
		_ = conn.Close()
		return err
	}
	defer func() { _ = client.Close() }()
	if s.o.TLS == "starttls" {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return errors.New("服务器不支持 STARTTLS")
		}
		if err := client.StartTLS(tlsConfig); err != nil {
			return err
		}
	}
	if s.o.Username != "" {
		if err := client.Auth(smtp.PlainAuth("", s.o.Username, s.o.Password, s.o.Host)); err != nil {
			return err
		}
	}
	if err := client.Mail(s.from.Address); err != nil {
		return err
	}
	if err := client.Rcpt(to.Address); err != nil {
		return err
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(body); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return client.Quit()
}

// build 拼 MIME 邮件：multipart/alternative（纯文本 + HTML），正文 quoted-printable，主题按 RFC 2047 编码。
func (s *smtpSender) build(m Message, to *mail.Address) ([]byte, error) {
	if strings.ContainsAny(m.Subject, "\r\n") || strings.ContainsAny(m.MessageID, "\r\n<> ") {
		return nil, errors.New("邮件头里有非法字符")
	}
	var boundary [12]byte
	if _, err := rand.Read(boundary[:]); err != nil {
		return nil, err
	}
	sep := "armadra-" + hex.EncodeToString(boundary[:])
	var b bytes.Buffer
	header := func(name, value string) { fmt.Fprintf(&b, "%s: %s\r\n", name, value) }
	header("From", s.from.String())
	header("To", to.String())
	header("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	header("Date", s.o.Now().UTC().Format(time.RFC1123Z))
	header("Message-ID", "<"+m.MessageID+">")
	header("MIME-Version", "1.0")
	header("Auto-Submitted", "auto-generated")
	header("Content-Type", `multipart/alternative; boundary="`+sep+`"`)
	b.WriteString("\r\n")
	part := func(contentType, body string) error {
		fmt.Fprintf(&b, "--%s\r\nContent-Type: %s; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n", sep, contentType)
		w := quotedprintable.NewWriter(&b)
		if _, err := w.Write([]byte(body)); err != nil {
			return err
		}
		if err := w.Close(); err != nil {
			return err
		}
		b.WriteString("\r\n")
		return nil
	}
	if err := part("text/plain", m.Text); err != nil {
		return nil, err
	}
	if err := part("text/html", m.HTML); err != nil {
		return nil, err
	}
	fmt.Fprintf(&b, "--%s--\r\n", sep)
	return b.Bytes(), nil
}
