package api

import (
	"encoding/json"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/schema"
)

// queryObject 把 query 变成入参对象：
//   - schema 声明为 array 的参数收全部值，三种写法都认：重复 tag=a&tag=b、tag[]=a、tag[0]=a&tag[1]=b
//     （oRPC 客户端写的是下标形式；下标按数值排序并入，同一下标出现两次算错）；
//   - 其余参数只许出现一次（重复算错，不再取最后一个）；a[b]=c 展开成嵌套对象（deepObject）；
//   - 同一个父键既有平铺写法又有括号写法（tag 与 tag[x]）算错；
//   - 不成形的括号键（bad[、a[b][c]）当普通键原样保留，由校验拒绝。
//
// 值都是字符串，数字与布尔按 schema 声明的类型转换（与契约里 z.coerce 的行为一致，空串当 0）。
// 键按字典序处理，同一份 query 的结果与错误是确定的。
func queryObject(values url.Values, input *schema.Schema) (map[string]any, error) {
	type group struct {
		flat     []string
		children map[string][]string
	}
	groups := map[string]*group{}
	names := make([]string, 0, len(values))
	for key, list := range values {
		if len(list) == 0 {
			continue
		}
		parent, child, bracket := splitBracket(key)
		g := groups[parent]
		if g == nil {
			g = &group{children: map[string][]string{}}
			groups[parent] = g
			names = append(names, parent)
		}
		if bracket {
			g.children[child] = append(g.children[child], list...)
		} else {
			g.flat = append(g.flat, list...)
		}
	}
	sort.Strings(names)
	out := map[string]any{}
	for _, name := range names {
		g := groups[name]
		if len(g.flat) > 0 && len(g.children) > 0 {
			return nil, core.Fail(gen.ErrBadRequest)
		}
		if declaredArray(input, name) {
			items, err := arrayItems(g.flat, g.children)
			if err != nil {
				return nil, err
			}
			out[name] = items
			continue
		}
		if len(g.children) == 0 {
			if len(g.flat) != 1 {
				return nil, core.Fail(gen.ErrBadRequest)
			}
			out[name] = g.flat[0]
			continue
		}
		nested := make(map[string]any, len(g.children))
		for child, list := range g.children {
			if len(list) != 1 {
				return nil, core.Fail(gen.ErrBadRequest)
			}
			nested[child] = list[0]
		}
		out[name] = nested
	}
	coerce(out, input)
	return out, nil
}

// splitBracket 把 a[b] 拆成 a 与 b；tag[] 的 b 是空串。不成形（没有父名、括号嵌套、缺右括号）
// 的键当普通键。
func splitBracket(key string) (parent, child string, bracket bool) {
	open := strings.IndexByte(key, '[')
	if open <= 0 || !strings.HasSuffix(key, "]") {
		return key, "", false
	}
	parent, child = key[:open], key[open+1:len(key)-1]
	if strings.ContainsAny(child, "[]") {
		return key, "", false
	}
	return parent, child, true
}

// arrayItems 合并数组参数的三种写法：平铺重复、tag[]、tag[<下标>]；混用或下标重复算错。
func arrayItems(flat []string, children map[string][]string) ([]any, error) {
	var list []string
	switch {
	case len(children) == 0:
		list = flat
	case len(children) == 1 && children[""] != nil:
		list = children[""]
	default:
		type indexed struct {
			index int
			value string
		}
		var entries []indexed
		for child, values := range children {
			index, err := strconv.Atoi(child)
			if child == "" || err != nil || index < 0 || len(values) != 1 {
				return nil, core.Fail(gen.ErrBadRequest)
			}
			entries = append(entries, indexed{index, values[0]})
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].index < entries[j].index })
		for _, entry := range entries {
			list = append(list, entry.value)
		}
	}
	items := make([]any, len(list))
	for i, item := range list {
		items[i] = item
	}
	return items, nil
}

// declaredArray 判断入参 schema 里名为 name 的顶层属性是不是数组。
func declaredArray(input *schema.Schema, name string) bool {
	if input == nil {
		return false
	}
	for _, property := range input.Properties {
		if property.Name == name {
			return property.Schema.Type == "array"
		}
	}
	return false
}

// coerce 按 schema 的声明把字符串换成数字或布尔；换不动的原样留着，由校验拒绝。
func coerce(object map[string]any, s *schema.Schema) {
	if s == nil {
		return
	}
	for _, property := range s.Properties {
		value, present := object[property.Name]
		if !present {
			continue
		}
		switch typed := value.(type) {
		case string:
			object[property.Name] = coerceString(typed, property.Schema.Type)
		case map[string]any:
			coerce(typed, property.Schema)
		case []any:
			if property.Schema.Items == nil {
				continue
			}
			for i, item := range typed {
				if text, ok := item.(string); ok {
					typed[i] = coerceString(text, property.Schema.Items.Type)
				}
			}
		}
	}
}

func coerceString(value, typ string) any {
	switch typ {
	case "integer", "number":
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return float64(0)
		}
		if number, err := strconv.ParseFloat(trimmed, 64); err == nil {
			return number
		}
	case "boolean":
		switch value {
		case "true":
			return true
		case "false":
			return false
		}
	}
	return value
}

// decodeInput 校验并剔掉 schema 没列出的字段（与 zod 的默认一致），答入参对象。
func decodeInput(input *schema.Schema, object map[string]any) (map[string]any, error) {
	data, err := json.Marshal(object)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := schema.Decode(input, data, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}
