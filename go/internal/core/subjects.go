package core

import (
	"regexp"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

// 账号标识（sub）：不透明字符串。个人中转的账号本人是 acct:<账号名>，经链接进来的访客是
// guest:<linkId>:<随机>。

var guestSubject = regexp.MustCompile(`^guest:([0-9a-f]{16}):[0-9a-f]+$`)

// OwnerSubject 是 acct:<账号名>：个人中转唯一账号的 accountId。
func OwnerSubject(name string) string { return store.OwnerSubject(name) }

// GuestPrefix 是访客 sub 的前缀 guest:<linkId>:。
func GuestPrefix(linkID string) string { return store.GuestPrefix(linkID) }

// GuestLinkID：guest:<linkId>:<随机> → linkId；不是访客答 ok=false。
func GuestLinkID(subject string) (string, bool) {
	m := guestSubject.FindStringSubmatch(subject)
	if m == nil {
		return "", false
	}
	return m[1], true
}
