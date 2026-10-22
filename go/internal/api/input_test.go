package api

import (
	"net/url"
	"reflect"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

func TestQueryObjectCoercesByDeclaredType(t *testing.T) {
	input := gen.RouteInputs["me.traffic"]
	values := url.Values{"fromMs": {"1759913600000"}, "toMs": {"1760000000000"}, "sourceId": {"0123456789abcdef0123456789abcdef"}, "granularity": {"day"}}
	got := queryObject(values, input)
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

	// 同名多次出现取最后一个；换不动的数字原样留着，由校验拒绝。
	got = queryObject(url.Values{"fromMs": {"1", "abc"}, "toMs": {"2"}}, input)
	if got["fromMs"] != "abc" {
		t.Fatalf("应取最后一个：%v", got["fromMs"])
	}
	if _, err := decodeInput(input, got); err == nil {
		t.Fatal("fromMs 不是数字应被拒绝")
	}
	// 空串当 0（z.coerce 的行为）。
	if got = queryObject(url.Values{"fromMs": {""}, "toMs": {"5"}}, input); got["fromMs"] != float64(0) {
		t.Fatalf("空串应当 0：%v", got["fromMs"])
	}
}

func TestQueryObjectExpandsDeepObject(t *testing.T) {
	got := queryObject(url.Values{"scope[workspaceId]": {"ws_01"}, "scope[readOnly]": {"true"}, "plain": {"x"}, "bad[": {"y"}, "a[b][c]": {"z"}}, nil)
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
