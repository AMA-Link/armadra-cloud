package main

import (
	"fmt"
	"strings"
)

// 用到的 schema 关键字。出现别的关键字就报错：宁可让生成器停下，也不悄悄丢掉约束。
var knownKeywords = map[string]bool{
	"$id": true, "$schema": true, "type": true, "const": true, "enum": true,
	"pattern": true, "minLength": true, "maxLength": true, "minimum": true,
	"maximum": true, "exclusiveMinimum": true, "properties": true,
	"required": true, "items": true, "prefixItems": true, "maxItems": true,
	"anyOf": true,
	// default 只是注解（不是约束），校验时没有作用，直接略过。
	"default": true,
}

// unsupportedKeyword 是 literal 遇到的、生成器不认识的 schema 关键字。
type unsupportedKeyword struct{ keyword string }

func (e *unsupportedKeyword) Error() string {
	return fmt.Sprintf("不认识的 schema 关键字 %q", e.keyword)
}

type typeDef struct {
	name string
	doc  string
	body string
}

type enumDef struct {
	name   string
	values []string
}

// structGen 把一组 JSON Schema 对象转成 Go 结构体与校验规则。结构完全相同的嵌套对象
// 共用一个类型（例如握手里的 protocol 与 ready.limits）。
type structGen struct {
	defs    []typeDef
	schemas []typeDef
	enums   []enumDef
	bySig   map[string]string
	names   map[string]bool
}

func newStructGen() *structGen {
	return &structGen{bySig: map[string]string{}, names: map[string]bool{}}
}

// add 登记一个顶层 schema：生成结构体 Name 与规则变量 NameSchema。
func (g *structGen) add(name, id string, root *value) error {
	if root.get("type").str != "object" {
		return fmt.Errorf("%s：顶层应为 object", id)
	}
	if _, err := g.structOf(name, id, root, true); err != nil {
		return err
	}
	lit, err := literal(root)
	if err != nil {
		return fmt.Errorf("%s：%w", id, err)
	}
	g.schemas = append(g.schemas, typeDef{
		name: name + "Schema",
		doc:  fmt.Sprintf("// %sSchema 是 %s 的校验规则。", name, id),
		body: lit,
	})
	return nil
}

func (g *structGen) structOf(name, id string, obj *value, top bool) (string, error) {
	props := obj.get("properties")
	if props == nil || props.kind != 'o' || len(props.keys) == 0 {
		return "", fmt.Errorf("%s：%s 没有 properties", id, name)
	}
	required := map[string]bool{}
	if req := obj.get("required"); req != nil {
		for _, item := range req.arr {
			required[item.str] = true
		}
	}
	var lines []string
	for _, key := range props.keys {
		prop := props.obj[key]
		typ, omit, err := g.fieldType(name+pascal(key), id, prop, required[key])
		if err != nil {
			return "", fmt.Errorf("%s.%s：%w", id, key, err)
		}
		tag := key
		if omit {
			tag += ",omitempty"
		}
		lines = append(lines, fmt.Sprintf("%s %s `json:%q`", pascal(key), typ, tag))
		if values := enumValues(prop); values != nil {
			g.enums = append(g.enums, enumDef{name: name + pascal(key) + "Values", values: values})
		}
	}
	sig := strings.Join(lines, "\n")
	if existing, ok := g.bySig[sig]; ok && !top {
		return existing, nil
	}
	if g.names[name] {
		return "", fmt.Errorf("类型名重复：%s", name)
	}
	g.names[name] = true
	if _, ok := g.bySig[sig]; !ok {
		g.bySig[sig] = name
	}
	g.defs = append(g.defs, typeDef{
		name: name,
		doc:  fmt.Sprintf("// %s 对应 schema %s。", name, id),
		body: sig,
	})
	return name, nil
}

func enumValues(prop *value) []string {
	enum := prop.get("enum")
	if enum == nil && prop.get("items") != nil {
		enum = prop.get("items").get("enum")
	}
	if enum == nil {
		return nil
	}
	var out []string
	for _, item := range enum.arr {
		out = append(out, item.str)
	}
	return out
}

// fieldType 返回字段的 Go 类型，以及 JSON tag 是否带 omitempty。
// 可选字段：标量与对象用指针，数组用可为 nil 的切片。
func (g *structGen) fieldType(name, id string, prop *value, required bool) (string, bool, error) {
	if anyOf := prop.get("anyOf"); anyOf != nil {
		if len(anyOf.arr) == 2 && anyOf.arr[0].get("type").str == "string" && anyOf.arr[1].get("type").str == "null" {
			return "*string", !required, nil
		}
		return "", false, fmt.Errorf("只支持 anyOf [string, null]")
	}
	ptr := func(typ string) (string, bool, error) {
		if required {
			return typ, false, nil
		}
		return "*" + typ, true, nil
	}
	switch prop.get("type").str {
	case "string":
		return ptr("string")
	case "integer":
		return ptr("int64")
	case "number":
		return ptr("float64")
	case "boolean":
		return ptr("bool")
	case "object":
		inner, err := g.structOf(name, id, prop, false)
		if err != nil {
			return "", false, err
		}
		return ptr(inner)
	case "array":
		elem, err := g.elemType(name, id, prop.get("items"))
		if err != nil {
			return "", false, err
		}
		return "[]" + elem, !required, nil
	}
	return "", false, fmt.Errorf("不支持的类型 %q", prop.get("type").str)
}

