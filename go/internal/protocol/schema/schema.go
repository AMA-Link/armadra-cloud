// Package schema 是协议包 JSON Schema 导出的最小解释器。
//
// 只实现协议包实际会写出的关键字：type、const、enum、pattern、长度与数值界、
// properties、required、items、prefixItems、maxItems、anyOf。未知字段一律放过
// （协议 schema 非 strict，更高 minor 加的字段被忽略）。
// 描述数据（Schema 值）由 tools/schemagen 生成；这里只负责按它校验已解码的 JSON。
package schema

import (
	"fmt"
	"math"
	"reflect"
	"regexp"
	"sync"
)

// Schema 是一条生成出来的校验规则。零值表示不加限制。
type Schema struct {
	// Type 取 object、string、integer、number、boolean、null、array；空表示不限。
	Type     string
	Const    any
	HasConst bool
	Enum     []any
	// Pattern 是不加锚的正则搜索，与 JSON Schema 一致。
	Pattern   string
	MinLength *int
	MaxLength *int
	Minimum   *float64
	Maximum   *float64
	// ExclusiveMinimum 是开区间下界。
	ExclusiveMinimum *float64
	Properties       []Property
	Required         []string
	Items            *Schema
	// PrefixItems 按位置校验，并要求数组长度恰好相等（与 zod 的 tuple 一致）。
	PrefixItems []*Schema
	MaxItems    *int
	AnyOf       []*Schema
}

// Property 是对象的一个属性。
type Property struct {
	Name   string
	Schema *Schema
}

// Int 与 Num 给生成代码取指针用。
func Int(n int) *int { return &n }

// Num 返回 n 的指针。
func Num(n float64) *float64 { return &n }

// Error 是校验失败，带出错位置（JSON Pointer 风格）。
type Error struct {
	Path string
	Msg  string
}

func (e *Error) Error() string {
	if e.Path == "" {
		return e.Msg
	}
	return e.Path + "：" + e.Msg
}

var patterns sync.Map

func compile(pattern string) *regexp.Regexp {
	if cached, ok := patterns.Load(pattern); ok {
		return cached.(*regexp.Regexp)
	}
	compiled := regexp.MustCompile(pattern)
	patterns.Store(pattern, compiled)
	return compiled
}

// utf16Len 按 UTF-16 码元计长，与 TypeScript 里 zod 对字符串长度的算法一致。
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// Validate 校验 json.Unmarshal 到 any 的值；通过时返回 nil。
func (s *Schema) Validate(value any) error {
	return s.validate("", value)
}

func fail(path, format string, args ...any) error {
	return &Error{Path: path, Msg: fmt.Sprintf(format, args...)}
}

func isInteger(f float64) bool {
	return !math.IsInf(f, 0) && !math.IsNaN(f) && f == math.Trunc(f)
}

func (s *Schema) validate(path string, value any) error {
	if len(s.AnyOf) > 0 {
		for _, option := range s.AnyOf {
			if option.validate(path, value) == nil {
				return nil
			}
		}
		return fail(path, "不符合任何一个候选")
	}
	if err := s.checkType(path, value); err != nil {
		return err
	}
	if s.HasConst && !reflect.DeepEqual(s.Const, value) {
		return fail(path, "不等于固定值 %v", s.Const)
	}
	if len(s.Enum) > 0 {
		found := false
		for _, option := range s.Enum {
			if reflect.DeepEqual(option, value) {
				found = true
				break
			}
		}
		if !found {
			return fail(path, "不在枚举里")
		}
	}
	switch v := value.(type) {
	case string:
		return s.checkString(path, v)
	case float64:
		return s.checkNumber(path, v)
	case []any:
		return s.checkArray(path, v)
	case map[string]any:
		return s.checkObject(path, v)
	}
	return nil
}

func (s *Schema) checkType(path string, value any) error {
	ok := true
	switch s.Type {
	case "":
	case "object":
		_, ok = value.(map[string]any)
	case "array":
		_, ok = value.([]any)
	case "string":
		_, ok = value.(string)
	case "boolean":
		_, ok = value.(bool)
	case "null":
		ok = value == nil
	case "number":
		_, ok = value.(float64)
	case "integer":
		var f float64
		f, ok = value.(float64)
		ok = ok && isInteger(f)
	default:
		panic("schema: 未知类型 " + s.Type)
	}
	if !ok {
		return fail(path, "类型应为 %s", s.Type)
	}
	return nil
}

func (s *Schema) checkString(path, v string) error {
	n := utf16Len(v)
	if s.MinLength != nil && n < *s.MinLength {
		return fail(path, "长度小于 %d", *s.MinLength)
	}
	if s.MaxLength != nil && n > *s.MaxLength {
		return fail(path, "长度大于 %d", *s.MaxLength)
	}
	if s.Pattern != "" && !compile(s.Pattern).MatchString(v) {
		return fail(path, "不符合格式 %s", s.Pattern)
	}
	return nil
}

func (s *Schema) checkNumber(path string, v float64) error {
	if s.Minimum != nil && v < *s.Minimum {
		return fail(path, "小于 %v", *s.Minimum)
	}
	if s.Maximum != nil && v > *s.Maximum {
		return fail(path, "大于 %v", *s.Maximum)
	}
	if s.ExclusiveMinimum != nil && v <= *s.ExclusiveMinimum {
		return fail(path, "不大于 %v", *s.ExclusiveMinimum)
	}
	return nil
}

func (s *Schema) checkArray(path string, v []any) error {
	if s.MaxItems != nil && len(v) > *s.MaxItems {
		return fail(path, "多于 %d 项", *s.MaxItems)
	}
	if len(s.PrefixItems) > 0 && len(v) != len(s.PrefixItems) {
		return fail(path, "应恰好 %d 项", len(s.PrefixItems))
	}
	for i, item := range v {
		at := fmt.Sprintf("%s/%d", path, i)
		switch {
		case i < len(s.PrefixItems):
			if err := s.PrefixItems[i].validate(at, item); err != nil {
				return err
			}
		case s.Items != nil:
			if err := s.Items.validate(at, item); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Schema) checkObject(path string, v map[string]any) error {
	for _, name := range s.Required {
		if _, ok := v[name]; !ok {
			return fail(path+"/"+name, "缺少必填字段")
		}
	}
	for _, p := range s.Properties {
		if item, ok := v[p.Name]; ok {
			if err := p.Schema.validate(path+"/"+p.Name, item); err != nil {
				return err
			}
		}
	}
	return nil
}
