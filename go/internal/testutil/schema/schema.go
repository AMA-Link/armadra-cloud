// Package schema 用 JSON Schema 校验 cloud-api 夹具的形状。只在测试里用。
//
// schema 取自协议包导出的 cloud-openapi.json：每个过程（operationId）的 200 响应。
// 这一批只证明夹具能通过自校验；G2 的控制核服务会拿它校验真实的响应。
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
}

type document struct {
	Paths map[string]map[string]struct {
		OperationID string `json:"operationId"`
		Responses   map[string]struct {
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
	out := &OpenAPI{outputs: map[string]*jsonschema.Schema{}}
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
