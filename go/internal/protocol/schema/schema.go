// Package schema 是协议包 JSON Schema 导出的最小解释器。
//
// 只实现协议包实际会写出的关键字：type、const、enum、pattern、长度与数值界、
// properties、required、items、prefixItems、minItems、maxItems、anyOf、format（email、uri）、
// propertyNames（值不限的 record）。未知字段一律放过
// （协议 schema 非 strict，更高 minor 加的字段被忽略）。
// 描述数据（Schema 值）由 tools/schemagen 生成；这里只负责按它校验已解码的 JSON。
package schema

import (
	"encoding/json"
	"fmt"
	"math"
	"net/netip"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/net/idna"
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
	MinItems    *int
	MaxItems    *int
	AnyOf       []*Schema
	// Format 取 email 或 uri，只对字符串生效。email 按 FormatPattern（协议包导出的
	// z.email() 正则源码）判；uri 要求 url.Parse 成功且有 scheme 与 host。
	Format        string
	FormatPattern string
	// PropertyNames 非空表示对象的键都要过它（record 的键 schema）；值不限，剪枝时保留全部键。
	PropertyNames *Schema
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

// 协议包的邮箱正则（JavaScript 语法）开头带两个否定前瞻，Go 的 RE2 不支持：
// 前瞻换成等价的直接检查（不以点开头、不含连续两个点），其余部分原样编译。
const (
	lookNoLeadingDot = `(?!\.)`
	lookNoDoubleDot  = `(?!.*\.\.)`
)

type emailMatcher struct {
	rest         *regexp.Regexp
	noLeadingDot bool
	noDoubleDot  bool
}

var emailMatchers sync.Map

// EmailMatches 按协议包导出的邮箱正则源码判一个字符串；源码用了本包不认识的前瞻就 panic（生成物有误）。
func EmailMatches(pattern, value string) bool {
	cached, ok := emailMatchers.Load(pattern)
	if !ok {
		cached, _ = emailMatchers.LoadOrStore(pattern, newEmailMatcher(pattern))
	}
	m := cached.(*emailMatcher)
	if m.noLeadingDot && strings.HasPrefix(value, ".") {
		return false
	}
	if m.noDoubleDot && strings.Contains(value, "..") {
		return false
	}
	return m.rest.MatchString(value)
}

func newEmailMatcher(pattern string) *emailMatcher {
	body, ok := strings.CutPrefix(pattern, "^")
	if !ok {
		panic("schema: 邮箱正则应以 ^ 开头")
	}
	m := &emailMatcher{}
	for {
		switch {
		case !m.noLeadingDot && strings.HasPrefix(body, lookNoLeadingDot):
			m.noLeadingDot = true
			body = body[len(lookNoLeadingDot):]
			continue
		case !m.noDoubleDot && strings.HasPrefix(body, lookNoDoubleDot):
			m.noDoubleDot = true
			body = body[len(lookNoDoubleDot):]
			continue
		}
		break
	}
	if strings.Contains(body, "(?!") || strings.Contains(body, "(?=") || strings.Contains(body, "(?<") {
		panic("schema: 邮箱正则含不认识的前瞻")
	}
	m.rest = regexp.MustCompile("^" + body)
	return m
}

// uriMatches 判 format: uri：url.Parse 成功，有 scheme 与非空主机名；端口若有须在 0–65535；
// 主机名限定 IP-literal（方括号里的 IPv6，不带 zone）或 RFC 3986 的 reg-name（含 IPv4）。
// 比 z.url() 严（无 host 的 mailto: 也拒绝），但凡 z.url() 拒绝的这里都拒绝。
func uriMatches(value string) bool {
	u, err := url.Parse(value)
	if err != nil || u.Scheme == "" {
		return false
	}
	host := u.Hostname()
	if host == "" {
		return false
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 0 || n > 65535 {
			return false
		}
	}
	if strings.HasPrefix(u.Host, "[") {
		addr, err := netip.ParseAddr(host)
		return err == nil && addr.Zone() == ""
	}
	host, ok := asciiHost(host)
	if !ok {
		return false
	}
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("-._~!$&'()*+,;=", r):
		default:
			return false
		}
	}
	return true
}

// idnaProfile 与 WHATWG URL 的主机名处理对齐（z.url() 经它接受 IDN）：做 UTS 46 映射与 punycode
// 转换，不套 STD3 / DNS 长度这类 DNS 注册层面的限制。
var idnaProfile = idna.New(idna.MapForLookup(), idna.BidiRule(), idna.StrictDomainName(false), idna.VerifyDNSLength(false), idna.CheckHyphens(false))

// asciiHost 把非 ASCII 主机名转成 ASCII（punycode）；纯 ASCII 原样返回。转换失败答 ok=false。
func asciiHost(host string) (string, bool) {
	for i := 0; i < len(host); i++ {
		if host[i] >= 0x80 {
			ascii, err := idnaProfile.ToASCII(host)
			return ascii, err == nil && ascii != ""
		}
	}
	return host, true
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
	switch s.Format {
	case "":
	case "email":
		if !EmailMatches(s.FormatPattern, v) {
			return fail(path, "不是合法邮箱")
		}
	case "uri":
		if !uriMatches(v) {
			return fail(path, "不是合法 URL")
		}
	default:
		panic("schema: 未知格式 " + s.Format)
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
	if s.MinItems != nil && len(v) < *s.MinItems {
		return fail(path, "少于 %d 项", *s.MinItems)
	}
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
	if s.PropertyNames != nil {
		for key := range v {
			if err := s.PropertyNames.validate(path+"/"+key, key); err != nil {
				return err
			}
		}
	}
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

// Decode 把 JSON 文本按 s 校验后解到 out（结构体指针）。
//
// 先解成通用值校验，再剔掉 schema 没列出的字段才解进结构体：encoding/json 对键名不分
// 大小写，不剔的话，多出来的 "KIND" 会覆盖已校验过的 "kind"。文本不是合法 UTF-8 也算失败。
func Decode(s *Schema, data []byte, out any) error {
	if !utf8.Valid(data) {
		return &Error{Msg: "不是合法 UTF-8"}
	}
	var generic any
	if err := json.Unmarshal(data, &generic); err != nil {
		return &Error{Msg: "不是合法 JSON"}
	}
	if err := s.Validate(generic); err != nil {
		return err
	}
	clean, err := json.Marshal(s.prune(generic))
	if err != nil {
		return err
	}
	return json.Unmarshal(clean, out)
}

// prune 只留下 schema 列出的字段。调用前 value 已经通过校验。
func (s *Schema) prune(value any) any {
	if len(s.AnyOf) > 0 {
		for _, option := range s.AnyOf {
			if option.validate("", value) == nil {
				return option.prune(value)
			}
		}
		return value
	}
	switch v := value.(type) {
	case map[string]any:
		if len(s.Properties) == 0 {
			return v
		}
		out := make(map[string]any, len(v))
		for _, p := range s.Properties {
			if item, ok := v[p.Name]; ok {
				out[p.Name] = p.Schema.prune(item)
			}
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			switch {
			case i < len(s.PrefixItems):
				out[i] = s.PrefixItems[i].prune(item)
			case s.Items != nil:
				out[i] = s.Items.prune(item)
			default:
				out[i] = item
			}
		}
		return out
	}
	return value
}
