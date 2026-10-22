package main

import (
	"fmt"
)

func genErrors(root *value) ([]byte, error) {
	if root.kind != 'o' {
		return nil, fmt.Errorf("errors.json 应为对象")
	}
	var f file
	f.printf("// ErrorCode 是登记在协议包注册表里的稳定错误码。\ntype ErrorCode string\n\n")
	f.printf("const (\n")
	names := map[string]string{}
	for _, code := range root.keys {
		name := "Err" + pascal(code)
		if other, dup := names[name]; dup {
			return nil, fmt.Errorf("错误码常量名重复：%s（%s 与 %s）", name, other, code)
		}
		names[name] = code
		f.printf("%s ErrorCode = %s\n", name, quote(code))
	}
	f.printf(")\n\n")
	f.printf("// ErrorCodes 按注册表顺序列出全部错误码。\nvar ErrorCodes = []ErrorCode{\n")
	for _, code := range root.keys {
		f.printf("Err%s,\n", pascal(code))
	}
	f.printf("}\n\n")
	f.printf("// errorStatus 是错误码到 HTTP 状态码的表。\nvar errorStatus = map[ErrorCode]int{\n")
	for _, code := range root.keys {
		status := root.obj[code]
		if status.kind != 'n' {
			return nil, fmt.Errorf("errors.json 的 %s 应为数字", code)
		}
		f.printf("Err%s: %s,\n", pascal(code), status.num)
	}
	f.printf("}\n\n")
	f.printf("// HTTPStatus 返回错误码对应的 HTTP 状态码；没登记的码返回 false。\n")
	f.printf("func (c ErrorCode) HTTPStatus() (int, bool) {\n\tstatus, ok := errorStatus[c]\n\treturn status, ok\n}\n")
	return f.source("gen", "", nil)
}
