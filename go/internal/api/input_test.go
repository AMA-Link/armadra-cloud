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
	// 下标按数值排序，不是字典序（0..11 里 10、11 排在 2 之后）。
	got, err := parse("tag[10]=k&tag[2]=c&tag[0]=a&tag[1]=b&tag[3]=d&tag[4]=e&tag[5]=f&tag[6]=g&tag[7]=h&tag[8]=i&tag[9]=j&tag[11]=l")
	if err != nil || !reflect.DeepEqual(got["tag"], []any{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l"}) {
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
		"space[kind][x]=team",                   // 不成形的括号键，前缀是已声明的属性
		"tag[0][x]=a",                           // 同上，前缀是数组属性
		"space[kind=team",                       // 缺右括号，前缀是已声明的属性
		"tag[01]=a",                             // 下标带前导 0
		"tag[+1]=a",                             // 下标带正号
		"tag[1]=a",                              // 下标不从 0 开始
		"tag[0]=a&tag[2]=b",                     // 下标有空洞
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

func TestQueryKeepsUndeclaredMalformedKeys(t *testing.T) {
	input := gen.RouteInputs["sources.list"]
	// 前缀不是已声明的属性：原样保留，由校验剪枝（与之前一致）。
	q, _ := url.ParseQuery("unknown[a][b]=1&bad[=2")
	if _, err := queryObject(q, input); err != nil {
		t.Fatalf("未声明的前缀不该算错：%v", err)
	}
	q, _ = url.ParseQuery("tag[1]=b&tag[0]=a&tag[2]=c")
	got, err := queryObject(q, input)
	if err != nil || len(got["tag"].([]any)) != 3 || got["tag"].([]any)[0] != "a" {
		t.Fatalf("下标 0..n-1 乱序应按序并入：%v %v", got, err)
	}
}
