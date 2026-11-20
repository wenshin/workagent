package tool

import (
	"strings"
	"testing"

	"github.com/workagent/auto-agent/internal/sandbox"
)

func TestSetInstallMirrors(t *testing.T) {
	orig := installMirror
	defer func() { installMirror = orig }()

	SetInstallMirrors(InstallMirrors{NPM: "https://npm.internal/"})
	if got := CurrentInstallMirrors().NPM; got != "https://npm.internal/" {
		t.Fatalf("NPM 未被覆盖：%q", got)
	}
	// 空字段保留原值：部署只想换一个源时，不该把其余三个清空。
	if CurrentInstallMirrors().PyPI != orig.PyPI {
		t.Fatalf("未指定的字段应保留原值，得到 %q", CurrentInstallMirrors().PyPI)
	}
	if got := InstallMirrorSources()["npm"]; got != "https://npm.internal/" {
		t.Fatalf("提示词来源与注入值不一致：%q", got)
	}
}

// installEnv 是「提示词说用什么源」和「命令实际用什么源」的唯一出处。
// 这里断言的路径必须落在 .deps 下——出网执行时那是唯一挂载的子目录，
// 写到别处的依赖下次调用就没了。
func TestInstallEnvStaysInsideDeps(t *testing.T) {
	workDir := "/data/space/files"
	deps := sandbox.DepsDir(workDir)
	env := installEnv(workDir)
	if len(env) == 0 {
		t.Fatal("installEnv 不应为空")
	}
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			t.Errorf("环境变量格式不对：%q", kv)
			continue
		}
		// 安装源是 URL，不是路径：这类变量按定义就指向外部。
		if strings.Contains(k, "URL") || strings.Contains(k, "REGISTRY") ||
			strings.Contains(k, "MIRROR") || strings.Contains(k, "INDEX") || k == "GOPROXY" {
			continue
		}
		if !strings.HasPrefix(v, deps) {
			t.Errorf("%s 指向 .deps 之外：%s", k, v)
		}
	}
}
