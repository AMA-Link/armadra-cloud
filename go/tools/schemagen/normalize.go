package main

import "fmt"

// normalize 把 zod 4.6 起 z.toJSONSchema 的几种等价写法改回生成器认识的形状，原地修改：
//   - type: [T, "null"] 改成 anyOf [{type: T}, {type: null}]（可空）；
//   - format 为 starts_with / ends_with / includes 时去掉 format，约束已在同层的 pattern 里；
//   - 与 prefixItems 同层的 items: false 去掉，元组本来就按 prefixItems 要求长度恰好相等。
//
// 其余写法原样保留，交给 literal 判断，认不出的照旧报错。
func normalize(v *value) error {
	switch v.kind {
	case 'a':
		for _, item := range v.arr {
			if err := normalize(item); err != nil {
				return err
			}
		}
		return nil
	case 'o':
	default:
		return nil
	}
	if t := v.obj["type"]; t != nil && t.kind == 'a' {
		if len(t.arr) != 2 || !t.arr[0].isString() || t.arr[0].str == "null" || !t.arr[1].isString() || t.arr[1].str != "null" {
			return fmt.Errorf("type 数组只支持 [T, \"null\"]")
		}
		if _, dup := v.obj["anyOf"]; dup {
			return fmt.Errorf("type 数组与 anyOf 不能同时出现")
		}
		v.obj["anyOf"] = &value{kind: 'a', arr: []*value{typeOnly(t.arr[0].str), typeOnly("null")}}
		delete(v.obj, "type")
		for i, key := range v.keys {
			if key == "type" {
				v.keys[i] = "anyOf"
			}
		}
	}
	if f := v.obj["format"]; f.isString() && v.obj["pattern"].isString() {
		switch f.str {
		case "starts_with", "ends_with", "includes":
			v.drop("format")
		}
	}
	if items := v.obj["items"]; items != nil && items.kind == 'b' && !items.flag && v.obj["prefixItems"] != nil {
		v.drop("items")
	}
	for _, key := range v.keys {
		if err := normalize(v.obj[key]); err != nil {
			return fmt.Errorf("%s：%w", key, err)
		}
	}
	return nil
}

func typeOnly(t string) *value {
	return &value{kind: 'o', keys: []string{"type"}, obj: map[string]*value{"type": {kind: 's', str: t}}}
}

// drop 去掉对象的一个键（保持其余键的顺序）。
func (v *value) drop(key string) {
	delete(v.obj, key)
	for i, k := range v.keys {
		if k == key {
			v.keys = append(v.keys[:i], v.keys[i+1:]...)
			return
		}
	}
}
