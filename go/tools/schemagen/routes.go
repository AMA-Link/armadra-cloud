package main

import (
	"errors"
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

// genInputs 写各过程的入参 schema（RouteInputs）：路径参数、query 参数与 JSON 体合成一个对象，
// 与 TypeScript 路由器里 safeParse({ ...体或query, ...路径参数 }) 的输入同形。
// 用了生成器还不认识的关键字的过程（例如邮箱格式、passkey 的 record）不进 RouteInputs，
// 记进 UnsupportedInputs 并写明是哪个关键字——这些过程在 Go 版里不是启用的，路由器答 404 / 501。
func genInputs(root *value) ([]byte, error) {
	paths := root.get("paths")
	if paths == nil || paths.kind != 'o' {
		return nil, fmt.Errorf("cloud-openapi.json 没有 paths")
	}
	type entry struct{ op, literal, keyword string }
	var entries []entry
	for _, path := range paths.keys {
		item := paths.obj[path]
		for _, method := range item.keys {
			operation := item.obj[method]
			op := operation.get("operationId").str
			lit, err := inputLiteral(operation)
			var unsupported *unsupportedKeyword
			switch {
			case err == nil:
				entries = append(entries, entry{op: op, literal: lit})
			case errors.As(err, &unsupported):
				entries = append(entries, entry{op: op, keyword: unsupported.keyword})
			default:
				return nil, fmt.Errorf("%s：%w", op, err)
			}
		}
	}
	var f file
	f.printf("// RouteInputs 是各过程的入参 schema，键是 operationId。路径参数、query 参数与 JSON 体合成一个对象。\n")
	f.printf("var RouteInputs = map[string]*schema.Schema{\n")
	for _, e := range entries {
		if e.keyword == "" {
			f.printf("%s: %s,\n", quote(e.op), e.literal)
		}
	}
	f.printf("}\n\n")
	f.printf("// UnsupportedInputs 列出入参 schema 用了生成器还不认识的关键字的过程（值是那个关键字）。\n")
	f.printf("var UnsupportedInputs = map[string]string{\n")
	for _, e := range entries {
		if e.keyword != "" {
			f.printf("%s: %s,\n", quote(e.op), quote(e.keyword))
		}
	}
	f.printf("}\n")
	return f.source("gen", "", []string{"github.com/AMA-Link/armadra-cloud/go/internal/protocol/schema"})
}

// inputLiteral 把一个过程的参数与请求体合成一个入参 schema 的 Go 字面量。
func inputLiteral(operation *value) (string, error) {
	var props, required []string
	if params := operation.get("parameters"); params != nil {
		for _, param := range params.arr {
			lit, err := literal(param.get("schema"))
			if err != nil {
				return "", fmt.Errorf("参数 %s：%w", param.get("name").str, err)
			}
			props = append(props, fmt.Sprintf("{Name: %s, Schema: %s}", quote(param.get("name").str), lit))
			if param.get("required") != nil && param.get("required").flag {
				required = append(required, quote(param.get("name").str))
			}
		}
	}
	body := operation.get("requestBody").get("content").get("application/json").get("schema")
	build := func(extraProps, extraRequired []string, typ string) string {
		parts := []string{fmt.Sprintf("Type: %s", quote(typ))}
		if len(extraProps) > 0 {
			parts = append(parts, fmt.Sprintf("Properties: []schema.Property{\n%s,\n}", strings.Join(extraProps, ",\n")))
		}
		if len(extraRequired) > 0 {
			parts = append(parts, fmt.Sprintf("Required: []string{%s}", strings.Join(extraRequired, ", ")))
		}
		return "&schema.Schema{\n" + strings.Join(parts, ",\n") + ",\n}"
	}
	// bodyParts 取一个对象型请求体的属性与必填字段。
	bodyParts := func(object *value) ([]string, []string, error) {
		if object.get("type").str != "object" {
			return nil, nil, fmt.Errorf("请求体应为 object")
		}
		for _, key := range object.keys {
			if !knownKeywords[key] {
				return nil, nil, &unsupportedKeyword{keyword: key}
			}
		}
		var ps, rs []string
		if properties := object.get("properties"); properties != nil {
			for _, key := range properties.keys {
				lit, err := literal(properties.obj[key])
				if err != nil {
					return nil, nil, fmt.Errorf("%s：%w", key, err)
				}
				ps = append(ps, fmt.Sprintf("{Name: %s, Schema: %s}", quote(key), lit))
			}
		}
		if req := object.get("required"); req != nil {
			for _, item := range req.arr {
				rs = append(rs, quote(item.str))
			}
		}
		return ps, rs, nil
	}
	switch {
	case body == nil:
		return build(props, required, "object"), nil
	case body.get("anyOf") != nil:
		var options []string
		for _, option := range body.get("anyOf").arr {
			ps, rs, err := bodyParts(option)
			if err != nil {
				return "", err
			}
			options = append(options, build(append(append([]string{}, props...), ps...), append(append([]string{}, required...), rs...), "object"))
		}
		return "&schema.Schema{\nAnyOf: []*schema.Schema{\n" + strings.Join(options, ",\n") + ",\n},\n}", nil
	}
	ps, rs, err := bodyParts(body)
	if err != nil {
		return "", err
	}
	return build(append(append([]string{}, props...), ps...), append(append([]string{}, required...), rs...), "object"), nil
}
