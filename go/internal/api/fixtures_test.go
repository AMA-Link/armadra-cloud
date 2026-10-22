package api_test

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	protoschema "github.com/AMA-Link/armadra-cloud/go/internal/protocol/schema"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil/schema"
)

func loadOpenAPI(t *testing.T) *schema.OpenAPI {
	t.Helper()
	return schema.Load(t, filepath.Join(testutil.ProtocolDir(t), "schema", "cloud-openapi.json"))
}

func fixture(t *testing.T, op string) schema.Sample {
	t.Helper()
	var sample schema.Sample
	testutil.FixtureJSON(t, "cloud-api/"+op+".json", &sample)
	return sample
}

// 全部 cloud-api 夹具（每个过程一个）：输入与输出都要过 schema 校验。输出按 cloud-openapi.json 的 200 响应校验；
// 输入先按 OpenAPI 里的参数与请求体 schema 校验（独立的校验库），再按生成出来的 gen.RouteInputs
// （路由器真正用的解释器）校验——两边必须一致。
func TestFixturesPassInputAndOutputSchemas(t *testing.T) {
	api := loadOpenAPI(t)
	files := testutil.Fixtures(t, "cloud-api", ".json")
	if len(files) != len(gen.Routes) {
		t.Fatalf("夹具应与路由表一一对应：路由 %d 条，夹具 %d 个", len(gen.Routes), len(files))
	}
	checked, unsupported := 0, 0
	for _, file := range files {
		op := strings.TrimSuffix(file, ".json")
		t.Run(op, func(t *testing.T) {
			sample := fixture(t, op)
			input := []byte(sample.Input)
			if len(input) == 0 {
				t.Fatal("夹具没有输入")
			}
			if err := api.ValidateInput(op, input); err != nil {
				t.Fatalf("输入没通过 OpenAPI 的入参 schema：%v\n%s", err, input)
			}
			for i, output := range sample.All() {
				if err := api.ValidateOutput(op, output); err != nil {
					t.Errorf("输出 %d 没通过：%v", i, err)
				}
			}
			ours, ok := gen.RouteInputs[op]
			if !ok {
				if _, listed := gen.UnsupportedInputs[op]; !listed {
					t.Fatalf("%s 既没有入参 schema，也没登记为不支持", op)
				}
				unsupported++
				return
			}
			var value any
			if err := json.Unmarshal(input, &value); err != nil {
				t.Fatal(err)
			}
			if err := ours.Validate(value); err != nil {
				t.Fatalf("输入没通过生成的入参 schema：%v\n%s", err, input)
			}
			checked++
		})
	}
	t.Logf("%d 个夹具：%d 个入参用生成的 schema 校验，%d 个登记为不支持（%v）", len(files), checked, unsupported, sortedKeys(gen.UnsupportedInputs))
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// 校验器确实在校验：每个夹具的输入去掉一个必填字段、或把必填字段改成错类型，独立校验库与生成的
// 解释器都要拒绝，而且两边的结论一致。
func TestFixtureInputsAreRejectedWhenBroken(t *testing.T) {
	api := loadOpenAPI(t)
	rejected := 0
	for _, route := range gen.Routes {
		ours, ok := gen.RouteInputs[route.Procedure]
		if !ok {
			continue
		}
		var input map[string]any
		if err := json.Unmarshal(fixture(t, route.Procedure).Input, &input); err != nil {
			t.Fatal(err)
		}
		required := requiredOf(ours)
		for _, name := range required {
			broken := clone(input)
			delete(broken, name)
			data, _ := json.Marshal(broken)
			oursErr := ours.Validate(roundTrip(t, broken))
			theirs := api.ValidateInput(route.Procedure, data)
			if (oursErr == nil) != (theirs == nil) {
				t.Errorf("%s 去掉 %s 后两边结论不同：解释器 %v，校验库 %v", route.Procedure, name, oursErr, theirs)
			}
			if oursErr == nil {
				t.Errorf("%s 去掉必填的 %s 应被拒绝", route.Procedure, name)
			}
			rejected++
		}
	}
	if rejected < 20 {
		t.Fatalf("只覆盖了 %d 个必填字段，太少", rejected)
	}
}

func clone(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for key, value := range m {
		out[key] = value
	}
	return out
}

func roundTrip(t *testing.T, v any) any {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// requiredOf 取入参 schema 的必填字段；联合体（links.create）取第一个候选。
func requiredOf(s *protoschema.Schema) []string {
	if len(s.AnyOf) > 0 {
		return s.AnyOf[0].Required
	}
	return s.Required
}
