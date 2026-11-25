package plugin

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/workagent/auto-agent/internal/storage"
	"github.com/workagent/auto-agent/internal/storage/localfs"
)

func putBundleFile(t *testing.T, fs storage.Storage, key, body string) {
	t.Helper()
	if _, err := fs.Put(context.Background(), key, strings.NewReader(body), int64(len(body))); err != nil {
		t.Fatalf("写入 %s 失败: %v", key, err)
	}
}

// 依赖目录不能进 bundle。
//
// 装依赖是插件开发的第一步（deno cache 会往插件目录里写 node_modules/ 与
// vendor/），而 bundle 只允许 200 个文件 / 5MB：依赖一旦被算进去，
// 「装完依赖就试运行不了、也发布不了」，且报错指向的是「插件太大」，
// 和真正的原因（装了依赖）毫无关系。
func TestBundleSkipsDependencyDirs(t *testing.T) {
	fs, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatalf("初始化本地存储: %v", err)
	}
	const root = "plugins/demo"
	putBundleFile(t, fs, root+"/"+ManifestFile, toolManifest(`, "version": "1.0.0"`))
	putBundleFile(t, fs, root+"/main.ts", `import ms from "npm:ms@2";`)
	putBundleFile(t, fs, root+"/deno.lock", `{"version":"5"}`)
	// 超过 maxBundleFiles 的文件数，以及超过 maxBundleBytes 的单文件。
	for i := 0; i < maxBundleFiles+50; i++ {
		putBundleFile(t, fs, fmt.Sprintf("%s/node_modules/ms/f%03d.js", root, i), "x")
	}
	putBundleFile(t, fs, root+"/vendor/jsr.io/@std/fmt/mod.ts", strings.Repeat("x", maxBundleBytes+1))

	// publishing=true：走的是发布的严格校验路径，正是原来会失败的那条。
	b, err := LoadBundle(context.Background(), fs, root, true)
	if err != nil {
		t.Fatalf("依赖目录不该进 bundle: %v", err)
	}
	for _, deps := range []string{"node_modules/ms/f000.js", "vendor/jsr.io/@std/fmt/mod.ts"} {
		if _, ok := b.Files[deps]; ok {
			t.Errorf("依赖不应出现在 bundle 里: %s", deps)
		}
	}
	for _, want := range []string{"main.ts", "deno.lock"} {
		if _, ok := b.Files[want]; !ok {
			t.Errorf("源码与锁文件必须在 bundle 里: %s", want)
		}
	}
}

// 引用了远程依赖却没有 deno.lock：发布只提醒，不阻断。
func TestDepsUnlocked(t *testing.T) {
	cases := []struct {
		name  string
		files map[string][]byte
		want  bool
	}{
		{
			name:  "有 npm 依赖有锁",
			files: map[string][]byte{"main.ts": []byte(`import ms from "npm:ms@2";`), "deno.lock": []byte(`{}`)},
			want:  false,
		},
		{
			name:  "有 jsr 依赖没锁",
			files: map[string][]byte{"main.ts": []byte(`import { format } from "jsr:@std/fmt@1/bytes";`)},
			want:  true,
		},
		{
			name:  "只有本地 import",
			files: map[string][]byte{"main.ts": []byte(`import { x } from "./lib/x.ts";`)},
			want:  false,
		},
		{
			name:  "注释里提到 npm 不算依赖",
			files: map[string][]byte{"main.ts": []byte("// 不要用 npm: 包\nconsole.log(1);")},
			want:  false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := &Bundle{Files: c.files}
			if got := b.DepsUnlocked(); got != c.want {
				t.Fatalf("DepsUnlocked()=%v，期望 %v", got, c.want)
			}
		})
	}
}
