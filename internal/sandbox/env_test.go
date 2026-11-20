package sandbox

import "testing"

// execve 里 getenv 取第一个匹配，所以覆盖必须发生在原地，
// 追加同名项等于没设——这条曾经让 XDG_CACHE_HOME 静默失效。
func TestMergeEnvOverridesInPlace(t *testing.T) {
	base := []string{"HOME=/tmp/x", "XDG_CACHE_HOME=/tmp/x"}
	got := mergeEnv(base, []string{"XDG_CACHE_HOME=/data/.deps/cache"})

	if len(got) != 2 {
		t.Fatalf("覆盖不应产生重复键，得到 %v", got)
	}
	if got[0] != "HOME=/tmp/x" {
		t.Errorf("无关变量不该被改动：%v", got)
	}
	if got[1] != "XDG_CACHE_HOME=/data/.deps/cache" {
		t.Errorf("覆盖未生效：%v", got)
	}
}

func TestMergeEnvAppendsNewKeys(t *testing.T) {
	got := mergeEnv([]string{"A=1"}, []string{"B=2"})
	if len(got) != 2 || got[1] != "B=2" {
		t.Fatalf("新键应追加，得到 %v", got)
	}
}

func TestMergeEnvIgnoresMalformed(t *testing.T) {
	got := mergeEnv([]string{"A=1"}, []string{"NOEQUALS", "=orphan"})
	if len(got) != 1 || got[0] != "A=1" {
		t.Fatalf("没有等号或空名的项应被忽略，得到 %v", got)
	}
}
