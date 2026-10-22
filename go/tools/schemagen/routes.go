package main

import (
	"fmt"
	"strings"
)

// 路由表只收 OpenAPI 里写得出来的：procedure（operationId）、方法与路径模板。
// scope 在协议包的 oRPC meta 里，OpenAPI 导出没有它，见 PR 说明。
func genRoutes(root *value) ([]byte, error) {
	paths := root.get("paths")
	if paths == nil || paths.kind != 'o' {
		return nil, fmt.Errorf("cloud-openapi.json 没有 paths")
	}
	var f file
	f.printf("// Route 是 /v1 契约里的一条过程。\ntype Route struct {\n")
	f.printf("// Procedure 是 operationId，如 auth.login。\nProcedure string\n")
	f.printf("// Method 是大写的 HTTP 方法。\nMethod string\n")
	f.printf("// Path 是路径模板，参数写成 {name}。\nPath string\n}\n\n")
	f.printf("// Routes 按 OpenAPI 里的顺序列出全部过程（含 /health 与 /.well-known/*）。\nvar Routes = []Route{\n")
	for _, path := range paths.keys {
		item := paths.obj[path]
		for _, method := range item.keys {
			op := item.obj[method].get("operationId")
			if !op.isString() {
				return nil, fmt.Errorf("%s %s 没有 operationId", method, path)
			}
			f.printf("{Procedure: %s, Method: %s, Path: %s},\n", quote(op.str), quote(strings.ToUpper(method)), quote(path))
		}
	}
	f.printf("}\n")
	return f.source("gen", "", nil)
}
