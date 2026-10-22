package api

import (
	"net/url"
	"reflect"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/schema"
)

func mustQuery(t *testing.T, values url.Values, input *schema.Schema) map[string]any {
	t.Helper()
	out, err := queryObject(values, input)
	if err != nil {
		t.Fatalf("query 不该出错：%v", err)
	}
	return out
}

func TestQueryObjectCoercesByDeclaredType(t *testing.T) {
	input := gen.RouteInputs["me.traffic"]
	values := url.Values{"fromMs": {"1759913600000"}, "toMs": {"1760000000000"}, "sourceId": {"0123456789abcdef0123456789abcdef"}, "granularity": {"day"}}
	got := mustQuery(t, values, input)
	want := map[string]any{
		"fromMs": float64(1759913600000), "toMs": float64(1760000000000),
		"sourceId": "0123456789abcdef0123456789abcdef", "granularity": "day",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("query 转换：\n得到 %+v\n应为 %+v", got, want)
	}
	if _, err := decodeInput(input, got); err != nil {
		t.Fatalf("合格的 query 应通过校验：%v", err)
	}

	// 非数组参数重复出现算错；换不动的数字原样留着，由校验拒绝。
	if _, err := queryObject(url.Values{"fromMs": {"1", "2"}, "toMs": {"2"}}, input); err == nil {
		t.Fatal("非数组参数重复应被拒绝")
	}
	got = mustQuery(t, url.Values{"fromMs": {"abc"}, "toMs": {"2"}}, input)
	if _, err := decodeInput(input, got); err == nil {
		t.Fatal("fromMs 不是数字应被拒绝")
	}
	// 空串当 0（z.coerce 的行为）。
	if got = mustQuery(t, url.Values{"fromMs": {""}, "toMs": {"5"}}, input); got["fromMs"] != float64(0) {
		t.Fatalf("空串应当 0：%v", got["fromMs"])
	}
}

// schema 声明为 array 的 query 参数收全部同名值（sources.list 的 tag），不再只取最后一个。
func TestQueryObjectCollectsArrayParameters(t *testing.T) {
	input := gen.RouteInputs["sources.list"]
	got := mustQuery(t, url.Values{"tag": {"a", "b", "c"}}, input)
	if !reflect.DeepEqual(got["tag"], []any{"a", "b", "c"}) {
		t.Fatalf("应收全部同名值：%v", got["tag"])
	}
	if _, err := decodeInput(input, got); err != nil {
		t.Fatalf("合格的数组 query 应通过校验：%v", err)
	}
	if got = mustQuery(t, url.Values{"tag": {"only"}}, input); !reflect.DeepEqual(got["tag"], []any{"only"}) {
		t.Fatalf("单个值也是数组：%v", got["tag"])
	}
}

func TestQueryObjectExpandsDeepObject(t *testing.T) {
	got := mustQuery(t, url.Values{"scope[workspaceId]": {"ws_01"}, "scope[readOnly]": {"true"}, "plain": {"x"}, "bad[": {"y"}, "a[b][c]": {"z"}}, nil)
	want := map[string]any{
		"scope": map[string]any{"workspaceId": "ws_01", "readOnly": "true"},
		"plain": "x", "bad[": "y", "a[b][c]": "z",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("deepObject 展开：\n得到 %+v\n应为 %+v", got, want)
	}
}

func TestDecodeInputDropsUnlistedFields(t *testing.T) {
	out, err := decodeInput(gen.RouteInputs["auth.refresh"], map[string]any{"refreshToken": "abc", "多出来的": 1})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, map[string]any{"refreshToken": "abc"}) {
		t.Fatalf("未列出的字段应被丢掉：%+v", out)
	}
	if _, err := decodeInput(gen.RouteInputs["auth.refresh"], map[string]any{}); err != nil {
		t.Fatalf("refreshToken 可省：%v", err)
	}
	if _, err := decodeInput(gen.RouteInputs["auth.login"], map[string]any{}); err == nil {
		t.Fatal("缺必填字段应被拒绝")
	}
}

