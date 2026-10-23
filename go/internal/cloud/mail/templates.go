package mail

import (
	"bytes"
	"embed"
	"fmt"
	htmltemplate "html/template"
	"io/fs"
	"path"
	"sort"
	"strings"
	texttemplate "text/template"
)

// 模板在 templates/<locale>/<name>.{subject,txt,html}（设计 §6.3）；两种语言的集合必须相同（测试守住）。
//
//go:embed templates
var templateFS embed.FS

// 界面语言（accounts.locale、outbox_emails.locale 的取值）。
const (
	LocaleZH = "zh-CN"
	LocaleEN = "en"
)

// Locales 是支持的语言。
var Locales = []string{LocaleZH, LocaleEN}

// 模板名。
const (
	VerifyEmail     = "verify_email"
	EmailChange     = "email_change"
	EmailChanged    = "email_changed"
	PasswordReset   = "password_reset"
	PasswordChanged = "password_changed"
	NewDevice       = "new_device"
	// TeamInvitation 是团队邀请邮件：链接带邀请 id 与一次性令牌（令牌在 # 片段里，S4）。
	TeamInvitation = "team_invitation"
	// HostInvitation 是主机邀请邮件（S6）：同团队邀请，链接带邀请 id 与一次性令牌。
	HostInvitation = "host_invitation"
)

// Data 是渲染模板的数据。令牌只出现在 Link 的 # 片段里，且只在内存中。
type Data struct {
	DisplayName string
	// Link 是带令牌片段的完整链接；不带令牌的模板为空。
	Link       string
	NewEmail   string
	DeviceName string
	Platform   string
	// TeamName、InviterName、RoleLabel 是团队邀请的：团队名与邀请人的显示名（都是用户填的字符串，
	// 渲染前已 SanitizeLabel），角色名按收件语言取（RoleLabel）。
	TeamName    string
	InviterName string
	RoleLabel   string
	// HostName 是主机邀请的主机名（用户填的字符串，渲染前已 SanitizeLabel）。
	HostName string
}

// roleLabels 是团队角色与主机角色在邮件里的叫法。
var roleLabels = map[string]map[string]string{
	LocaleZH: {
		"admin": "管理员", "member": "成员", "billing": "账单管理员", "guest": "访客",
		"viewer": "查看者", "editor": "编辑者", "operator": "操作者", "driver": "驱动者",
	},
	LocaleEN: {
		"admin": "an admin", "member": "a member", "billing": "a billing manager", "guest": "a guest",
		"viewer": "a viewer", "editor": "an editor", "operator": "an operator", "driver": "a driver",
	},
}

// RoleLabel 是团队角色在某种语言里的叫法；不认识的角色答空串，语言不认识时退回中文。
func RoleLabel(locale, role string) string {
	labels, ok := roleLabels[locale]
	if !ok {
		labels = roleLabels[LocaleZH]
	}
	return labels[role]
}

type compiled struct {
	subject *texttemplate.Template
	text    *texttemplate.Template
	html    *htmltemplate.Template
}

// Templates 是编译好的模板集合。
type Templates struct {
	byLocale map[string]map[string]compiled
}

// LoadTemplates 编译随包的模板；缺文件、语法错、两种语言的集合不同都报错。
func LoadTemplates() (*Templates, error) {
	t := &Templates{byLocale: map[string]map[string]compiled{}}
	for _, locale := range Locales {
		names, err := templateNames(locale)
		if err != nil {
			return nil, err
		}
		set := map[string]compiled{}
		for _, name := range names {
			c, err := compile(locale, name)
			if err != nil {
				return nil, err
			}
			set[name] = c
		}
		t.byLocale[locale] = set
	}
	zh, en := t.Names(LocaleZH), t.Names(LocaleEN)
	if strings.Join(zh, ",") != strings.Join(en, ",") {
		return nil, fmt.Errorf("两种语言的模板集合不同：%v / %v", zh, en)
	}
	return t, nil
}

func templateNames(locale string) ([]string, error) {
	entries, err := fs.ReadDir(templateFS, path.Join("templates", locale))
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		if name, _, ok := strings.Cut(entry.Name(), "."); ok {
			seen[name] = true
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func read(locale, name, ext string) (string, error) {
	data, err := templateFS.ReadFile(path.Join("templates", locale, name+"."+ext))
	return string(data), err
}

func compile(locale, name string) (compiled, error) {
	var c compiled
	subject, err := read(locale, name, "subject")
	if err != nil {
		return c, err
	}
	text, err := read(locale, name, "txt")
	if err != nil {
		return c, err
	}
	html, err := read(locale, name, "html")
	if err != nil {
		return c, err
	}
	c.subject, err = texttemplate.New(name).Option("missingkey=error").Parse(strings.TrimSpace(subject))
	if err != nil {
		return c, err
	}
	if c.text, err = texttemplate.New(name).Option("missingkey=error").Parse(text); err != nil {
		return c, err
	}
	c.html, err = htmltemplate.New(name).Option("missingkey=error").Parse(html)
	return c, err
}

// Names 是某种语言的全部模板名（升序）。
func (t *Templates) Names(locale string) []string {
	var names []string
	for name := range t.byLocale[locale] {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Render 渲染一个模板；语言不认识时退回中文。
func (t *Templates) Render(locale, name string, data Data) (subject, text, html string, err error) {
	set, ok := t.byLocale[locale]
	if !ok {
		set = t.byLocale[LocaleZH]
	}
	c, ok := set[name]
	if !ok {
		return "", "", "", fmt.Errorf("没有这个模板：%s", name)
	}
	var s, x, h bytes.Buffer
	if err := c.subject.Execute(&s, data); err != nil {
		return "", "", "", err
	}
	if err := c.text.Execute(&x, data); err != nil {
		return "", "", "", err
	}
	if err := c.html.Execute(&h, data); err != nil {
		return "", "", "", err
	}
	return s.String(), x.String(), h.String(), nil
}
