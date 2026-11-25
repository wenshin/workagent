package plugin

import (
	"context"
	"slices"
	"testing"

	"github.com/workagent/auto-agent/internal/sandbox"
	"github.com/workagent/auto-agent/internal/storage/localfs"
)

// 预检命令的每个标志都被 deno 的版本差异坑过一次，写错就是「永远失败」：
//   - `cache` 不认 --cached-only（2.9.4 / 2.9.8 都报 unexpected argument）；
//   - `check` 在 2.9.4 不认 --no-check。
//
// 这里钉住「只用两个版本都有的标志」，以及 lock 的两条分支。
func TestVerifyDepsArgs(t *testing.T) {
	entry := "file:///plugin/main.ts"

	withLock := verifyDepsArgs("deno", entry, true)
	for _, want := range []string{
		"deno", "cache",
		// 不执行、不联网、不读作者的 deno.json
		"--no-check", "--no-config",
		// 依赖只从插件目录解析，缺了就报错
		"--node-modules-dir=manual", "--vendor",
		// 按作者验过的版本装
		"--lock=/plugin/deno.lock", "--frozen-lockfile",
		entry,
	} {
		if !slices.Contains(withLock, want) {
			t.Fatalf("预检参数缺少 %q: %v", want, withLock)
		}
	}
	// 这两个是「看起来更严格、实际直接报错」的写法：绝不能出现。
	for _, banned := range []string{"--cached-only", "run", "check", "--frozen"} {
		if slices.Contains(withLock, banned) {
			t.Fatalf("预检参数不该有 %q（deno cache 不认）: %v", banned, withLock)
		}
	}

	// 没有 lock 时不能带 --lock + --frozen-lockfile：lock 不存在会被判成
	// 「lockfile 过期」，预检必然失败。
	noLock := verifyDepsArgs("deno", entry, false)
	if !slices.Contains(noLock, "--no-lock") {
		t.Fatalf("没有 lock 时应带 --no-lock: %v", noLock)
	}
	for _, banned := range []string{"--frozen-lockfile", "--lock=/plugin/deno.lock"} {
		if slices.Contains(noLock, banned) {
			t.Fatalf("没有 lock 时不该带 %q: %v", banned, noLock)
		}
	}
	if noLock[len(noLock)-1] != entry {
		t.Fatalf("入口要在最后: %v", noLock)
	}
}

// 预检只对常驻工具生效，且必须先把清单读出来。
//
// 两个 case 都用 denoBin="" 的 runtime：走到 deno 就会报「没有 deno」，
// 返回 nil 说明在更早的判据上就放行了。
func TestVerifyResidentDepsSkipsWhatItCannotCheck(t *testing.T) {
	fs, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatalf("初始化本地存储: %v", err)
	}
	m := &Manager{rt: &toolRuntime{fs: fs, runner: sandbox.NewLocal()}, o: Options{Storage: fs}}

	// 目录里没有清单：交给后续加载去报错，预检不该编一句「依赖没装」。
	if err := m.verifyResidentDeps(context.Background(), "sp1", "demo"); err != nil {
		t.Fatalf("没有清单时应放行: %v", err)
	}

	// 一次性工具：进程就是调用本身，缺依赖在第一次调用时报错更直接。
	dir := DraftKey(fs, "sp1", "demo")
	putBundleFile(t, fs, dir+"/"+ManifestFile, toolManifest(""))
	putBundleFile(t, fs, dir+"/main.ts", "console.log(1);\n")
	if err := m.verifyResidentDeps(context.Background(), "sp1", "demo"); err != nil {
		t.Fatalf("非常驻插件不该被预检拦: %v", err)
	}
}
