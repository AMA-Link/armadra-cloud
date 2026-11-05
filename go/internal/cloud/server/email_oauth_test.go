package server_test

import (
	"strings"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/oauth"
)

// 改邮箱免口令（OAuth-only 账号）：要求提权；申请时旧地址就收到通知（新地址打码）。

func TestEmailChangeWithoutPassword(t *testing.T) {
	t.Parallel()
	r, f := oauthRig(t, true, oauth.GitHub)
	oldEmail, newEmail := r.email(), r.email()
	s := parseSession(r, r.oauthLogin(f, "github", ghIdentity("9001", oldEmail), false))
	change := func(body map[string]any) response { return r.do("POST", "/v1/me/email", body, s.bearer()) }

	// 无口令且未提权：401 mfa_required，什么都没发。
	r.expect(change(map[string]any{"newEmail": newEmail}), 401, "mfa_required")
	if r.queuedMails(newEmail, "email_change") != 0 || r.queuedMails(oldEmail, "email_change_requested") != 0 {
		t.Fatal("拒绝时不该发信")
	}
	// 经 OAuth 提权后可以改。
	r.expect(r.stepUpOAuth(r.reauthBegin(f, s, "github", ghIdentity("9001", oldEmail), s.bearer()), s.bearer()), 200, "")
	r.expect(change(map[string]any{"newEmail": strings.ToUpper(newEmail)}), 200, "")
	if r.queuedMails(newEmail, "email_change") != 1 {
		t.Fatal("新地址应收到确认邮件")
	}
	// 旧地址在申请时就收到通知，里面是打码的新地址，没有链接。
	notice := r.waitSubject(oldEmail, "收到更改邮箱的申请")
	if strings.Contains(notice.Text, newEmail) || !strings.Contains(notice.Text, newEmail[:1]+"***@") || linkPattern.MatchString(notice.Text) {
		t.Fatalf("旧地址通知：%+v", notice)
	}
	// 确认前旧邮箱照常生效。
	if r.queryString("SELECT email FROM accounts WHERE account_id = $1", s.accountID) != oldEmail {
		t.Fatal("确认前不该改邮箱")
	}
	token := tokenOf(t, r.waitSubject(newEmail, subjectConfirmEmail), "/verify")
	r.expect(r.verifyEmail(token), 200, "")
	if r.queryString("SELECT email FROM accounts WHERE account_id = $1", s.accountID) != newEmail {
		t.Fatal("确认后应改邮箱")
	}
	// 确认后旧地址照旧收到「邮箱已更改」。
	r.waitSubject(oldEmail, subjectEmailChanged)
}

// 带口令的分支不变：不要求提权，也同样通知旧地址；口令错答 401 credentials_invalid。
func TestEmailChangeWithPasswordStillWorks(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	oldEmail, newEmail := r.email(), r.email()
	s := r.registerBearer(oldEmail)
	r.expect(r.do("POST", "/v1/me/email", map[string]any{"newEmail": newEmail, "password": "wrong-password-xx"}, s.bearer()), 401, "credentials_invalid")
	r.expect(r.do("POST", "/v1/me/email", map[string]any{"newEmail": newEmail, "password": password}, s.bearer()), 200, "")
	r.waitSubject(oldEmail, "收到更改邮箱的申请")
	r.waitSubject(newEmail, subjectConfirmEmail)
}

// 带了口令但口令错：不因为会话已提权而放行。
func TestEmailChangeWrongPasswordIgnoresElevation(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	s := r.registerBearer(r.email())
	r.elevate(s)
	r.expect(r.do("POST", "/v1/me/email", map[string]any{"newEmail": r.email(), "password": "wrong-password-xx"}, s.bearer()), 401, "credentials_invalid")
}
