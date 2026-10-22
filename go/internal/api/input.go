package api

import (
	"encoding/json"
	"net/url"
	"strconv"
	"strings"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/schema"
)

// queryObject 把 query 变成入参对象：同名多次出现取最后一个；a[b]=c 展开成嵌套对象（deepObject）。
// 值都是字符串，数字与布尔按 schema 声明的类型转换（与契约里 z.coerce 的行为一致，空串当 0）。
func queryObject(values url.Values, input *schema.Schema) map[string]any {
	out := map[string]any{}
	for key, list := range values {
		if len(list) == 0 {
			continue
		}
		value := list[len(list)-1]
		open := strings.IndexByte(key, '[')
		if open <= 0 || !strings.HasSuffix(key, "]") {
			out[key] = value
			continue
		}
		parent, child := key[:open], key[open+1:len(key)-1]
		if child == "" || strings.ContainsAny(child, "[]") {
			out[key] = value
			continue
		}
		nested, _ := out[parent].(map[string]any)
		if nested == nil {
			nested = map[string]any{}
			out[parent] = nested
		}
		nested[child] = value
	}
	coerce(out, input)
	return out
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