func (g *structGen) elemType(name, id string, items *value) (string, error) {
	if items == nil {
		return "", fmt.Errorf("数组没有 items")
	}
	switch items.get("type").str {
	case "string":
		return "string", nil
	case "object":
		return g.structOf(name, id, items, false)
	case "array":
		prefix := items.get("prefixItems")
		if prefix == nil {
			return "", fmt.Errorf("嵌套数组只支持 prefixItems 元组")
		}
		for _, item := range prefix.arr {
			if item.get("type").str != "string" {
				return "", fmt.Errorf("元组只支持字符串")
			}
		}
		return fmt.Sprintf("[%d]string", len(prefix.arr)), nil
	}
	return "", fmt.Errorf("不支持的元素类型 %q", items.get("type").str)
}

func (g *structGen) render(f *file) {
	for _, d := range g.defs {
		f.printf("%s\ntype %s struct {\n%s\n}\n\n", d.doc, d.name, d.body)
	}
	for _, e := range g.enums {
		var items []string
		for _, v := range e.values {
			items = append(items, quote(v))
		}
		f.printf("// %s 列出对应字段允许的取值。\nvar %s = []string{%s}\n\n", e.name, e.name, strings.Join(items, ", "))
	}
	for _, s := range g.schemas {
		f.printf("%s\nvar %s = %s\n\n", s.doc, s.name, s.body)
	}
}

// literal 把 schema 写成 schema.Schema 的 Go 字面量。
func literal(v *value) (string, error) {
	for _, key := range v.keys {
		if !knownKeywords[key] {
			return "", &unsupportedKeyword{keyword: key}
		}
	}
	var parts []string
	add := func(format string, args ...any) { parts = append(parts, fmt.Sprintf(format, args...)) }
	if t := v.get("type"); t != nil {
		add("Type: %s", quote(t.str))
	}
	if c := v.get("const"); c != nil {
		lit, err := anyLiteral(c)
		if err != nil {
			return "", err
		}
		add("Const: %s", lit)
		add("HasConst: true")
	}
	if e := v.get("enum"); e != nil {
		var items []string
		for _, item := range e.arr {
			lit, err := anyLiteral(item)
			if err != nil {
				return "", err
			}
			items = append(items, lit)
		}
		add("Enum: []any{%s}", strings.Join(items, ", "))
	}
	if p := v.get("pattern"); p != nil {
		add("Pattern: %s", quote(p.str))
	}
	for _, kw := range []struct{ key, field string }{
		{"minLength", "MinLength"}, {"maxLength", "MaxLength"}, {"maxItems", "MaxItems"},
	} {
		if n := v.get(kw.key); n != nil {
			add("%s: schema.Int(%s)", kw.field, n.num)
		}
	}
	for _, kw := range []struct{ key, field string }{
		{"minimum", "Minimum"}, {"maximum", "Maximum"}, {"exclusiveMinimum", "ExclusiveMinimum"},
	} {
		if n := v.get(kw.key); n != nil {
			add("%s: schema.Num(%s)", kw.field, n.num)
		}
	}
	if props := v.get("properties"); props != nil {
		var items []string
		for _, key := range props.keys {
			inner, err := literal(props.obj[key])
			if err != nil {
				return "", fmt.Errorf("%s：%w", key, err)
			}
			items = append(items, fmt.Sprintf("{Name: %s, Schema: %s}", quote(key), inner))
		}
		add("Properties: []schema.Property{\n%s,\n}", strings.Join(items, ",\n"))
	}
	if req := v.get("required"); req != nil {
		var items []string
		for _, item := range req.arr {
			items = append(items, quote(item.str))
		}
		add("Required: []string{%s}", strings.Join(items, ", "))
	}
	if items := v.get("items"); items != nil {
		inner, err := literal(items)
		if err != nil {
			return "", err
		}
		add("Items: %s", inner)
	}
	for _, kw := range []struct{ key, field string }{{"prefixItems", "PrefixItems"}, {"anyOf", "AnyOf"}} {
		if list := v.get(kw.key); list != nil {
			var items []string
			for _, item := range list.arr {
				inner, err := literal(item)
				if err != nil {
					return "", err
				}
				items = append(items, inner)
			}
			add("%s: []*schema.Schema{\n%s,\n}", kw.field, strings.Join(items, ",\n"))
		}
	}
	return "&schema.Schema{\n" + strings.Join(parts, ",\n") + ",\n}", nil
}

func anyLiteral(v *value) (string, error) {
	switch v.kind {
	case 's':
		return quote(v.str), nil
	case 'n':
		return "float64(" + v.num + ")", nil
	case 'b':
		return fmt.Sprint(v.flag), nil
	}
	return "", fmt.Errorf("const / enum 只支持字符串、数字与布尔")
}
