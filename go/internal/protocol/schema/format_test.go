package schema_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/schema"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil"
)

type vectorSet struct {
	Valid   []string `json:"valid"`
	Invalid []string `json:"invalid"`
}

func loadVectors(t *testing.T) (email, uri vectorSet) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(testutil.ProtocolDir(t), "test", "format-vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Email vectorSet `json:"email"`
		URI   vectorSet `json:"uri"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Email, doc.URI
}

// 邮箱正则与协议包同源：Go 用的就是 constants.json 导出的 z.email() 源码，
// 且与 TypeScript 测试共用同一份向量（packages/platform-protocol/test/format-vectors.json）。
func TestEmailFormatAgreesWithProtocolVectors(t *testing.T) {
	email, _ := loadVectors(t)
	if gen.ValidationEmailPattern == "" {
		t.Fatal("constants.json 没导出邮箱正则")
	}
	s := &schema.Schema{Type: "string", Format: "email", FormatPattern: gen.ValidationEmailPattern}
	for _, value := range email.Valid {
		if err := s.Validate(value); err != nil {
			t.Errorf("%q 应是合法邮箱：%v", value, err)
		}
	}
	for _, value := range email.Invalid {
		if err := s.Validate(value); err == nil {
			t.Errorf("%q 应被拒绝", value)
		}
	}
	// 非字符串仍由 type 拒绝。
	if s.Validate(float64(1)) == nil {
		t.Error("数字不是字符串")
	}
}

// 生成出来的入参里 format: email 带的正则源码与常量是同一个。
func TestGeneratedEmailInputsUseTheExportedPattern(t *testing.T) {
	register := gen.RouteInputs["auth.register"]
	var field *schema.Schema
	for _, p := range register.Properties {
		if p.Name == "email" {
			field = p.Schema
		}
	}
	if field == nil || field.Format != "email" || field.FormatPattern != gen.ValidationEmailPattern {
		t.Fatalf("auth.register 的 email：%+v", field)
	}
}

func TestURIFormatAgreesWithProtocolVectors(t *testing.T) {
	_, uri := loadVectors(t)
	s := &schema.Schema{Type: "string", Format: "uri"}
	for _, value := range uri.Valid {
		if err := s.Validate(value); err != nil {
			t.Errorf("%q 应是合法 URL：%v", value, err)
		}
	}
	for _, value := range uri.Invalid {
		if err := s.Validate(value); err == nil {
			t.Errorf("%q 应被拒绝", value)
		}
	}
	// 与 z.url() 的差异（设计 §5.5）：有 scheme 没有 host 的（mailto:）在这里也拒绝。
	if s.Validate("mailto:a@b.co") == nil {
		t.Error("没有 host 应被拒绝")
	}
}

func TestMinItems(t *testing.T) {
	s := &schema.Schema{Type: "array", MinItems: schema.Int(1), Items: &schema.Schema{Type: "string"}}
	if s.Validate([]any{}) == nil {
		t.Error("空数组应被 minItems 拒绝")
	}
	if err := s.Validate([]any{"a"}); err != nil {
		t.Errorf("一项应通过：%v", err)
	}
}

func TestPropertyNamesRecordKeepsEveryKey(t *testing.T) {
	s := &schema.Schema{
		Type: "object",
		Properties: []schema.Property{
			{Name: "assertion", Schema: &schema.Schema{Type: "object", PropertyNames: &schema.Schema{Type: "string"}}},
		},
		Required: []string{"assertion"},
	}
	var out struct {
		Assertion map[string]any `json:"assertion"`
	}
	doc := `{"assertion":{"id":"x","nested":{"a":[1,2]},"n":3},"extra":1}`
	if err := schema.Decode(s, []byte(doc), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Assertion) != 3 || out.Assertion["id"] != "x" {
		t.Fatalf("record 应保留全部键：%v", out.Assertion)
	}
	if err := schema.Decode(s, []byte(`{"assertion":[]}`), &out); err == nil {
		t.Error("record 不是对象应被拒绝")
	}
	// 键 schema 不满足时拒绝。
	strict := &schema.Schema{Type: "object", PropertyNames: &schema.Schema{Type: "string", MaxLength: schema.Int(2)}}
	if strict.Validate(map[string]any{"abc": 1.0}) == nil {
		t.Error("键过长应被拒绝")
	}
}
