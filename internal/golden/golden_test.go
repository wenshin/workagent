package golden

import (
	"strings"
	"testing"
)

// TestHarnessGolden 是 harness 的回归闸门：循环行为一旦变了，
// 这里会先失败，而不是等用户收到一条空回答。
func TestHarnessGolden(t *testing.T) {
	suite, err := LoadFile("testdata/harness.json")
	if err != nil {
		t.Fatalf("加载黄金用例失败：%v", err)
	}
	results := RunSuite(suite, Options{})
	if len(results) == 0 {
		t.Fatal("没有 agent 类用例被执行")
	}
	for _, r := range results {
		if !r.Passed {
			t.Errorf("用例 %q 未通过：\n  %s\n  观察：%s",
				r.Name, strings.Join(r.Failures, "\n  "), r.Detail)
		}
	}
}

func TestSuiteRejectsEmptyAssertions(t *testing.T) {
	// 没有任何断言的用例会稳定通过，比没有用例更危险
	raw := `{"version":1,"cases":[{"name":"空断言","kind":"agent","query":"x","script":[{"content":"y"}]}]}`
	if _, err := Load(strings.NewReader(raw)); err == nil {
		t.Fatal("缺少断言的用例应当被拒绝")
	}
}

func TestSuiteRejectsUnknownFields(t *testing.T) {
	// 拼错的断言字段必须报错，否则用例会变成「假绿」
	raw := `{"version":1,"cases":[{"name":"拼错字段","kind":"agent","query":"x",
		"script":[{"content":"y"}],"expect":{"content_contain":["y"]}}]}`
	if _, err := Load(strings.NewReader(raw)); err == nil {
		t.Fatal("未知字段应当被拒绝")
	}
}
