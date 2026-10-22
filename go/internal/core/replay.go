package core

import "sync"

// ReplayCache 是纯内存的 jti 重放表（协议包 ReplayCache 的镜像）。一个 jti 记到
// max(expMs, nowMs + ttlMs)；超过容量时先清过期的，再淘汰最早记下的。
type ReplayCache struct {
	ttlMs int64
	max   int

	mu      sync.Mutex
	seq     uint64
	entries map[string]replayEntry
}

type replayEntry struct {
	until int64
	seq   uint64
}

// NewReplayCache 建重放表；max 为 0 时取 10 000。
func NewReplayCache(ttlMs int64, max int) *ReplayCache {
	if max == 0 {
		max = 10_000
	}
	return &ReplayCache{ttlMs: ttlMs, max: max, entries: map[string]replayEntry{}}
}

// Seen：见过且未过期 → true（重放）；否则记下并返回 false。
func (r *ReplayCache) Seen(jti string, expMs, nowMs int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if entry, ok := r.entries[jti]; ok {
		if entry.until > nowMs {
			return true
		}
		delete(r.entries, jti)
	}
	if len(r.entries) >= r.max {
		r.evict(nowMs)
	}
	r.seq++
	r.entries[jti] = replayEntry{until: max(expMs, nowMs+r.ttlMs), seq: r.seq}
	return false
}

func (r *ReplayCache) evict(nowMs int64) {
	for key, entry := range r.entries {
		if entry.until <= nowMs {
			delete(r.entries, key)
		}
	}
	for len(r.entries) >= r.max {
		oldest, found := "", false
		var lowest uint64
		for key, entry := range r.entries {
			if !found || entry.seq < lowest {
				oldest, lowest, found = key, entry.seq, true
			}
		}
		if !found {
			return
		}
		delete(r.entries, oldest)
	}
}
