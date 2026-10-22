package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// value 是保留对象键顺序的 JSON 值：生成的字段顺序要与 schema 里的一致。
type value struct {
	kind byte // o 对象、a 数组、s 字符串、n 数字、b 布尔、z null
	keys []string
	obj  map[string]*value
	arr  []*value
	str  string
	num  string
	flag bool
}

func parseOrdered(data []byte) (*value, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := readValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("JSON 末尾有多余内容")
	}
	return v, nil
}

func readValue(dec *json.Decoder) (*value, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		if t == '{' {
			v := &value{kind: 'o', obj: map[string]*value{}}
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key := keyTok.(string)
				item, err := readValue(dec)
				if err != nil {
					return nil, err
				}
				if _, dup := v.obj[key]; dup {
					return nil, fmt.Errorf("重复的键 %q", key)
				}
				v.keys = append(v.keys, key)
				v.obj[key] = item
			}
			_, err := dec.Token()
			return v, err
		}
		v := &value{kind: 'a'}
		for dec.More() {
			item, err := readValue(dec)
			if err != nil {
				return nil, err
			}
			v.arr = append(v.arr, item)
		}
		_, err := dec.Token()
		return v, err
	case string:
		return &value{kind: 's', str: t}, nil
	case json.Number:
		return &value{kind: 'n', num: t.String()}, nil
	case bool:
		return &value{kind: 'b', flag: t}, nil
	case nil:
		return &value{kind: 'z'}, nil
	}
	return nil, fmt.Errorf("意外的 JSON 记号 %v", tok)
}

// get 取对象的一个键；不是对象或没有这个键时返回 nil。
func (v *value) get(key string) *value {
	if v == nil || v.kind != 'o' {
		return nil
	}
	return v.obj[key]
}

func (v *value) isString() bool { return v != nil && v.kind == 's' }
