package core

import (
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

// LinkScopeInput 是请求里的分享范围（links.create 的 scope）。
type LinkScopeInput = store.LinkScope

// NormalizeLinkScope 规范化分享范围：空的答 nil（整台）；sessionId 要有 workspaceId；会话范围总是
// 只读，显式 readOnly: false 答 bad_request；readOnly: false 等同缺省，不存。
func NormalizeLinkScope(scope *LinkScopeInput) (*store.LinkScope, error) {
	if scope == nil {
		return nil, nil
	}
	if scope.SessionID != nil {
		if scope.WorkspaceID == nil || (scope.ReadOnly != nil && !*scope.ReadOnly) {
			return nil, Fail(gen.ErrBadRequest)
		}
		readOnly := true
		return &store.LinkScope{WorkspaceID: scope.WorkspaceID, SessionID: scope.SessionID, ReadOnly: &readOnly}, nil
	}
	out := &store.LinkScope{}
	if scope.WorkspaceID != nil {
		out.WorkspaceID = scope.WorkspaceID
	}
	if scope.ReadOnly != nil && *scope.ReadOnly {
		readOnly := true
		out.ReadOnly = &readOnly
	}
	if out.WorkspaceID == nil && out.ReadOnly == nil {
		return nil, nil
	}
	return out, nil
}

// ScopeClaims 把分享范围换成断言的 scp（顺序固定：ws:、sess:、ro）。整台可写答空切片。
func ScopeClaims(scope *store.LinkScope) []string {
	out := []string{}
	if scope == nil {
		return out
	}
	if scope.WorkspaceID != nil {
		out = append(out, gen.ScopePrefixWorkspace+*scope.WorkspaceID)
	}
	if scope.SessionID != nil {
		out = append(out, gen.ScopePrefixSession+*scope.SessionID)
	}
	if (scope.ReadOnly != nil && *scope.ReadOnly) || scope.SessionID != nil {
		out = append(out, gen.ScopeReadOnly)
	}
	return out
}

func copyScope(scope *store.LinkScope) *store.LinkScope {
	if scope == nil {
		return nil
	}
	out := &store.LinkScope{}
	if scope.WorkspaceID != nil {
		v := *scope.WorkspaceID
		out.WorkspaceID = &v
	}
	if scope.SessionID != nil {
		v := *scope.SessionID
		out.SessionID = &v
	}
	if scope.ReadOnly != nil {
		v := *scope.ReadOnly
		out.ReadOnly = &v
	}
	return out
}
