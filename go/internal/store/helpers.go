package store

import (
	"bytes"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
)

// 会话寿命（协议常量的镜像在 core；这里只为 IsStaleDevice 用）。
const refreshTTLMs = int64(2_592_000_000)

// GuestPrefix 是访客 sub 的前缀 guest:<linkID>:。
func GuestPrefix(linkID string) string { return "guest:" + linkID + ":" }

// OwnerSubject 是个人中转账号本人的 sub：acct:<账号名>。
func OwnerSubject(name string) string { return "acct:" + name }

// IsLiveDevice：设备未撤销且未过期。
func IsLiveDevice(device DeviceRecord, nowMs int64) bool {
	return device.RevokedAtMs == nil && device.ExpiresAtMs > nowMs
}

// IsStaleDevice：早已失效、可以从存储里清掉的设备行（失效超过一个刷新令牌寿命）。
func IsStaleDevice(device DeviceRecord, nowMs int64) bool {
	if IsLiveDevice(device, nowMs) {
		return false
	}
	end := device.ExpiresAtMs
	if device.RevokedAtMs != nil {
		end = max(end, *device.RevokedAtMs)
	} else {
		end = max(end, 0)
	}
	return end <= nowMs-refreshTTLMs
}

// Marshal 序列化记录用于「改没改」的比较与 *_json 列；失败是程序错误。
func Marshal(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic("store: 记录无法序列化：" + err.Error())
	}
	return data
}

// Same 判断两个记录序列化后是否相同（与 TypeScript 一侧的 JSON.stringify 比较同义）。
func Same(a, b any) bool { return bytes.Equal(Marshal(a), Marshal(b)) }

// NonNil 把 nil 切片换成空切片（JSON 里是 [] 不是 null）。
func NonNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// SameHex 定时安全地比较两个 hex 摘要；长度不同、为空或不是 hex 一律假。
func SameHex(left, right string) bool {
	a, errA := hex.DecodeString(left)
	b, errB := hex.DecodeString(right)
	return errA == nil && errB == nil && len(a) == len(b) && len(a) > 0 && subtle.ConstantTimeCompare(a, b) == 1
}
