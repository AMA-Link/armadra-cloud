package main

import (
	"fmt"
	"sort"
	"strings"
)

// constPrefixes 把 constants.json 里的路径映射成 Go 名字的前缀；取最长匹配，
// 路径剩下的部分接在后面。
var constPrefixes = map[string]string{
	"protocol":              "Protocol",
	"paths":                 "Path",
	"headers":               "Header",
	"subprotocols":          "Subprotocol",
	"tokens.types":          "TokenType",
	"tokens.audiences":      "TokenAudience",
	"tokens.claimsVersion":  "ClaimsVersion",
	"tokens.scope.prefix":   "ScopePrefix",
	"tokens.scope.readOnly": "ScopeReadOnly",
	"tokens.readOnly":       "ReadOnly",
	"tunnel.limits":         "Limit",
	"tunnel.frameTypes":     "FrameType",
	"tunnel.rst":            "Rst",
	"tunnel.close":          "Close",
	"e2e":                   "E2E",
	"e2e.flags":             "E2EFlag",
	"e2e.labels":            "E2ELabel",
	"e2e.limits":            "E2ELimit",
	"identity":              "Identity",
}

func constName(path []string) (string, error) {
	for n := len(path); n > 0; n-- {
		prefix, ok := constPrefixes[strings.Join(path[:n], ".")]
		if !ok {
			continue
		}
		var b strings.Builder
		b.WriteString(prefix)
		for _, part := range path[n:] {
			b.WriteString(pascal(part))
		}
		return b.String(), nil
	}
	return "", fmt.Errorf("constants.json 的路径 %s 没有登记前缀", strings.Join(path, "."))
}

type constant struct {
	name    string
	literal string
	slice   bool
}

func collectConstants(path []string, v *value, out *[]constant) error {
	switch v.kind {
	case 'o':
		for _, key := range v.keys {
			if err := collectConstants(append(append([]string{}, path...), key), v.obj[key], out); err != nil {
				return err
			}
		}
		return nil
	case 'a':
		name, err := constName(path)
		if err != nil {
			return err
		}
		var items []string
		for _, item := range v.arr {
			if item.kind != 's' {
				return fmt.Errorf("%s：数组只支持字符串", strings.Join(path, "."))
			}
			items = append(items, quote(item.str))
		}
		*out = append(*out, constant{name: name, literal: "[]string{" + strings.Join(items, ", ") + "}", slice: true})
		return nil
	}
	name, err := constName(path)
	if err != nil {
		return err
	}
	switch v.kind {
	case 's':
		*out = append(*out, constant{name: name, literal: quote(v.str)})
	case 'n':
		*out = append(*out, constant{name: name, literal: v.num})
	case 'b':
		*out = append(*out, constant{name: name, literal: fmt.Sprint(v.flag)})
	default:
		return fmt.Errorf("%s：不支持的值类型", strings.Join(path, "."))
	}
	return nil
}

func genConstants(root *value) ([]byte, error) {
	var all []constant
	if err := collectConstants(nil, root, &all); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, c := range all {
		if seen[c.name] {
			return nil, fmt.Errorf("常量名重复：%s", c.name)
		}
		seen[c.name] = true
	}
	var f file
	f.printf("const (\n")
	for _, c := range all {
		if !c.slice {
			f.printf("%s = %s\n", c.name, c.literal)
		}
	}
	f.printf(")\n\n")
	var slices []constant
	for _, c := range all {
		if c.slice {
			slices = append(slices, c)
		}
	}
	sort.SliceStable(slices, func(i, j int) bool { return slices[i].name < slices[j].name })
	for _, c := range slices {
		f.printf("var %s = %s\n\n", c.name, c.literal)
	}
	return f.source("gen", "// Package gen 是 packages/platform-protocol/schema/ 的 Go 投影：常量、错误码、令牌声明、\n// 握手消息与路由表。整个目录由 tools/schemagen 生成。\n", nil)
}
