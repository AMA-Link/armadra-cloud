package protocol_test

import (
	"regexp"
	"sort"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil"
)

// fixtures/errors.json 与 gen/errors.go 一一对应：码、HTTP 状态。
func TestErrorCodesMatchFixture(t *testing.T) {
	var fixture map[string]struct {
		Status int `json:"status"`
	}
	testutil.FixtureJSON(t, "errors.json", &fixture)
	if len(fixture) == 0 {
		t.Fatal("夹具是空的")
	}

	seen := map[string]bool{}
	for _, code := range gen.ErrorCodes {
		if seen[string(code)] {
			t.Errorf("gen.ErrorCodes 里 %s 出现了两次", code)
		}
		seen[string(code)] = true
		want, ok := fixture[string(code)]
		if !ok {
			t.Errorf("夹具里没有 %s", code)
			continue
		}
		got, ok := code.HTTPStatus()
		if !ok || got != want.Status {
			t.Errorf("%s 的状态码：生成物 %d（登记 %v），夹具 %d", code, got, ok, want.Status)
		}
	}
	var missing []string
	for code := range fixture {
		if !seen[code] {
			missing = append(missing, code)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("生成物缺这些码：%v", missing)
	}
	if len(gen.ErrorCodes) != len(fixture) {
		t.Errorf("生成物 %d 个码，夹具 %d 个", len(gen.ErrorCodes), len(fixture))
	}
	if _, ok := gen.ErrorCode("nope").HTTPStatus(); ok {
		t.Error("没登记的码不该有状态")
	}
}

func TestErrorCodesAreSnakeCaseWithErrorStatus(t *testing.T) {
	snake := regexp.MustCompile(`^[a-z]+(_[a-z]+)*$`)
	for _, code := range gen.ErrorCodes {
		if !snake.MatchString(string(code)) {
			t.Errorf("%s 不是 snake_case", code)
		}
		if status, _ := code.HTTPStatus(); status < 400 || status >= 600 {
			t.Errorf("%s 的状态码 %d 不在 4xx / 5xx", code, status)
		}
	}
}
