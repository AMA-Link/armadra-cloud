package server_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
)

// onlyOK 只认 "ok" 开头的令牌，其余答不通过（同一令牌在 300 秒内只能用一次，所以每次用不同的）。
type onlyOK struct{}

func (onlyOK) Verify(_ context.Context, request core.VerifyRequest) (core.Verdict, error) {
	if strings.HasPrefix(request.Token, "ok") {
		return core.Verdict{OK: true}, nil
	}
	return core.Verdict{Reasons: []string{"invalid-input-response"}}, nil
}

func challengeRig(t *testing.T) *rig {
	t.Helper()
	return newRig(t, rigOptions{turnstile: true, verifier: onlyOK{}, env: map[string]string{
		"CHALLENGE_SCOPE": "auth.login,auth.register,auth.passwordResetStart",
	}})
}

func token(value string) map[string]any {
	return map[string]any{"provider": "turnstile", "token": value}
}

func (r *rig) registerWith(email string, challenge any) response {
	r.t.Helper()
	body := map[string]any{"email": email, "password": password, "displayName": "测试用户", "device": device}
	if challenge != nil {
		body["challenge"] = challenge
	}
	return r.do("POST", "/v1/auth/register", body, nil)
}

func TestRegisterChallenge(t *testing.T) {
	r := challengeRig(t)
	info := r.do("GET", "/.well-known/armadra-platform", nil, nil)
	r.expect(info, 200, "")
	scope := info.json["challenge"].(map[string]any)["scope"]
	if !reflect.DeepEqual(scope, []any{"auth.login", "auth.register", "auth.passwordResetStart"}) {
		t.Fatalf("platform.info 如实报范围：%v", scope)
	}

	email := r.email()
	r.expect(r.registerWith(email, nil), 400, "challenge_required")
	r.expect(r.registerWith(email, token("bad")), 400, "challenge_invalid")
	// 挑战先于口令策略：弱口令 + 没带挑战答 challenge_required，不泄露口令是否合格。
	res := r.do("POST", "/v1/auth/register", map[string]any{"email": email, "password": "short", "displayName": "测试用户", "device": device}, nil)
	r.expect(res, 400, "challenge_required")
	// 失败的挑战没有建出账号。
	var count int
	if err := r.run.DB.Pool().QueryRow(context.Background(), "SELECT count(*) FROM accounts WHERE email = $1", email).Scan(&count); err != nil || count != 0 {
		t.Fatalf("挑战失败不建账号：%d %v", count, err)
	}
	r.expect(r.registerWith(email, token("ok-register")), 200, "")
}

func TestFailedChallengeDoesNotCountAsLoginFailure(t *testing.T) {
	r := challengeRig(t)
	email := r.email()
	r.expect(r.registerWith(email, token("ok-1")), 200, "")
	login := func(pw string, challenge any) response {
		body := map[string]any{"account": email, "password": pw, "device": device}
		if challenge != nil {
			body["challenge"] = challenge
		}
		return r.do("POST", "/v1/auth/login", body, nil)
	}
	// 挑战失败 8 次（超过锁定阈值）：不计入失败次数，之后正确口令照样登录。
	for range 8 {
		r.expect(login(password, token("bad")), 400, "challenge_invalid")
	}
	r.expect(login(password, token("ok-2")), 200, "")
	var failures int
	if err := r.run.DB.Pool().QueryRow(context.Background(), "SELECT failures FROM accounts WHERE email = $1", email).Scan(&failures); err != nil || failures != 0 {
		t.Fatalf("失败次数：%d %v", failures, err)
	}
}

func TestPasswordResetStartChallenge(t *testing.T) {
	r := challengeRig(t)
	email := r.email()
	r.expect(r.registerWith(email, token("ok-r")), 200, "")
	start := func(challenge any) response {
		body := map[string]any{"email": email}
		if challenge != nil {
			body["challenge"] = challenge
		}
		return r.do("POST", "/v1/auth/password-reset/start", body, nil)
	}
	// 缺挑战 / 无效挑战：不入队任何邮件，也不占用按邮箱每小时 3 次的额度（挑战先于邮箱限流；按 IP 的限流照常先计，所以失败只试 4 次）。
	for range 2 {
		r.expect(start(nil), 400, "challenge_required")
		r.expect(start(token("bad")), 400, "challenge_invalid")
	}
	var queued int
	if err := r.run.DB.Pool().QueryRow(context.Background(), "SELECT count(*) FROM outbox_emails WHERE template = 'password_reset'").Scan(&queued); err != nil || queued != 0 {
		t.Fatalf("挑战失败不入队：%d %v", queued, err)
	}
	for i := range 3 {
		r.expect(start(token(fmt.Sprintf("ok-%d", i))), 200, "")
	}
	r.expect(start(token("ok-3")), 429, "rate_limited")
}
