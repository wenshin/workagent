package tool

import (
	"path/filepath"
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
//
// 两类目录不能混：
//   - 装好的包（npm 全局、venv）必须在空间目录的 .pkg 下，Pod 重建后还在
//   - 下载缓存必须在租户缓存的 CacheRoot 下，跨空间共享
//
// 混了的表现：包被放进 /cache 后被另一个空间用同名版本覆盖，
// 或者缓存落进空间目录，每个空间各存一份。
func TestInstallEnvSplitsSpaceAndCache(t *testing.T) {
	// 沙箱内的固定路径：宿主目录不进环境变量
	pkg := sandbox.PkgDir
	cache := sandbox.CacheDir

	env := InstallEnv()
	if len(env) == 0 {
		t.Fatal("installEnv 不应为空")
	}

	// 装好的包：必须落在空间目录
	for _, key := range []string{"NPM_CONFIG_PREFIX", "VIRTUAL_ENV"} {
		v, ok := lookupEnv(env, key)
		if !ok {
			t.Fatalf("缺少 %s", key)
		}
		if !strings.HasPrefix(v, pkg) {
			t.Errorf("%s 应落在 %s 下，实际 %s", key, pkg, v)
		}
	}

	// 下载缓存：必须落在租户缓存
	for _, key := range []string{"NPM_CONFIG_CACHE", "PIP_CACHE_DIR", "XDG_CACHE_HOME", "GOMODCACHE"} {
		v, ok := lookupEnv(env, key)
		if !ok {
			t.Fatalf("缺少 %s", key)
		}
		if !strings.HasPrefix(v, cache) {
			t.Errorf("%s 应落在租户缓存 %s 下，实际 %s", key, cache, v)
		}
	}

	// PATH 必须以 fallback 结尾：--clearenv 之后只拼私有目录，
	// 连 /bin/sh 都找不到。
	path, ok := lookupEnv(env, "PATH")
	if !ok {
		t.Fatal("缺少 PATH")
	}
	if !strings.HasSuffix(path, sandbox.FallbackPath()) {
		t.Errorf("PATH 必须以 fallbackPath 结尾，实际 %q", path)
	}
	if !strings.HasPrefix(path, filepath.Join(pkg, "venv", "bin")) {
		t.Errorf("venv 必须排在 PATH 最前，实际 %q", path)
	}
}

// venv 里的 pip 失效时（跨镜像复用、Python 版本变了），pip 会静默回落到
// /usr/bin/pip3，再被 PEP 668 拒绝——用户看到的是「装了两次都失败」。
// 平台必须自己把这个坑填掉，而不是让模型记住 --break-system-packages。
func TestInstallEnvBreaksPEP668(t *testing.T) {
	if v, ok := lookupEnv(InstallEnv(), "PIP_BREAK_SYSTEM_PACKAGES"); !ok || v != "1" {
		t.Fatalf("PIP_BREAK_SYSTEM_PACKAGES 必须是 1，实际 %q（存在=%v）", v, ok)
	}
}

func lookupEnv(env []string, key string) (string, bool) {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v, true
		}
	}
	return "", false
}
