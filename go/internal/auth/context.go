// Package auth 校验中继令牌与隧道令牌（中继设计 §3.1、§4.2）。
// 验签与声明检查在 jose 包，这里接上控制面（issuer、撤销表、JWKS）并映射到线上错误码。
package auth

import (
	"context"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/jose"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/schema"
)

// Context 是两种令牌校验共用的上下文：控制面（issuer、撤销表）、JWKS 缓存与时钟。
type Context struct {
	Control control.Plane
	JWKS    *jose.JWKSCache
	Now     func() time.Time
}

// NewContext 建上下文；now 为 nil 时用 time.Now。
func NewContext(c control.Plane, now func() time.Time) *Context {
	if now == nil {
		now = time.Now
	}
	return &Context{
		Control: c,
		Now:     now,
		JWKS: jose.NewJWKSCache(jose.JWKSOptions{
			Fetch:      c.JWKS,
			TTL:        jose.DefaultJWKSTTL,
			MinRefresh: jose.DefaultMinRefresh,
			Now:        now,
		}),
	}
}

// Options 给一种令牌拼出验证参数：iss 取控制面的 issuer，撤销查控制面。
func (a *Context) Options(typ string, s *schema.Schema, aud string) jose.Options {
	return jose.Options{
		Typ:         typ,
		Schema:      s,
		ExpectedAud: aud,
		ExpectedIss: a.Control.Issuer(),
		Now:         a.Now(),
		Keys:        a.JWKS,
		IsRevoked: func(ctx context.Context, jti string) (bool, error) {
			return a.Control.Tokens().IsRevoked(ctx, jti)
		},
	}
}