func TestQuerySpaceUsesBracketForm(t *testing.T) {
	input := gen.RouteInputs["sources.list"]
	query, err := url.ParseQuery("space[kind]=team&space[teamId]=t1")
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeInput(input, mustQuery(t, query, input))
	if err != nil {
		t.Fatalf("括号形式的 space 应被接受：%v", err)
	}
	want := map[string]any{"space": map[string]any{"kind": "team", "teamId": "t1"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("得到 %+v，应为 %+v", got, want)
	}
	// 点号与 JSON 串不是约定的写法。
	for _, raw := range []string{"space.kind=team&space.teamId=t1", "space=%7B%22kind%22%3A%22team%22%7D"} {
		q, _ := url.ParseQuery(raw)
		if out, err := decodeInput(input, mustQuery(t, q, input)); err == nil && out["space"] != nil {
			t.Fatalf("%s 不应被当成 space：%+v", raw, out)
		}
	}
}

// oRPC 客户端（@orpc/openapi-client）把数组写成 tag[0]=a&tag[1]=b，对象写成 space[kind]=…。
func TestQueryAcceptsTheOfficialClientSerialization(t *testing.T) {
	input := gen.RouteInputs["sources.list"]
	parse := func(raw string) (map[string]any, error) {
		q, err := url.ParseQuery(raw)
		if err != nil {
			t.Fatal(err)
		}
		got, err := queryObject(q, input)
		if err != nil {
			return nil, err
		}
		return decodeInput(input, got)
	}
	want := map[string]any{
		"space": map[string]any{"kind": "team", "teamId": "t"},
		"tag":   []any{"a", "b"},
	}
	for _, raw := range []string{
		"space[kind]=team&space[teamId]=t&tag[0]=a&tag[1]=b",
		"space[kind]=team&space[teamId]=t&tag[]=a&tag[]=b",
		"space[kind]=team&space[teamId]=t&tag=a&tag=b",
		"space[kind]=team&space[teamId]=t&tag[1]=b&tag[0]=a",
		"space[kind]=team&space[teamId]=t&tag%5B0%5D=a&tag%5B1%5D=b",
	} {
		got, err := parse(raw)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%s：得到 %+v（%v），应为 %+v", raw, got, err, want)
		}
	}
	// 下标按数值排序，不是字典序。
	got, err := parse("tag[10]=k&tag[2]=c")
	if err != nil || !reflect.DeepEqual(got["tag"], []any{"c", "k"}) {
		t.Errorf("下标应按数值排序：%v %v", got, err)
	}
}

func TestQueryRejectsAmbiguousForms(t *testing.T) {
	input := gen.RouteInputs["sources.list"]
	for _, raw := range []string{
		"status=online&status=offline",          // 非数组参数重复
		"tag=a&tag[0]=b",                        // 平铺与括号混用
		"tag[]=a&tag[0]=b",                      // [] 与下标混用
		"tag[0]=a&tag[0]=b",                     // 下标重复
		"tag[x]=a",                              // 数组的键不是数字
		"tag[-1]=a",                             // 负下标
		"space=team&space[kind]=team",           // 对象平铺与括号混用
		"space[kind]=team&space[kind]=personal", // 对象成员重复
	} {
		q, _ := url.ParseQuery(raw)
		if _, err := queryObject(q, input); err == nil {
			t.Errorf("%s 应被拒绝", raw)
		}
	}
	// 结果与遍历顺序无关：多次运行结论一致。
	for i := 0; i < 20; i++ {
		q, _ := url.ParseQuery("tag=a&tag[0]=b&status=x&status=y")
		if _, err := queryObject(q, input); err == nil {
			t.Fatal("应稳定地被拒绝")
		}
	}
}
