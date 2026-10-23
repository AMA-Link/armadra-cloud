package hosts

import (
	"strconv"
	"strings"
)

// semver 是解出来的语义化版本：主、次、修订与是否带预发布段（构建元数据忽略）。
type semver struct {
	nums [3]int
	pre  bool
}

func parseVersion(text string) (semver, bool) {
	text = strings.TrimPrefix(strings.TrimSpace(text), "v")
	text, _, _ = strings.Cut(text, "+")
	core, pre, hasPre := strings.Cut(text, "-")
	if hasPre && pre == "" {
		return semver{}, false
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return semver{}, false
	}
	out := semver{pre: hasPre}
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 || part != strconv.Itoa(n) {
			return semver{}, false
		}
		out.nums[i] = n
	}
	return out, true
}

// AtLeast 报告版本 version 是否不低于 min（语义化版本：预发布版低于同号的正式版）。
// 任何一个解析不了都答 false：解析不了的 core 版本不放行（设计 §4.6）。
func AtLeast(version, min string) bool {
	have, ok := parseVersion(version)
	if !ok {
		return false
	}
	want, ok := parseVersion(min)
	if !ok {
		return false
	}
	for i := range have.nums {
		if have.nums[i] != want.nums[i] {
			return have.nums[i] > want.nums[i]
		}
	}
	return !have.pre || want.pre
}
