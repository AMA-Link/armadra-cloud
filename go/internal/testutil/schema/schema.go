// Package schema 用 JSON Schema 校验 cloud-api 夹具的形状。只在测试里用。
//
// schema 取自协议包导出的 cloud-openapi.json：每个过程（operationId）的 200 响应。
// 夹具的输入与输出都用它校验；控制核的 HTTP 层测试也拿它校验真实的响应。
package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// OpenAPI 是编译好的过程输出 schema。
type OpenAPI struct {
	outputs map[string]*jsonschema.Schema
	inputs  map[string]*inputSchemas
}

// inputSchemas 是一个过程的入参 schema：路径 / query 参数各一个，加上请求体（有就有）。
type inputSchemas struct {
	params map[string]*jsonschema.Schema
	// required 是必填参数名。
	required []string
	body     *jsonschema.Schema
}

type document struct {
	Paths map[string]map[string]struct {
		OperationID string `json:"operationId"`
		Parameters  []struct {
			Name     string          `json:"name"`
			In       string          `json:"in"`
			Required bool            `json:"required"`
			Schema   json.RawMessage `json:"schema"`
		} `json:"parameters"`
		RequestBody *struct {
			Content map[string]struct {
				Schema json.RawMessage `json:"schema"`
			} `json:"content"`
		} `json:"requestBody"`
		Responses map[string]struct {
			Content map[string]struct {
				Schema json.RawMessage `json:"schema"`
			} `json:"content"`
		} `json:"responses"`
	} `json:"paths"`
}

// Load 读 cloud-openapi.json，编译每个过程的 200 响应 schema。
func Load(t testing.TB, path string) *OpenAPI {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc document
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("%s：%v", path, err)
	}
	compiler := jsonschema.NewCompiler()
	urls := map[string]string{}
	for route, methods := range doc.Paths {
		for method, op := range methods {
			ok, found := op.Responses["200"]
			body, hasJSON := ok.Content["application/json"]
			if !found || !hasJSON || len(body.Schema) == 0 {
				t.Fatalf("%s %s（%s）没有 200 的 JSON 响应", method, route, op.OperationID)
			}
			url := "https://armadra.test/openapi/" + op.OperationID + ".json"
			inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(body.Schema))
			if err != nil {
				t.Fatal(err)
			}
			if err := compiler.AddResource(url, inst); err != nil {
				t.Fatal(err)
			}
			if _, dup := urls[op.OperationID]; dup {
				t.Fatalf("operationId 重复：%s", op.OperationID)
			}
			urls[op.OperationID] = url
		}
	}
	out := &OpenAPI{outputs: map[string]*jsonschema.Schema{}, inputs: map[string]*inputSchemas{}}
	for _, methods := range doc.Paths {
		for _, op := range methods {
			in := &inputSchemas{params: map[string]*jsonschema.Schema{}}
			compile := func(name string, raw json.RawMessage) *jsonschema.Schema {
				url := "https://armadra.test/openapi/in/" + op.OperationID + "/" + name + ".json"
				inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
				if err != nil {
					t.Fatal(err)
				}
				if err := compiler.AddResource(url, inst); err != nil {
					t.Fatal(err)
				}
				compiled, err := compiler.Compile(url)
				if err != nil {
					t.Fatalf("%s 的入参 %s 编译失败：%v", op.OperationID, name, err)
				}
				return compiled
			}
			for _, param := range op.Parameters {
				in.params[param.Name] = compile("param-"+param.Name, param.Schema)
				if param.Required {
					in.required = append(in.required, param.Name)
				}
			}
			if op.RequestBody != nil {
				if body, ok := op.RequestBody.Content["application/json"]; ok {
					in.body = compile("body", body.Schema)
				}
			}
			out.inputs[op.OperationID] = in
		}
	}
	for id, url := range urls {
		compiled, err := compiler.Compile(url)
		if err != nil {
			t.Fatalf("%s 的 schema 编译失败：%v", id, err)
		}
		out.outputs[id] = compiled
	}
	return out
}

// Operations 列出全部过程名（排序后）。
func (o *OpenAPI) Operations() []string {
	names := make([]string, 0, len(o.outputs))
	for name := range o.outputs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ValidateOutput 校验 JSON 文本是不是 op 的一个合法输出。
func (o *OpenAPI) ValidateOutput(op string, instance []byte) error {
	compiled, ok := o.outputs[op]
	if !ok {
		return fmt.Errorf("没有过程 %s", op)
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(instance))
	if err != nil {
		return err
	}
	return compiled.Validate(value)
}

// Sample 是 fixtures/cloud-api/<procedure>.json：一个输入，加一个或几个输出。
type Sample struct {
	Input   json.RawMessage   `json:"input"`
	Output  json.RawMessage   `json:"output"`
	Outputs []json.RawMessage `json:"outputs"`
}

// All 返回全部输出（`output` 与 `outputs` 两种写法都认）。
func (s Sample) All() []json.RawMessage {
	if len(s.Outputs) > 0 {
		return s.Outputs
	}
	if len(s.Output) > 0 {
		return []json.RawMessage{s.Output}
	}
	return nil
}

// ValidateInput 校验一个过程的入参（JSON 对象）：参数名对应的键按参数 schema 校验，其余的键整体
// 按请求体 schema 校验；必填参数不能缺。夹具里的 input 就是这个形状。
func (o *OpenAPI) ValidateInput(op string, instance []byte) error {
	in, ok := o.inputs[op]
	if !ok {
		return fmt.Errorf("没有过程 %s", op)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(instance, &fields); err != nil {
		return err
	}
	for _, name := range in.required {
		if _, present := fields[name]; !present {
			return fmt.Errorf("缺少必填参数 %s", name)
		}
	}
	rest := map[string]json.RawMessage{}
	for key, raw := range fields {
		schema, isParam := in.params[key]
		if !isParam {
			rest[key] = raw
			continue
		}
		value, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			return err
		}
		if err := schema.Validate(value); err != nil {
			return fmt.Errorf("参数 %s：%w", key, err)
		}
	}
	if in.body == nil {
		return nil
	}
	body, err := json.Marshal(rest)
	if err != nil {
		return err
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		return err
	}
	return in.body.Validate(value)
}
