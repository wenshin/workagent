package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/workagent/auto-agent/internal/storage/localfs"
)

type fakeSecrets struct{ values map[string]string }

func (f fakeSecrets) Get(_ context.Context, scope, name string) (string, error) {
	if v, ok := f.values[scope+"/"+name]; ok {
		return v, nil
	}
	return "", errors.New("不存在")
}

func secretTestRuntime() *toolRuntime {
	return &toolRuntime{
		secrets: fakeSecrets{values: map[string]string{"plugin/erp_token": "s3cr3t"}},
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// 声明过的名字才能取到值。
func TestSecretGetReturnsDeclared(t *testing.T) {
	rt := secretTestRuntime()
	in := invokeInput{Secrets: map[string]bool{"erp_token": true}}

	out, err := rt.secretGet(context.Background(), in, json.RawMessage(`{"name":"erp_token"}`))
	if err != nil {
		t.Fatalf("取凭证失败: %v", err)
	}
	got, _ := out.(map[string]any)
	if got["value"] != "s3cr3t" {
		t.Fatalf("凭证值 = %v，期望 s3cr3t", got["value"])
	}
}

// 没声明的名字必须被拒绝，不能因为它恰好存在就放行——
// 否则「写了 secret.use」就等于「能读整个 plugin scope」。
func TestSecretGetRejectsUndeclared(t *testing.T) {
	rt := secretTestRuntime()
	in := invokeInput{Secrets: map[string]bool{"erp_token": true}}

	if _, err := rt.secretGet(context.Background(), in, json.RawMessage(`{"name":"other_token"}`)); err == nil {
		t.Fatal("未声明的凭证名应该被拒绝")
	}
}

// 声明了但凭证不存在：报错要能定位，而不是返回空串让请求带着半个凭证出去。
func TestSecretGetMissingValue(t *testing.T) {
	rt := secretTestRuntime()
	in := invokeInput{Secrets: map[string]bool{"absent": true}}

	if _, err := rt.secretGet(context.Background(), in, json.RawMessage(`{"name":"absent"}`)); err == nil {
		t.Fatal("不存在的凭证应该报错")
	}
}

// 配置对脚本可见，且不需要在 capabilities 里声明：读的是安装方
// 为这个插件自己填的值，不涉及跨主体授权。
func TestHostCallConfigGet(t *testing.T) {
	rt := secretTestRuntime()
	in := invokeInput{Config: map[string]string{"region": "intl"}}

	out, err := rt.hostCall(context.Background(), in, rpcFrame{Method: MethodConfigGet})
	if err != nil {
		t.Fatalf("取配置失败：%v", err)
	}
	got, _ := out.(map[string]any)
	cfg, _ := got["config"].(map[string]string)
	if cfg["region"] != "intl" {
		t.Fatalf("配置没下发：%v", got)
	}
}

// 插件进程的命令行必须用「执行体里的」deno，不能用控制面解析出来的绝对路径。
//
// 容器化之后两者不是同一个文件系统：控制面在 macOS 上给出 /Users/.../deno，
// 执行体里没有这个文件，现场报错是
// `bwrap: execvp /Users/.../deno: No such file or directory`——
// 进程起来就退，控制面只读到 EOF（「插件进程已退出: 读取插件输出失败: EOF」）。
func TestProcSpecUsesExecutorDeno(t *testing.T) {
	fs, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatalf("初始化本地存储: %v", err)
	}
	rt := &toolRuntime{
		fs:         fs,
		deno:       "/Users/someone/.nvm/versions/node/v18/bin/deno",
		denoBin:    "deno",
		runtimeDir: "/host/plugin-runtime",
	}
	spec, err := rt.procSpec(invokeInput{
		Dir:   "spaces/s/files/plugins/dingtalk_bot",
		Slug:  "dingtalk_bot",
		Entry: "index.ts",
	}, true)
	if err != nil {
		t.Fatalf("构造沙箱规格: %v", err)
	}
	if spec.Cmd[0] != "deno" {
		t.Fatalf("命令行第一个元素应为执行体内的 deno，得到 %q", spec.Cmd[0])
	}
	if !spec.Resident {
		t.Fatal("常驻插件应标记 Resident：否则会被墙钟超时杀掉")
	}
}

// 只声明 http.request 的插件也必须拿到 HTTPS_PROXY：
// 沙箱是 --unshare-net，没有代理时 https:// 直连只是重试到超时，
// 而且审计里连一条「拒绝」都没有——白名单看起来生效，实际是被绕过的。
func TestProcSpecProxyWithoutWS(t *testing.T) {
	fs, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatalf("初始化本地存储: %v", err)
	}
	rt := &toolRuntime{fs: fs, denoBin: "deno", runtimeDir: "/host/plugin-runtime"}
	eg := newEgressPool(nil)
	t.Cleanup(func() { eg.Close() })
	rt.egress = eg

	spec, err := rt.procSpec(invokeInput{
		Dir:      "spaces/s/files/plugins/demo",
		Slug:     "demo",
		Entry:    "index.ts",
		SpaceID:  "s",
		NetHosts: []string{"api.example.com"},
	}, true)
	if err != nil {
		t.Fatalf("构造沙箱规格: %v", err)
	}
	if spec.Proxy == "" {
		t.Fatal("声明了 http.request 就该建代理")
	}
	var hasProxyEnv, envAllowed bool
	for _, e := range spec.Env {
		if strings.HasPrefix(e, "HTTPS_PROXY=") {
			hasProxyEnv = true
		}
	}
	for _, a := range spec.Cmd {
		if a == "--allow-env=WA_SPACE_DIR,WA_EGRESS_PROXY,HTTPS_PROXY" {
			envAllowed = true
		}
	}
	if !hasProxyEnv {
		t.Fatalf("没有 ws.connect 时也必须注入 HTTPS_PROXY，环境变量: %v", spec.Env)
	}
	if !envAllowed {
		t.Fatalf("deno 必须被允许读 HTTPS_PROXY，否则注入等于没注入：%v", spec.Cmd)
	}
}
