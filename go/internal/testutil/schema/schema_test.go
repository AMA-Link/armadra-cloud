package schema_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil/schema"
)

func load(t *testing.T) *schema.OpenAPI {
	return schema.Load(t, filepath.Join(testutil.ProtocolDir(t), "schema", "cloud-openapi.json"))
}

func sample(t *testing.T, op string) schema.Sample {
	var s schema.Sample
	testutil.FixtureJSON(t, "cloud-api/"+op+".json", &s)
	return s
}

// 每个过程正好一份夹具，夹具的每个输出都通过 cloud-openapi.json 里的 200 响应 schema。
func TestFixturesMatchOutputSchemas(t *testing.T) {
	api := load(t)
	ops := api.Operations()

	files := testutil.Fixtures(t, "cloud-api", ".json")
	var names []string
	for _, f := range files {
		names = append(names, strings.TrimSuffix(f, ".json"))
	}
	if strings.Join(names, ",") != strings.Join(ops, ",") {
		t.Fatalf("夹具与过程不一一对应：\n夹具 %v\n过程 %v", names, ops)
	}

	outputs := 0
	for _, op := range ops {
		t.Run(op, func(t *testing.T) {
			all := sample(t, op).All()
			if len(all) == 0 {
				t.Fatal("夹具没有输出")
			}
			for i, out := range all {
				outputs++
				if err := api.ValidateOutput(op, out); err != nil {
					t.Errorf("输出 %d 没通过：%v", i, err)
				}
			}
		})
	}
	t.Logf("%d 个过程，%d 个输出", len(ops), outputs)
}

// 路由表里的每个过程都有输出 schema，反过来也一样。
func TestRoutesCoverEveryOperation(t *testing.T) {
	api := load(t)
	var routes []string
	for _, r := range gen.Routes {
		routes = append(routes, r.Procedure)
	}
	ops := api.Operations()
	if len(routes) != len(ops) {
		t.Fatalf("路由表 %d 条，OpenAPI %d 个过程", len(routes), len(ops))
	}
	seen := map[string]bool{}
	for _, r := range routes {
		seen[r] = true
	}
	for _, op := range ops {
		if !seen[op] {
			t.Errorf("路由表缺 %s", op)
		}
	}
}

// 校验器确实在校验：改坏一个输出就过不了。
func TestBrokenOutputsFail(t *testing.T) {
	api := load(t)
	good := sample(t, "auth.login").All()[0]

	var doc map[string]any
	if err := json.Unmarshal(good, &doc); err != nil {
		t.Fatal(err)
	}
	session := doc["session"].(map[string]any)
	session["accessExpiresAtMs"] = "不是数字"
	broken, _ := json.Marshal(doc)
	if err := api.ValidateOutput("auth.login", broken); err == nil {
		t.Error("字段类型不对应当失败")
	}

	delete(session, "accessToken")
	session["accessExpiresAtMs"] = 1
	missing, _ := json.Marshal(doc)
	if err := api.ValidateOutput("auth.login", missing); err == nil {
		t.Error("缺必填字段应当失败")
	}

	if err := api.ValidateOutput("auth.login", []byte(`[]`)); err == nil {
		t.Error("形状完全不对应当失败")
	}
	if err := api.ValidateOutput("nope", good); err == nil {
		t.Error("没有的过程应当报错")
	}
}
