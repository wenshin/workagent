package agentspec

import (
	"errors"
	"strings"
	"testing"
)

func boolPtr(v bool) *bool { return &v }

func TestValidateOK(t *testing.T) {
	specs := []Spec{
		{ID: "base", Name: "通用助理", Version: "1.0.0"},
		{ID: "analyst", Name: "分析师", Version: "2.3.1", Extends: BaseID},
		{ID: "a", Name: "x", Version: "0.0.1", Enabled: boolPtr(false)},
	}
	for _, s := range specs {
		if err := s.Validate(); err != nil {
			t.Errorf("Validate(%s) 不该失败: %v", s.ID, err)
		}
	}
}

func TestValidateRejects(t *testing.T) {
	cases := map[string]Spec{
		"大写 id": {ID: "Base", Name: "x", Version: "1.0.0"},
		"数字开头":  {ID: "1base", Name: "x", Version: "1.0.0"},
		"非法字符":  {ID: "base/../x", Name: "x", Version: "1.0.0"},
		"空名称":   {ID: "base", Name: "  ", Version: "1.0.0"},
		"版本不完整": {ID: "base", Name: "x", Version: "1.0"},
		"链式继承":  {ID: "c", Name: "x", Version: "1.0.0", Extends: "analyst"},
		"继承自身":  {ID: BaseID, Name: "x", Version: "1.0.0", Extends: BaseID},
	}
	for name, spec := range cases {
		if err := spec.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
}

func TestResolveAppendsBaseFirst(t *testing.T) {
	spec := Spec{ID: "analyst", Name: "分析师", Version: "1.0.0", Extends: BaseID}
	got, err := resolve(spec, "BASE-RULE", "OWN-RULE")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.Prompt, "BASE-RULE") {
		t.Fatalf("base 必须在前，实际: %q", got.Prompt)
	}
	if !strings.Contains(got.Prompt, "OWN-RULE") {
		t.Fatalf("追加内容丢失: %q", got.Prompt)
	}
	if strings.Index(got.Prompt, "BASE-RULE") > strings.Index(got.Prompt, "OWN-RULE") {
		t.Fatal("追加内容不能出现在 base 之前")
	}
	if got.ID != "analyst" || got.Version != "1.0.0" {
		t.Fatalf("元信息丢失: %+v", got)
	}
}

func TestResolveWithoutExtendsIgnoresBase(t *testing.T) {
	spec := Spec{ID: "base", Name: "通用助理", Version: "1.0.0"}
	got, err := resolve(spec, "BASE-RULE", "OWN-RULE")
	if err != nil {
		t.Fatal(err)
	}
	if got.Prompt != "OWN-RULE" {
		t.Fatalf("不继承时不该带上 base，实际: %q", got.Prompt)
	}
}

func TestResolveDisabled(t *testing.T) {
	spec := Spec{ID: "off", Name: "停用", Version: "1.0.0", Enabled: boolPtr(false)}
	if _, err := resolve(spec, "", "x"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("err = %v, want ErrDisabled", err)
	}
}

func TestResolveRejectsOversizePrompt(t *testing.T) {
	spec := Spec{ID: "big", Name: "大", Version: "1.0.0"}
	big := strings.Repeat("a", MaxPromptBytes+1)
	if _, err := resolve(spec, "", big); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func TestResolveEmptyBoth(t *testing.T) {
	spec := Spec{ID: "analyst", Name: "分析师", Version: "1.0.0", Extends: BaseID}
	got, err := resolve(spec, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Prompt != "" {
		t.Fatalf("两端都空时应为空串，实际: %q", got.Prompt)
	}
}
