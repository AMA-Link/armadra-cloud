package schema_test

import (
	"encoding/json"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/schema"
)

var sample = &schema.Schema{
	Type: "object",
	Properties: []schema.Property{
		{Name: "name", Schema: &schema.Schema{Type: "string", MinLength: schema.Int(1), MaxLength: schema.Int(4)}},
		{Name: "n", Schema: &schema.Schema{Type: "integer", Minimum: schema.Num(0), Maximum: schema.Num(10)}},
		{Name: "pos", Schema: &schema.Schema{Type: "number", ExclusiveMinimum: schema.Num(0)}},
		{Name: "kind", Schema: &schema.Schema{Type: "string", Enum: []any{"a", "b"}}},
		{Name: "ver", Schema: &schema.Schema{Type: "number", Const: float64(1), HasConst: true}},
		{Name: "id", Schema: &schema.Schema{Type: "string", Pattern: "^[0-9a-f]{4}$"}},
		{Name: "tags", Schema: &schema.Schema{Type: "array", MaxItems: schema.Int(2), Items: &schema.Schema{Type: "string"}}},
		{Name: "pair", Schema: &schema.Schema{Type: "array", PrefixItems: []*schema.Schema{{Type: "string"}, {Type: "integer"}}}},
		{Name: "maybe", Schema: &schema.Schema{AnyOf: []*schema.Schema{{Type: "string"}, {Type: "null"}}}},
	},
	Required: []string{"name"},
}

func check(t *testing.T, doc string, ok bool) {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(doc), &v); err != nil {
		t.Fatal(err)
	}
	if err := sample.Validate(v); (err == nil) != ok {
		t.Errorf("%s：期望通过=%v，实际错误 %v", doc, ok, err)
	}
}

func TestValidate(t *testing.T) {
	check(t, `{"name":"abcd"}`, true)
	check(t, `{}`, false)
	check(t, `{"name":""}`, false)
	check(t, `{"name":"abcde"}`, false)
	check(t, `{"name":"😀😀"}`, true)   // 按 UTF-16 码元计：4
	check(t, `{"name":"😀😀😀"}`, false) // 6
	check(t, `{"name":"a","n":10}`, true)
	check(t, `{"name":"a","n":11}`, false)
	check(t, `{"name":"a","n":-1}`, false)
	check(t, `{"name":"a","n":1.5}`, false)
	check(t, `{"name":"a","n":1.0}`, true)
	check(t, `{"name":"a","pos":0}`, false)
	check(t, `{"name":"a","pos":0.1}`, true)
	check(t, `{"name":"a","kind":"c"}`, false)
	check(t, `{"name":"a","ver":1}`, true)
	check(t, `{"name":"a","ver":2}`, false)
	check(t, `{"name":"a","id":"00ff"}`, true)
	check(t, `{"name":"a","id":"00ffx"}`, false)
	check(t, `{"name":"a","tags":["x","y"]}`, true)
	check(t, `{"name":"a","tags":["x","y","z"]}`, false)
	check(t, `{"name":"a","tags":[1]}`, false)
	check(t, `{"name":"a","pair":["x",1]}`, true)
	check(t, `{"name":"a","pair":["x"]}`, false)
	check(t, `{"name":"a","pair":["x",1,2]}`, false)
	check(t, `{"name":"a","maybe":null}`, true)
	check(t, `{"name":"a","maybe":"s"}`, true)
	check(t, `{"name":"a","maybe":1}`, false)
	check(t, `{"name":"a","unknown":{"x":1}}`, true)
	check(t, `[]`, false)
}

func TestDecodeIgnoresKeysThatOnlyDifferInCase(t *testing.T) {
	type out struct {
		Name string `json:"name"`
	}
	var got out
	if err := schema.Decode(sample, []byte(`{"name":"ok","NAME":"evil"}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.Name != "ok" {
		t.Fatalf("name = %q，大小写不同的键不该覆盖已校验的值", got.Name)
	}
	if err := schema.Decode(sample, []byte(`{"NAME":"x"}`), &got); err == nil {
		t.Fatal("只有大小写不同的键不算提供了必填字段")
	}
	if err := schema.Decode(sample, []byte("{\"name\":\"\xff\"}"), &got); err == nil {
		t.Fatal("非法 UTF-8 应被拒")
	}
}

func TestErrorShowsPath(t *testing.T) {
	var v any
	_ = json.Unmarshal([]byte(`{"name":"a","tags":["x",2]}`), &v)
	err := sample.Validate(v)
	if err == nil || err.Error() != "/tags/1：类型应为 string" {
		t.Fatalf("得到 %v", err)
	}
}
